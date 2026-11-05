package luna

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Adding calendars from the page: Dor signs in to Google through one of the configured
// apps, picks calendars from that account's list, and Luna keeps the account's key in an
// encrypted file next to its cache. Those calendars join the hub without a restart.

var calendarConnectPageTemplate = mustParseTemplate("calendar-connect.html", "document.html", "footer.html")

const (
	calendarConnectCookie = "luna_calendar_connect"
	calendarConnectFor    = 10 * time.Minute
	calendarTasksID       = "@tasks"
)

var calendarConnectScopes = []string{
	"https://www.googleapis.com/auth/calendar.readonly",
	"https://www.googleapis.com/auth/calendar.events",
	"https://www.googleapis.com/auth/contacts.readonly",
	"https://www.googleapis.com/auth/contacts.other.readonly",
	"https://www.googleapis.com/auth/tasks.readonly",
}

var calendarGoogleAuthURL = "https://accounts.google.com/o/oauth2/v2/auth"

type calendarConnectConfig struct {
	// The callback address registered on every app, such as
	// https://homepage.example.com/api/calendar/connect/callback
	Redirect string `yaml:"redirect"`
	// Any long random string; the store is encrypted with a key made from it
	Key string `yaml:"key"`
	// Where the encrypted accounts file lives, in a writable folder
	Store string               `yaml:"store"`
	Apps  []calendarConnectApp `yaml:"apps"`
}

// A Google "web application" OAuth client. A Workspace app set to internal only accepts
// its own domain's accounts; an external one accepts any Google account.
type calendarConnectApp struct {
	Label        string `yaml:"label"`
	ClientID     string `yaml:"client-id"`
	ClientSecret string `yaml:"client-secret"`
	// Also read the Workspace directory for guest photos and names
	Directory bool `yaml:"directory"`
}

type calendarConnect struct {
	config  *calendarConnectConfig
	key     []byte
	mu      sync.Mutex
	pending map[string]*calendarPending
}

// A sign-in between Google's answer and Dor picking calendars
type calendarPending struct {
	app       int
	created   time.Time
	back      string
	Email     string
	refresh   string
	Calendars []calendarChoice
}

type calendarChoice struct {
	ID      string
	Name    string
	Primary bool
	Added   bool
}

type calendarStore struct {
	Accounts []calendarAccount `json:"accounts"`
}

type calendarAccount struct {
	Email     string           `json:"email"`
	ClientID  string           `json:"clientId"`
	Refresh   string           `json:"refresh"`
	Calendars []calendarStored `json:"calendars"`
}

type calendarStored struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Remind bool   `json:"remind"`
}

func newCalendarConnect(config *calendarConnectConfig) (*calendarConnect, error) {
	if config.Redirect == "" || config.Store == "" || len(config.Key) < 16 {
		return nil, errors.New("needs redirect, store and a key of at least 16 characters")
	}
	if _, err := url.Parse(config.Redirect); err != nil {
		return nil, fmt.Errorf("redirect: %v", err)
	}
	if len(config.Apps) == 0 {
		return nil, errors.New("needs at least one app")
	}
	for i, app := range config.Apps {
		if app.ClientID == "" || app.ClientSecret == "" {
			return nil, fmt.Errorf("app %d needs client-id and client-secret", i+1)
		}
		if app.Label == "" {
			config.Apps[i].Label = "Google account"
		}
	}
	key := sha256.Sum256([]byte(config.Key))
	return &calendarConnect{config: config, key: key[:], pending: map[string]*calendarPending{}}, nil
}

func (connect *calendarConnect) app(clientID string) (calendarConnectApp, bool) {
	for _, app := range connect.config.Apps {
		if app.ClientID == clientID {
			return app, true
		}
	}
	return calendarConnectApp{}, false
}

func (connect *calendarConnect) load() (calendarStore, error) {
	var store calendarStore
	sealed, err := os.ReadFile(connect.config.Store)
	if errors.Is(err, fs.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return store, err
	}
	block, err := aes.NewCipher(connect.key)
	if err != nil {
		return store, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return store, err
	}
	if len(sealed) < gcm.NonceSize() {
		return store, errors.New("store is too short")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return store, errors.New("store does not open with this key")
	}
	return store, json.Unmarshal(plain, &store)
}

// save writes the store encrypted, through a temporary file so a crash cannot leave half of it
func (connect *calendarConnect) save(store calendarStore) error {
	plain, err := json.Marshal(store)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(connect.key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	if err := os.MkdirAll(filepath.Dir(connect.config.Store), 0o700); err != nil {
		return err
	}
	temporary := connect.config.Store + ".tmp"
	if err := os.WriteFile(temporary, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, connect.config.Store)
}

// feeds turns the stored accounts into hub feeds
func (connect *calendarConnect) feeds() ([]calendarFeed, error) {
	store, err := connect.load()
	if err != nil {
		return nil, err
	}
	var feeds []calendarFeed
	for _, account := range store.Accounts {
		app, ok := connect.app(account.ClientID)
		if !ok {
			slog.Warn("Connected calendar account has no app in the config any more", "account", account.Email)
			continue
		}
		for _, stored := range account.Calendars {
			feeds = append(feeds, calendarConnectFeed(app, account, stored))
		}
	}
	return feeds, nil
}

func calendarConnectFeed(app calendarConnectApp, account calendarAccount, stored calendarStored) calendarFeed {
	remind := stored.Remind
	feed := calendarFeed{
		Name: stored.Name, Remind: &remind, connected: account.Email,
		Google: &calendarGoogle{ClientID: app.ClientID, ClientSecret: app.ClientSecret, RefreshToken: account.Refresh, Calendar: stored.ID},
	}
	if stored.ID == calendarTasksID {
		feed.Google.Calendar, feed.Google.Tasks = "primary", true
		feed.Every = durationField(time.Hour)
	}
	return feed
}

func randomToken() string {
	raw := make([]byte, 24)
	rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// sameOrigin keeps another site from posting these forms through Dor's browser
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host == r.Host
}

// backPath keeps the return address on this Luna, as a path
func (a *application) backPath(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Path == "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") {
		return a.Config.Server.BaseURL + "/"
	}
	return parsed.Path
}

type calendarConnectPage struct {
	App     *application
	Page    *page
	Request templateRequestData
	// What the page shows: the account list, or the calendar picker after a sign-in
	Picking   *calendarPending
	State     string
	Back      string
	Apps      []calendarConnectApp
	Accounts  []calendarAccount
	Config    []string
	Error     string
	ActionURL string
}

func (a *application) renderCalendarConnect(w http.ResponseWriter, r *http.Request, page calendarConnectPage) {
	page.App = a
	page.ActionURL = a.Config.Server.BaseURL + "/api/calendar/connect"
	a.populateTemplateRequestData(&page.Request, r)
	var out bytes.Buffer
	if err := calendarConnectPageTemplate.Execute(&out, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Write(out.Bytes())
}

func (a *application) calendarConnectList(w http.ResponseWriter, r *http.Request, back, problem string) {
	hub := a.calendar
	store, err := hub.connect.load()
	if err != nil && problem == "" {
		problem = "The saved accounts could not be read: " + err.Error()
	}
	var config []string
	for _, feed := range hub.feedList() {
		if feed.connected == "" && !feed.removed {
			config = append(config, feed.Name)
		}
	}
	a.renderCalendarConnect(w, r, calendarConnectPage{Back: back, Apps: hub.connect.config.Apps, Accounts: store.Accounts, Config: config, Error: problem})
}

func (a *application) handleCalendarConnect(w http.ResponseWriter, r *http.Request) {
	if !a.isAuthorized(w, r) {
		http.Redirect(w, r, a.Config.Server.BaseURL+"/login", http.StatusSeeOther)
		return
	}
	back := r.URL.Query().Get("back")
	if back == "" {
		back = r.Header.Get("Referer")
	}
	a.calendarConnectList(w, r, a.backPath(back), "")
}

func (a *application) handleCalendarConnectStart(w http.ResponseWriter, r *http.Request) {
	if !a.isAuthorized(w, r) || !sameOrigin(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	connect := a.calendar.connect
	index, err := strconv.Atoi(r.FormValue("app"))
	if err != nil || index < 0 || index >= len(connect.config.Apps) {
		http.Error(w, "unknown app", http.StatusBadRequest)
		return
	}
	state := randomToken()
	connect.mu.Lock()
	for key, pending := range connect.pending {
		if time.Since(pending.created) > calendarConnectFor {
			delete(connect.pending, key)
		}
	}
	connect.pending[state] = &calendarPending{app: index, created: time.Now(), back: a.backPath(r.FormValue("back"))}
	connect.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: calendarConnectCookie, Value: state, Path: a.Config.Server.BaseURL + "/api/calendar/connect",
		MaxAge: int(calendarConnectFor.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	app := connect.config.Apps[index]
	scopes := calendarConnectScopes
	if app.Directory {
		scopes = append(slices.Clone(scopes), "https://www.googleapis.com/auth/directory.readonly")
	}
	target := calendarGoogleAuthURL + "?" + url.Values{
		"client_id": {app.ClientID}, "redirect_uri": {connect.config.Redirect}, "response_type": {"code"},
		"scope": {strings.Join(scopes, " ")}, "access_type": {"offline"}, "prompt": {"consent select_account"}, "state": {state},
	}.Encode()
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// pendingFor is the sign-in this browser started, by its cookie
func (connect *calendarConnect) pendingFor(r *http.Request, state string) (*calendarPending, bool) {
	cookie, err := r.Cookie(calendarConnectCookie)
	if err != nil || state == "" || cookie.Value != state {
		return nil, false
	}
	connect.mu.Lock()
	defer connect.mu.Unlock()
	pending, ok := connect.pending[state]
	if !ok || time.Since(pending.created) > calendarConnectFor {
		return nil, false
	}
	return pending, true
}

func (a *application) handleCalendarConnectCallback(w http.ResponseWriter, r *http.Request) {
	if !a.isAuthorized(w, r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	connect := a.calendar.connect
	state := r.URL.Query().Get("state")
	pending, ok := connect.pendingFor(r, state)
	if !ok {
		a.calendarConnectList(w, r, a.Config.Server.BaseURL+"/", "That sign-in expired or came from another browser. Start again.")
		return
	}
	if problem := r.URL.Query().Get("error"); problem != "" {
		a.calendarConnectList(w, r, pending.back, "Google did not connect the account ("+problem+").")
		return
	}
	app := connect.config.Apps[pending.app]
	token, err := cockpitFetch(mustFormRequest(calendarGoogleTokenURL, url.Values{
		"code": {r.URL.Query().Get("code")}, "client_id": {app.ClientID}, "client_secret": {app.ClientSecret},
		"redirect_uri": {connect.config.Redirect}, "grant_type": {"authorization_code"},
	}))
	if err != nil {
		a.calendarConnectList(w, r, pending.back, "Google did not hand over the key: "+cockpitErrorText(err))
		return
	}
	refresh, access := token.Get("refresh_token").String(), token.Get("access_token").String()
	if refresh == "" {
		a.calendarConnectList(w, r, pending.back, "Google sent no lasting key. Remove Luna at myaccount.google.com/permissions for that account and connect again.")
		return
	}
	request, err := cockpitRequest("GET", calendarGoogleAPI+"/users/me/calendarList?maxResults=250", map[string]string{"Authorization": "Bearer " + access}, nil)
	if err != nil {
		a.calendarConnectList(w, r, pending.back, err.Error())
		return
	}
	list, err := cockpitFetch(request)
	if err != nil {
		a.calendarConnectList(w, r, pending.back, "Could not list that account's calendars: "+cockpitErrorText(err))
		return
	}
	store, _ := connect.load()
	added := map[string]bool{}
	var email string
	for _, item := range list.Get("items").Array() {
		if item.Get("primary").Bool() {
			email = item.Get("id").String()
		}
	}
	for _, account := range store.Accounts {
		if strings.EqualFold(account.Email, email) {
			for _, stored := range account.Calendars {
				added[stored.ID] = true
			}
		}
	}
	var choices []calendarChoice
	for _, item := range list.Get("items").Array() {
		name := item.Get("summaryOverride").String()
		if name == "" {
			name = item.Get("summary").String()
		}
		id := item.Get("id").String()
		choices = append(choices, calendarChoice{ID: id, Name: name, Primary: item.Get("primary").Bool(), Added: added[id]})
	}
	// The main calendar first, then the rest by name
	slices.SortStableFunc(choices, func(x, y calendarChoice) int {
		if x.Primary != y.Primary {
			return map[bool]int{true: -1, false: 1}[x.Primary]
		}
		return strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name))
	})
	choices = append(choices, calendarChoice{ID: calendarTasksID, Name: "Tasks", Added: added[calendarTasksID]})
	connect.mu.Lock()
	pending.Email, pending.refresh, pending.Calendars = email, refresh, choices
	connect.mu.Unlock()
	a.renderCalendarConnect(w, r, calendarConnectPage{Picking: pending, State: state, Back: pending.back})
}

func mustFormRequest(target string, form url.Values) *http.Request {
	request, _ := http.NewRequest("POST", target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return request
}

func (a *application) handleCalendarConnectSave(w http.ResponseWriter, r *http.Request) {
	if !a.isAuthorized(w, r) || !sameOrigin(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	hub := a.calendar
	connect := hub.connect
	state := r.FormValue("state")
	pending, ok := connect.pendingFor(r, state)
	if !ok || pending.refresh == "" {
		a.calendarConnectList(w, r, a.Config.Server.BaseURL+"/", "That sign-in expired. Start again.")
		return
	}
	app := connect.config.Apps[pending.app]
	taken := map[string]bool{}
	for _, info := range hub.Calendars() {
		taken[strings.ToLower(info.Name)] = true
	}
	var picked []calendarStored
	for i, choice := range pending.Calendars {
		if choice.Added || r.FormValue("pick-"+strconv.Itoa(i)) == "" {
			continue
		}
		name := strings.TrimSpace(r.FormValue("name-" + strconv.Itoa(i)))
		if name == "" {
			name = choice.Name
		}
		// The legend switches calendars by name, so two cannot share one
		for base, n := name, 2; taken[strings.ToLower(name)]; n++ {
			name = fmt.Sprintf("%s %d", base, n)
		}
		taken[strings.ToLower(name)] = true
		picked = append(picked, calendarStored{ID: choice.ID, Name: name, Remind: r.FormValue("remind-"+strconv.Itoa(i)) != ""})
	}
	store, err := connect.load()
	if err != nil {
		a.calendarConnectList(w, r, pending.back, "The saved accounts could not be read, so nothing was added: "+err.Error())
		return
	}
	index := slices.IndexFunc(store.Accounts, func(account calendarAccount) bool {
		return strings.EqualFold(account.Email, pending.Email) && account.ClientID == app.ClientID
	})
	if index < 0 {
		store.Accounts = append(store.Accounts, calendarAccount{Email: pending.Email, ClientID: app.ClientID})
		index = len(store.Accounts) - 1
	}
	account := &store.Accounts[index]
	account.Refresh = pending.refresh
	account.Calendars = append(account.Calendars, picked...)
	if err := connect.save(store); err != nil {
		a.calendarConnectList(w, r, pending.back, "Could not save: "+err.Error())
		return
	}
	var feeds []calendarFeed
	for _, stored := range picked {
		feeds = append(feeds, calendarConnectFeed(app, *account, stored))
	}
	hub.addFeeds(feeds)
	connect.mu.Lock()
	delete(connect.pending, state)
	connect.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: calendarConnectCookie, Path: a.Config.Server.BaseURL + "/api/calendar/connect", MaxAge: -1})
	http.Redirect(w, r, pending.back, http.StatusSeeOther)
}

func (a *application) handleCalendarConnectRemove(w http.ResponseWriter, r *http.Request) {
	if !a.isAuthorized(w, r) || !sameOrigin(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	hub := a.calendar
	email, id, back := r.FormValue("account"), r.FormValue("calendar"), a.backPath(r.FormValue("back"))
	store, err := hub.connect.load()
	if err != nil {
		a.calendarConnectList(w, r, back, "The saved accounts could not be read: "+err.Error())
		return
	}
	for i := range store.Accounts {
		if store.Accounts[i].Email == email {
			store.Accounts[i].Calendars = slices.DeleteFunc(store.Accounts[i].Calendars, func(stored calendarStored) bool { return stored.ID == id })
		}
	}
	// An account with nothing left is forgotten, key and all
	store.Accounts = slices.DeleteFunc(store.Accounts, func(account calendarAccount) bool { return len(account.Calendars) == 0 })
	if err := hub.connect.save(store); err != nil {
		a.calendarConnectList(w, r, back, "Could not save: "+err.Error())
		return
	}
	feedID := id
	if id == calendarTasksID {
		feedID = "primary"
	}
	hub.removeFeeds(email, feedID, id == calendarTasksID)
	http.Redirect(w, r, a.Config.Server.BaseURL+"/api/calendar/connect?back="+url.QueryEscape(back), http.StatusSeeOther)
}

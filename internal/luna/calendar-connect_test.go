package luna

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCalendarConnectStoreIsEncrypted(t *testing.T) {
	store := filepath.Join(t.TempDir(), "accounts.enc")
	connect, err := newCalendarConnect(&calendarConnectConfig{Redirect: "http://luna.test/cb", Key: "a long random test key", Store: store,
		Apps: []calendarConnectApp{{ClientID: "id", ClientSecret: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := calendarStore{Accounts: []calendarAccount{{Email: "a@x.org", ClientID: "id", Refresh: "refresh-token-value", Calendars: []calendarStored{{ID: "primary", Name: "A", Remind: true}}}}}
	if err := connect.save(want); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(store)
	if strings.Contains(string(raw), "refresh-token-value") || strings.Contains(string(raw), "a@x.org") {
		t.Fatal("store is readable without the key")
	}
	got, err := connect.load()
	if err != nil || len(got.Accounts) != 1 || got.Accounts[0].Refresh != "refresh-token-value" {
		t.Fatalf("got %+v, %v", got, err)
	}
	other, _ := newCalendarConnect(&calendarConnectConfig{Redirect: "http://luna.test/cb", Key: "a different key here", Store: store,
		Apps: []calendarConnectApp{{ClientID: "id", ClientSecret: "secret"}}})
	if _, err := other.load(); err == nil {
		t.Fatal("store opened with the wrong key")
	}
}

func TestCalendarConnectFlowAddsAndRemovesCalendars(t *testing.T) {
	google := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			if r.Form.Get("code") != "the-code" || r.Form.Get("redirect_uri") != "http://luna.test/api/calendar/connect/callback" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"access_token":"access","refresh_token":"new-refresh","expires_in":3600}`))
		case "/users/me/calendarList":
			w.Write([]byte(`{"items":[{"id":"team@group.calendar.google.com","summary":"Team"},{"id":"sam@x.org","summary":"sam@x.org","primary":true}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer google.Close()
	calendarGoogleTokenURL, calendarGoogleAPI = google.URL+"/token", google.URL
	defer func() {
		calendarGoogleTokenURL, calendarGoogleAPI = "https://oauth2.googleapis.com/token", "https://www.googleapis.com/calendar/v3"
	}()

	store := filepath.Join(t.TempDir(), "accounts.enc")
	hub, err := newCalendarHub(calendarConfig{
		Feeds: []calendarFeed{{Name: "Team", URL: "http://ical.test/team.ics"}},
		Connect: &calendarConnectConfig{Redirect: "http://luna.test/api/calendar/connect/callback", Key: "a long random test key", Store: store,
			Apps: []calendarConnectApp{{Label: "Google account", ClientID: "web-id", ClientSecret: "web-secret"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	app := &application{calendar: hub}
	do := func(method, target string, form url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		var request *http.Request
		if form != nil {
			request = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			request = httptest.NewRequest(method, target, nil)
		}
		if origin != "" {
			request.Header.Set("Origin", origin)
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		recorder := httptest.NewRecorder()
		switch {
		case strings.HasSuffix(request.URL.Path, "/start"):
			app.handleCalendarConnectStart(recorder, request)
		case strings.HasSuffix(request.URL.Path, "/callback"):
			app.handleCalendarConnectCallback(recorder, request)
		case strings.HasSuffix(request.URL.Path, "/save"):
			app.handleCalendarConnectSave(recorder, request)
		case strings.HasSuffix(request.URL.Path, "/remove"):
			app.handleCalendarConnectRemove(recorder, request)
		}
		return recorder
	}

	if code := do("POST", "http://luna.test/api/calendar/connect/start", url.Values{"app": {"0"}}, nil, "https://evil.test").Code; code != http.StatusForbidden {
		t.Fatalf("start from another site: %d", code)
	}
	start := do("POST", "http://luna.test/api/calendar/connect/start", url.Values{"app": {"0"}, "back": {"/calendar"}}, nil, "http://luna.test")
	location, _ := url.Parse(start.Header().Get("Location"))
	state := location.Query().Get("state")
	if start.Code != http.StatusSeeOther || state == "" || location.Query().Get("client_id") != "web-id" || !strings.Contains(location.Query().Get("scope"), "calendar.events") {
		t.Fatalf("start: %d %s", start.Code, location)
	}
	cookie := start.Result().Cookies()[0]

	// Google's answer with the right state but no cookie is someone else's sign-in
	if body := do("GET", "http://luna.test/api/calendar/connect/callback?state="+state+"&code=the-code", nil, nil, "").Body.String(); !strings.Contains(body, "expired") {
		t.Fatalf("callback without the cookie was accepted: %.400s", do("GET", "http://luna.test/api/calendar/connect/callback?state="+state+"&code=the-code", nil, nil, "").Body.String())
	}
	picker := do("GET", "http://luna.test/api/calendar/connect/callback?state="+state+"&code=the-code", nil, cookie, "")
	body := picker.Body.String()
	if picker.Code != http.StatusOK || !strings.Contains(body, "sam@x.org") || strings.Index(body, `value="sam@x.org"`) > strings.Index(body, `value="Team"`) || !strings.Contains(body, "Tasks") {
		t.Fatalf("picker: %d\n%s", picker.Code, body)
	}

	// Pick the main calendar and Team (renamed off the config's "Team" automatically), with reminders on the first only
	saved := do("POST", "http://luna.test/api/calendar/connect/save", url.Values{
		"state": {state}, "pick-0": {"on"}, "name-0": {"Sam"}, "remind-0": {"on"}, "pick-1": {"on"}, "name-1": {"Team"},
	}, cookie, "http://luna.test")
	if saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != "/calendar" {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	names := []string{}
	for _, info := range hub.Calendars() {
		names = append(names, info.Name)
	}
	if strings.Join(names, ",") != "Team,Sam,Team 2" {
		t.Fatalf("calendars %v", names)
	}
	feeds := hub.feedList()
	if feeds[1].Google.RefreshToken != "new-refresh" || feeds[1].Google.ClientSecret != "web-secret" || !*feeds[1].Remind || *feeds[2].Remind {
		t.Fatalf("feeds %+v %+v", feeds[1], feeds[2])
	}

	// A restart reads them back from the encrypted store
	again, err := newCalendarHub(calendarConfig{Connect: hub.connect.config})
	if err != nil || len(again.Calendars()) != 2 {
		t.Fatalf("after restart: %v %v", again.Calendars(), err)
	}

	removed := do("POST", "http://luna.test/api/calendar/connect/remove", url.Values{"account": {"sam@x.org"}, "calendar": {"team@group.calendar.google.com"}}, nil, "http://luna.test")
	if removed.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d", removed.Code)
	}
	names = names[:0]
	for _, info := range hub.Calendars() {
		names = append(names, info.Name)
	}
	if strings.Join(names, ",") != "Team,Sam" {
		t.Fatalf("after remove %v", names)
	}
	left, _ := hub.connect.load()
	if len(left.Accounts) != 1 || len(left.Accounts[0].Calendars) != 1 {
		t.Fatalf("store after remove %+v", left)
	}
}

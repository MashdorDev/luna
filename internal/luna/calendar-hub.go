package luna

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The calendar hub reads every feed on its own clock and keeps the next two weeks of events.
// It runs once per app, outside page views, and Home and the week widget both read from it.
// Reminders are browser notifications sent by calendar-view.js from any open Luna tab.

const (
	calendarTick         = 30 * time.Second
	calendarMeetingEvery = 10 * time.Minute
	calendarHolidayEvery = 6 * time.Hour
	calendarRetry        = time.Minute
	calendarHorizonDays  = 15
	// The week view starts on Monday, up to 6 days back, plus a day for zones
	calendarLookbackDays = 8
	// A meeting moved this recently is flagged, since a moved meeting is easy to miss
	calendarChangedWithin = 72 * time.Hour
)

type calendarConfig struct {
	Timezone  string         `yaml:"timezone"`
	Feeds     []calendarFeed `yaml:"feeds"`
	Reminders struct {
		Before []durationField `yaml:"before"`
	} `yaml:"reminders"`
	// Extra zones a meeting's details show its time in
	Zones []calendarZone `yaml:"zones"`
}

type calendarZone struct {
	Timezone string `json:"zone" yaml:"timezone"`
	Label    string `json:"label" yaml:"label"`
}

type calendarFeed struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Holidays bool   `yaml:"holidays"`
	// Off until the viewer turns it on
	Hidden bool          `yaml:"hidden"`
	Remind *bool         `yaml:"remind"`
	Every  durationField `yaml:"every"`
	// Read through the Calendar API instead of an iCal url, for Workspace
	// accounts whose admin hides the secret iCal address
	Google *calendarGoogle `yaml:"google"`
}

type calendarGoogle struct {
	ClientID     string `yaml:"client-id"`
	ClientSecret string `yaml:"client-secret"`
	RefreshToken string `yaml:"refresh-token"`
	Calendar     string `yaml:"calendar"`
	// Only the contact birthdays Google adds to the calendar, which every other feed skips
	Birthdays bool `yaml:"birthdays"`
}

type calendarEvent struct {
	Name      string
	Start     time.Time
	End       time.Time
	AllDay    bool
	Calendars []string
	Holiday   bool
	Link      string
	Location  string
	Notes     string
	Tentative bool
	// Only the Calendar API reports guests and Dor's own answer; iCal feeds leave these empty
	Attendees []calendarAttendee
	Response  string
	Open      string
	Files     []calendarFile
	Dial      *calendarDial
	Recurring bool
	// Minutes before the start that Dor set on this meeting in Google, on top of the usual ones
	Reminders []int
	// When the hub saw this meeting move to a new time. The feeds' own last-modified
	// stamps change on every RSVP, so they cannot tell a move from an answer.
	Changed time.Time
	uid     string
	remind  bool
}

type calendarInfo struct {
	Name     string
	Hidden   bool
	Holidays bool
	Color    int
}

type calendarFeedState struct {
	events  []calendarEvent
	fetched time.Time
	err     error
	// Calendar API access token and when it stops working
	token        string
	tokenExpires time.Time
	// When each meeting, by uid, was last seen moving
	moved map[string]time.Time
}

type calendarHub struct {
	feeds   []calendarFeed
	zone    *time.Location
	remind  []int
	zones   []calendarZone
	mu      sync.Mutex
	states  []calendarFeedState
	version atomic.Int64
}

func newCalendarHub(c calendarConfig) (*calendarHub, error) {
	hub := &calendarHub{feeds: c.Feeds, zone: time.Local}
	if c.Timezone != "" {
		zone, err := time.LoadLocation(c.Timezone)
		if err != nil {
			return nil, fmt.Errorf("calendar timezone: %v", err)
		}
		hub.zone = zone
	}
	for i := range hub.feeds {
		feed := &hub.feeds[i]
		if feed.Name == "" || (feed.URL == "") == (feed.Google == nil) {
			return nil, fmt.Errorf("calendar feed %d needs a name and either a url or google", i+1)
		}
		if g := feed.Google; g != nil {
			if g.ClientID == "" || g.ClientSecret == "" || g.RefreshToken == "" {
				return nil, fmt.Errorf("calendar feed %s: google needs client-id, client-secret and refresh-token", feed.Name)
			}
			if g.Calendar == "" {
				g.Calendar = "primary"
			}
		}
		if feed.Every == 0 {
			feed.Every = durationField(ternary(feed.Holidays, calendarHolidayEvery, calendarMeetingEvery))
		}
		feed.URL = calendarFeedURL(feed.URL)
	}
	hub.remind = []int{}
	for _, before := range c.Reminders.Before {
		hub.remind = append(hub.remind, int(time.Duration(before).Minutes()))
	}
	hub.zones = []calendarZone{}
	for _, zone := range c.Zones {
		if _, err := time.LoadLocation(zone.Timezone); err != nil {
			return nil, fmt.Errorf("calendar zone: %v", err)
		}
		hub.zones = append(hub.zones, zone)
	}
	hub.states = make([]calendarFeedState, len(hub.feeds))
	return hub, nil
}

// glance-ical returns 5 events a year ahead by default, so a busy calendar lost meetings
// next week behind ones next month. The look back keeps this week's earlier days.
func calendarFeedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.Query().Has("url") {
		return raw
	}
	query := parsed.Query()
	query.Set("limit", "500")
	query.Set("horizon_days", fmt.Sprint(calendarHorizonDays))
	query.Set("lookback_days", fmt.Sprint(calendarLookbackDays))
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func (hub *calendarHub) run(ctx context.Context) {
	hub.tick(time.Now())
	ticker := time.NewTicker(calendarTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			hub.tick(now)
		}
	}
}

// Version changes whenever a feed brings different events, so widgets can redraw at once
func (hub *calendarHub) Version() int64 {
	return hub.version.Load()
}

func (hub *calendarHub) tick(now time.Time) {
	var wg sync.WaitGroup
	for i := range hub.feeds {
		hub.mu.Lock()
		due := now.Sub(hub.states[i].fetched) >= time.Duration(hub.feeds[i].Every)
		if due {
			// Set before fetching so a slow feed is not started twice
			hub.states[i].fetched = now
		}
		hub.mu.Unlock()
		if !due {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			events, err := hub.fetchFeed(i)
			hub.mu.Lock()
			defer hub.mu.Unlock()
			hub.states[i].err = err
			// A failed fetch keeps the last events, so a Google hiccup does not empty the agenda.
			// It retries in a minute: glance-ical can still be starting when Luna boots.
			if err == nil {
				hub.states[i].markMoved(events, now)
				if !reflect.DeepEqual(hub.states[i].events, events) {
					hub.version.Add(1)
				}
				hub.states[i].events = events
			} else {
				hub.states[i].fetched = now.Add(calendarRetry - time.Duration(hub.feeds[i].Every))
				slog.Error("Calendar feed failed", "calendar", hub.feeds[i].Name, "error", cockpitErrorText(err))
			}
		}()
	}
	wg.Wait()
}

// markMoved flags events whose time changed since the last fetch. Recurring meetings
// share one uid in iCal feeds, so only a uid that appears once on both sides is compared.
func (state *calendarFeedState) markMoved(events []calendarEvent, now time.Time) {
	single := func(list []calendarEvent) map[string]calendarEvent {
		found, count := map[string]calendarEvent{}, map[string]int{}
		for _, event := range list {
			if event.uid != "" {
				found[event.uid] = event
				count[event.uid]++
			}
		}
		for uid, n := range count {
			if n > 1 {
				delete(found, uid)
			}
		}
		return found
	}
	if state.moved == nil {
		state.moved = map[string]time.Time{}
	}
	before, after := single(state.events), single(events)
	for uid, event := range after {
		if old, ok := before[uid]; ok && (!old.Start.Equal(event.Start) || !old.End.Equal(event.End)) {
			state.moved[uid] = now
		}
	}
	for uid, at := range state.moved {
		if now.Sub(at) > calendarChangedWithin {
			delete(state.moved, uid)
		}
	}
	for i := range events {
		events[i].Changed = state.moved[events[i].uid]
	}
}

var (
	calendarAnchorPattern = regexp.MustCompile(`(?is)<a\s[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)
	calendarBreakPattern  = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>`)
	calendarTagPattern    = regexp.MustCompile(`<[^>]*>`)
	calendarGapPattern    = regexp.MustCompile(`\n{3,}`)
	// Google wraps its join details in a line of -::~:~::~ that means nothing to a reader
	calendarRulePattern = regexp.MustCompile(`(?m)^-?::~[:~]*-?$`)
)

const calendarNotesLimit = 4000

// Google sends invite notes as HTML. The page shows them as text, so a link keeps its
// address next to its label instead of disappearing with the tag.
func calendarPlainText(raw string) string {
	text := calendarAnchorPattern.ReplaceAllStringFunc(raw, func(anchor string) string {
		parts := calendarAnchorPattern.FindStringSubmatch(anchor)
		href, label := html.UnescapeString(parts[1]), strings.TrimSpace(html.UnescapeString(calendarTagPattern.ReplaceAllString(parts[2], "")))
		if label == "" || label == href {
			return href
		}
		return label + " (" + href + ")"
	})
	text = calendarBreakPattern.ReplaceAllString(text, "\n")
	text = html.UnescapeString(calendarTagPattern.ReplaceAllString(text, ""))
	text = calendarRulePattern.ReplaceAllString(text, "")
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\u00a0")
	}
	text = strings.TrimSpace(calendarGapPattern.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
	if runes := []rune(text); len(runes) > calendarNotesLimit {
		text = string(runes[:calendarNotesLimit]) + "…"
	}
	return text
}

var calendarLinkPattern = regexp.MustCompile(`https://[^\s"<>]*(meet\.google\.com|zoom\.us|teams\.microsoft\.com|teams\.live\.com|whereby\.com|discord\.gg|discord\.com)[^\s"<>]*`)

// One event as a feed reports it, before it becomes a calendarEvent
type calendarRaw struct {
	name, uid, status, location, description string
	start, end                               time.Time
	allDay                                   bool
	link                                     string
	attendees                                []calendarAttendee
	response, open                           string
	files                                    []calendarFile
	dial                                     *calendarDial
	recurring                                bool
	reminders                                []int
}

// A file attached to the invite, usually a Google Doc for the meeting
type calendarFile struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Icon  string `json:"icon,omitempty"`
}

// The phone way into a video call
type calendarDial struct {
	Number string `json:"number"`
	URI    string `json:"uri"`
	Pin    string `json:"pin,omitempty"`
}

// A guest as the Calendar API reports them. Status is accepted, declined, tentative
// or needsAction (no answer yet).
type calendarAttendee struct {
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	Status    string `json:"status,omitempty"`
	Organizer bool   `json:"organizer,omitempty"`
	Optional  bool   `json:"optional,omitempty"`
	Self      bool   `json:"self,omitempty"`
	// What they wrote with their answer
	Comment string `json:"comment,omitempty"`
}

func (hub *calendarHub) fetchFeed(index int) ([]calendarEvent, error) {
	feed := hub.feeds[index]
	var raws []calendarRaw
	var err error
	if feed.Google != nil {
		raws, err = hub.fetchGoogle(index)
	} else {
		raws, err = fetchICal(feed.URL)
	}
	if err != nil {
		return nil, err
	}
	remind := !feed.Holidays && (feed.Remind == nil || *feed.Remind)
	var events []calendarEvent
	for _, raw := range raws {
		if strings.EqualFold(raw.status, "CANCELLED") || raw.start.IsZero() {
			continue
		}
		start, end := raw.start, raw.end
		if !end.After(start) {
			end = start.Add(time.Hour)
		}
		event := calendarEvent{
			Name: raw.name, Start: start, End: end, AllDay: raw.allDay,
			Calendars: []string{feed.Name}, Holiday: feed.Holidays, uid: raw.uid, remind: remind,
		}
		// All-day events come as midnight UTC. Keep the date and let each viewer place it in their own day.
		if event.AllDay {
			event.Start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
			event.End = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
			if !event.End.After(event.Start) {
				event.End = event.Start.AddDate(0, 0, 1)
			}
		}
		event.Tentative = strings.EqualFold(raw.status, "TENTATIVE")
		event.Location = strings.TrimSpace(raw.location)
		event.Notes = calendarPlainText(raw.description)
		event.Link = raw.link
		event.Attendees, event.Response, event.Open = raw.attendees, raw.response, raw.open
		event.Files, event.Dial, event.Recurring, event.Reminders = raw.files, raw.dial, raw.recurring, raw.reminders
		if event.Link == "" {
			event.Link = calendarLinkPattern.FindString(event.Location)
		}
		if event.Link == "" {
			event.Link = calendarLinkPattern.FindString(event.Notes)
		}
		events = append(events, event)
	}
	return events, nil
}

func fetchICal(feedURL string) ([]calendarRaw, error) {
	request, err := cockpitRequest("GET", feedURL, nil, nil)
	if err != nil {
		return nil, err
	}
	data, err := cockpitFetch(request)
	if err != nil {
		return nil, err
	}
	var raws []calendarRaw
	for _, entry := range data.Get("events").Array() {
		start, err := time.Parse(time.RFC3339, entry.Get("start").String())
		if err != nil {
			continue
		}
		end, _ := time.Parse(time.RFC3339, entry.Get("end").String())
		raws = append(raws, calendarRaw{
			name: entry.Get("name").String(), uid: entry.Get("uid").String(), status: entry.Get("status").String(),
			location: entry.Get("location").String(), description: entry.Get("description").String(),
			start: start, end: end, allDay: entry.Get("all_day").Bool(),
		})
	}
	return raws, nil
}

var (
	calendarGoogleTokenURL = "https://oauth2.googleapis.com/token"
	calendarGoogleAPI      = "https://www.googleapis.com/calendar/v3"
)

// googleToken trades the refresh token for an access token, kept until a minute before it expires
func (hub *calendarHub) googleToken(index int, now time.Time) (string, error) {
	hub.mu.Lock()
	token, expires := hub.states[index].token, hub.states[index].tokenExpires
	hub.mu.Unlock()
	if token != "" && now.Before(expires) {
		return token, nil
	}
	g := hub.feeds[index].Google
	form := url.Values{
		"client_id": {g.ClientID}, "client_secret": {g.ClientSecret},
		"refresh_token": {g.RefreshToken}, "grant_type": {"refresh_token"},
	}
	request, err := http.NewRequest("POST", calendarGoogleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	data, err := cockpitFetch(request)
	if err != nil {
		return "", fmt.Errorf("google sign-in: %w", err)
	}
	token = data.Get("access_token").String()
	if token == "" {
		return "", fmt.Errorf("google sign-in returned no access token")
	}
	hub.mu.Lock()
	hub.states[index].token = token
	hub.states[index].tokenExpires = now.Add(time.Duration(data.Get("expires_in").Int())*time.Second - time.Minute)
	hub.mu.Unlock()
	return token, nil
}

func (hub *calendarHub) fetchGoogle(index int) ([]calendarRaw, error) {
	now := time.Now()
	token, err := hub.googleToken(index, now)
	if err != nil {
		return nil, err
	}
	query := url.Values{
		"singleEvents": {"true"}, "orderBy": {"startTime"}, "maxResults": {"500"},
		"timeMin": {now.AddDate(0, 0, -calendarLookbackDays).Format(time.RFC3339)},
		"timeMax": {now.AddDate(0, 0, calendarHorizonDays).Format(time.RFC3339)},
	}
	birthdays := hub.feeds[index].Google.Birthdays
	if birthdays {
		query.Set("eventTypes", "birthday")
	}
	target := calendarGoogleAPI + "/calendars/" + url.PathEscape(hub.feeds[index].Google.Calendar) + "/events?" + query.Encode()
	request, err := cockpitRequest("GET", target, map[string]string{"Authorization": "Bearer " + token}, nil)
	if err != nil {
		return nil, err
	}
	data, err := cockpitFetch(request)
	if err != nil {
		// A revoked or expired token gets a fresh one on the retry
		hub.mu.Lock()
		hub.states[index].token = ""
		hub.mu.Unlock()
		return nil, err
	}
	var raws []calendarRaw
	for _, item := range data.Get("items").Array() {
		// Working location, focus time and out of office are blocks of Dor's own day, not meetings
		kind := item.Get("eventType").String()
		if birthdays != (kind == "birthday") || (!birthdays && kind != "" && kind != "default" && kind != "fromGmail") {
			continue
		}
		raw := calendarRaw{
			name: item.Get("summary").String(), uid: item.Get("id").String(), status: item.Get("status").String(),
			location: item.Get("location").String(), description: item.Get("description").String(),
			link: item.Get("hangoutLink").String(), open: item.Get("htmlLink").String(),
		}
		account := ""
		for _, entry := range item.Get("attendees").Array() {
			if entry.Get("resource").Bool() {
				continue // meeting rooms
			}
			attendee := calendarAttendee{
				Name: entry.Get("displayName").String(), Email: entry.Get("email").String(), Status: entry.Get("responseStatus").String(),
				Organizer: entry.Get("organizer").Bool(), Optional: entry.Get("optional").Bool(), Self: entry.Get("self").Bool(),
				Comment: entry.Get("comment").String(),
			}
			if attendee.Self {
				raw.response, account = attendee.Status, attendee.Email
			}
			raw.attendees = append(raw.attendees, attendee)
		}
		if raw.response == "declined" {
			continue
		}
		// Someone else's event with no guest list still has an organizer worth showing
		if len(raw.attendees) == 0 && !item.Get("organizer.self").Bool() && item.Get("organizer.email").String() != "" {
			raw.attendees = []calendarAttendee{{
				Name: item.Get("organizer.displayName").String(), Email: item.Get("organizer.email").String(), Organizer: true,
			}}
		}
		if account == "" && item.Get("organizer.self").Bool() {
			account = item.Get("organizer.email").String()
		}
		// Google opens the event in whichever account is signed in first unless told which
		if raw.open != "" && account != "" {
			raw.open += "&authuser=" + url.QueryEscape(account)
		}
		for _, entry := range item.Get("conferenceData.entryPoints").Array() {
			switch entry.Get("entryPointType").String() {
			case "video":
				if raw.link == "" {
					raw.link = entry.Get("uri").String()
				}
			case "phone":
				if raw.dial == nil {
					raw.dial = &calendarDial{Number: entry.Get("label").String(), URI: entry.Get("uri").String(), Pin: entry.Get("pin").String()}
				}
			}
		}
		for _, file := range item.Get("attachments").Array() {
			raw.files = append(raw.files, calendarFile{Title: file.Get("title").String(), URL: file.Get("fileUrl").String(), Icon: file.Get("iconLink").String()})
		}
		raw.recurring = item.Get("recurringEventId").String() != ""
		for _, reminder := range item.Get("reminders.overrides").Array() {
			if reminder.Get("method").String() == "popup" {
				raw.reminders = append(raw.reminders, int(reminder.Get("minutes").Int()))
			}
		}
		if day := item.Get("start.date").String(); day != "" {
			raw.allDay = true
			raw.start, _ = time.Parse(time.DateOnly, day)
			raw.end, _ = time.Parse(time.DateOnly, item.Get("end.date").String())
		} else {
			raw.start, _ = time.Parse(time.RFC3339, item.Get("start.dateTime").String())
			raw.end, _ = time.Parse(time.RFC3339, item.Get("end.dateTime").String())
		}
		raws = append(raws, raw)
	}
	return raws, nil
}

// Events returns every event that overlaps from..to, one entry per meeting even when
// several invited accounts each report it, sorted by start.
func (hub *calendarHub) Events(from, to time.Time) []calendarEvent {
	hub.mu.Lock()
	var events []calendarEvent
	for _, state := range hub.states {
		for _, event := range state.events {
			if event.End.After(from) && event.Start.Before(to) {
				event.Calendars = slices.Clone(event.Calendars)
				events = append(events, event)
			}
		}
	}
	hub.mu.Unlock()
	return mergeCalendarEvents(events)
}

// calendarEntry is an event as the page scripts read it. All-day events carry dates
// instead of times so each viewer places them in their own day.
type calendarEntry struct {
	Name      string             `json:"name"`
	Start     string             `json:"start,omitempty"`
	End       string             `json:"end,omitempty"`
	Day       string             `json:"day,omitempty"`
	EndDay    string             `json:"endDay,omitempty"`
	Calendars []string           `json:"calendars"`
	Holiday   bool               `json:"holiday,omitempty"`
	Link      string             `json:"link,omitempty"`
	Location  string             `json:"location,omitempty"`
	Notes     string             `json:"notes,omitempty"`
	Quiet     bool               `json:"quiet,omitempty"`
	Tentative bool               `json:"tentative,omitempty"`
	Changed   string             `json:"changed,omitempty"`
	Attendees []calendarAttendee `json:"attendees,omitempty"`
	Response  string             `json:"response,omitempty"`
	Open      string             `json:"open,omitempty"`
	Files     []calendarFile     `json:"files,omitempty"`
	Reminders []int              `json:"reminders,omitempty"`
	Dial      *calendarDial      `json:"dial,omitempty"`
	Recurring bool               `json:"recurring,omitempty"`
	Color     int                `json:"color"`
}

// JSON is what the details panel opens with
func (entry calendarEntry) JSON() string {
	encoded, _ := json.Marshal(entry)
	return string(encoded)
}

func (hub *calendarHub) Entries(from, to time.Time) []calendarEntry {
	colors := map[string]int{}
	for _, info := range hub.Calendars() {
		colors[info.Name] = info.Color
	}
	events := hub.Events(from, to)
	entries := make([]calendarEntry, len(events))
	order := make([]time.Time, len(events))
	for i, event := range events {
		entries[i] = calendarEntry{
			Name: event.Name, Calendars: event.Calendars, Holiday: event.Holiday, Link: event.Link, Location: event.Location, Notes: event.Notes, Color: colors[event.Calendars[0]],
			Quiet: !event.remind || event.Holiday, Tentative: event.Tentative,
			Attendees: event.Attendees, Response: event.Response, Open: event.Open,
			Files: event.Files, Dial: event.Dial, Recurring: event.Recurring, Reminders: event.Reminders,
			Start: event.Start.Format(time.RFC3339), End: event.End.Format(time.RFC3339),
		}
		order[i] = event.Start
		if !event.Changed.IsZero() && time.Since(event.Changed) < calendarChangedWithin {
			entries[i].Changed = event.Changed.Format(time.RFC3339)
		}
		if event.AllDay {
			entries[i].Start, entries[i].End = "", ""
			entries[i].Day, entries[i].EndDay = event.Start.Format("2006-01-02"), event.End.Format("2006-01-02")
			// Midnight UTC is the evening before in Toronto, so all-day events sort by
			// their own date's midnight to stay at the top of their day
			order[i] = time.Date(event.Start.Year(), event.Start.Month(), event.Start.Day(), 0, 0, 0, 0, hub.zone)
		}
	}
	indexes := make([]int, len(entries))
	for i := range indexes {
		indexes[i] = i
	}
	slices.SortStableFunc(indexes, func(a, b int) int { return order[a].Compare(order[b]) })
	sorted := make([]calendarEntry, len(entries))
	for i, index := range indexes {
		sorted[i] = entries[index]
	}
	return sorted
}

func mergeCalendarEvents(events []calendarEvent) []calendarEvent {
	slices.SortStableFunc(events, func(a, b calendarEvent) int {
		if order := a.Start.Compare(b.Start); order != 0 {
			return order
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	var merged []calendarEvent
	for _, event := range events {
		if last := len(merged) - 1; last >= 0 && merged[last].Start.Equal(event.Start) && strings.EqualFold(merged[last].Name, event.Name) {
			for _, name := range event.Calendars {
				if !slices.Contains(merged[last].Calendars, name) {
					merged[last].Calendars = append(merged[last].Calendars, name)
				}
			}
			merged[last].remind = merged[last].remind || event.remind
			merged[last].Holiday = merged[last].Holiday && event.Holiday
			if merged[last].Link == "" {
				merged[last].Link = event.Link
			}
			if merged[last].Location == "" {
				merged[last].Location = event.Location
			}
			if len(event.Notes) > len(merged[last].Notes) {
				merged[last].Notes = event.Notes
			}
			merged[last].Tentative = merged[last].Tentative && event.Tentative
			if len(merged[last].Attendees) == 0 {
				merged[last].Attendees, merged[last].Response, merged[last].Open = event.Attendees, event.Response, event.Open
			}
			if len(merged[last].Files) == 0 {
				merged[last].Files = event.Files
			}
			if merged[last].Dial == nil {
				merged[last].Dial = event.Dial
			}
			merged[last].Recurring = merged[last].Recurring || event.Recurring
			for _, minutes := range event.Reminders {
				if !slices.Contains(merged[last].Reminders, minutes) {
					merged[last].Reminders = append(merged[last].Reminders, minutes)
				}
			}
			if event.Changed.After(merged[last].Changed) {
				merged[last].Changed = event.Changed
			}
			continue
		}
		merged = append(merged, event)
	}
	return merged
}

func (hub *calendarHub) Errors() []string {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	var errs []string
	for i, state := range hub.states {
		if state.err != nil {
			errs = append(errs, fmt.Sprintf("Calendar %s: %s", hub.feeds[i].Name, cockpitErrorText(state.err)))
		}
	}
	return errs
}

func (hub *calendarHub) Calendars() []calendarInfo {
	infos := make([]calendarInfo, len(hub.feeds))
	for i, feed := range hub.feeds {
		infos[i] = calendarInfo{Name: feed.Name, Hidden: feed.Hidden, Holidays: feed.Holidays, Color: i % 8}
	}
	return infos
}

func (hub *calendarHub) Zone() string {
	return hub.zone.String()
}

// The calendars a viewer sees switched off until they turn them on, for the page scripts
func (hub *calendarHub) HiddenJSON() string {
	names := []string{}
	for _, feed := range hub.feeds {
		if feed.Hidden {
			names = append(names, feed.Name)
		}
	}
	encoded, _ := json.Marshal(names)
	return string(encoded)
}

// RemindJSON lists the minutes before a meeting that the page notifies at
func (hub *calendarHub) RemindJSON() string {
	encoded, _ := json.Marshal(hub.remind)
	return string(encoded)
}

func (hub *calendarHub) ZonesJSON() string {
	encoded, _ := json.Marshal(hub.zones)
	return string(encoded)
}

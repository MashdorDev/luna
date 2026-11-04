package luna

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCalendarFeedURLWidensTheWindow(t *testing.T) {
	got, _ := url.Parse(calendarFeedURL("http://glance-ical:8076/events?url=https%3A%2F%2Fx.test%2Fa.ics&limit=5&horizon_days=365&lookback_days=0"))
	query := got.Query()
	if query.Get("limit") != "500" || query.Get("horizon_days") != "15" || query.Get("lookback_days") != "1" {
		t.Fatalf("got %s", got.RawQuery)
	}
	if query.Get("url") != "https://x.test/a.ics" {
		t.Fatalf("lost the feed address: %s", query.Get("url"))
	}
	if plain := "https://x.test/feed.json"; calendarFeedURL(plain) != plain {
		t.Fatal("changed a URL that is not a glance-ical one")
	}
}

func TestMergeCalendarEventsJoinsInvitedAccounts(t *testing.T) {
	at := time.Date(2026, 11, 5, 15, 0, 0, 0, time.UTC)
	merged := mergeCalendarEvents([]calendarEvent{
		{Name: "Standup", Start: at, Calendars: []string{"Work"}},
		{Name: "Lunch", Start: at.Add(-time.Hour), Calendars: []string{"Family"}},
		{Name: "standup", Start: at, Calendars: []string{"Personal"}, remind: true, Link: "https://meet.google.com/x"},
	})
	if len(merged) != 2 || merged[0].Name != "Lunch" {
		t.Fatalf("got %+v", merged)
	}
	standup := merged[1]
	if strings.Join(standup.Calendars, ",") != "Work,Personal" || !standup.remind || standup.Link == "" {
		t.Fatalf("got %+v", standup)
	}
}

func TestCalendarHubReadsFeeds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	day := now.AddDate(0, 0, 1).Format("2006-01-02")
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{
			{"name": "Client call", "uid": "a", "start": at(10 * time.Minute), "end": at(40 * time.Minute), "location": "https://zoom.us/j/1", "status": "TENTATIVE", "last_modified": at(-time.Hour)},
			{"name": "Dropped", "uid": "b", "start": at(10 * time.Minute), "end": at(time.Hour), "status": "CANCELLED"},
			{"name": "Day off", "uid": "c", "all_day": true, "start": day + "T00:00:00+00:00", "end": now.AddDate(0, 0, 2).Format("2006-01-02") + "T00:00:00+00:00"},
		}})
	}))
	defer feed.Close()

	quiet := false
	config := calendarConfig{Timezone: "America/Toronto", Feeds: []calendarFeed{{Name: "Work", URL: feed.URL}, {Name: "Family", URL: feed.URL, Remind: &quiet}}}
	config.Reminders.Before = []durationField{durationField(10 * time.Minute), durationField(time.Minute)}
	config.Zones = []calendarZone{{Timezone: "Asia/Jerusalem", Label: "Tel Aviv"}}
	hub, err := newCalendarHub(config)
	if err != nil {
		t.Fatal(err)
	}
	hub.tick(now)
	if hub.RemindJSON() != "[10,1]" || !strings.Contains(hub.ZonesJSON(), `"Tel Aviv"`) {
		t.Fatalf("got %s %s", hub.RemindJSON(), hub.ZonesJSON())
	}
	entries := hub.Entries(now.Add(-24*time.Hour), now.Add(72*time.Hour))
	if len(entries) != 2 {
		t.Fatalf("got %+v", entries)
	}
	call, off := entries[0], entries[1]
	if call.Name != "Client call" || call.Link != "https://zoom.us/j/1" || !call.Tentative || call.Changed != "" {
		t.Fatalf("got %+v", call)
	}
	// One of the two calendars wants reminders, so the merged meeting gets them
	if call.Quiet || strings.Join(call.Calendars, ",") != "Work,Family" {
		t.Fatalf("got %+v", call)
	}
	if off.Day != day || off.Start != "" {
		t.Fatalf("all-day event moved: %+v", off)
	}
}

func TestCalendarHubKeepsEventsWhenAFeedFails(t *testing.T) {
	var fail atomic.Bool
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"events":[{"name":"Sync","start":"2026-11-05T15:00:00+00:00","end":"2026-11-05T16:00:00+00:00"}]}`))
	}))
	defer feed.Close()
	hub, err := newCalendarHub(calendarConfig{Feeds: []calendarFeed{{Name: "Work", URL: feed.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
	hub.tick(now)
	fail.Store(true)
	hub.tick(now.Add(calendarMeetingEvery))
	fail.Store(false)
	hub.tick(now.Add(calendarMeetingEvery + calendarRetry))
	if errs := hub.Errors(); len(errs) != 0 {
		t.Fatalf("did not retry a minute after failing: %v", errs)
	}
	fail.Store(true)
	hub.tick(now.Add(2*calendarMeetingEvery + calendarRetry))
	if len(hub.Events(now, now.Add(24*time.Hour))) != 1 {
		t.Fatal("a failed refresh emptied the calendar")
	}
	if errs := hub.Errors(); len(errs) != 1 || !strings.Contains(errs[0], "Work") {
		t.Fatalf("got %v", errs)
	}
}

func TestCalendarPlainTextKeepsLinkAddresses(t *testing.T) {
	raw := `Agenda:<br>1. Budget &amp; scope<br>-::~:~::~:~:~:~::~:~::-<br><br><a href="https://zoom.us/j/9?pwd=x">Join Zoom Meeting</a><br><a href="https://x.test/doc">https://x.test/doc</a>`
	want := "Agenda:\n1. Budget & scope\n\nJoin Zoom Meeting (https://zoom.us/j/9?pwd=x)\nhttps://x.test/doc"
	if got := calendarPlainText(raw); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCalendarHubReadsGoogleCalendarAPI(t *testing.T) {
	var tokens atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			r.ParseForm()
			if r.Form.Get("refresh_token") != "refresh" || r.Form.Get("client_secret") != "secret" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			tokens.Add(1)
			w.Write([]byte(`{"access_token":"access","expires_in":3600}`))
		case r.URL.Path == "/calendars/primary/events":
			if r.Header.Get("Authorization") != "Bearer access" || r.URL.Query().Get("singleEvents") != "true" {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"items":[
				{"summary":"Standup","status":"confirmed","start":{"dateTime":"2026-11-05T10:00:00-05:00"},"end":{"dateTime":"2026-11-05T10:15:00-05:00"},"hangoutLink":"https://meet.google.com/abc-defg-hij","updated":"2026-11-04T12:00:00Z"},
				{"summary":"Offsite","status":"tentative","start":{"date":"2026-11-06"},"end":{"date":"2026-11-07"}},
				{"summary":"Skipped","start":{"dateTime":"2026-11-05T12:00:00Z"},"end":{"dateTime":"2026-11-05T13:00:00Z"},"attendees":[{"self":true,"responseStatus":"declined"}]},
				{"summary":"Home","eventType":"workingLocation","start":{"date":"2026-11-05"},"end":{"date":"2026-11-06"}},
				{"summary":"Focus","eventType":"focusTime","start":{"dateTime":"2026-11-05T14:00:00Z"},"end":{"dateTime":"2026-11-05T16:00:00Z"}},
				{"summary":"Away","eventType":"outOfOffice","start":{"dateTime":"2026-11-06T14:00:00Z"},"end":{"dateTime":"2026-11-06T22:00:00Z"}}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	calendarGoogleTokenURL, calendarGoogleAPI = server.URL+"/token", server.URL
	defer func() {
		calendarGoogleTokenURL, calendarGoogleAPI = "https://oauth2.googleapis.com/token", "https://www.googleapis.com/calendar/v3"
	}()

	hub, err := newCalendarHub(calendarConfig{Feeds: []calendarFeed{{Name: "Lost Tribe", Google: &calendarGoogle{ClientID: "id", ClientSecret: "secret", RefreshToken: "refresh"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		events, err := hub.fetchFeed(0)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 2 {
			t.Fatalf("want 2 events, got %d", len(events))
		}
		if events[0].Link != "https://meet.google.com/abc-defg-hij" || events[0].End.Sub(events[0].Start) != 15*time.Minute {
			t.Fatalf("standup: %+v", events[0])
		}
		if !events[1].AllDay || !events[1].Tentative || !events[1].Start.Equal(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("offsite: %+v", events[1])
		}
	}
	if tokens.Load() != 1 {
		t.Fatalf("access token fetched %d times, want once", tokens.Load())
	}
	if _, err := newCalendarHub(calendarConfig{Feeds: []calendarFeed{{Name: "x", Google: &calendarGoogle{ClientID: "id"}}}}); err == nil {
		t.Fatal("google feed without a refresh token was accepted")
	}
}

func TestCalendarMarkMovedFlagsOnlyTimeChanges(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return now.Add(time.Duration(h) * time.Hour) }
	state := calendarFeedState{}
	first := []calendarEvent{
		{Name: "Sync", uid: "sync", Start: at(2), End: at(3)},
		{Name: "Weekly", uid: "weekly", Start: at(4), End: at(5)},
		{Name: "Weekly", uid: "weekly", Start: at(28), End: at(29)},
	}
	state.markMoved(first, now)
	state.events = first
	if !first[0].Changed.IsZero() {
		t.Fatal("first fetch flagged a move")
	}
	// Notes change (an RSVP), Weekly instances shift: neither is a move we can tell
	second := []calendarEvent{
		{Name: "Sync", uid: "sync", Start: at(2), End: at(3), Notes: "Alice accepted"},
		{Name: "Weekly", uid: "weekly", Start: at(5), End: at(6)},
		{Name: "Weekly", uid: "weekly", Start: at(29), End: at(30)},
	}
	state.markMoved(second, now.Add(time.Minute))
	state.events = second
	for _, event := range second {
		if !event.Changed.IsZero() {
			t.Fatalf("%s flagged without a single-uid time change", event.Name)
		}
	}
	third := []calendarEvent{{Name: "Sync", uid: "sync", Start: at(3), End: at(4)}}
	state.markMoved(third, now.Add(2*time.Minute))
	state.events = third
	if !third[0].Changed.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("move not flagged: %v", third[0].Changed)
	}
	later := []calendarEvent{{Name: "Sync", uid: "sync", Start: at(3), End: at(4)}}
	state.markMoved(later, now.Add(time.Hour))
	if !later[0].Changed.Equal(now.Add(2 * time.Minute)) {
		t.Fatal("the move was forgotten on the next fetch")
	}
	state.markMoved(later, now.Add(80*time.Hour))
	if !later[0].Changed.IsZero() {
		t.Fatal("a move older than 72 hours is still flagged")
	}
}

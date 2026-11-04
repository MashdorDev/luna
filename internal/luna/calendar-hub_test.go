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
	if call.Name != "Client call" || call.Link != "https://zoom.us/j/1" || !call.Tentative || call.Changed == "" {
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

package luna

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

func TestCalendarHubFetchesAndReminds(t *testing.T) {
	now := time.Date(2026, 11, 5, 14, 50, 30, 0, time.UTC)
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{
			{"name": "Client call", "uid": "a", "start": "2026-11-05T15:00:00+00:00", "end": "2026-11-05T15:30:00+00:00", "location": "https://zoom.us/j/1", "status": "CONFIRMED"},
			{"name": "Dropped", "uid": "b", "start": "2026-11-05T15:00:00+00:00", "end": "2026-11-05T16:00:00+00:00", "status": "CANCELLED"},
			{"name": "Day off", "uid": "c", "all_day": true, "start": "2026-11-06T00:00:00+00:00", "end": "2026-11-07T00:00:00+00:00"},
		}})
	}))
	defer feed.Close()

	var mu sync.Mutex
	var sent []map[string]any
	var auth string
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message map[string]any
		json.NewDecoder(r.Body).Decode(&message)
		mu.Lock()
		sent = append(sent, message)
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte("{}"))
	}))
	defer ntfy.Close()

	config := calendarConfig{Timezone: "America/Toronto", Feeds: []calendarFeed{{Name: "Work", URL: feed.URL}}}
	config.Reminders.URL, config.Reminders.Topic, config.Reminders.Token = ntfy.URL, "meetings", "tk"
	config.Reminders.Before = []durationField{durationField(10 * time.Minute), durationField(time.Minute)}
	hub, err := newCalendarHub(config)
	if err != nil {
		t.Fatal(err)
	}

	hub.tick(now)
	hub.tick(now.Add(calendarTick))
	events := hub.Events(now, now.Add(48*time.Hour))
	if len(events) != 2 || events[0].Name != "Client call" || events[0].Link != "https://zoom.us/j/1" {
		t.Fatalf("got %+v", events)
	}
	if day := events[1]; !day.AllDay || day.Start.Format("2006-01-02") != "2026-11-06" {
		t.Fatalf("all-day event moved: %+v", day)
	}

	waitFor := func(count int) {
		t.Helper()
		for range 100 {
			mu.Lock()
			n := len(sent)
			mu.Unlock()
			if n >= count {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("expected %d reminders", count)
	}
	waitFor(1)
	// Later ticks send the 1 minute reminder once and never repeat the 10 minute one
	hub.tick(now.Add(8*time.Minute + 45*time.Second))
	hub.tick(now.Add(9 * time.Minute))
	hub.tick(now.Add(13 * time.Minute))
	waitFor(2)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("sent %d reminders, want 2: %+v", len(sent), sent)
	}
	first := sent[0]
	if first["topic"] != "meetings" || first["title"] != "Client call" || first["click"] != "https://zoom.us/j/1" || auth != "Bearer tk" {
		t.Fatalf("got %+v, auth %q", first, auth)
	}
	if want := "In 10 min, 10:00AM to 10:30AM Toronto. Work"; first["message"] != want {
		t.Fatalf("got %q, want %q", first["message"], want)
	}
	if sent[1]["priority"].(float64) != 5 {
		t.Fatalf("1 minute reminder should be urgent: %+v", sent[1])
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
	if len(hub.Events(now, now.Add(24*time.Hour))) != 1 {
		t.Fatal("a failed refresh emptied the calendar")
	}
	if errs := hub.Errors(); len(errs) != 1 || !strings.Contains(errs[0], "Work") {
		t.Fatalf("got %v", errs)
	}
}

package luna

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
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
// It runs once per app, outside page views, so reminders go out even when no tab is open.
// Home and the week widget both read from it.

const (
	calendarTick         = 30 * time.Second
	calendarMeetingEvery = 10 * time.Minute
	calendarHolidayEvery = 6 * time.Hour
	calendarRetry        = time.Minute
	calendarHorizonDays  = 15
	// A reminder that comes later than this, after a restart or a late feed, would only confuse
	calendarRemindLate = 2 * time.Minute
)

type calendarConfig struct {
	Timezone  string         `yaml:"timezone"`
	Feeds     []calendarFeed `yaml:"feeds"`
	Reminders struct {
		URL    string          `yaml:"url"`
		Topic  string          `yaml:"topic"`
		Token  string          `yaml:"token"`
		Before []durationField `yaml:"before"`
	} `yaml:"reminders"`
}

type calendarFeed struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url"`
	Holidays bool   `yaml:"holidays"`
	// Off until the viewer turns it on
	Hidden bool          `yaml:"hidden"`
	Remind *bool         `yaml:"remind"`
	Every  durationField `yaml:"every"`
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
	uid       string
	remind    bool
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
}

type calendarHub struct {
	feeds     []calendarFeed
	zone      *time.Location
	reminders struct {
		url, topic, token string
		before            []time.Duration
	}

	mu      sync.Mutex
	states  []calendarFeedState
	version atomic.Int64
}

// Reminders already sent. A config reload builds a new hub, so this outlives it.
var (
	calendarSentMu sync.Mutex
	calendarSent   = map[string]time.Time{}
	// Reminders that came due before this process started were sent by the one before it
	calendarProcessStart = time.Now()
)

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
		if feed.Name == "" || feed.URL == "" {
			return nil, fmt.Errorf("calendar feed %d needs a name and a url", i+1)
		}
		if feed.Every == 0 {
			feed.Every = durationField(ternary(feed.Holidays, calendarHolidayEvery, calendarMeetingEvery))
		}
		feed.URL = calendarFeedURL(feed.URL)
	}
	r := c.Reminders
	if r.URL != "" {
		if r.Topic == "" {
			return nil, fmt.Errorf("calendar reminders need a topic")
		}
		hub.reminders.url, hub.reminders.topic, hub.reminders.token = strings.TrimRight(r.URL, "/"), r.Topic, r.Token
		for _, before := range r.Before {
			hub.reminders.before = append(hub.reminders.before, time.Duration(before))
		}
		if len(hub.reminders.before) == 0 {
			hub.reminders.before = []time.Duration{10 * time.Minute}
		}
	}
	hub.states = make([]calendarFeedState, len(hub.feeds))
	return hub, nil
}

// glance-ical returns 5 events a year ahead by default, so a busy calendar lost meetings
// next week behind ones next month. One day back keeps meetings that are still going on.
func calendarFeedURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.Query().Has("url") {
		return raw
	}
	query := parsed.Query()
	query.Set("limit", "500")
	query.Set("horizon_days", fmt.Sprint(calendarHorizonDays))
	query.Set("lookback_days", "1")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func (hub *calendarHub) run(ctx context.Context) {
	hub.tick(ctx, time.Now())
	ticker := time.NewTicker(calendarTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			hub.tick(ctx, now)
		}
	}
}

// Version changes whenever a feed brings different events, so widgets can redraw at once
func (hub *calendarHub) Version() int64 {
	return hub.version.Load()
}

func (hub *calendarHub) tick(ctx context.Context, now time.Time) {
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
	// A reloaded config has a new hub, and this one must not remind alongside it
	if ctx.Err() == nil {
		hub.remind(now)
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

func (hub *calendarHub) fetchFeed(index int) ([]calendarEvent, error) {
	feed := hub.feeds[index]
	request, err := cockpitRequest("GET", feed.URL, nil, nil)
	if err != nil {
		return nil, err
	}
	data, err := cockpitFetch(request)
	if err != nil {
		return nil, err
	}
	remind := !feed.Holidays && (feed.Remind == nil || *feed.Remind)
	var events []calendarEvent
	for _, entry := range data.Get("events").Array() {
		if strings.EqualFold(entry.Get("status").String(), "CANCELLED") {
			continue
		}
		start, err := time.Parse(time.RFC3339, entry.Get("start").String())
		if err != nil {
			continue
		}
		end, err := time.Parse(time.RFC3339, entry.Get("end").String())
		if err != nil || !end.After(start) {
			end = start.Add(time.Hour)
		}
		event := calendarEvent{
			Name: entry.Get("name").String(), Start: start, End: end, AllDay: entry.Get("all_day").Bool(),
			Calendars: []string{feed.Name}, Holiday: feed.Holidays, uid: entry.Get("uid").String(), remind: remind,
		}
		// All-day events come as midnight UTC. Keep the date and let each viewer place it in their own day.
		if event.AllDay {
			event.Start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
			event.End = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
			if !event.End.After(event.Start) {
				event.End = event.Start.AddDate(0, 0, 1)
			}
		}
		event.Location = strings.TrimSpace(entry.Get("location").String())
		event.Notes = calendarPlainText(entry.Get("description").String())
		event.Link = calendarLinkPattern.FindString(event.Location)
		if event.Link == "" {
			event.Link = calendarLinkPattern.FindString(event.Notes)
		}
		events = append(events, event)
	}
	return events, nil
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
	Name      string   `json:"name"`
	Start     string   `json:"start,omitempty"`
	End       string   `json:"end,omitempty"`
	Day       string   `json:"day,omitempty"`
	EndDay    string   `json:"endDay,omitempty"`
	Calendars []string `json:"calendars"`
	Holiday   bool     `json:"holiday,omitempty"`
	Link      string   `json:"link,omitempty"`
	Location  string   `json:"location,omitempty"`
	Notes     string   `json:"notes,omitempty"`
	Color     int      `json:"color"`
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
			Start: event.Start.Format(time.RFC3339), End: event.End.Format(time.RFC3339),
		}
		order[i] = event.Start
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

func (hub *calendarHub) remind(now time.Time) {
	if hub.reminders.url == "" {
		return
	}
	for _, event := range hub.Events(now, now.Add(24*time.Hour)) {
		if event.AllDay || event.Holiday || !event.remind {
			continue
		}
		for _, before := range hub.reminders.before {
			at := event.Start.Add(-before)
			if now.Before(at) || now.Sub(at) > calendarRemindLate || at.Before(calendarProcessStart.Add(-calendarTick)) {
				continue
			}
			key := fmt.Sprintf("%s|%s|%d|%s", event.uid, strings.ToLower(event.Name), event.Start.Unix(), before)
			calendarSentMu.Lock()
			_, done := calendarSent[key]
			if !done {
				calendarSent[key] = event.Start
			}
			calendarSentMu.Unlock()
			if !done {
				go hub.notify(event, before)
			}
		}
	}
	calendarSentMu.Lock()
	for key, start := range calendarSent {
		if now.Sub(start) > 24*time.Hour {
			delete(calendarSent, key)
		}
	}
	calendarSentMu.Unlock()
}

func calendarReminderText(event calendarEvent, before time.Duration, zone *time.Location) string {
	place := zone.String()
	if slash := strings.LastIndex(place, "/"); slash > -1 {
		place = strings.ReplaceAll(place[slash+1:], "_", " ")
	}
	text := fmt.Sprintf("In %d min, %s to %s %s", int(before.Minutes()), event.Start.In(zone).Format(time.Kitchen), event.End.In(zone).Format(time.Kitchen), place)
	return text + ". " + strings.Join(event.Calendars, ", ")
}

func (hub *calendarHub) notify(event calendarEvent, before time.Duration) {
	message := map[string]any{
		"topic":    hub.reminders.topic,
		"title":    event.Name,
		"message":  calendarReminderText(event, before, hub.zone),
		"tags":     []string{"calendar"},
		"priority": ternary(before <= time.Minute, 5, 4),
	}
	if event.Link != "" {
		message["click"] = event.Link
	}
	headers := map[string]string{}
	if hub.reminders.token != "" {
		headers["Authorization"] = "Bearer " + hub.reminders.token
	}
	request, err := cockpitRequest("POST", hub.reminders.url, headers, message)
	if err == nil {
		_, err = cockpitFetch(request)
	}
	if err != nil {
		slog.Error("Calendar reminder failed", "event", event.Name, "error", cockpitErrorText(err))
	}
}

package luna

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"strings"
	"time"
)

var calendarWeekWidgetTemplate = mustParseTemplate("calendar-week.html", "widget-base.html")

// calendarWeekWidget lays out the next days from the calendar hub. The grid is drawn by
// calendar-view.js, since only the device knows which zone its days start in.
type calendarWeekWidget struct {
	widgetBase `yaml:",inline"`
	Days       int          `yaml:"days"`
	Calendar   *calendarHub `yaml:"-"`
	EventsJSON string       `yaml:"-"`
}

func (widget *calendarWeekWidget) initialize() error {
	widget.withTitle("Week").withCacheDuration(time.Minute)
	if widget.Days == 0 {
		widget.Days = 7
	}
	if widget.Days < 1 || widget.Days > calendarHorizonDays-1 {
		return errors.New("days must be between 1 and 14")
	}
	return nil
}

func (widget *calendarWeekWidget) update(ctx context.Context) {
	if widget.Providers == nil || widget.Providers.calendar == nil {
		widget.withError(errors.New("no calendar feeds in the config"))
		widget.scheduleNextUpdate()
		return
	}
	hub := widget.Providers.calendar
	now := time.Now()
	// The week view runs Monday to Sunday for this week and next, and the list runs from
	// today, so this covers all the hub has (a day either side covers devices in other zones)
	encoded, err := json.Marshal(hub.Entries(now.AddDate(0, 0, -calendarLookbackDays), now.AddDate(0, 0, calendarHorizonDays)))
	if err != nil {
		widget.withError(err)
		widget.scheduleNextUpdate()
		return
	}
	// A failing feed keeps its last events, so it is a notice on the header, not an error page
	var notice error
	if failures := hub.Errors(); len(failures) > 0 {
		notice = errors.New(strings.Join(failures, ", "))
	}
	widget.renderMu.Lock()
	changed := widget.EventsJSON != string(encoded) || fmt.Sprint(widget.Notice) != fmt.Sprint(notice)
	widget.Calendar = hub
	widget.EventsJSON = string(encoded)
	widget.Notice = notice
	widget.withError(nil)
	widget.renderMu.Unlock()
	widget.scheduleNextUpdate()

	// Open tabs redraw only when told, including the first render after a restart
	if changed {
		publishEvent("custom-api:data_changed", map[string]any{"widget_id": widget.GetID(), "title": widget.Title})
	}
}

func (widget *calendarWeekWidget) Render() template.HTML {
	return widget.renderTemplate(widget, calendarWeekWidgetTemplate)
}

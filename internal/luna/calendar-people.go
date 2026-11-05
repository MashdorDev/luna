package luna

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var calendarPeopleAPI = "https://people.googleapis.com/v1"

// The directory and contact lists are read again after this long
const calendarPhotoFor = 24 * time.Hour

// A person as the People API lists them
type calendarPerson struct {
	Names []struct {
		DisplayName string `json:"displayName"`
	} `json:"names"`
	Photos []struct {
		URL     string `json:"url"`
		Default bool   `json:"default"`
	} `json:"photos"`
	EmailAddresses []struct {
		Value string `json:"value"`
	} `json:"emailAddresses"`
}

// calendarPhotosFrom maps each email of the listed people to their photo, skipping
// Google's generated letter avatars, and to their name. Each list names its array differently.
func calendarPhotosFrom(body []byte, found, names map[string]string) (next string) {
	var page struct {
		People        []calendarPerson `json:"people"`
		Connections   []calendarPerson `json:"connections"`
		OtherContacts []calendarPerson `json:"otherContacts"`
		NextPageToken string           `json:"nextPageToken"`
	}
	if json.Unmarshal(body, &page) != nil {
		return ""
	}
	for _, person := range append(append(page.People, page.Connections...), page.OtherContacts...) {
		if len(person.Names) > 0 && person.Names[0].DisplayName != "" {
			for _, address := range person.EmailAddresses {
				if email := strings.ToLower(address.Value); email != "" && names[email] == "" {
					names[email] = person.Names[0].DisplayName
				}
			}
		}
		photo := ""
		for _, candidate := range person.Photos {
			if !candidate.Default && candidate.URL != "" {
				photo = candidate.URL
				break
			}
		}
		if photo == "" {
			continue
		}
		for _, address := range person.EmailAddresses {
			if email := strings.ToLower(address.Value); email != "" && found[email] == "" {
				found[email] = photo
			}
		}
	}
	return page.NextPageToken
}

// lookupPhotos reads, once a day per feed, the profile pictures of everyone the feed's
// account can see: its Workspace directory, its contacts and the people it has emailed.
// One listing each instead of a search per guest, which Google rate limits after a dozen.
// A list the sign-in has no scope for fails quietly.
func (hub *calendarHub) lookupPhotos(index int, now time.Time) {
	hub.mu.Lock()
	if hub.photos == nil {
		hub.photos, hub.names, hub.photosRead = map[string]string{}, map[string]string{}, map[int]time.Time{}
	}
	fresh := now.Sub(hub.photosRead[index]) < calendarPhotoFor
	if !fresh {
		hub.photosRead[index] = now
	}
	hub.mu.Unlock()
	if fresh {
		return
	}
	token, err := hub.googleToken(index, now)
	if err != nil {
		return
	}
	found, names := map[string]string{}, map[string]string{}
	failures := 0
	lists := []struct {
		path  string
		query url.Values
	}{
		{"/people:listDirectoryPeople", url.Values{"sources": {"DIRECTORY_SOURCE_TYPE_DOMAIN_PROFILE"}, "readMask": {"names,photos,emailAddresses"}, "pageSize": {"1000"}}},
		{"/people/me/connections", url.Values{"personFields": {"names,photos,emailAddresses"}, "pageSize": {"1000"}}},
		{"/otherContacts", url.Values{"readMask": {"names,photos,emailAddresses"}, "pageSize": {"1000"}}},
	}
	for _, list := range lists {
		// Pages are capped so a huge directory cannot hold the tick
		for page, next := 0, ""; page < 5; page++ {
			if next != "" {
				list.query.Set("pageToken", next)
			}
			request, err := http.NewRequest("GET", calendarPeopleAPI+list.path+"?"+list.query.Encode(), nil)
			if err != nil {
				break
			}
			request.Header.Set("Authorization", "Bearer "+token)
			data, err := cockpitFetch(request)
			if err != nil {
				failures++
				break
			}
			if next = calendarPhotosFrom([]byte(data.Raw), found, names); next == "" {
				break
			}
		}
	}
	if failures == len(lists) {
		slog.Warn("Calendar guest photos unavailable, the Google sign-in may lack the contacts scope", "calendar", hub.feeds[index].Name)
	}
	hub.mu.Lock()
	changed := false
	for email, photo := range found {
		if hub.photos[email] != photo {
			hub.photos[email] = photo
			changed = true
		}
	}
	for email, name := range names {
		if hub.names[email] != name {
			hub.names[email] = name
			changed = true
		}
	}
	if changed {
		hub.version.Add(1)
	}
	hub.mu.Unlock()
}

// nameFor is the directory or contact name for an email, or empty
func (hub *calendarHub) nameFor(email string) string {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.names[strings.ToLower(email)]
}

var calendarTasksAPI = "https://tasks.googleapis.com/tasks/v1"

// fetchTasks reads the open Google Tasks that have a due date in the hub's window, from
// every task list. Google keeps only the date of a due time, so each is an all-day entry.
func (hub *calendarHub) fetchTasks(index int) ([]calendarRaw, error) {
	now := time.Now()
	token, err := hub.googleToken(index, now)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	request, err := cockpitRequest("GET", calendarTasksAPI+"/users/@me/lists?maxResults=100", headers, nil)
	if err != nil {
		return nil, err
	}
	lists, err := cockpitFetch(request)
	if err != nil {
		hub.mu.Lock()
		hub.states[index].token = ""
		hub.mu.Unlock()
		return nil, err
	}
	query := url.Values{
		"showCompleted": {"false"}, "maxResults": {"100"},
		"dueMin": {now.AddDate(0, 0, -calendarLookbackDays).Format(time.RFC3339)},
		"dueMax": {now.AddDate(0, 0, calendarHorizonDays).Format(time.RFC3339)},
	}
	var raws []calendarRaw
	for _, list := range lists.Get("items").Array() {
		request, err := cockpitRequest("GET", calendarTasksAPI+"/lists/"+url.PathEscape(list.Get("id").String())+"/tasks?"+query.Encode(), headers, nil)
		if err != nil {
			return nil, err
		}
		tasks, err := cockpitFetch(request)
		if err != nil {
			return nil, err
		}
		for _, task := range tasks.Get("items").Array() {
			due, err := time.Parse(time.RFC3339, task.Get("due").String())
			if err != nil {
				continue
			}
			day := time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, time.UTC)
			raws = append(raws, calendarRaw{
				name: task.Get("title").String(), uid: task.Get("id").String(), description: task.Get("notes").String(),
				start: day, end: day.AddDate(0, 0, 1), allDay: true, open: task.Get("webViewLink").String(), task: true,
			})
		}
	}
	return raws, nil
}

// photoFor is the cached picture for an email, or empty
func (hub *calendarHub) photoFor(email string) string {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.photos[strings.ToLower(email)]
}

var calendarAnswers = map[string]bool{"accepted": true, "tentative": true, "declined": true}

// respond sets Dor's answer to an invite in Google and in the hub's copy, so the page
// shows it before the next fetch. Google mails the organizer as it would from Calendar.
func (hub *calendarHub) respond(index int, eventID, answer string) error {
	if index < 0 || index >= len(hub.feeds) || hub.feeds[index].Google == nil || eventID == "" || !calendarAnswers[answer] {
		return errors.New("unknown meeting or answer")
	}
	token, err := hub.googleToken(index, time.Now())
	if err != nil {
		return err
	}
	target := calendarGoogleAPI + "/calendars/" + url.PathEscape(hub.feeds[index].Google.Calendar) + "/events/" + url.PathEscape(eventID)
	headers := map[string]string{"Authorization": "Bearer " + token}
	request, err := cockpitRequest("GET", target, headers, nil)
	if err != nil {
		return err
	}
	current, err := cockpitFetch(request)
	if err != nil {
		return fmt.Errorf("reading the invite: %w", err)
	}
	// The attendee list is replaced whole, so it goes back as Google sent it with only
	// Dor's answer changed
	var attendees []map[string]any
	if err := json.Unmarshal([]byte(current.Get("attendees").Raw), &attendees); err != nil {
		return errors.New("this meeting has no guest list to answer")
	}
	found := false
	for _, attendee := range attendees {
		if self, _ := attendee["self"].(bool); self {
			attendee["responseStatus"] = answer
			found = true
		}
	}
	if !found {
		return errors.New("Dor is not on this meeting's guest list")
	}
	request, err = cockpitRequest("PATCH", target+"?sendUpdates=all", headers, map[string]any{"attendees": attendees})
	if err != nil {
		return err
	}
	if _, err := cockpitFetch(request); err != nil {
		return fmt.Errorf("saving the answer: %w", err)
	}

	hub.mu.Lock()
	defer hub.mu.Unlock()
	events := hub.states[index].events
	for i := range events {
		if events[i].uid != eventID {
			continue
		}
		events[i].Response = answer
		events[i].Attendees = append([]calendarAttendee(nil), events[i].Attendees...)
		for j := range events[i].Attendees {
			if events[i].Attendees[j].Self {
				events[i].Attendees[j].Status = answer
			}
		}
	}
	// A declined meeting leaves the calendar, as it would on the next fetch
	if answer == "declined" {
		kept := events[:0:0]
		for _, event := range events {
			if event.uid != eventID {
				kept = append(kept, event)
			}
		}
		hub.states[index].events = kept
	}
	hub.version.Add(1)
	return nil
}

// handleCalendarRespond answers an invite for Dor. The custom header and the origin check
// keep another site from sending this through Dor's browser.
func (a *application) handleCalendarRespond(hub *calendarHub) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.isAuthorized(w, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Luna-Calendar") != "1" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if parsed, err := url.Parse(origin); err != nil || parsed.Host != r.Host {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
		var body struct {
			ID     string `json:"id"`
			Answer string `json:"answer"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		feed, eventID, _ := strings.Cut(body.ID, ":")
		index, err := strconv.Atoi(feed)
		if err != nil {
			http.Error(w, "unknown meeting", http.StatusBadRequest)
			return
		}
		if err := hub.respond(index, eventID, body.Answer); err != nil {
			slog.Error("Calendar answer failed", "error", cockpitErrorText(err))
			http.Error(w, cockpitErrorText(err), http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

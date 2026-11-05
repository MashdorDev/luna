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

const (
	// A photo, or the lack of one, is looked up again after this long
	calendarPhotoFor = 24 * time.Hour
	// Lookups per feed per fetch, so a meeting with 200 guests does not stall a tick
	calendarPhotoBatch = 40
)

type calendarPhoto struct {
	url     string
	checked time.Time
}

// lookupPhotos finds profile pictures for the guests of a Google feed's events: the
// Workspace directory first, then Dor's contacts and the people Dor has emailed. Needs the
// contacts and directory scopes; without them each lookup fails quietly and is not retried
// for a day.
func (hub *calendarHub) lookupPhotos(index int, events []calendarEvent, now time.Time) {
	var wanted []string
	hub.mu.Lock()
	if hub.photos == nil {
		hub.photos = map[string]calendarPhoto{}
	}
	seen := map[string]bool{}
	for _, event := range events {
		for _, attendee := range event.Attendees {
			email := strings.ToLower(attendee.Email)
			if email == "" || seen[email] {
				continue
			}
			seen[email] = true
			if photo, ok := hub.photos[email]; ok && now.Sub(photo.checked) < calendarPhotoFor {
				continue
			}
			wanted = append(wanted, email)
		}
	}
	hub.mu.Unlock()
	if len(wanted) > calendarPhotoBatch {
		wanted = wanted[:calendarPhotoBatch]
	}
	if len(wanted) == 0 {
		return
	}
	token, err := hub.googleToken(index, now)
	if err != nil {
		return
	}
	get := func(path string, query url.Values) []byte {
		request, err := http.NewRequest("GET", calendarPeopleAPI+path+"?"+query.Encode(), nil)
		if err != nil {
			return nil
		}
		request.Header.Set("Authorization", "Bearer "+token)
		data, err := cockpitFetch(request)
		if err != nil {
			return nil
		}
		return []byte(data.Raw)
	}
	// Google asks for an empty search first to warm its contact cache
	get("/people:searchContacts", url.Values{"query": {""}, "readMask": {"names"}})
	get("/otherContacts:search", url.Values{"query": {""}, "readMask": {"names"}})
	failures := 0
	for _, email := range wanted {
		found := ""
		for _, source := range []struct{ path, extra string }{
			{"/people:searchDirectoryPeople", "DIRECTORY_SOURCE_TYPE_DOMAIN_PROFILE"},
			{"/people:searchContacts", ""},
			{"/otherContacts:search", ""},
		} {
			query := url.Values{"query": {email}, "readMask": {"photos,emailAddresses"}, "pageSize": {"5"}}
			if source.extra != "" {
				query.Set("sources", source.extra)
			}
			body := get(source.path, query)
			if body == nil {
				failures++
				continue
			}
			found = calendarPhotoFrom(body, email)
			if found != "" {
				break
			}
		}
		hub.mu.Lock()
		if found != "" && hub.photos[email].url != found {
			hub.version.Add(1)
		}
		hub.photos[email] = calendarPhoto{url: found, checked: now}
		hub.mu.Unlock()
	}
	if failures == len(wanted)*3 {
		slog.Warn("Calendar guest photos unavailable, the Google sign-in may lack the contacts scope", "calendar", hub.feeds[index].Name)
	}
}

// A person in a People API search answer
type calendarPeopleMatch struct {
	Person struct {
		Photos []struct {
			URL     string `json:"url"`
			Default bool   `json:"default"`
		} `json:"photos"`
		EmailAddresses []struct {
			Value string `json:"value"`
		} `json:"emailAddresses"`
	} `json:"person"`
}

// calendarPhotoFrom picks the photo of the person whose email matches, skipping
// Google's generated letter avatars. Directory search answers in "people", contact
// searches in "results".
func calendarPhotoFrom(body []byte, email string) string {
	var result struct {
		People  []calendarPeopleMatch `json:"people"`
		Results []calendarPeopleMatch `json:"results"`
	}
	if json.Unmarshal(body, &result) != nil {
		return ""
	}
	for _, match := range append(result.People, result.Results...) {
		owns := false
		for _, address := range match.Person.EmailAddresses {
			owns = owns || strings.EqualFold(address.Value, email)
		}
		if !owns {
			continue
		}
		for _, photo := range match.Person.Photos {
			if !photo.Default && photo.URL != "" {
				return photo.URL
			}
		}
	}
	return ""
}

// photoFor is the cached picture for an email, or empty
func (hub *calendarHub) photoFor(email string) string {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.photos[strings.ToLower(email)].url
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

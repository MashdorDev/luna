package luna

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCalendarRespondChangesOnlyDorsAnswer(t *testing.T) {
	var patched map[string]any
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Write([]byte(`{"access_token":"access","expires_in":3600}`))
		case r.URL.Path == "/calendars/primary/events/ev1" && r.Method == "GET":
			w.Write([]byte(`{"id":"ev1","attendees":[{"email":"pete@x.org","organizer":true,"responseStatus":"accepted"},{"email":"dor@x.org","self":true,"responseStatus":"needsAction","comment":"hi"}]}`))
		case r.URL.Path == "/calendars/primary/events/ev1" && r.Method == "PATCH":
			query = r.URL.RawQuery
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &patched)
			w.Write([]byte(`{}`))
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
	hub.states[0].events = []calendarEvent{{Name: "Weekly", uid: "ev1", Response: "needsAction", Attendees: []calendarAttendee{{Email: "dor@x.org", Self: true, Status: "needsAction"}}}}

	app := &application{}
	handler := app.handleCalendarRespond(hub)
	send := func(headers map[string]string, body string) int {
		request := httptest.NewRequest("POST", "http://luna.test/api/calendar/respond", strings.NewReader(body))
		for k, v := range headers {
			request.Header.Set(k, v)
		}
		recorder := httptest.NewRecorder()
		handler(recorder, request)
		return recorder.Code
	}
	if code := send(nil, `{"id":"0:ev1","answer":"accepted"}`); code != http.StatusForbidden {
		t.Fatalf("no header: got %d", code)
	}
	if code := send(map[string]string{"X-Luna-Calendar": "1", "Origin": "https://evil.test"}, `{"id":"0:ev1","answer":"accepted"}`); code != http.StatusForbidden {
		t.Fatalf("other origin: got %d", code)
	}
	if code := send(map[string]string{"X-Luna-Calendar": "1"}, `{"id":"0:ev1","answer":"maybe"}`); code != http.StatusBadGateway {
		t.Fatalf("bad answer: got %d", code)
	}
	if code := send(map[string]string{"X-Luna-Calendar": "1", "Origin": "http://luna.test"}, `{"id":"0:ev1","answer":"accepted"}`); code != http.StatusNoContent {
		t.Fatalf("answer: got %d", code)
	}
	if query != "sendUpdates=all" {
		t.Fatalf("query %q", query)
	}
	attendees := patched["attendees"].([]any)
	pete, dor := attendees[0].(map[string]any), attendees[1].(map[string]any)
	if pete["responseStatus"] != "accepted" || dor["responseStatus"] != "accepted" || dor["comment"] != "hi" || len(attendees) != 2 {
		t.Fatalf("patched %v", attendees)
	}
	if got := hub.states[0].events[0]; got.Response != "accepted" || got.Attendees[0].Status != "accepted" {
		t.Fatalf("hub copy not updated: %+v", got)
	}
}

func TestCalendarPhotoFromSkipsLetterAvatars(t *testing.T) {
	body := []byte(`{"results":[
		{"person":{"emailAddresses":[{"value":"Other@x.org"}],"photos":[{"url":"https://p/other"}]}},
		{"person":{"emailAddresses":[{"value":"Gus@x.org"}],"photos":[{"url":"https://p/letter","default":true},{"url":"https://p/gus"}]}}
	]}`)
	if got := calendarPhotoFrom(body, "gus@x.org"); got != "https://p/gus" {
		t.Fatalf("got %q", got)
	}
	if got := calendarPhotoFrom([]byte(`{"people":[{"person":{"emailAddresses":[{"value":"a@x.org"}],"photos":[{"url":"https://p/a","default":true}]}}]}`), "a@x.org"); got != "" {
		t.Fatalf("letter avatar used: %q", got)
	}
}

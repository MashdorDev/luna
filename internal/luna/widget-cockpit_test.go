package luna

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCockpitDaysBetweenAcrossDaylightSaving(t *testing.T) {
	toronto, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	due := time.Date(2027, 3, 14, 0, 0, 0, 0, toronto)
	now := time.Date(2027, 3, 15, 9, 0, 0, 0, toronto)
	if got := cockpitDaysBetween(due, now); got != 1 {
		t.Fatalf("got %d days, want 1", got)
	}
	if got := cockpitDaysBetween(now, due); got != -1 {
		t.Fatalf("got %d days, want -1", got)
	}
}

func TestCockpitErrorTextHidesSecrets(t *testing.T) {
	wrapped := &url.Error{Op: "Get", URL: "https://x.test/?apikey=SECRET", Err: errors.New("timeout")}
	if got := cockpitErrorText(wrapped); got != "no answer" {
		t.Fatalf("got %q", got)
	}
	if got := cockpitErrorText(errors.New("bad apikey=SECRET here")); strings.Contains(got, "SECRET") {
		t.Fatalf("leaked secret: %q", got)
	}
}

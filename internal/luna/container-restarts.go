package luna

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const containerRestartWindow = 24 * time.Hour

type containerRestart struct {
	Name  string
	Count int
	Last  time.Time
}

// containerRestartTracker notices Docker's RestartCount going up between checks. Only a restart
// policy raises that count, so a deploy, reboot or docker restart does not show here. A worker that
// exits 0 on purpose does raise it, as Solidtime's queue does every hour, so the exit codes in
// Docker's events decide what counts as a crash. A recreated container has a new ID and starts at
// its first count without a warning.
type containerRestartTracker struct {
	// Held across the fetch too: an older snapshot recorded over a newer one counts a restart twice
	mu      sync.Mutex
	counts  map[string]int
	recent  map[string]*containerRestart
	checked time.Time
}

var containerRestarts = &containerRestartTracker{
	counts: make(map[string]int),
	recent: make(map[string]*containerRestart),
}

func (tracker *containerRestartTracker) record(counts, crashes map[string]int, names map[string]string, now time.Time) []containerRestart {
	for id, count := range crashes {
		if count == 0 {
			continue
		}
		restart := tracker.recent[names[id]]
		if restart == nil {
			restart = &containerRestart{Name: names[id]}
			tracker.recent[names[id]] = restart
		}
		restart.Count += count
		restart.Last = now
	}
	tracker.counts = counts

	current := make([]containerRestart, 0, len(tracker.recent))
	for name, restart := range tracker.recent {
		if now.Sub(restart.Last) > containerRestartWindow {
			delete(tracker.recent, name)
			continue
		}
		current = append(current, *restart)
	}
	slices.SortFunc(current, func(a, b containerRestart) int { return b.Last.Compare(a.Last) })

	return current
}

// update honours the luna.hide label but not per-widget settings, because every caller shares one
// history. Only one Docker socket is expected: a second would reset the first one's counts.
func (tracker *containerRestartTracker) update(source string, now time.Time) ([]containerRestart, error) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()

	containers, err := fetchDockerContainersFromSource(source, "", false, nil)
	if err != nil {
		return nil, err
	}
	client, hostname, err := newDockerClient(source)
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int, len(containers))
	crashes := make(map[string]int)
	names := make(map[string]string, len(containers))
	for i := range containers {
		container := &containers[i]
		if isDockerContainerHidden(container, false) {
			continue
		}
		count, exists, err := fetchContainerRestartCount(client, hostname, container.ID)
		if err != nil {
			return nil, err
		}
		// Removed since the list was fetched, as a deploy does
		if !exists {
			continue
		}
		counts[container.ID] = count
		names[container.ID] = strings.TrimLeft(itemAtIndexOrDefault(container.Names, 0, container.ID), "/")

		if previous, known := tracker.counts[container.ID]; known && count > previous {
			crashes[container.ID], err = fetchContainerCrashCount(client, hostname, container.ID, tracker.checked)
			if err != nil {
				return nil, err
			}
		}
	}

	tracker.checked = now
	return tracker.record(counts, crashes, names, now), nil
}

func fetchContainerRestartCount(client *http.Client, hostname, id string) (int, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, "GET", "http://"+hostname+"/containers/"+id+"/json", nil)
	if err != nil {
		return 0, false, fmt.Errorf("creating request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, false, fmt.Errorf("sending request to socket: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotFound {
		return 0, false, nil
	}
	if response.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("non-200 response status: %s", response.Status)
	}

	var details struct {
		RestartCount int `json:"RestartCount"`
	}
	if err := json.NewDecoder(response.Body).Decode(&details); err != nil {
		return 0, false, fmt.Errorf("decoding response: %w", err)
	}

	return details.RestartCount, true, nil
}

// The window ends now, not when the check began: a crash during the check has raised the count
// already, and would never be seen again.
func fetchContainerCrashCount(client *http.Client, hostname, id string, since time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	query := url.Values{
		"since":   {strconv.FormatInt(since.Unix(), 10)},
		"until":   {strconv.FormatInt(time.Now().Unix(), 10)},
		"filters": {fmt.Sprintf(`{"container":[%q],"event":["die"]}`, id)},
	}
	request, err := http.NewRequestWithContext(ctx, "GET", "http://"+hostname+"/events?"+query.Encode(), nil)
	if err != nil {
		return 0, fmt.Errorf("creating request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("sending request to socket: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("non-200 response status: %s", response.Status)
	}

	crashes := 0
	decoder := json.NewDecoder(response.Body)
	for {
		var event struct {
			Actor struct {
				Attributes struct {
					ExitCode string `json:"exitCode"`
				} `json:"Attributes"`
			} `json:"Actor"`
		}
		if err := decoder.Decode(&event); err == io.EOF {
			return crashes, nil
		} else if err != nil {
			return 0, fmt.Errorf("decoding events: %w", err)
		}
		if event.Actor.Attributes.ExitCode != "0" {
			crashes++
		}
	}
}

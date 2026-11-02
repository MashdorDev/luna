package luna

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const containerRestartWindow = 24 * time.Hour
const containerExitStreamRetry = 5 * time.Second

// Checks run every few minutes, so an older clean exit came from a docker stop or restart and has
// nothing to do with a later crash
const containerCleanExitMaxAge = 5 * time.Minute

type containerRestart struct {
	Name  string
	Count int
	Last  time.Time
}

// containerRestartTracker notices Docker's RestartCount going up between checks. Only a restart
// policy raises that count, so a deploy, reboot or docker restart does not show here. A worker that
// exits 0 on purpose does raise it, as Solidtime's queue does every hour, so clean exits are taken
// off the rise. They come from a live event stream, because Docker's event history is too short to
// read afterwards on a server with many health checks. A rise with no clean exit to explain it,
// such as during a dropped stream, counts as a crash. A recreated container has a new ID and starts
// at its first count without a warning.
type containerRestartTracker struct {
	mu     sync.Mutex
	counts map[string]int
	recent map[string]*containerRestart

	watch      sync.Once
	exitsMu    sync.Mutex
	cleanExits map[string][]time.Time
}

var containerRestarts = newContainerRestartTracker()

func newContainerRestartTracker() *containerRestartTracker {
	return &containerRestartTracker{
		counts:     make(map[string]int),
		recent:     make(map[string]*containerRestart),
		cleanExits: make(map[string][]time.Time),
	}
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

	client, hostname, err := newDockerClient(source)
	if err != nil {
		return nil, err
	}
	tracker.watch.Do(func() { go tracker.watchExits(client, hostname) })

	containers, err := fetchDockerContainersFromSource(source, "", false, nil)
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int, len(containers))
	rises := make(map[string]int)
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
			rises[container.ID] = count - previous
		}
	}

	// Only once every inspect has worked: a failed check sees these rises again next time
	crashes := make(map[string]int, len(rises))
	for id, rise := range rises {
		crashes[id] = tracker.takeCleanExits(id, rise, now)
	}
	tracker.forgetExits(counts)
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

// takeCleanExits returns how many of a container's restarts its recent clean exits do not explain
func (tracker *containerRestartTracker) takeCleanExits(id string, restarts int, now time.Time) int {
	tracker.exitsMu.Lock()
	defer tracker.exitsMu.Unlock()

	exits := slices.DeleteFunc(tracker.cleanExits[id], func(exit time.Time) bool {
		return now.Sub(exit) > containerCleanExitMaxAge
	})
	explained := min(len(exits), restarts)
	tracker.cleanExits[id] = exits[explained:]
	return restarts - explained
}

func (tracker *containerRestartTracker) forgetExits(live map[string]int) {
	tracker.exitsMu.Lock()
	defer tracker.exitsMu.Unlock()

	for id := range tracker.cleanExits {
		if _, ok := live[id]; !ok {
			delete(tracker.cleanExits, id)
		}
	}
}

func (tracker *containerRestartTracker) watchExits(client *http.Client, hostname string) {
	for {
		if err := tracker.readExits(client, hostname); err != nil {
			slog.Error("Docker event stream dropped", "error", err)
		}
		time.Sleep(containerExitStreamRetry)
	}
}

func (tracker *containerRestartTracker) readExits(client *http.Client, hostname string) error {
	query := url.Values{"filters": {`{"type":["container"],"event":["die"]}`}}
	response, err := client.Get("http://" + hostname + "/events?" + query.Encode())
	if err != nil {
		return fmt.Errorf("sending request to socket: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("non-200 response status: %s", response.Status)
	}

	decoder := json.NewDecoder(response.Body)
	for {
		var event struct {
			Actor struct {
				ID         string `json:"ID"`
				Attributes struct {
					ExitCode string `json:"exitCode"`
				} `json:"Attributes"`
			} `json:"Actor"`
		}
		if err := decoder.Decode(&event); err != nil {
			return fmt.Errorf("decoding events: %w", err)
		}
		if event.Actor.Attributes.ExitCode == "0" {
			tracker.exitsMu.Lock()
			tracker.cleanExits[event.Actor.ID] = append(tracker.cleanExits[event.Actor.ID], time.Now())
			tracker.exitsMu.Unlock()
		}
	}
}

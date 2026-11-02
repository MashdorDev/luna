package luna

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRenderTemplateKeepsConcurrentRendersApart(t *testing.T) {
	page := template.Must(template.New("t").Parse(`{{ range . }}<img src="{{ . }}">{{ end }}`))
	data := strings.Split(strings.Repeat("a,", 500), ",")

	var widget widgetBase
	want := string(widget.renderTemplate(data, page))

	var wg sync.WaitGroup
	got := make([]string, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = string(widget.renderTemplate(data, page))
		}()
	}
	wg.Wait()

	for i, html := range got {
		if html != want {
			t.Fatalf("render %d is %d bytes, want %d", i, len(html), len(want))
		}
	}
}

func TestDockerContainersFailingKeepsOnlyProblems(t *testing.T) {
	containers := dockerContainerList{
		{Name: "up", StateIcon: dockerContainerStateIconOK},
		{Name: "down", StateIcon: dockerContainerStateIconWarn},
		{Name: "paused", StateIcon: dockerContainerStateIconPaused},
	}
	got := containers.failing()
	if len(got) != 2 || got[0].Name != "down" || got[1].Name != "paused" {
		t.Fatalf("got %+v", got)
	}
}

func TestDockerContainersFailingKeepsParentOfFailingChild(t *testing.T) {
	containers := dockerContainerList{
		{Name: "app", StateIcon: dockerContainerStateIconOK, Children: dockerContainerList{
			{Name: "app-db", StateIcon: dockerContainerStateIconOther},
		}},
		{Name: "up", StateIcon: dockerContainerStateIconOK},
	}
	got := containers.failing()
	if len(got) != 1 || got[0].Name != "app" || got[0].StateIcon != dockerContainerStateIconWarn {
		t.Fatalf("got %+v", got)
	}
	if containers.count() != 3 {
		t.Fatalf("count = %d", containers.count())
	}
}

func TestDockerContainerStateIconReadsHealthFromStatus(t *testing.T) {
	cases := []struct{ state, status, want string }{
		{"running", "Up 3 hours", dockerContainerStateIconOK},
		{"running", "Up 3 hours (healthy)", dockerContainerStateIconOK},
		{"running", "Up 3 hours (unhealthy)", dockerContainerStateIconWarn},
		{"exited", "Exited (1) 2 hours ago", dockerContainerStateIconWarn},
		{"restarting", "Restarting (1) 5 seconds ago", dockerContainerStateIconOther},
	}
	for _, c := range cases {
		if got := dockerContainerStateToStateIcon(c.state, c.status); got != c.want {
			t.Errorf("%s %q = %s, want %s", c.state, c.status, got, c.want)
		}
	}
}

const arrayStatusSample = `["parity"]
name="parity"
status="DISK_OK"
numErrors="0"
["disk1"]
name="disk1"
status="DISK_DSBL"
numErrors="0"
["parity2"]
name="parity2"
status="DISK_NP_DSBL"
idSb=""
numErrors="0"
["disk2"]
status="DISK_OK"
numErrors="3"
["disk3"]
status="DISK_NP_DSBL"
idSb="WDC_123"
numErrors="0"
["disk4"]
status="DISK_OK"
fsStatus="Unmountable: wrong or no file system"
numErrors="0"
["disk5"]
status="DISK_OK"
fsStatus="Mounted"
numErrors="0"
["flash"]
status="DISK_OK"
numErrors="0"
`

func TestParseArrayStatus(t *testing.T) {
	total, problems := parseArrayStatus(arrayStatusSample)
	if total != 6 {
		t.Fatalf("total = %d, want 6", total)
	}
	want := []arrayDiskProblem{{"disk1", "disabled"}, {"disk2", "3 errors"}, {"disk3", "missing"}, {"disk4", "unmountable"}}
	if !slices.Equal(problems, want) {
		t.Fatalf("got %+v", problems)
	}
}

func TestReadArrayStatusRejectsAStaleOrEmptyCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks.ini")
	if err := os.WriteFile(path, []byte(arrayStatusSample), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := readArrayStatus(path, time.Now()); err != nil {
		t.Fatalf("fresh copy: %v", err)
	}
	if _, _, err := readArrayStatus(path, time.Now().Add(arrayStatusMaxAge+time.Minute)); err == nil {
		t.Fatal("stale copy should fail")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readArrayStatus(path, time.Now()); err == nil {
		t.Fatal("empty copy should fail")
	}
	if _, _, err := readArrayStatus(path+".missing", time.Now()); err == nil {
		t.Fatal("missing copy should fail")
	}
}

const arraySmartHealthy = `SMART overall-health self-assessment test result: PASSED
ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE
  5 Reallocated_Sector_Ct   0x0033   200   200   140    Pre-fail  Always       -       0
  9 Power_On_Hours          0x0032   025   025   000    Old_age   Always       -       55312
194 Temperature_Celsius     0x0022   108   100   000    Old_age   Always       -       35
197 Current_Pending_Sector  0x0032   200   200   000    Old_age   Always       -       0
199 UDMA_CRC_Error_Count    0x0032   200   200   000    Old_age   Always       -       1
`

func TestContainerRestartTrackerCountsOnlyNewRestarts(t *testing.T) {
	tracker := &containerRestartTracker{counts: map[string]int{}, recent: map[string]*containerRestart{}}
	names := map[string]string{"a": "pocket-id", "b": "luna"}
	now := time.Now()

	// An old count on first sight is not news
	if got := tracker.record(map[string]int{"a": 5, "b": 0}, names, now); len(got) != 0 {
		t.Fatalf("first check: %+v", got)
	}
	got := tracker.record(map[string]int{"a": 8, "b": 0}, names, now.Add(time.Minute))
	if len(got) != 1 || got[0].Name != "pocket-id" || got[0].Count != 3 {
		t.Fatalf("restarted: %+v", got)
	}
	// A recreated container has a new ID, so its count starts over without a warning
	names["c"] = "luna"
	got = tracker.record(map[string]int{"a": 9, "c": 0}, names, now.Add(2*time.Minute))
	if len(got) != 1 || got[0].Count != 4 {
		t.Fatalf("second restart: %+v", got)
	}
	if got := tracker.record(map[string]int{"a": 9, "c": 0}, names, now.Add(containerRestartWindow+3*time.Minute)); len(got) != 0 {
		t.Fatalf("after the window: %+v", got)
	}
}

func TestContainerRestartsReadFromDocker(t *testing.T) {
	count := 2
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containers/json":
			fmt.Fprint(w, `[{"Id":"a","Names":["/pocket-id"]},{"Id":"h","Names":["/one-shot"],"Labels":{"luna.hide":"true"}},{"Id":"gone","Names":["/replaced"]}]`)
		case "/containers/a/json":
			fmt.Fprintf(w, `{"RestartCount":%d}`, count)
		case "/containers/gone/json":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	tracker := &containerRestartTracker{counts: map[string]int{}, recent: map[string]*containerRestart{}}
	if _, err := tracker.update(server.URL, time.Now()); err != nil {
		t.Fatal(err)
	}
	count = 3
	got, err := tracker.update(server.URL, time.Now())
	if err != nil || len(got) != 1 || got[0].Name != "pocket-id" || got[0].Count != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParseArraySmart(t *testing.T) {
	if got := parseArraySmart(arraySmartHealthy); got != "" {
		t.Fatalf("healthy: %q", got)
	}

	failing := `SMART overall-health self-assessment test result: FAILED!
  5 Reallocated_Sector_Ct   0x0033   100   100   140    Pre-fail  Always   FAILING_NOW 12
194 Temperature_Celsius     0x0022   108   100   000    Old_age   Always       -       58 (Min/Max 20/60)
197 Current_Pending_Sector  0x0032   200   200   000    Old_age   Always       -       1
199 UDMA_CRC_Error_Count    0x0032   200   200   000    Old_age   Always       -       10
  5 Reallocated_Sector_Ct   0x0033   100   100   140    Pre-fail  Always   FAILING_NOW 12
`
	want := "SMART failed, 12 reallocated sectors, 58°C, 1 pending sector, 10 cable errors"
	if got := parseArraySmart(failing); got != want {
		t.Fatalf("got %q", got)
	}
}

func TestReadArraySmartProblems(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	if got := readArraySmartProblems(filepath.Join(dir, "missing"), now); len(got) != 0 {
		t.Fatalf("no directory should mean no check: %+v", got)
	}
	stale := []arrayDiskProblem{{Name: "SMART", Status: "no recent data"}}
	if got := readArraySmartProblems(dir, now); !slices.Equal(got, stale) {
		t.Fatalf("empty directory: %+v", got)
	}

	bad := "197 Current_Pending_Sector  0x0032   200   200   000    Old_age   Always       -       3\n"
	for name, content := range map[string]string{"parity": arraySmartHealthy, "disk1": bad} {
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := []arrayDiskProblem{{"disk1", "3 pending sectors"}}
	if got := readArraySmartProblems(dir, now); !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
	oldWant := append(want, stale...)
	if got := readArraySmartProblems(dir, now.Add(arraySmartMaxAge+time.Minute)); !slices.Equal(got, oldWant) {
		t.Fatalf("old files keep their warning: %+v", got)
	}
}

func TestArrayStatusRendersEachState(t *testing.T) {
	widget := &arrayStatusWidget{File: "unused"}
	if err := widget.initialize(); err != nil {
		t.Fatal(err)
	}
	widget.ContentAvailable = true

	widget.Total = 3
	if got := string(widget.Render()); !strings.Contains(got, "All 3 disks OK") {
		t.Fatalf("healthy render: %s", got)
	}
	widget.Problems = []arrayDiskProblem{{"disk1", "disabled"}}
	if got := string(widget.Render()); !strings.Contains(got, "disk1") || !strings.Contains(got, "disabled") {
		t.Fatalf("problem render: %s", got)
	}
	widget.Error = errors.New("stale")
	if got := string(widget.Render()); !strings.Contains(got, "Could not read disk status") || strings.Contains(got, "disks OK") {
		t.Fatalf("error render: %s", got)
	}
}

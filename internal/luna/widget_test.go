package luna

import (
	"errors"
	"html/template"
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
	if total != 7 {
		t.Fatalf("total = %d, want 7", total)
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

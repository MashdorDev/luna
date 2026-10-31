package luna

import (
	"html/template"
	"strings"
	"sync"
	"testing"
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

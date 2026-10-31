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

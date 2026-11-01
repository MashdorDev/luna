package luna

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

var arrayStatusWidgetTemplate = mustParseTemplate("array-status.html", "widget-base.html")

// Unraid rewrites disks.ini every 20 seconds or so, and the copy keeps that time, so an older
// file means Unraid or the copy job stopped
const arrayStatusMaxAge = 10 * time.Minute

type arrayDiskProblem struct {
	Name   string
	Status string
}

type arrayStatusWidget struct {
	widgetBase `yaml:",inline"`
	File       string             `yaml:"file"`
	URL        string             `yaml:"url"`
	Total      int                `yaml:"-"`
	Problems   []arrayDiskProblem `yaml:"-"`
}

func (widget *arrayStatusWidget) initialize() error {
	widget.withTitle("Array").withCacheDuration(1 * time.Minute)

	if widget.File == "" {
		return errors.New("file is required")
	}

	return nil
}

func (widget *arrayStatusWidget) update(ctx context.Context) {
	total, problems, err := readArrayStatus(widget.File, time.Now())

	widget.renderMu.Lock()
	changed := (err != nil) != (widget.Error != nil) ||
		(err == nil && (total != widget.Total || !slices.Equal(problems, widget.Problems)))
	if widget.canContinueUpdateAfterHandlingErr(err) {
		widget.Total = total
		widget.Problems = problems
	}
	widget.renderMu.Unlock()

	if changed {
		// page.js refetches and morphs this widget in every open tab
		publishEvent("custom-api:data_changed", map[string]any{"widget_id": widget.GetID(), "title": widget.Title})
	}
}

func (widget *arrayStatusWidget) Render() template.HTML {
	return widget.renderTemplate(widget, arrayStatusWidgetTemplate)
}

func readArrayStatus(path string, now time.Time) (int, []arrayDiskProblem, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, nil, fmt.Errorf("reading disk status: %w", err)
	}
	if age := now.Sub(info.ModTime()); age > arrayStatusMaxAge {
		return 0, nil, fmt.Errorf("disk status file is %s old", age.Round(time.Minute))
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, fmt.Errorf("reading disk status: %w", err)
	}

	total, problems := parseArrayStatus(string(content))
	if total == 0 {
		return 0, nil, errors.New("no disks in the status file")
	}

	return total, problems, nil
}

func parseArrayStatus(content string) (int, []arrayDiskProblem) {
	total := 0
	problems := make([]arrayDiskProblem, 0)

	var name, status, assignedID, filesystem string
	var readErrors int
	flush := func() {
		// A slot that never held a disk has no assigned ID. One that lost its disk keeps it,
		// and that is an array running without protection
		empty := status == "DISK_NP" || status == "DISK_NP_DSBL"
		if status == "" || (empty && assignedID == "") {
			return
		}
		// The boot USB is reported like a disk but is not one to count
		if name != "flash" {
			total++
		}
		if text := arrayDiskProblemText(status, filesystem, readErrors); text != "" {
			problems = append(problems, arrayDiskProblem{Name: name, Status: text})
		}
	}

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			flush()
			name, status, assignedID, filesystem, readErrors = strings.Trim(line, `[]"`), "", "", "", 0
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"`)

		switch key {
		case "status":
			status = value
		case "idSb":
			assignedID = value
		case "fsStatus":
			filesystem = value
		case "numErrors":
			readErrors, _ = strconv.Atoi(value)
		}
	}
	flush()

	return total, problems
}

func arrayDiskProblemText(status, filesystem string, readErrors int) string {
	switch status {
	case "DISK_OK":
		if strings.Contains(strings.ToLower(filesystem), "unmountable") {
			return "unmountable"
		}
		if readErrors > 0 {
			return pluralWord(readErrors, "1 error", strconv.Itoa(readErrors)+" errors")
		}
		return ""
	case "DISK_DSBL":
		return "disabled"
	case "DISK_DSBL_NEW":
		return "replaced, needs rebuild"
	case "DISK_NP", "DISK_NP_DSBL", "DISK_NP_MISSING":
		return "missing"
	case "DISK_WRONG":
		return "wrong disk"
	case "DISK_INVALID":
		return "invalid"
	default:
		return strings.ToLower(strings.TrimPrefix(status, "DISK_"))
	}
}

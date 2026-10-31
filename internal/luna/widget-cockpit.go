package luna

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

var cockpitWidgetTemplate = mustParseTemplate("cockpit.html", "widget-base.html")

const (
	cockpitQuotesEvery    = 15 * time.Minute
	cockpitSkyEvery       = 30 * time.Minute
	cockpitCalendarsEvery = 30 * time.Minute
	cockpitDrawerErrors   = 10
	cockpitNextEvents     = 4
	cockpitNextHolidays   = 1
)

// update holds the page lock while it fetches, so a hung source can't be allowed the default 15 seconds
var cockpitHTTPClient = &http.Client{Timeout: 8 * time.Second}

var cockpitSecretPattern = regexp.MustCompile(`(?i)(api_?key|token)=[^&\s"]+`)

// Never show URLs on the page or log query strings: they hold API keys, internal addresses and calendar emails
func cockpitErrorText(err error) string {
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		return "no answer"
	}
	return cockpitSecretPattern.ReplaceAllString(err.Error(), "$1=redacted")
}

// Whole calendar days from a to b, safe across daylight saving changes
func cockpitDaysBetween(a, b time.Time) int {
	from := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	to := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(to.Sub(from).Hours() / 24)
}

type cockpitWidget struct {
	widgetBase `yaml:",inline"`
	Apps       []struct {
		Title string          `yaml:"title"`
		URL   string          `yaml:"url"`
		Icon  customIconField `yaml:"icon"`
		Site  string          `yaml:"site"`
		Down  bool            `yaml:"-"`
	} `yaml:"apps"`
	Sites []struct {
		*SiteStatusRequest `yaml:",inline"`
		Title              string `yaml:"title"`
		AltStatusCodes     []int  `yaml:"alt-status-codes"`
	} `yaml:"sites"`
	GitHubToken string `yaml:"github-token"`
	GlitchTip   struct {
		URL   string `yaml:"url"`
		Org   string `yaml:"org"`
		Token string `yaml:"token"`
	} `yaml:"glitchtip"`
	Plane struct {
		URL       string `yaml:"url"`
		Workspace string `yaml:"workspace"`
		Project   string `yaml:"project"`
		ProjectID string `yaml:"project-id"`
		Token     string `yaml:"token"`
		Label     string `yaml:"label"`
	} `yaml:"plane"`
	Invoices []struct {
		URL        string `yaml:"url"`
		Name       string `yaml:"name"`
		Token      string `yaml:"token"`
		Currency   string `yaml:"currency"`
		CurrencyID string `yaml:"currency-id"`
	} `yaml:"invoices"`
	Solidtime struct {
		URL   string `yaml:"url"`
		Host  string `yaml:"host"`
		Org   string `yaml:"org"`
		Token string `yaml:"token"`
	} `yaml:"solidtime"`
	Calendars []struct {
		Name     string `yaml:"name"`
		URL      string `yaml:"url"`
		Holidays bool   `yaml:"holidays"`
	} `yaml:"calendars"`
	Location string `yaml:"location"`
	Zones    []struct {
		Timezone string `yaml:"timezone"`
		Label    string `yaml:"label"`
	} `yaml:"zones"`
	Markets []struct {
		marketRequest `yaml:",inline"`
		Provider      string `yaml:"provider"`
		APIKey        string `yaml:"api-key"`
	} `yaml:"markets"`

	Now         []cockpitRow   `yaml:"-"`
	Later       []cockpitRow   `yaml:"-"`
	Failures    []string       `yaml:"-"`
	SiteCount   int            `yaml:"-"`
	SitesDown   []string       `yaml:"-"`
	Hours       cockpitHours   `yaml:"-"`
	Next        []cockpitEvent `yaml:"-"`
	Sky         *cockpitSky    `yaml:"-"`
	Quotes      marketList     `yaml:"-"`
	Updated     time.Time      `yaml:"-"`
	place       *openMeteoPlaceResponseJson
	quotesAt    map[string]time.Time
	skyAt       time.Time
	calendarsAt time.Time
	calendarErr error
}

type cockpitItem struct {
	Title string
	Tag   string
	URL   string
	Tone  string
}

type cockpitSegment struct {
	Flex    float64
	Color   string
	Opacity float64
}

type cockpitRow struct {
	Kind  string
	Group string
	Tone  string
	Big   string
	Label string
	Sub   string
	Hint  string
	Items []cockpitItem
	Stack []cockpitSegment
	Pips  []string
	Spark []int
}

type cockpitHours struct {
	Week  float64
	Today float64
	Days  []cockpitDay
	Error string
}

type cockpitDay struct {
	Letter string
	Hours  float64
	Height int
	Today  bool
}

type cockpitEvent struct {
	Name     string
	When     string
	Calendar string
	holiday  bool
	start    time.Time
}

type cockpitSky struct {
	Temperature int
	Condition   string
	Sunrise     string
	Sunset      string
	RiseAt      float64
	SetAt       float64
	Timezone    string
}

func (widget *cockpitWidget) initialize() error {
	widget.withTitle("Home").withCacheDuration(3 * time.Minute)
	widget.HideHeader = true
	return nil
}

func (widget *cockpitWidget) Render() template.HTML {
	return widget.renderTemplate(widget, cockpitWidgetTemplate)
}

func cockpitRequest(method, target string, headers map[string]string, body any) (*http.Request, error) {
	var reader *bytes.Reader = bytes.NewReader(nil)
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}

	request, err := http.NewRequest(method, target, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	return request, nil
}

func cockpitFetch(request *http.Request) (gjson.Result, error) {
	response, err := cockpitHTTPClient.Do(request)
	if err != nil {
		return gjson.Result{}, err
	}
	defer response.Body.Close()

	var buffer bytes.Buffer
	if _, err := buffer.ReadFrom(response.Body); err != nil {
		return gjson.Result{}, err
	}
	if response.StatusCode != http.StatusOK {
		return gjson.Result{}, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if !gjson.ValidBytes(buffer.Bytes()) {
		return gjson.Result{}, errors.New("invalid JSON")
	}
	return gjson.ParseBytes(buffer.Bytes()), nil
}

func (widget *cockpitWidget) update(ctx context.Context) {
	now := time.Now()
	var mu sync.Mutex
	var wg sync.WaitGroup
	var rows []cockpitRow
	var failures []string

	run := func(name string, enabled bool, fetch func() ([]cockpitRow, error)) {
		if !enabled {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := fetch()
			mu.Lock()
			defer mu.Unlock()
			rows = append(rows, result...)
			if err != nil {
				text := cockpitErrorText(err)
				slog.Error("Cockpit source failed", "source", name, "error", text)
				failures = append(failures, name+": "+text)
			}
		}()
	}

	run("GitHub", widget.GitHubToken != "", widget.fetchGitHub)
	run("GlitchTip", widget.GlitchTip.URL != "", func() ([]cockpitRow, error) { return widget.fetchGlitchTip(now) })
	run("Plane", widget.Plane.URL != "", func() ([]cockpitRow, error) { return widget.fetchPlane(now) })
	for i := range widget.Invoices {
		i := i
		run(widget.Invoices[i].Name, true, func() ([]cockpitRow, error) { return widget.fetchInvoices(i, now) })
	}

	var hours cockpitHours
	next := widget.Next
	sky := widget.Sky
	var quotes marketList
	var down []string

	wg.Add(4)
	go func() {
		defer wg.Done()
		var err error
		if hours, err = widget.fetchHours(now); err != nil {
			text := cockpitErrorText(err)
			slog.Error("Cockpit hours failed", "error", text)
			hours.Error = "Solidtime: " + text
		}
	}()
	go func() {
		defer wg.Done()
		upcoming := func(events []cockpitEvent) []cockpitEvent {
			return slices.DeleteFunc(slices.Clone(events), func(event cockpitEvent) bool {
				return event.start.Before(now.Add(-time.Hour))
			})
		}
		// glance-ical refetches the whole feed each call: polling it every few minutes
		// once pulled 9 GB a day from Google, so calendars keep a slower clock
		if now.Sub(widget.calendarsAt) >= cockpitCalendarsEvery {
			widget.calendarsAt = now
			fresh, err := widget.fetchNextEvents(now)
			widget.calendarErr = err
			if fresh != nil || err == nil {
				next = fresh
			}
		}
		next = upcoming(next)
		if widget.calendarErr != nil {
			mu.Lock()
			failures = append(failures, widget.calendarErr.Error())
			mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		if now.Sub(widget.skyAt) < cockpitSkyEvery {
			return
		}
		fresh, err := widget.fetchSky()
		if err != nil {
			slog.Error("Cockpit weather failed", "error", cockpitErrorText(err))
			return
		}
		sky = fresh
		widget.skyAt = now
	}()
	go func() {
		defer wg.Done()
		quotes = widget.fetchQuotes(now)
	}()

	requests := make([]*SiteStatusRequest, len(widget.Sites))
	for i := range widget.Sites {
		requests[i] = widget.Sites[i].SiteStatusRequest
	}
	statuses, err := fetchStatusForSites(requests)
	wg.Wait()

	if err != nil {
		failures = append(failures, "Sites: "+cockpitErrorText(err))
	} else {
		for i := range widget.Sites {
			status := statuses[i]
			if slices.Contains(widget.Sites[i].AltStatusCodes, status.Code) {
				continue
			}
			if status.Code >= 400 || status.Error != nil {
				down = append(down, widget.Sites[i].Title)
			}
		}
	}

	if len(down) > 0 {
		items := make([]cockpitItem, len(down))
		for i, title := range down {
			items[i] = cockpitItem{Title: title, Tag: "not responding", Tone: "hot", URL: "/infra"}
		}
		rows = append(rows, cockpitRow{
			Kind: "down", Group: "down", Tone: "hot", Big: strconv.Itoa(len(down)),
			Label: pluralWord(len(down), "site down", "sites down"), Sub: strings.Join(down, ", "), Hint: "See sites", Items: items,
		})
	}

	for i := range widget.Apps {
		widget.Apps[i].Down = slices.Contains(down, widget.Apps[i].Title) || slices.Contains(down, widget.Apps[i].Site)
	}

	order := map[string]int{"down": 0, "money": 1, "fail": 2, "review": 3, "err": 4, "iss": 5, "owed": 6, "draft": 7}
	// Sources finish in any order, so break ties on Group to keep rows from swapping between updates
	slices.SortStableFunc(rows, func(a, b cockpitRow) int {
		if order[a.Kind] != order[b.Kind] {
			return order[a.Kind] - order[b.Kind]
		}
		return strings.Compare(a.Group, b.Group)
	})

	var needs, later []cockpitRow
	for _, row := range rows {
		if row.Tone != "" {
			needs = append(needs, row)
		} else {
			later = append(later, row)
		}
	}

	slices.Sort(failures)
	widget.Now = needs
	widget.Later = later
	widget.Failures = failures
	widget.SiteCount = len(widget.Sites)
	widget.SitesDown = down
	widget.Hours = hours
	widget.Next = next
	widget.Sky = sky
	widget.Quotes = quotes
	widget.Updated = now
	widget.withError(nil).scheduleNextUpdate()

	// page.js refetches and morphs this widget in every open tab
	publishEvent("custom-api:data_changed", map[string]any{"widget_id": widget.GetID(), "title": widget.Title})
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (widget *cockpitWidget) fetchGitHub() ([]cockpitRow, error) {
	const query = `{
mine:search(query:"is:pr is:open author:@me archived:false",type:ISSUE,first:50){nodes{...on PullRequest{title url repository{name nameWithOwner} commits(last:1){nodes{commit{statusCheckRollup{state}}}}}}}
review:search(query:"is:pr is:open review-requested:@me archived:false",type:ISSUE,first:30){nodes{...on PullRequest{title url repository{name nameWithOwner}}}}
}`
	request, err := cockpitRequest("POST", "https://api.github.com/graphql",
		map[string]string{"Authorization": "Bearer " + widget.GitHubToken}, map[string]string{"query": query})
	if err != nil {
		return nil, err
	}
	data, err := cockpitFetch(request)
	if err != nil {
		return nil, err
	}
	if !data.Get("data.mine").Exists() {
		return nil, errors.New("GitHub sent no data")
	}
	// GitHub answers 200 with both data and errors when the token can't read some PRs
	var partial error
	if data.Get("errors").Exists() {
		partial = errors.New("some PRs are hidden, the token can't read them")
	}

	var rows []cockpitRow
	var failing, pips []string
	var failItems, reviewItems []cockpitItem
	repos := map[string]bool{}
	running, open := 0, 0

	for _, node := range data.Get("data.mine.nodes").Array() {
		if node.Get("title").String() == "" {
			continue
		}
		open++
		state := node.Get("commits.nodes.0.commit.statusCheckRollup.state").String()
		if state == "FAILURE" || state == "ERROR" {
			failing = append(failing, node.Get("title").String())
			repos[node.Get("repository.name").String()] = true
			failItems = append(failItems, cockpitItem{
				Title: node.Get("title").String(), Tag: node.Get("repository.name").String(),
				URL: node.Get("url").String(), Tone: "hot",
			})
		} else if state == "PENDING" {
			running++
		}
	}

	for i := 0; i < open; i++ {
		switch {
		case i < len(failing):
			pips = append(pips, "hot")
		case i < len(failing)+running:
			pips = append(pips, "warn")
		default:
			pips = append(pips, "")
		}
	}

	if len(failing) > 0 {
		names := make([]string, 0, len(repos))
		for name := range repos {
			names = append(names, name)
		}
		slices.Sort(names)
		sub := "On " + strings.Join(names, ", ")
		if len(names) == 1 {
			sub = "All on " + names[0]
		}
		rows = append(rows, cockpitRow{
			Kind: "fail", Group: "fail", Tone: "hot", Big: strconv.Itoa(len(failing)),
			Label: pluralWord(len(failing), "PR failing checks", "PRs failing checks"), Sub: sub,
			Hint: "See PRs", Items: failItems, Pips: pips,
		})
	}

	seen := map[string]bool{}
	var reviewRepos []string
	for _, node := range data.Get("data.review.nodes").Array() {
		if node.Get("title").String() == "" {
			continue
		}
		name := node.Get("repository.nameWithOwner").String()
		reviewItems = append(reviewItems, cockpitItem{
			Title: node.Get("title").String(), Tag: name, URL: node.Get("url").String(), Tone: "warn",
		})
		if !seen[name] {
			seen[name] = true
			reviewRepos = append(reviewRepos, name)
		}
	}
	if len(reviewItems) > 0 {
		rows = append(rows, cockpitRow{
			Kind: "review", Group: "review", Tone: "warn", Big: strconv.Itoa(len(reviewItems)),
			Label: pluralWord(len(reviewItems), "review waiting on you", "reviews waiting on you"),
			Sub:   strings.Join(reviewRepos, ", "), Hint: "See reviews", Items: reviewItems,
		})
	}
	return rows, partial
}

func (widget *cockpitWidget) fetchGlitchTip(now time.Time) ([]cockpitRow, error) {
	g := widget.GlitchTip
	target := fmt.Sprintf("%s/api/0/organizations/%s/issues/?query=%s&limit=100", g.URL, g.Org, url.QueryEscape("is:unresolved"))
	request, err := cockpitRequest("GET", target, map[string]string{"Authorization": "Bearer " + g.Token}, nil)
	if err != nil {
		return nil, err
	}
	data, err := cockpitFetch(request)
	if err != nil {
		return nil, err
	}

	// The same error lands in several issues, one per release or culprit
	type group struct {
		title, id, project string
		events             int
	}
	var groups []*group
	byTitle := map[string]*group{}
	total := 0
	for _, issue := range data.Array() {
		title := issue.Get("title").String()
		events, _ := strconv.Atoi(issue.Get("count").String())
		total += events
		key := issue.Get("project.slug").String() + "|" + title
		if existing, ok := byTitle[key]; ok {
			existing.events += events
			continue
		}
		g := &group{title: title, id: issue.Get("id").String(), project: issue.Get("project.slug").String(), events: events}
		byTitle[key] = g
		groups = append(groups, g)
	}
	if len(groups) == 0 {
		return nil, nil
	}
	slices.SortStableFunc(groups, func(a, b *group) int { return b.events - a.events })

	items := make([]cockpitItem, len(groups))
	var spark []int
	peak := groups[0].events
	projects := map[string]bool{}
	for i, entry := range groups {
		projects[entry.project] = true
		items[i] = cockpitItem{
			Title: entry.title, Tag: strconv.Itoa(entry.events),
			URL: fmt.Sprintf("%s/%s/issues/%s", widget.GlitchTip.URL, widget.GlitchTip.Org, entry.id),
		}
		if i < 6 && peak > 0 {
			spark = append(spark, max(8, entry.events*100/peak))
		}
	}
	if len(items) > cockpitDrawerErrors {
		more := cockpitItem{
			Title: fmt.Sprintf("All %d in GlitchTip", len(items)),
			URL:   fmt.Sprintf("%s/%s/issues", widget.GlitchTip.URL, widget.GlitchTip.Org),
		}
		items = append(items[:cockpitDrawerErrors:cockpitDrawerErrors], more)
	}

	sub := intl.Sprintf("%d events", total)
	if len(projects) == 1 {
		sub += ", " + groups[0].project
	} else {
		sub += fmt.Sprintf(", %d projects", len(projects))
	}
	return []cockpitRow{{
		Kind: "err", Group: "err", Big: strconv.Itoa(len(groups)), Label: pluralWord(len(groups), "error", "errors"),
		Sub: sub, Hint: "See errors", Items: items, Spark: spark,
	}}, nil
}

func (widget *cockpitWidget) fetchPlane(now time.Time) ([]cockpitRow, error) {
	p := widget.Plane
	target := fmt.Sprintf("%s/api/v1/workspaces/%s/projects/%s/issues/?per_page=100&order_by=-updated_at&expand=state", p.URL, p.Workspace, p.ProjectID)
	request, err := cockpitRequest("GET", target, map[string]string{"X-API-Key": p.Token}, nil)
	if err != nil {
		return nil, err
	}
	data, err := cockpitFetch(request)
	if err != nil {
		return nil, err
	}

	var doing, todo []cockpitItem
	backlog, overdue := 0, 0
	nextDue := -1
	for _, issue := range data.Get("results").Array() {
		group := issue.Get("state.group").String()
		if group == "backlog" {
			backlog++
			continue
		}
		if group != "started" && group != "unstarted" {
			continue
		}
		seq := issue.Get("sequence_id").String()
		tag := strings.ToLower(issue.Get("state.name").String())
		if due, err := time.ParseInLocation("2006-01-02", issue.Get("target_date").String(), now.Location()); err == nil {
			days := cockpitDaysBetween(now, due)
			if days < 0 {
				tag += fmt.Sprintf(", %dd overdue", -days)
				overdue++
			} else {
				tag += fmt.Sprintf(", %dd", days)
				if nextDue < 0 || days < nextDue {
					nextDue = days
				}
			}
		}
		item := cockpitItem{
			Title: issue.Get("name").String(), Tag: tag,
			URL: fmt.Sprintf("%s/%s/browse/%s-%s/", p.URL, p.Workspace, p.Project, seq),
		}
		if group == "started" {
			doing = append(doing, item)
		} else {
			todo = append(todo, item)
		}
	}

	active := len(doing) + len(todo)
	if active == 0 && backlog == 0 {
		return nil, nil
	}
	sub := fmt.Sprintf("%d in progress", len(doing))
	if overdue > 0 {
		sub += fmt.Sprintf(", %d overdue", overdue)
	}
	if nextDue >= 0 {
		sub += fmt.Sprintf(", next due in %d days", nextDue)
	}
	if data.Get("next_page_results").Bool() {
		sub += ", older issues not shown"
	}
	return []cockpitRow{{
		Kind: "iss", Group: "iss", Big: strconv.Itoa(active), Label: p.Label + " issues active", Sub: sub, Hint: "See issues",
		Items: append(doing, todo...),
		Stack: []cockpitSegment{
			{Flex: float64(len(doing)), Color: "positive", Opacity: 1},
			{Flex: float64(len(todo)), Color: "primary", Opacity: 1},
			{Flex: float64(backlog), Color: "faint", Opacity: 1},
		},
	}}, nil
}

func startOfCockpitDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func (widget *cockpitWidget) fetchInvoices(index int, now time.Time) ([]cockpitRow, error) {
	company := widget.Invoices[index]
	headers := map[string]string{"X-API-TOKEN": company.Token, "X-Requested-With": "XMLHttpRequest"}
	list := func(status string) (gjson.Result, error) {
		target := company.URL + "/api/v1/invoices?per_page=100&status=active&include=client&sort=" + url.QueryEscape("due_date|asc") + "&client_status=" + status
		request, err := cockpitRequest("GET", target, headers, nil)
		if err != nil {
			return gjson.Result{}, err
		}
		return cockpitFetch(request)
	}

	unpaid, err := list("unpaid")
	if err != nil {
		return nil, err
	}
	drafts, err := list("draft")
	if err != nil {
		return nil, err
	}

	lateTotal, owedTotal, draftTotal := 0.0, 0.0, 0.0
	oldest, lateOther, owedOther, draftOther := 0, 0, 0, 0
	clients := map[string]bool{}
	var lateItems, owedItems, draftItems []cockpitItem
	var stack []cockpitSegment
	link := func(id string) string { return company.URL + "/invoices/" + id + "/edit" }
	label := func(invoice gjson.Result) string {
		return invoice.Get("client.display_name").String() + " #" + invoice.Get("number").String()
	}
	counted := func(invoice gjson.Result) bool {
		return invoice.Get("client.settings.currency_id").String() == company.CurrencyID
	}

	for _, invoice := range unpaid.Get("data").Array() {
		balance := invoice.Get("balance").Float()
		due, err := time.ParseInLocation("2006-01-02", invoice.Get("due_date").String(), now.Location())
		days := 0
		if err == nil {
			days = cockpitDaysBetween(due, now)
		}
		suffix := ""
		if !counted(invoice) {
			suffix = " other currency"
		}
		if err == nil && days > 0 {
			if counted(invoice) {
				lateTotal += balance
				stack = append(stack, cockpitSegment{Flex: balance, Color: "negative", Opacity: ternary(days > 30, 1.0, 0.5)})
			} else {
				lateOther++
			}
			oldest = max(oldest, days)
			clients[invoice.Get("client.display_name").String()] = true
			lateItems = append(lateItems, cockpitItem{
				Title: label(invoice), Tag: intl.Sprintf("%.0f, %dd late", balance, days) + suffix,
				URL: link(invoice.Get("id").String()), Tone: "hot",
			})
			continue
		}
		if counted(invoice) {
			owedTotal += balance
		} else {
			owedOther++
		}
		tag := intl.Sprintf("%.0f", balance) + suffix
		if err == nil {
			tag += ", due " + invoice.Get("due_date").String()
		}
		owedItems = append(owedItems, cockpitItem{Title: label(invoice), Tag: tag, URL: link(invoice.Get("id").String())})
	}
	for _, invoice := range drafts.Get("data").Array() {
		tag := intl.Sprintf("%.0f", invoice.Get("amount").Float())
		if counted(invoice) {
			draftTotal += invoice.Get("amount").Float()
		} else {
			draftOther++
			tag += " other currency"
		}
		draftItems = append(draftItems, cockpitItem{Title: label(invoice), Tag: tag, URL: link(invoice.Get("id").String())})
	}

	truncated := unpaid.Get("meta.pagination.total").Int() > int64(len(unpaid.Get("data").Array()))
	const otherNote = ", other currencies not counted"
	var rows []cockpitRow
	if len(lateItems) > 0 {
		from := fmt.Sprintf("%d clients", len(clients))
		if len(clients) == 1 {
			for name := range clients {
				from = name
			}
		}
		sub := fmt.Sprintf("Oldest is %d days overdue", oldest)
		if truncated {
			sub += ", over 100 unpaid, total too low"
		}
		big, what := intl.Sprintf("%.0f", lateTotal), company.Currency+" late from "+from
		if lateOther == len(lateItems) {
			big, what = strconv.Itoa(lateOther), pluralWord(lateOther, "invoice", "invoices")+" late from "+from
		} else if lateOther > 0 {
			sub += otherNote
		}
		rows = append(rows, cockpitRow{
			Kind: "money", Group: fmt.Sprintf("money-%d", index), Tone: "hot", Big: big,
			Label: what, Sub: sub, Hint: "See invoices", Items: lateItems, Stack: stack,
		})
	}
	if len(owedItems) > 0 {
		sub := company.Name
		if owedOther > 0 {
			sub += otherNote
		}
		if truncated {
			sub += ", over 100 unpaid, total too low"
		}
		big, what := intl.Sprintf("%.0f", owedTotal), company.Currency+" not due yet"
		if owedOther == len(owedItems) {
			big, what = strconv.Itoa(owedOther), pluralWord(owedOther, "invoice", "invoices")+" not due yet"
		}
		rows = append(rows, cockpitRow{
			Kind: "owed", Group: fmt.Sprintf("owed-%d", index), Big: big, Label: what,
			Sub: sub, Hint: "See invoices", Items: owedItems,
		})
	}
	if len(draftItems) > 0 {
		sub := fmt.Sprintf("%d not sent, %s", len(draftItems), company.Name)
		if draftOther > 0 {
			sub += otherNote
		}
		big, what := intl.Sprintf("%.0f", draftTotal), company.Currency+" in draft"
		if draftOther == len(draftItems) {
			big, what = strconv.Itoa(draftOther), pluralWord(draftOther, "draft", "drafts")+" in other currencies"
		}
		rows = append(rows, cockpitRow{
			Kind: "draft", Group: fmt.Sprintf("draft-%d", index), Big: big, Label: what,
			Sub: sub, Hint: "See drafts", Items: draftItems,
		})
	}
	return rows, nil
}

func (widget *cockpitWidget) solidtimeGet(path string) (gjson.Result, error) {
	s := widget.Solidtime
	request, err := cockpitRequest("GET", fmt.Sprintf("%s/api/v1/organizations/%s/charts/%s", s.URL, s.Org, path),
		map[string]string{"Authorization": "Bearer " + s.Token}, nil)
	if err != nil {
		return gjson.Result{}, err
	}
	// Solidtime rejects any Host outside TRUSTED_HOSTS, and Cloudflare blocks API calls on the public address
	request.Host = s.Host
	return cockpitFetch(request)
}

func (widget *cockpitWidget) fetchHours(now time.Time) (cockpitHours, error) {
	if widget.Solidtime.URL == "" {
		return cockpitHours{}, nil
	}
	daily, err := widget.solidtimeGet("daily-tracked-hours")
	if err != nil {
		return cockpitHours{}, err
	}
	seconds := map[string]float64{}
	for _, day := range daily.Array() {
		seconds[day.Get("date").String()] = day.Get("duration").Float()
	}

	today := startOfCockpitDay(now)
	monday := today.AddDate(0, 0, -(int(today.Weekday())+6)%7)
	hours := cockpitHours{Days: make([]cockpitDay, 7)}
	peak := 0.0
	for i := range hours.Days {
		date := monday.AddDate(0, 0, i)
		value := seconds[date.Format("2006-01-02")] / 3600
		hours.Week += value
		peak = max(peak, value)
		hours.Days[i] = cockpitDay{Letter: date.Format("Mon")[:1], Hours: value, Today: date.Equal(today)}
		if date.Equal(today) {
			hours.Today = value
		}
	}
	for i := range hours.Days {
		if peak > 0 {
			hours.Days[i].Height = max(5, int(hours.Days[i].Hours/peak*100))
		} else {
			hours.Days[i].Height = 5
		}
	}
	return hours, nil
}

// fetchNextEvents merges every calendar into one list. One failing feed only loses that
// feed's events, but if all fail the caller keeps the previous list.
func (widget *cockpitWidget) fetchNextEvents(now time.Time) ([]cockpitEvent, error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	var events []cockpitEvent
	var lastErr error
	answered := 0

	for _, calendar := range widget.Calendars {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request, err := cockpitRequest("GET", calendar.URL, nil, nil)
			var data gjson.Result
			if err == nil {
				data, err = cockpitFetch(request)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				lastErr = fmt.Errorf("Calendar %s: %s", calendar.Name, cockpitErrorText(err))
				slog.Error("Cockpit calendar failed", "calendar", calendar.Name, "error", cockpitErrorText(err))
				return
			}
			answered++
			for _, entry := range data.Get("events").Array() {
				start, err := time.Parse(time.RFC3339, entry.Get("start").String())
				if err != nil || start.Before(now.Add(-time.Hour)) {
					continue
				}
				event := cockpitEvent{
					Name: entry.Get("name").String(), Calendar: calendar.Name, holiday: calendar.Holidays, start: start,
					When: start.In(time.Local).Format("Mon 2 Jan, 3:04 PM"),
				}
				// All-day events come as midnight UTC, which would land on the evening before in Toronto
				if entry.Get("all_day").Bool() {
					event.start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local)
					event.When = start.Format("Mon 2 Jan")
				}
				events = append(events, event)
			}
		}()
	}
	wg.Wait()

	if answered == 0 && lastErr != nil {
		return nil, lastErr
	}
	// Accounts invited to the same meeting each report it, and feeds finish in any order
	slices.SortStableFunc(events, func(a, b cockpitEvent) int {
		if order := a.start.Compare(b.start); order != 0 {
			return order
		}
		return strings.Compare(a.Name+a.Calendar, b.Name+b.Calendar)
	})
	events = slices.CompactFunc(events, func(a, b cockpitEvent) bool {
		return a.start.Equal(b.start) && strings.EqualFold(a.Name, b.Name)
	})
	// Holiday feeds would otherwise fill the list before any real meeting
	holidays := 0
	events = slices.DeleteFunc(events, func(event cockpitEvent) bool {
		if !event.holiday {
			return false
		}
		holidays++
		return holidays > cockpitNextHolidays
	})
	return events[:min(len(events), cockpitNextEvents)], lastErr
}

func (widget *cockpitWidget) fetchSky() (*cockpitSky, error) {
	if widget.Location == "" {
		return nil, nil
	}
	if widget.place == nil {
		place, err := fetchOpenMeteoPlaceFromName(widget.Location)
		if err != nil {
			return nil, err
		}
		widget.place = place
	}
	current, err := fetchWeatherForOpenMeteoPlace(widget.place, "metric")
	if err != nil {
		return nil, err
	}
	sky := &cockpitSky{Temperature: current.Temperature, Condition: strings.ToLower(current.WeatherCodeAsString()), Timezone: widget.place.Timezone}

	target := fmt.Sprintf("https://api.open-meteo.com/v1/forecast?latitude=%f&longitude=%f&timezone=%s&forecast_days=1&daily=sunrise,sunset",
		widget.place.Latitude, widget.place.Longitude, url.QueryEscape(widget.place.Timezone))
	request, err := cockpitRequest("GET", target, nil, nil)
	if err != nil {
		return sky, nil
	}
	data, err := cockpitFetch(request)
	if err != nil {
		return sky, nil
	}
	parse := func(path string) (string, float64) {
		at, err := time.Parse("2006-01-02T15:04", data.Get(path).String())
		if err != nil {
			return "", 0
		}
		return at.Format("15:04"), float64(at.Hour()*60+at.Minute()) / 14.4
	}
	sky.Sunrise, sky.RiseAt = parse("daily.sunrise.0")
	sky.Sunset, sky.SetAt = parse("daily.sunset.0")
	return sky, nil
}

// Twelve Data allows 800 calls a day shared with the Markets page, so each symbol
// keeps its own clock and one failing provider can't make the others refetch.
func (widget *cockpitWidget) fetchQuotes(now time.Time) marketList {
	var quotes marketList
	if widget.quotesAt == nil {
		widget.quotesAt = map[string]time.Time{}
	}
	previous := func(symbol string) {
		for _, quote := range widget.Quotes {
			if quote.Symbol == symbol {
				quotes = append(quotes, quote)
			}
		}
	}
	for _, entry := range widget.Markets {
		if now.Sub(widget.quotesAt[entry.Symbol]) < cockpitQuotesEvery {
			previous(entry.Symbol)
			continue
		}
		widget.quotesAt[entry.Symbol] = now
		provider, err := getMarketProvider(entry.Provider)
		if err != nil {
			slog.Error("Cockpit market provider", "error", err)
			previous(entry.Symbol)
			continue
		}
		result, err := provider.FetchMarkets([]marketRequest{entry.marketRequest}, entry.APIKey)
		if err != nil {
			slog.Error("Cockpit market failed", "symbol", entry.Symbol, "error", cockpitErrorText(err))
			// Keep the last price rather than dropping the line
			previous(entry.Symbol)
			continue
		}
		quotes = append(quotes, result...)
	}
	return quotes
}

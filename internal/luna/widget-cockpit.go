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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

var cockpitWidgetTemplate = mustParseTemplate("cockpit.html", "widget-base.html")

const (
	cockpitQuotesEvery  = 15 * time.Minute
	cockpitDrawerErrors = 10
)

type cockpitWidget struct {
	widgetBase `yaml:",inline"`
	Apps       []struct {
		Title string          `yaml:"title"`
		URL   string          `yaml:"url"`
		Icon  customIconField `yaml:"icon"`
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
	CalendarURL string `yaml:"calendar-url"`
	Location    string `yaml:"location"`
	Zones       []struct {
		Timezone string `yaml:"timezone"`
		Label    string `yaml:"label"`
	} `yaml:"zones"`
	Markets []struct {
		marketRequest `yaml:",inline"`
		Provider      string `yaml:"provider"`
		APIKey        string `yaml:"api-key"`
	} `yaml:"markets"`

	Now       []cockpitRow  `yaml:"-"`
	Later     []cockpitRow  `yaml:"-"`
	Failures  []string      `yaml:"-"`
	SiteCount int           `yaml:"-"`
	SitesDown []string      `yaml:"-"`
	Hours     cockpitHours  `yaml:"-"`
	Next      *cockpitEvent `yaml:"-"`
	Sky       *cockpitSky   `yaml:"-"`
	Quotes    marketList    `yaml:"-"`
	Updated   time.Time     `yaml:"-"`
	place     *openMeteoPlaceResponseJson
	quotesAt  time.Time
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
	Name string
	When string
	URL  string
}

type cockpitSky struct {
	Temperature int
	Condition   string
	Sunrise     string
	Sunset      string
	RiseAt      float64
	SetAt       float64
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
	response, err := defaultHTTPClient.Do(request)
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
			if err != nil {
				slog.Error("Cockpit source failed", "source", name, "error", err)
				failures = append(failures, name+": "+err.Error())
				return
			}
			rows = append(rows, result...)
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
	var next *cockpitEvent
	var sky *cockpitSky
	var quotes marketList
	var down []string

	wg.Add(4)
	go func() {
		defer wg.Done()
		var err error
		if hours, err = widget.fetchHours(now); err != nil {
			slog.Error("Cockpit hours failed", "error", err)
			hours.Error = "Solidtime: " + err.Error()
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if next, err = widget.fetchNextEvent(); err != nil {
			slog.Error("Cockpit calendar failed", "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		if sky, err = widget.fetchSky(); err != nil {
			slog.Error("Cockpit weather failed", "error", err)
		}
	}()
	go func() {
		defer wg.Done()
		// Twelve Data allows 800 calls a day, shared with the Markets page
		if now.Sub(widget.quotesAt) < cockpitQuotesEvery {
			quotes = widget.Quotes
			return
		}
		quotes = widget.fetchQuotes()
		widget.quotesAt = now
	}()

	requests := make([]*SiteStatusRequest, len(widget.Sites))
	for i := range widget.Sites {
		requests[i] = widget.Sites[i].SiteStatusRequest
	}
	statuses, err := fetchStatusForSites(requests)
	wg.Wait()

	if err != nil {
		failures = append(failures, "Sites: "+err.Error())
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
			Group: "down", Tone: "hot", Big: strconv.Itoa(len(down)),
			Label: pluralWord(len(down), "site down", "sites down"), Sub: strings.Join(down, ", "), Hint: "See sites", Items: items,
		})
	}

	for i := range widget.Apps {
		widget.Apps[i].Down = slices.Contains(down, widget.Apps[i].Title)
	}

	order := map[string]int{"down": 0, "money": 1, "fail": 2, "review": 3, "err": 4, "iss": 5, "owed": 6, "draft": 7}
	slices.SortStableFunc(rows, func(a, b cockpitRow) int { return order[a.Group] - order[b.Group] })

	widget.Now, widget.Later = nil, nil
	for _, row := range rows {
		if row.Tone != "" {
			widget.Now = append(widget.Now, row)
		} else {
			widget.Later = append(widget.Later, row)
		}
	}

	slices.Sort(failures)
	widget.Failures = failures
	widget.SiteCount = len(widget.Sites)
	widget.SitesDown = down
	widget.Hours = hours
	widget.Next = next
	widget.Sky = sky
	widget.Quotes = quotes
	widget.Updated = now
	widget.withError(nil)
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
		return nil, errors.New(data.Get("errors.0.message").String())
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
			Group: "fail", Tone: "hot", Big: strconv.Itoa(len(failing)),
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
			Group: "review", Tone: "warn", Big: strconv.Itoa(len(reviewItems)),
			Label: pluralWord(len(reviewItems), "review waiting on you", "reviews waiting on you"),
			Sub:   strings.Join(reviewRepos, ", "), Hint: "See reviews", Items: reviewItems,
		})
	}
	return rows, nil
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
		Group: "err", Big: strconv.Itoa(len(groups)), Label: pluralWord(len(groups), "error", "errors"),
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
	backlog := 0
	nextDue := -1
	today := startOfCockpitDay(now)
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
			days := int(due.Sub(today).Hours() / 24)
			tag += fmt.Sprintf(", %dd", days)
			if nextDue < 0 || days < nextDue {
				nextDue = days
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
	if nextDue >= 0 {
		sub += fmt.Sprintf(", next due in %d days", nextDue)
	}
	if data.Get("next_page_results").Bool() {
		sub += ", older issues not shown"
	}
	return []cockpitRow{{
		Group: "iss", Big: strconv.Itoa(active), Label: p.Label + " issues active", Sub: sub, Hint: "See issues",
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

	today := startOfCockpitDay(now)
	lateTotal, owedTotal, draftTotal := 0.0, 0.0, 0.0
	oldest := 0
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
			days = int(today.Sub(due).Hours() / 24)
		}
		suffix := ""
		if !counted(invoice) {
			suffix = " other currency"
		}
		if err == nil && days > 0 {
			if counted(invoice) {
				lateTotal += balance
				stack = append(stack, cockpitSegment{Flex: balance, Color: "negative", Opacity: ternary(days > 30, 1.0, 0.5)})
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
		}
		tag := intl.Sprintf("%.0f", balance) + suffix
		if err == nil {
			tag += ", due " + invoice.Get("due_date").String()
		}
		owedItems = append(owedItems, cockpitItem{Title: label(invoice), Tag: tag, URL: link(invoice.Get("id").String())})
	}
	for _, invoice := range drafts.Get("data").Array() {
		draftTotal += invoice.Get("amount").Float()
		draftItems = append(draftItems, cockpitItem{
			Title: label(invoice), Tag: intl.Sprintf("%.0f", invoice.Get("amount").Float()), URL: link(invoice.Get("id").String()),
		})
	}

	var rows []cockpitRow
	if len(lateItems) > 0 {
		from := fmt.Sprintf("%d clients", len(clients))
		if len(clients) == 1 {
			for name := range clients {
				from = name
			}
		}
		sub := fmt.Sprintf("Oldest is %d days overdue", oldest)
		if unpaid.Get("meta.pagination.total").Int() > int64(len(unpaid.Get("data").Array())) {
			sub += ", over 100 unpaid, total too low"
		}
		rows = append(rows, cockpitRow{
			Group: "money", Tone: "hot", Big: intl.Sprintf("%.0f", lateTotal),
			Label: company.Currency + " late from " + from, Sub: sub, Hint: "See invoices", Items: lateItems, Stack: stack,
		})
	}
	if len(owedItems) > 0 {
		rows = append(rows, cockpitRow{
			Group: "owed", Big: intl.Sprintf("%.0f", owedTotal), Label: company.Currency + " not due yet",
			Sub: company.Name, Hint: "See invoices", Items: owedItems,
		})
	}
	if len(draftItems) > 0 {
		rows = append(rows, cockpitRow{
			Group: "draft", Big: intl.Sprintf("%.0f", draftTotal), Label: company.Currency + " in draft",
			Sub: fmt.Sprintf("%d not sent, %s", len(draftItems), company.Name), Hint: "See drafts", Items: draftItems,
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

func (widget *cockpitWidget) fetchNextEvent() (*cockpitEvent, error) {
	if widget.CalendarURL == "" {
		return nil, nil
	}
	request, err := cockpitRequest("GET", widget.CalendarURL, nil, nil)
	if err != nil {
		return nil, err
	}
	data, err := cockpitFetch(request)
	if err != nil {
		return nil, err
	}
	first := data.Get("events.0")
	if !first.Exists() {
		return nil, nil
	}
	start, err := time.Parse(time.RFC3339, first.Get("start").String())
	if err != nil {
		return nil, err
	}
	return &cockpitEvent{
		Name: first.Get("name").String(), URL: first.Get("url").String(),
		When: start.In(time.Local).Format("Mon 2 Jan, 3:04 PM"),
	}, nil
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
	sky := &cockpitSky{Temperature: current.Temperature, Condition: strings.ToLower(current.WeatherCodeAsString())}

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

func (widget *cockpitWidget) fetchQuotes() marketList {
	var quotes marketList
	for _, entry := range widget.Markets {
		provider, err := getMarketProvider(entry.Provider)
		if err != nil {
			slog.Error("Cockpit market provider", "error", err)
			continue
		}
		result, err := provider.FetchMarkets([]marketRequest{entry.marketRequest}, entry.APIKey)
		if err != nil {
			slog.Error("Cockpit market failed", "symbol", entry.Symbol, "error", err)
			// Keep the last price rather than dropping the line
			for _, previous := range widget.Quotes {
				if previous.Symbol == entry.Symbol {
					quotes = append(quotes, previous)
				}
			}
			continue
		}
		quotes = append(quotes, result...)
	}
	return quotes
}

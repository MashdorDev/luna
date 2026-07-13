package luna

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	redditWidgetHorizontalCardsTemplate = mustParseTemplate("reddit-horizontal-cards.html", "widget-base.html")
	redditWidgetVerticalCardsTemplate   = mustParseTemplate("reddit-vertical-cards.html", "widget-base.html")
)

type redditWidget struct {
	widgetBase          `yaml:",inline"`
	Posts               forumPostList     `yaml:"-"`
	PrevPostIDs         map[string]struct{} `yaml:"-"`
	Subreddit           string            `yaml:"subreddit"`
	Proxy               proxyOptionsField `yaml:"proxy"`
	Style               string            `yaml:"style"`
	ShowThumbnails      bool              `yaml:"show-thumbnails"`
	ShowFlairs          bool              `yaml:"show-flairs"`
	SortBy              string            `yaml:"sort-by"`
	TopPeriod           string            `yaml:"top-period"`
	Search              string            `yaml:"search"`
	ExtraSortBy         string            `yaml:"extra-sort-by"`
	CommentsURLTemplate string            `yaml:"comments-url-template"`
	Limit               int               `yaml:"limit"`
	CollapseAfter       int               `yaml:"collapse-after"`
	RequestURLTemplate  string            `yaml:"request-url-template"`

	AppAuth struct {
		Name   string `yaml:"name"`
		ID     string `yaml:"id"`
		Secret string `yaml:"secret"`

		enabled        bool
		accessToken    string
		tokenExpiresAt time.Time
	} `yaml:"app-auth"`
}

func (widget *redditWidget) initialize() error {
	if widget.Subreddit == "" {
		return errors.New("subreddit is required")
	}

	if widget.Limit <= 0 {
		widget.Limit = 15
	}

	if widget.CollapseAfter == 0 || widget.CollapseAfter < -1 {
		widget.CollapseAfter = 5
	}

	s := widget.SortBy
	if s != "hot" && s != "new" && s != "top" && s != "rising" {
		widget.SortBy = "hot"
	}

	p := widget.TopPeriod
	if p != "hour" && p != "day" && p != "week" && p != "month" && p != "year" && p != "all" {
		widget.TopPeriod = "day"
	}

	if widget.RequestURLTemplate != "" {
		if !strings.Contains(widget.RequestURLTemplate, "{REQUEST-URL}") {
			return errors.New("no `{REQUEST-URL}` placeholder specified")
		}
	}

	a := &widget.AppAuth
	if a.Name != "" || a.ID != "" || a.Secret != "" {
		if a.Name == "" || a.ID == "" || a.Secret == "" {
			return errors.New("application name, client ID and client secret are required")
		}
		a.enabled = true
	}

	widget.
		withTitle("r/" + widget.Subreddit).
		withTitleURL("https://www.reddit.com/r/" + widget.Subreddit + "/").
		withCacheDuration(30 * time.Minute)

	// show last-known posts immediately after a restart; the rate-limited
	// live refresh replaces them on the normal schedule
	widget.loadCachedPosts()

	return nil
}

// Reddit's unauthenticated per-IP token bucket refills at roughly one
// request per 40s (measured 2026-07: a request after 45s of quiet passes,
// one 10-20s after another 429s). Allow one reddit fetch per 60s across all
// widgets; a widget that doesn't get the slot skips its update and stays
// overdue, so the 15s background ticker drains the queue gradually instead
// of tripping the limit in one burst.
var (
	redditRequestMu     sync.Mutex
	redditNextRequestAt time.Time
	// set once the JSON API is confirmed blocked and RSS works, so
	// subsequent updates stop wasting a request on the doomed JSON call
	redditJSONBlocked atomic.Bool
)

// Reddit fills at only one widget per minute (rate limit above), so a restart
// used to mean up to an hour of empty tabs. Persist each widget's last posts
// to disk and load them at startup — the dashboard comes back with last-known
// content immediately and refreshes on the normal schedule.
const redditPostsCacheMaxAge = 24 * time.Hour

func redditCacheDir() string {
	if dir := os.Getenv("LUNA_CACHE_DIR"); dir != "" {
		return dir
	}
	return "/app/cache"
}

type redditPostsCacheFile struct {
	SavedAt time.Time     `json:"saved_at"`
	Posts   forumPostList `json:"posts"`
}

func (widget *redditWidget) postsCachePath() string {
	key := fmt.Sprintf("%s|%s|%s|%s|%d",
		widget.Subreddit, widget.SortBy, widget.TopPeriod, widget.Search, widget.Limit)
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(redditCacheDir(), "reddit-"+hex.EncodeToString(sum[:8])+".json")
}

func (widget *redditWidget) loadCachedPosts() {
	data, err := os.ReadFile(widget.postsCachePath())
	if err != nil {
		return
	}

	var cached redditPostsCacheFile
	if json.Unmarshal(data, &cached) != nil ||
		len(cached.Posts) == 0 ||
		time.Since(cached.SavedAt) > redditPostsCacheMaxAge {
		return
	}

	widget.Posts = cached.Posts
	widget.ContentAvailable = true

	// seed seen-post IDs so the first live refresh doesn't notify about
	// posts that were already on screen before the restart
	ids := make(map[string]struct{}, len(cached.Posts))
	for i := range cached.Posts {
		if id := cached.Posts[i].ID; id != "" {
			ids[id] = struct{}{}
		}
	}
	widget.PrevPostIDs = ids
}

func (widget *redditWidget) saveCachedPosts(posts forumPostList) {
	path := widget.postsCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return
	}

	data, err := json.Marshal(redditPostsCacheFile{SavedAt: time.Now(), Posts: posts})
	if err != nil {
		return
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		slog.Error("Failed to write reddit posts cache", "path", path, "error", err)
	}
}

func tryAcquireRedditRequestSlot() bool {
	redditRequestMu.Lock()
	defer redditRequestMu.Unlock()

	now := time.Now()
	if now.Before(redditNextRequestAt) {
		return false
	}
	redditNextRequestAt = now.Add(60 * time.Second)
	return true
}

// A 429 means the IP is in a penalty state — steady pressure keeps it there.
// Go fully quiet for a while so the throttle can lift.
func coolDownRedditRequests() {
	redditRequestMu.Lock()
	defer redditRequestMu.Unlock()

	cooldownUntil := time.Now().Add(5 * time.Minute)
	if cooldownUntil.After(redditNextRequestAt) {
		redditNextRequestAt = cooldownUntil
	}
}

func (widget *redditWidget) update(ctx context.Context) {
	// app-auth has its own generous quota; only unauthenticated requests
	// need to be spaced out
	if !widget.AppAuth.enabled && !tryAcquireRedditRequestSlot() {
		return
	}

	posts, err := widget.fetchSubredditPosts()
	if !widget.canContinueUpdateAfterHandlingErr(err) {
		return
	}

	if len(posts) > widget.Limit {
		posts = posts[:widget.Limit]
	}

	if widget.ExtraSortBy == "engagement" {
		posts.calculateEngagement()
		posts.sortByEngagement()
	}

	widget.notifyOnNewPosts(posts)

	widget.Posts = posts
	widget.saveCachedPosts(posts)
}

func (widget *redditWidget) Render() template.HTML {
	if widget.Style == "horizontal-cards" {
		return widget.renderTemplate(widget, redditWidgetHorizontalCardsTemplate)
	}

	if widget.Style == "vertical-cards" {
		return widget.renderTemplate(widget, redditWidgetVerticalCardsTemplate)
	}

	return widget.renderTemplate(widget, forumPostsTemplate)

}

type subredditResponseJson struct {
	Data struct {
		Children []struct {
			Data struct {
				Id            string  `json:"id"`
				Title         string  `json:"title"`
				Upvotes       int     `json:"ups"`
				Url           string  `json:"url"`
				Time          float64 `json:"created"`
				CommentsCount int     `json:"num_comments"`
				Domain        string  `json:"domain"`
				Permalink     string  `json:"permalink"`
				Stickied      bool    `json:"stickied"`
				Pinned        bool    `json:"pinned"`
				IsSelf        bool    `json:"is_self"`
				Thumbnail     string  `json:"thumbnail"`
				Flair         string  `json:"link_flair_text"`
				ParentList    []struct {
					Id        string `json:"id"`
					Subreddit string `json:"subreddit"`
					Permalink string `json:"permalink"`
				} `json:"crosspost_parent_list"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

func (widget *redditWidget) parseCustomCommentsURL(subreddit, postId, postPath string) string {
	template := strings.ReplaceAll(widget.CommentsURLTemplate, "{SUBREDDIT}", subreddit)
	template = strings.ReplaceAll(template, "{POST-ID}", postId)
	template = strings.ReplaceAll(template, "{POST-PATH}", strings.TrimLeft(postPath, "/"))

	return template
}

func (widget *redditWidget) fetchSubredditPosts() (forumPostList, error) {
	var client requestDoer = defaultHTTPClient
	var baseURL string
	var requestURL string
	var headers http.Header
	query := url.Values{}
	app := &widget.AppAuth

	if !app.enabled {
		baseURL = "https://www.reddit.com"
		headers = http.Header{
			"User-Agent": []string{getBrowserUserAgentHeader()},
		}
	} else {
		baseURL = "https://oauth.reddit.com"

		if app.accessToken == "" || time.Now().Add(time.Minute).After(app.tokenExpiresAt) {
			if err := widget.fetchNewAppAccessToken(); err != nil {
				return nil, fmt.Errorf("fetching new app access token: %v", err)
			}
		}

		headers = http.Header{
			"Authorization": []string{"Bearer " + app.accessToken},
			"User-Agent":    []string{app.Name + "/1.0"},
		}
	}

	if widget.Limit > 25 {
		query.Set("limit", strconv.Itoa(widget.Limit))
	}

	if widget.Search != "" {
		query.Set("q", widget.Search+" subreddit:"+widget.Subreddit)
		query.Set("sort", widget.SortBy)
		requestURL = fmt.Sprintf("%s/search.json?%s", baseURL, query.Encode())
	} else {
		if widget.SortBy == "top" {
			query.Set("t", widget.TopPeriod)
		}
		requestURL = fmt.Sprintf("%s/r/%s/%s.json?%s", baseURL, widget.Subreddit, widget.SortBy, query.Encode())
	}

	if widget.RequestURLTemplate != "" {
		requestURL = strings.ReplaceAll(widget.RequestURLTemplate, "{REQUEST-URL}", requestURL)
	} else if widget.Proxy.client != nil {
		client = widget.Proxy.client
	}

	canFallBackToRSS := !app.enabled && widget.RequestURLTemplate == ""

	// once the JSON API is known blocked, go straight to RSS instead of
	// burning a rate-limit slot on a request that will 403
	if canFallBackToRSS && redditJSONBlocked.Load() {
		posts, err := widget.fetchSubredditPostsViaRSS(client, requestURL)
		if err != nil {
			slog.Error("Reddit RSS fetch failed", "subreddit", widget.Subreddit, "error", err)
			// a 429 just means we're pacing through a throttle — stay on RSS
			if !strings.Contains(err.Error(), "429") {
				redditJSONBlocked.Store(false) // RSS broke; retry the API next cycle
			}
			return nil, err
		}
		return posts, nil
	}

	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header = headers

	responseJson, err := decodeJsonFromRequest[subredditResponseJson](client, request)
	if err != nil {
		// Reddit blocks the unauthenticated JSON API from many server IPs but
		// leaves the RSS feeds open to browser user agents — fall back to RSS
		// (same listing, no score/comment counts). Mark the API blocked on
		// 403/429 even if RSS also fails, so later attempts stop burning a
		// rate-limit slot on the doomed JSON call.
		if canFallBackToRSS {
			if strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "429") {
				redditJSONBlocked.Store(true)
			}
			posts, rssErr := widget.fetchSubredditPostsViaRSS(client, requestURL)
			if rssErr == nil {
				redditJSONBlocked.Store(true)
				return posts, nil
			}
			slog.Error("Reddit JSON and RSS fetch failed", "subreddit", widget.Subreddit, "jsonError", err, "rssError", rssErr)
		}
		return nil, err
	}

	if len(responseJson.Data.Children) == 0 {
		return nil, fmt.Errorf("no posts found")
	}

	posts := make(forumPostList, 0, len(responseJson.Data.Children))

	for i := range responseJson.Data.Children {
		post := &responseJson.Data.Children[i].Data

		if post.Stickied || post.Pinned {
			continue
		}

		var commentsUrl string

		if widget.CommentsURLTemplate == "" {
			commentsUrl = "https://www.reddit.com" + post.Permalink
		} else {
			commentsUrl = widget.parseCustomCommentsURL(widget.Subreddit, post.Id, post.Permalink)
		}

		forumPost := forumPost{
			ID:              post.Id,
			Title:           html.UnescapeString(post.Title),
			DiscussionUrl:   commentsUrl,
			TargetUrlDomain: post.Domain,
			CommentCount:    post.CommentsCount,
			Score:           post.Upvotes,
			TimePosted:      time.Unix(int64(post.Time), 0),
		}

		if post.Thumbnail != "" && post.Thumbnail != "self" && post.Thumbnail != "default" && post.Thumbnail != "nsfw" {
			forumPost.ThumbnailUrl = html.UnescapeString(post.Thumbnail)
		}

		if !post.IsSelf {
			forumPost.TargetUrl = post.Url
		}

		if widget.ShowFlairs && post.Flair != "" {
			forumPost.Tags = append(forumPost.Tags, post.Flair)
		}

		if len(post.ParentList) > 0 {
			forumPost.IsCrosspost = true
			forumPost.TargetUrlDomain = "r/" + post.ParentList[0].Subreddit

			if widget.CommentsURLTemplate == "" {
				forumPost.TargetUrl = "https://www.reddit.com" + post.ParentList[0].Permalink
			} else {
				forumPost.TargetUrl = widget.parseCustomCommentsURL(
					post.ParentList[0].Subreddit,
					post.ParentList[0].Id,
					post.ParentList[0].Permalink,
				)
			}
		}

		posts = append(posts, forumPost)
	}

	return posts, nil
}

func (widget *redditWidget) fetchSubredditPostsViaRSS(client requestDoer, jsonRequestURL string) (forumPostList, error) {
	requestURL := strings.Replace(jsonRequestURL, ".json?", ".rss?", 1)

	request, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, err
	}
	setBrowserUserAgentHeader(request)

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		if response.StatusCode == http.StatusTooManyRequests {
			coolDownRedditRequests()
		}
		return nil, fmt.Errorf("unexpected status code %d from %s", response.StatusCode, requestURL)
	}

	feed, err := feedParser.Parse(response.Body)
	if err != nil {
		return nil, err
	}

	posts := make(forumPostList, 0, len(feed.Items))

	for _, item := range feed.Items {
		post := forumPost{
			ID:            item.GUID,
			Title:         html.UnescapeString(item.Title),
			DiscussionUrl: item.Link,
			// RSS has no vote/comment data — negative hides them in templates
			Score:        -1,
			CommentCount: -1,
			TimePosted:   time.Now(),
		}

		if item.PublishedParsed != nil {
			post.TimePosted = *item.PublishedParsed
		} else if item.UpdatedParsed != nil {
			post.TimePosted = *item.UpdatedParsed
		}

		if media, ok := item.Extensions["media"]; ok {
			if thumbnails, ok := media["thumbnail"]; ok && len(thumbnails) > 0 {
				post.ThumbnailUrl = thumbnails[0].Attrs["url"]
			}
		}

		posts = append(posts, post)
	}

	if len(posts) == 0 {
		return nil, errNoContent
	}

	return posts, nil
}

func (widget *redditWidget) notifyOnNewPosts(posts forumPostList) {
	previousIDs := widget.PrevPostIDs
	currentIDs := make(map[string]struct{}, len(posts))
	newPosts := make([]forumPost, 0, len(posts))

	for i := range posts {
		id := posts[i].ID
		if id == "" {
			continue
		}
		currentIDs[id] = struct{}{}
		if previousIDs != nil {
			if _, exists := previousIDs[id]; !exists {
				newPosts = append(newPosts, posts[i])
			}
		}
	}

	widget.PrevPostIDs = currentIDs

	if previousIDs == nil || !widget.Notifications || !NotificationsEnabledForWidget("reddit") {
		return
	}

	if !StringSetChanged(previousIDs, currentIDs) {
		return
	}

	body := "Reddit feed updated."
	if len(newPosts) > 0 {
		maxItems := min(3, len(newPosts))
		lines := make([]string, 0, maxItems+1)
		lines = append(lines, fmt.Sprintf("%d new Reddit post(s)", len(newPosts)))
		for i := 0; i < maxItems; i++ {
			line := "- " + newPosts[i].Title
			url := newPosts[i].TargetUrl
			if url == "" {
				url = newPosts[i].DiscussionUrl
			}
			if url != "" {
				line = line + " (" + url + ")"
			}
			lines = append(lines, line)
		}
		body = strings.Join(lines, "\n")
	}

	SendWidgetNotification("reddit", "Reddit: "+widget.Title, body, "info")
}

func (widget *redditWidget) fetchNewAppAccessToken() error {
	body := strings.NewReader("grant_type=client_credentials")
	req, err := http.NewRequest("POST", "https://www.reddit.com/api/v1/access_token", body)
	if err != nil {
		return fmt.Errorf("creating request for app access token: %v", err)
	}

	app := &widget.AppAuth
	req.SetBasicAuth(app.ID, app.Secret)
	req.Header.Add("User-Agent", app.Name+"/1.0")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	type tokenResponse struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}

	client := ternary(widget.Proxy.client != nil, widget.Proxy.client, defaultHTTPClient)
	response, err := decodeJsonFromRequest[tokenResponse](client, req)
	if err != nil {
		return err
	}

	app.accessToken = response.AccessToken
	app.tokenExpiresAt = time.Now().Add(time.Duration(response.ExpiresIn) * time.Second)

	return nil
}

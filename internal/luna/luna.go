package luna

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

var (
	pageTemplate        = mustParseTemplate("page.html", "document.html", "footer.html")
	pageContentTemplate = mustParseTemplate("page-content.html")
	manifestTemplate    = mustParseTemplate("manifest.json")
)

const STATIC_ASSETS_CACHE_DURATION = 24 * time.Hour

var reservedPageSlugs = []string{"login", "logout"}

type application struct {
	Version   string
	CreatedAt time.Time
	Config    config

	parsedManifest []byte

	slugToPage map[string]*page
	widgetByID map[uint64]widget

	RequiresAuth           bool
	authSecretKey          []byte
	usernameHashToUsername map[string]string
	authAttemptsMu         sync.Mutex
	failedAuthAttempts     map[string]*failedAuthAttempt
	monitorCtx             context.Context
	monitorCancel          context.CancelFunc
}

// registerWidgetRecursive registers a widget and all its child widgets (if any)
func (a *application) registerWidgetRecursive(w widget) {
	a.widgetByID[w.GetID()] = w
	
	// If this widget is a container (group or split-column), register its children too
	switch cw := w.(type) {
	case *groupWidget:
		for _, childWidget := range cw.Widgets {
			a.registerWidgetRecursive(childWidget)
		}
	case *splitColumnWidget:
		for _, childWidget := range cw.Widgets {
			a.registerWidgetRecursive(childWidget)
		}
	}
}

func newApplication(c *config) (*application, error) {
	app := &application{
		Version:    buildVersion,
		CreatedAt:  time.Now(),
		Config:     *c,
		slugToPage: make(map[string]*page),
		widgetByID: make(map[uint64]widget),
	}

	// initialize global event hub
	globalEventHub = newEventHub()
	config := &app.Config

	//
	// Init auth
	//

	if len(config.Auth.Users) > 0 {
		secretBytes, err := base64.StdEncoding.DecodeString(config.Auth.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("decoding secret-key: %v", err)
		}

		if len(secretBytes) != AUTH_SECRET_KEY_LENGTH {
			return nil, fmt.Errorf("secret-key must be exactly %d bytes", AUTH_SECRET_KEY_LENGTH)
		}

		app.usernameHashToUsername = make(map[string]string)
		app.failedAuthAttempts = make(map[string]*failedAuthAttempt)
		app.RequiresAuth = true

		for username := range config.Auth.Users {
			user := config.Auth.Users[username]
			usernameHash, err := computeUsernameHash(username, secretBytes)
			if err != nil {
				return nil, fmt.Errorf("computing username hash for user %s: %v", username, err)
			}
			app.usernameHashToUsername[string(usernameHash)] = username

			if user.PasswordHashString != "" {
				user.PasswordHash = []byte(user.PasswordHashString)
				user.PasswordHashString = ""
			} else {
				hashedPassword, err := bcrypt.GenerateFromPassword([]byte(user.Password), bcrypt.DefaultCost)
				if err != nil {
					return nil, fmt.Errorf("hashing password for user %s: %v", username, err)
				}

				user.Password = ""
				user.PasswordHash = hashedPassword
			}
		}

		app.authSecretKey = secretBytes
	}

	//
	// Init themes
	//

	if !config.Theme.DisablePicker {
		themeKeys := make([]string, 0, 2)
		themeProps := make([]*themeProperties, 0, 2)

		defaultDarkTheme, ok := config.Theme.Presets.Get("default-dark")
		if ok && !config.Theme.SameAs(defaultDarkTheme) || !config.Theme.SameAs(&themeProperties{}) {
			themeKeys = append(themeKeys, "default-dark")
			themeProps = append(themeProps, &themeProperties{})
		}

		themeKeys = append(themeKeys, "default-light")
		themeProps = append(themeProps, &themeProperties{
			Light:                    true,
			BackgroundColor:          &hslColorField{240, 13, 95},
			PrimaryColor:             &hslColorField{230, 100, 30},
			NegativeColor:            &hslColorField{0, 70, 50},
			ContrastMultiplier:       1.3,
			TextSaturationMultiplier: 0.5,
		})

		themePresets, err := newOrderedYAMLMap(themeKeys, themeProps)
		if err != nil {
			return nil, fmt.Errorf("creating theme presets: %v", err)
		}
		config.Theme.Presets = *themePresets.Merge(&config.Theme.Presets)

		for key, properties := range config.Theme.Presets.Items() {
			properties.Key = key
			if err := properties.init(); err != nil {
				return nil, fmt.Errorf("initializing preset theme %s: %v", key, err)
			}
		}
	}

	config.Theme.Key = "default"
	if err := config.Theme.init(); err != nil {
		return nil, fmt.Errorf("initializing default theme: %v", err)
	}

	//
	// Init pages
	//

	app.slugToPage[""] = &config.Pages[0]

	providers := &widgetProviders{
		assetResolver: app.StaticAssetPath,
	}

	// initialize context for background workers
	app.monitorCtx, app.monitorCancel = context.WithCancel(context.Background())

	for p := range config.Pages {
		page := &config.Pages[p]
		page.PrimaryColumnIndex = -1

		if page.Slug == "" {
			page.Slug = titleToSlug(page.Title)
		}

		if slices.Contains(reservedPageSlugs, page.Slug) {
			return nil, fmt.Errorf("page slug \"%s\" is reserved", page.Slug)
		}

		app.slugToPage[page.Slug] = page

		if page.Width == "default" {
			page.Width = ""
		}

		if page.DesktopNavigationWidth == "" && page.DesktopNavigationWidth != "default" {
			page.DesktopNavigationWidth = page.Width
		}

		for i := range page.HeadWidgets {
			widget := page.HeadWidgets[i]
			app.registerWidgetRecursive(widget)
			widget.setProviders(providers)
		}

		for c := range page.Columns {
			column := &page.Columns[c]

			if page.PrimaryColumnIndex == -1 && column.Size == "full" {
				page.PrimaryColumnIndex = int8(c)
			}

			for w := range column.Widgets {
				widget := column.Widgets[w]
				app.registerWidgetRecursive(widget)
				widget.setProviders(providers)
			}
		}
	}

	config.Server.BaseURL = strings.TrimRight(config.Server.BaseURL, "/")
	config.Theme.CustomCSSFile = app.resolveUserDefinedAssetPath(config.Theme.CustomCSSFile)
	config.Branding.LogoURL = app.resolveUserDefinedAssetPath(config.Branding.LogoURL)

	config.Branding.FaviconURL = ternary(
		config.Branding.FaviconURL == "",
		app.StaticAssetPath("favicon.svg"),
		app.resolveUserDefinedAssetPath(config.Branding.FaviconURL),
	)

	config.Branding.FaviconType = ternary(
		strings.HasSuffix(config.Branding.FaviconURL, ".svg"),
		"image/svg+xml",
		"image/png",
	)

	if config.Branding.AppName == "" {
		config.Branding.AppName = "Luna"
	}

	if config.Branding.AppIconURL == "" {
		config.Branding.AppIconURL = app.StaticAssetPath("app-icon.png")
	}

	if config.Branding.AppBackgroundColor == "" {
		config.Branding.AppBackgroundColor = config.Theme.BackgroundColorAsHex
	}

	manifest, err := executeTemplateToString(manifestTemplate, templateData{App: app})
	if err != nil {
		return nil, fmt.Errorf("parsing manifest.json: %v", err)
	}
	app.parsedManifest = []byte(manifest)

	// start background updater to check monitor and custom-api widgets periodically
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()

		updateWidgetIfNeeded := func(w widget, now *time.Time) {
			// honor each widget's cache duration — update() reschedules
			// nextUpdate, so without this guard every tick refetches
			if !w.requiresUpdate(now) {
				return
			}
			if mon, ok := w.(*monitorWidget); ok {
				mon.update(app.monitorCtx)
			} else if api, ok := w.(*customAPIWidget); ok {
				api.update(app.monitorCtx)
			} else if reddit, ok := w.(*redditWidget); ok {
				// reddit updates must go through the ticker: page loads
				// grant only one rate-limit slot per wave, so overdue
				// widgets would otherwise never drain
				reddit.update(app.monitorCtx)
			}
		}

		var updateWidgetsRecursive func([]widget, *time.Time)
		updateWidgetsRecursive = func(widgets []widget, now *time.Time) {
			for i := range widgets {
				w := widgets[i]
				updateWidgetIfNeeded(w, now)
				// recursively update widgets in containers
				if group, ok := w.(*groupWidget); ok {
					updateWidgetsRecursive(group.Widgets, now)
				} else if split, ok := w.(*splitColumnWidget); ok {
					updateWidgetsRecursive(split.Widgets, now)
				}
			}
		}

		for {
			select {
			case <-app.monitorCtx.Done():
				return
			case <-ticker.C:
				now := time.Now()
				for _, page := range app.slugToPage {
					page.mu.Lock()
					// recursively update head and column widgets
					updateWidgetsRecursive(page.HeadWidgets, &now)
					for c := range page.Columns {
						updateWidgetsRecursive(page.Columns[c].Widgets, &now)
					}
					page.mu.Unlock()
				}
			}
		}
	}()

	return app, nil
}

func (p *page) updateOutdatedWidgets() {
	now := time.Now()

	var wg sync.WaitGroup
	context := context.Background()

	for w := range p.HeadWidgets {
		widget := p.HeadWidgets[w]

		if !widget.requiresUpdate(&now) {
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			widget.update(context)
		}()
	}

	for c := range p.Columns {
		for w := range p.Columns[c].Widgets {
			widget := p.Columns[c].Widgets[w]

			if !widget.requiresUpdate(&now) {
				continue
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				widget.update(context)
			}()
		}
	}

	wg.Wait()
}

func (p *page) publishChangedWidgets() {
	var checkWidgets func(widgets []widget)
	checkWidgets = func(widgets []widget) {
		for _, w := range widgets {
			if w.hasContentChanged() {
				publishEvent("widget:updated", map[string]any{
					"widget_id": w.GetID(),
					"slug":      p.Slug,
				})
				w.resetContentChanged()
			}
			// handle container widgets
			if group, ok := w.(*groupWidget); ok {
				checkWidgets(group.Widgets)
			} else if split, ok := w.(*splitColumnWidget); ok {
				checkWidgets(split.Widgets)
			}
		}
	}
	checkWidgets(p.HeadWidgets)
	for c := range p.Columns {
		checkWidgets(p.Columns[c].Widgets)
	}
}

func (a *application) resolveUserDefinedAssetPath(path string) string {
	if strings.HasPrefix(path, "/assets/") {
		return a.Config.Server.BaseURL + path
	}

	return path
}

type templateRequestData struct {
	Theme *themeProperties
}

type templateData struct {
	App     *application
	Page    *page
	Request templateRequestData
	// current widget HTML server-rendered into the page shell so the
	// browser paints content immediately instead of a loading spinner
	RenderedContent template.HTML
}

func (a *application) populateTemplateRequestData(data *templateRequestData, r *http.Request) {
	theme := &a.Config.Theme.themeProperties

	if !a.Config.Theme.DisablePicker {
		selectedTheme, err := r.Cookie("theme")
		if err == nil {
			preset, exists := a.Config.Theme.Presets.Get(selectedTheme.Value)
			if exists {
				theme = preset
			}
		}
	}

	data.Theme = theme
}

func (a *application) handlePageRequest(w http.ResponseWriter, r *http.Request) {
	page, exists := a.slugToPage[r.PathValue("page")]
	if !exists {
		a.handleNotFound(w, r)
		return
	}

	if a.handleUnauthorizedResponse(w, r, redirectToLogin) {
		return
	}

	data := templateData{
		Page: page,
		App:  a,
	}
	a.populateTemplateRequestData(&data.Request, r)

	// serve whatever widget content is in memory right now — stale is fine,
	// the background update below pushes freshness in via SSE. This is what
	// removes the loading spinner: the shell arrives with content in it.
	if content, contentErr := page.renderContent(); contentErr == nil {
		data.RenderedContent = template.HTML(content)
	}

	page.updateOutdatedWidgetsInBackground()

	var responseBytes bytes.Buffer
	err := pageTemplate.Execute(&responseBytes, data)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	w.Write(responseBytes.Bytes())
}

// renderContent returns the page's content HTML. It prefers rendering the
// live widget state, but if a background update currently holds the page
// lock it serves the last rendered copy instead of blocking the request.
func (p *page) renderContent() ([]byte, error) {
	renderLocked := func() ([]byte, error) {
		defer p.mu.Unlock()

		var buf bytes.Buffer
		if err := pageContentTemplate.Execute(&buf, templateData{Page: p}); err != nil {
			return nil, err
		}

		content := buf.Bytes()
		cached := make([]byte, len(content))
		copy(cached, content)
		p.renderedContentCache.Store(&cached)

		return content, nil
	}

	if p.mu.TryLock() {
		return renderLocked()
	}

	if cached := p.renderedContentCache.Load(); cached != nil {
		return *cached, nil
	}

	// nothing cached yet (first render of this page) — wait for the lock
	p.mu.Lock()
	return renderLocked()
}

// updateOutdatedWidgetsInBackground refreshes the page's due widgets off the
// request path and publishes per-widget SSE events for anything that changed.
// At most one update per page runs at a time.
func (p *page) updateOutdatedWidgetsInBackground() {
	if !p.updateInProgress.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer p.updateInProgress.Store(false)

		p.mu.Lock()
		defer p.mu.Unlock()

		p.updateOutdatedWidgets()
		p.publishChangedWidgets()
	}()
}

func (a *application) handlePageContentRequest(w http.ResponseWriter, r *http.Request) {
	page, exists := a.slugToPage[r.PathValue("page")]
	if !exists {
		a.handleNotFound(w, r)
		return
	}

	if a.handleUnauthorizedResponse(w, r, showUnauthorizedJSON) {
		return
	}

	// serve the current in-memory content immediately (no spinner time);
	// widget refreshes happen off the request path and reach clients via SSE
	content, err := page.renderContent()

	page.updateOutdatedWidgetsInBackground()

	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	w.Write(content)
}

func (a *application) addressOfRequest(r *http.Request) string {
	remoteAddrWithoutPort := func() string {
		for i := len(r.RemoteAddr) - 1; i >= 0; i-- {
			if r.RemoteAddr[i] == ':' {
				return r.RemoteAddr[:i]
			}
		}

		return r.RemoteAddr
	}

	if !a.Config.Server.Proxied {
		return remoteAddrWithoutPort()
	}

	// This should probably be configurable or look for multiple headers, not just this one
	forwardedFor := r.Header.Get("X-Forwarded-For")
	if forwardedFor == "" {
		return remoteAddrWithoutPort()
	}

	ips := strings.Split(forwardedFor, ",")
	if len(ips) == 0 || ips[0] == "" {
		return remoteAddrWithoutPort()
	}

	return ips[0]
}

func (a *application) handleNotFound(w http.ResponseWriter, _ *http.Request) {
	// TODO: add proper not found page
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("Page not found"))
}

func (a *application) handleWidgetRequest(w http.ResponseWriter, r *http.Request) {
	// TODO: this requires a rework of the widget update logic so that rather
	// than locking the entire page we lock individual widgets
	w.WriteHeader(http.StatusNotImplemented)

	// widgetValue := r.PathValue("widget")

	// widgetID, err := strconv.ParseUint(widgetValue, 10, 64)
	// if err != nil {
	// 	a.handleNotFound(w, r)
	// 	return
	// }

	// widget, exists := a.widgetByID[widgetID]

	// if !exists {
	// 	a.handleNotFound(w, r)
	// 	return
	// }

	// widget.handleRequest(w, r)
}

func (a *application) handleWidgetContentRequest(w http.ResponseWriter, r *http.Request) {
	widgetValue := r.PathValue("widget")

	widgetID, err := strconv.ParseUint(widgetValue, 10, 64)
	if err != nil {
		a.handleNotFound(w, r)
		return
	}

	widget, exists := a.widgetByID[widgetID]
	if !exists {
		a.handleNotFound(w, r)
		return
	}

	if a.handleUnauthorizedResponse(w, r, showUnauthorizedJSON) {
		return
	}

	// render widget HTML and return
	html := widget.Render()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}

func (a *application) StaticAssetPath(asset string) string {
	return a.Config.Server.BaseURL + "/static/" + staticFSHash + "/" + asset
}

func (a *application) VersionedAssetPath(asset string) string {
	return a.Config.Server.BaseURL + asset +
		"?v=" + strconv.FormatInt(a.CreatedAt.Unix(), 10)
}

func (a *application) server() (func() error, func() error) {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", a.handlePageRequest)
	mux.HandleFunc("GET /{page}", a.handlePageRequest)

	mux.HandleFunc("GET /api/pages/{page}/content/{$}", a.handlePageContentRequest)
	// widget content endpoint for partial updates
	mux.HandleFunc("GET /api/widgets/{widget}/content/{$}", a.handleWidgetContentRequest)
	// SSE endpoint for live events
	mux.HandleFunc("GET /api/events", a.handleEvents)

	if !a.Config.Theme.DisablePicker {
		mux.HandleFunc("POST /api/set-theme/{key}", a.handleThemeChangeRequest)
	}

	mux.HandleFunc("/api/widgets/{widget}/{path...}", a.handleWidgetRequest)
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	if a.RequiresAuth {
		mux.HandleFunc("GET /login", a.handleLoginPageRequest)
		mux.HandleFunc("GET /logout", a.handleLogoutRequest)
		mux.HandleFunc("POST /api/authenticate", a.handleAuthenticationAttempt)
	}

	mux.Handle(
		fmt.Sprintf("GET /static/%s/{path...}", staticFSHash),
		http.StripPrefix(
			"/static/"+staticFSHash,
			fileServerWithCache(http.FS(staticFS), STATIC_ASSETS_CACHE_DURATION),
		),
	)

	assetCacheControlValue := fmt.Sprintf(
		"public, max-age=%d",
		int(STATIC_ASSETS_CACHE_DURATION.Seconds()),
	)

	mux.HandleFunc(fmt.Sprintf("GET /static/%s/css/bundle.css", staticFSHash), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Cache-Control", assetCacheControlValue)
		w.Header().Add("Content-Type", "text/css; charset=utf-8")
		w.Write(bundledCSSContents)
	})

	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Cache-Control", assetCacheControlValue)
		w.Header().Add("Content-Type", "application/json")
		w.Write(a.parsedManifest)
	})

	var absAssetsPath string
	if a.Config.Server.AssetsPath != "" {
		absAssetsPath, _ = filepath.Abs(a.Config.Server.AssetsPath)
		assetsFS := fileServerWithCache(http.Dir(a.Config.Server.AssetsPath), 2*time.Hour)
		mux.Handle("/assets/{path...}", http.StripPrefix("/assets/", assetsFS))
	}

	server := http.Server{
		Addr:    fmt.Sprintf("%s:%d", a.Config.Server.Host, a.Config.Server.Port),
		Handler: mux,
	}

	start := func() error {
		log.Printf("Starting server on %s:%d (base-url: \"%s\", assets-path: \"%s\")\n",
			a.Config.Server.Host,
			a.Config.Server.Port,
			a.Config.Server.BaseURL,
			absAssetsPath,
		)

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}

		return nil
	}

	stop := func() error {
		return server.Close()
	}

	return start, stop
}

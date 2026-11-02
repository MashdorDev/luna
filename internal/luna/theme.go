package luna

import (
	"fmt"
	"html/template"
	"net/http"
	"time"
)

var (
	themeStyleTemplate         = mustParseTemplate("theme-style.gotmpl")
	themePresetPreviewTemplate = mustParseTemplate("theme-preset-preview.html")
	themeSystemPreviewTemplate = mustParseTemplate("theme-system-preview.html")
)

func (a *application) handleThemeChangeRequest(w http.ResponseWriter, r *http.Request) {
	themeKey := r.PathValue("key")

	if themeKey == "system" {
		if a.Config.Theme.System == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// no cookie is what makes the page follow the OS appearance
		http.SetCookie(w, &http.Cookie{
			Name:     "theme",
			Path:     a.Config.Server.BaseURL + "/",
			SameSite: http.SameSiteLaxMode,
			MaxAge:   -1,
		})
		w.WriteHeader(http.StatusNoContent)
		return
	}

	properties, exists := a.Config.Theme.Presets.Get(themeKey)
	if !exists && themeKey != "default" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if themeKey == "default" {
		properties = &a.Config.Theme.themeProperties
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "theme",
		Value:    themeKey,
		Path:     a.Config.Server.BaseURL + "/",
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(2 * 365 * 24 * time.Hour),
	})

	w.Header().Set("Content-Type", "text/css")
	w.Header().Set("X-Scheme", ternary(properties.Light, "light", "dark"))
	w.Write([]byte(properties.CSS))
}

type themeProperties struct {
	BackgroundColor          *hslColorField `yaml:"background-color"`
	PrimaryColor             *hslColorField `yaml:"primary-color"`
	PositiveColor            *hslColorField `yaml:"positive-color"`
	NegativeColor            *hslColorField `yaml:"negative-color"`
	Light                    bool           `yaml:"light"`
	ContrastMultiplier       float32        `yaml:"contrast-multiplier"`
	TextSaturationMultiplier float32        `yaml:"text-saturation-multiplier"`

	Key                  string        `yaml:"-"`
	CSS                  template.CSS  `yaml:"-"`
	PreviewHTML          template.HTML `yaml:"-"`
	BackgroundColorAsHex string        `yaml:"-"`
}

func (t *themeProperties) init() error {
	css, err := executeTemplateToString(themeStyleTemplate, t)
	if err != nil {
		return fmt.Errorf("compiling theme style: %v", err)
	}
	t.CSS = template.CSS(whitespaceAtBeginningOfLinePattern.ReplaceAllString(css, ""))

	previewHTML, err := executeTemplateToString(themePresetPreviewTemplate, t)
	if err != nil {
		return fmt.Errorf("compiling theme preview: %v", err)
	}
	t.PreviewHTML = template.HTML(previewHTML)

	if t.BackgroundColor != nil {
		t.BackgroundColorAsHex = t.BackgroundColor.ToHex()
	} else {
		t.BackgroundColorAsHex = "#151519"
	}

	return nil
}

func (t1 *themeProperties) SameAs(t2 *themeProperties) bool {
	if t1 == nil && t2 == nil {
		return true
	}
	if t1 == nil || t2 == nil {
		return false
	}
	if t1.Light != t2.Light {
		return false
	}
	if t1.ContrastMultiplier != t2.ContrastMultiplier {
		return false
	}
	if t1.TextSaturationMultiplier != t2.TextSaturationMultiplier {
		return false
	}
	if !t1.BackgroundColor.SameAs(t2.BackgroundColor) {
		return false
	}
	if !t1.PrimaryColor.SameAs(t2.PrimaryColor) {
		return false
	}
	if !t1.PositiveColor.SameAs(t2.PositiveColor) {
		return false
	}
	if !t1.NegativeColor.SameAs(t2.NegativeColor) {
		return false
	}
	return true
}

type systemThemes struct {
	Light *themeProperties `json:"-"`
	Dark  *themeProperties `json:"-"`
}

type systemThemeJS struct {
	Key        string `json:"key"`
	CSS        string `json:"css"`
	Background string `json:"background"`
	Light      bool   `json:"light"`
}

type clientThemes struct {
	All   map[string]systemThemeJS `json:"all"`
	Light string                   `json:"light"`
	Dark  string                   `json:"dark"`
}

// ClientThemes lets the page pick its theme from the cookie in the browser, so a
// page shell cached under another cookie (proxy or service worker) still renders
// the right theme
func (c *config) ClientThemes() clientThemes {
	js := func(t *themeProperties) systemThemeJS {
		return systemThemeJS{Key: t.Key, CSS: string(t.CSS), Background: t.BackgroundColorAsHex, Light: t.Light}
	}
	ct := clientThemes{All: map[string]systemThemeJS{"default": js(&c.Theme.themeProperties)}}
	for key, t := range c.Theme.Presets.Items() {
		ct.All[key] = js(t)
	}
	if c.Theme.System != nil {
		ct.Light, ct.Dark = c.Theme.System.Light.Key, c.Theme.System.Dark.Key
	}
	return ct
}

func (c *config) initSystemTheme() error {
	find := func(key string, fallback *themeProperties) (*themeProperties, error) {
		if key == "" {
			return fallback, nil
		}
		if key == "default" {
			return &c.Theme.themeProperties, nil
		}
		t, ok := c.Theme.Presets.Get(key)
		if !ok {
			return nil, fmt.Errorf("theme preset %q for the system theme does not exist", key)
		}
		return t, nil
	}

	defaultLight, _ := c.Theme.Presets.Get("default-light")
	var defaultDark *themeProperties
	if !c.Theme.Light {
		defaultDark = &c.Theme.themeProperties
	} else if t, ok := c.Theme.Presets.Get("default-dark"); ok {
		defaultDark = t
	}

	light, err := find(c.Theme.SystemLight, defaultLight)
	if err != nil {
		return err
	}
	dark, err := find(c.Theme.SystemDark, defaultDark)
	if err != nil {
		return err
	}
	if light == nil || dark == nil {
		return nil
	}

	c.Theme.System = &systemThemes{Light: light, Dark: dark}
	preview, err := executeTemplateToString(themeSystemPreviewTemplate, c.Theme.System)
	if err != nil {
		return fmt.Errorf("compiling system theme preview: %v", err)
	}
	c.Theme.SystemPreviewHTML = template.HTML(preview)
	return nil
}

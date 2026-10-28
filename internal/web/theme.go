package web

import (
	"html/template"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// Themes set the colour tokens of every page. A theme has a light and a
// dark palette (or only a dark one); the appearance setting picks one or
// follows the visitor's system. The accent setting overrides the theme's
// accent. The tokens are written into each page as a small <style>, so
// style.css only holds layout and the default (简约) palette.

type palette map[string]string

// Theme is one of the selectable colour schemes.
type Theme struct {
	Key, Label, Note string
	Light, Dark      palette
	DarkOnly         bool
}

// Accent overrides the accent colour of any theme.
type Accent struct {
	Key, Label, Swatch string
	Light, Dark        palette
}

// sansStack is the default body and heading font.
const sansStack = `-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif`

var baseLight = palette{
	"bg": "#fafafa", "bg-soft": "#f4f4f5", "card": "#ffffff", "text": "#09090b", "text-2": "#3f3f46", "muted": "#71717a",
	"line": "#e4e4e7", "line-2": "#d4d4d8",
	"accent": "#18181b", "accent-hover": "#27272a", "accent-soft": "#f4f4f5", "accent-ink": "#18181b", "accent-text": "#ffffff", "link": "#18181b",
	"ok": "#15803d", "ok-soft": "#f0fdf4", "warn": "#b45309", "warn-soft": "#fffbeb", "err": "#b91c1c", "err-soft": "#fef2f2",
	"blue": "#1d4ed8", "blue-soft": "#eff6ff", "info": "#3f3f46", "info-soft": "#f4f4f5", "code": "#f4f4f5", "price": "#09090b",
	"shadow-sm": "0 1px 2px rgba(0,0,0,.04)", "shadow": "0 1px 2px rgba(0,0,0,.04), 0 2px 8px rgba(0,0,0,.04)", "shadow-lg": "0 8px 24px rgba(0,0,0,.08)",
	"radius": "12px", "radius-sm": "8px", "font-head": sansStack,
	"ph-fg": "42%", "ph-bg": "93%", "ph-hi": "98%", "ph-sat": "8%",
	"logo-bg": "#18181b", "logo-fg": "#ffffff", "logo-leg": "#a3e635",
}

var baseDark = palette{
	"bg": "#09090b", "bg-soft": "#18181b", "card": "#111113", "text": "#fafafa", "text-2": "#d4d4d8", "muted": "#a1a1aa",
	"line": "#27272a", "line-2": "#3f3f46",
	"accent": "#fafafa", "accent-hover": "#e4e4e7", "accent-soft": "#27272a", "accent-ink": "#fafafa", "accent-text": "#09090b", "link": "#fafafa",
	"ok": "#4ade80", "ok-soft": "#0f2417", "warn": "#fbbf24", "warn-soft": "#291f0b", "err": "#f87171", "err-soft": "#2a1213",
	"blue": "#93c5fd", "blue-soft": "#0f1d36", "info": "#d4d4d8", "info-soft": "#18181b", "code": "#18181b", "price": "#fafafa",
	"shadow-sm": "none", "shadow": "none", "shadow-lg": "0 8px 24px rgba(0,0,0,.5)",
	"ph-fg": "66%", "ph-bg": "14%", "ph-hi": "19%", "ph-sat": "6%",
	"logo-bg": "#fafafa", "logo-fg": "#09090b", "logo-leg": "#65a30d",
}

var themes = []Theme{
	{Key: "", Label: "简约", Note: "中性灰，干净克制"},
	{Key: "paper", Label: "纸张", Note: "暖白纸色，衬线标题",
		Light: palette{"bg": "#f6f3ee", "bg-soft": "#eee8de", "card": "#fffdf9", "text": "#1f1a14", "text-2": "#4a4136", "muted": "#857a6a",
			"line": "#e6ddcf", "line-2": "#d6cab8", "accent": "#3b2f24", "accent-hover": "#2a2119", "accent-soft": "#eee8de", "accent-ink": "#3b2f24",
			"link": "#7c3a12", "code": "#eee8de", "price": "#7c3a12", "radius": "10px", "radius-sm": "6px",
			"font-head": `"Songti SC", "Noto Serif SC", "Source Han Serif SC", Georgia, serif`,
			"ph-h":      "35", "ph-sat": "18%", "logo-bg": "#3b2f24", "logo-leg": "#f59e0b"},
		Dark: palette{"bg": "#14110e", "bg-soft": "#1f1a15", "card": "#191510", "text": "#f3ede4", "text-2": "#d6ccbd", "muted": "#a39684",
			"line": "#2e2720", "line-2": "#40372d", "accent": "#f3ede4", "accent-hover": "#e6ddcf", "accent-soft": "#2e2720", "accent-ink": "#f3ede4",
			"link": "#fdba74", "code": "#1f1a15", "price": "#fdba74", "font-head": `"Songti SC", "Noto Serif SC", "Source Han Serif SC", Georgia, serif`,
			"ph-h": "35", "ph-sat": "14%", "logo-bg": "#f3ede4", "logo-fg": "#14110e", "logo-leg": "#d97706"}},
	{Key: "ocean", Label: "海洋", Note: "清爽蓝调，圆润",
		Light: palette{"bg": "#f4f7fb", "bg-soft": "#e9eff7", "card": "#ffffff", "text": "#0b1726", "text-2": "#334155", "muted": "#64748b",
			"line": "#e2e8f0", "line-2": "#cbd5e1", "accent": "#2563eb", "accent-hover": "#1d4ed8", "accent-soft": "#eff6ff", "accent-ink": "#1d4ed8",
			"link": "#1d4ed8", "code": "#eef2f7", "price": "#0b1726", "radius": "14px", "radius-sm": "10px",
			"ph-h": "214", "ph-sat": "22%", "ph-bg": "94%", "logo-bg": "#0b1726", "logo-leg": "#38bdf8"},
		Dark: palette{"bg": "#07101d", "bg-soft": "#0f1b2d", "card": "#0b1626", "text": "#e8eef7", "text-2": "#c0cbdb", "muted": "#8494ab",
			"line": "#1a2a40", "line-2": "#263a55", "accent": "#60a5fa", "accent-hover": "#93c5fd", "accent-soft": "#0f2341", "accent-ink": "#93c5fd",
			"link": "#93c5fd", "code": "#0f1b2d", "price": "#e8eef7", "ph-h": "214", "ph-sat": "18%", "logo-bg": "#e8eef7", "logo-fg": "#07101d", "logo-leg": "#0284c7"}},
	{Key: "forest", Label: "森林", Note: "松针绿，沉稳自然",
		Light: palette{"bg": "#f5f7f4", "bg-soft": "#eaefe8", "card": "#ffffff", "text": "#101a13", "text-2": "#36453a", "muted": "#68786c",
			"line": "#dfe6dc", "line-2": "#cbd5c7", "accent": "#166534", "accent-hover": "#14532d", "accent-soft": "#ecf5ee", "accent-ink": "#166534",
			"link": "#166534", "code": "#eaefe8", "price": "#101a13", "ph-h": "140", "ph-sat": "18%", "logo-bg": "#14301f", "logo-leg": "#a3e635"},
		Dark: palette{"bg": "#0a100c", "bg-soft": "#131c16", "card": "#0f1611", "text": "#ecf2ed", "text-2": "#c6d2c9", "muted": "#8a9a8e",
			"line": "#1f2b23", "line-2": "#2c3b30", "accent": "#4ade80", "accent-hover": "#86efac", "accent-soft": "#10291a", "accent-ink": "#86efac",
			"link": "#86efac", "accent-text": "#0a100c", "code": "#131c16", "price": "#ecf2ed", "ph-h": "140", "ph-sat": "14%", "logo-bg": "#ecf2ed", "logo-fg": "#0a100c", "logo-leg": "#4d7c0f"}},
	{Key: "sakura", Label: "樱花", Note: "柔和粉调，大圆角",
		Light: palette{"bg": "#fdf7f8", "bg-soft": "#f8ebee", "card": "#ffffff", "text": "#2a1419", "text-2": "#5b3a42", "muted": "#93727a",
			"line": "#f0dfe3", "line-2": "#e5cad1", "accent": "#d6336c", "accent-hover": "#be1e57", "accent-soft": "#fdeef3", "accent-ink": "#be1e57",
			"link": "#be1e57", "code": "#f8ebee", "price": "#be1e57", "radius": "18px", "radius-sm": "12px",
			"ph-h": "342", "ph-sat": "45%", "ph-bg": "95%", "logo-bg": "#2a1419", "logo-leg": "#f472b6"},
		Dark: palette{"bg": "#140b0e", "bg-soft": "#211317", "card": "#1a0f12", "text": "#f8ecef", "text-2": "#dcc5cb", "muted": "#a8878f",
			"line": "#33202a", "line-2": "#472c37", "accent": "#f472b6", "accent-hover": "#f9a8d4", "accent-soft": "#3a1426", "accent-ink": "#f9a8d4",
			"link": "#f9a8d4", "accent-text": "#140b0e", "code": "#211317", "price": "#f9a8d4", "ph-h": "342", "ph-sat": "30%", "logo-bg": "#f8ecef", "logo-fg": "#140b0e", "logo-leg": "#db2777"}},
	{Key: "midnight", Label: "午夜", Note: "深蓝夜色，始终深色", DarkOnly: true,
		Dark: palette{"bg": "#0a0e1a", "bg-soft": "#121933", "card": "#0e1426", "text": "#e6eaf5", "text-2": "#b8c1d9", "muted": "#7f8bab",
			"line": "#1c2541", "line-2": "#2a3559", "accent": "#8b9dff", "accent-hover": "#aab7ff", "accent-soft": "#1a2150", "accent-ink": "#aab7ff",
			"link": "#aab7ff", "accent-text": "#0a0e1a", "code": "#121933", "price": "#e6eaf5", "ph-h": "228", "ph-sat": "20%", "ph-bg": "15%", "ph-hi": "21%",
			"shadow": "0 1px 0 rgba(255,255,255,.03) inset", "logo-bg": "#8b9dff", "logo-fg": "#0a0e1a", "logo-leg": "#facc15"}},
}

var accents = []Accent{
	{Key: "", Label: "跟随主题"},
	{Key: "ink", Label: "墨黑", Swatch: "#18181b",
		Light: palette{"accent": "#18181b", "accent-hover": "#27272a", "accent-soft": "#f4f4f5", "accent-ink": "#18181b", "link": "#18181b", "accent-text": "#ffffff"},
		Dark:  palette{"accent": "#fafafa", "accent-hover": "#e4e4e7", "accent-soft": "#27272a", "accent-ink": "#fafafa", "link": "#fafafa", "accent-text": "#09090b"}},
	{Key: "indigo", Label: "靛蓝", Swatch: "#4f46e5",
		Light: palette{"accent": "#4f46e5", "accent-hover": "#4338ca", "accent-soft": "#eef2ff", "accent-ink": "#4338ca", "link": "#4338ca", "accent-text": "#ffffff"},
		Dark:  palette{"accent": "#818cf8", "accent-hover": "#a5b4fc", "accent-soft": "#1e1b4b", "accent-ink": "#a5b4fc", "link": "#a5b4fc", "accent-text": "#09090b"}},
	{Key: "emerald", Label: "翠绿", Swatch: "#059669",
		Light: palette{"accent": "#059669", "accent-hover": "#047857", "accent-soft": "#ecfdf5", "accent-ink": "#047857", "link": "#047857", "accent-text": "#ffffff"},
		Dark:  palette{"accent": "#34d399", "accent-hover": "#6ee7b7", "accent-soft": "#052e22", "accent-ink": "#6ee7b7", "link": "#6ee7b7", "accent-text": "#09090b"}},
	{Key: "amber", Label: "琥珀", Swatch: "#d97706",
		Light: palette{"accent": "#d97706", "accent-hover": "#b45309", "accent-soft": "#fffbeb", "accent-ink": "#b45309", "link": "#b45309", "accent-text": "#ffffff"},
		Dark:  palette{"accent": "#fbbf24", "accent-hover": "#fcd34d", "accent-soft": "#2b1d05", "accent-ink": "#fcd34d", "link": "#fcd34d", "accent-text": "#09090b"}},
	{Key: "rose", Label: "玫红", Swatch: "#e11d48",
		Light: palette{"accent": "#e11d48", "accent-hover": "#be123c", "accent-soft": "#fff1f2", "accent-ink": "#be123c", "link": "#be123c", "accent-text": "#ffffff"},
		Dark:  palette{"accent": "#fb7185", "accent-hover": "#fda4af", "accent-soft": "#3b0a17", "accent-ink": "#fda4af", "link": "#fda4af", "accent-text": "#09090b"}},
	{Key: "blue", Label: "海蓝", Swatch: "#2563eb",
		Light: palette{"accent": "#2563eb", "accent-hover": "#1d4ed8", "accent-soft": "#eff6ff", "accent-ink": "#1d4ed8", "link": "#1d4ed8", "accent-text": "#ffffff"},
		Dark:  palette{"accent": "#60a5fa", "accent-hover": "#93c5fd", "accent-soft": "#0b1d3a", "accent-ink": "#93c5fd", "link": "#93c5fd", "accent-text": "#09090b"}},
}

// Appearance modes.
var modes = [][2]string{{"", "跟随系统"}, {"light", "浅色"}, {"dark", "深色"}}

func findTheme(key string) Theme {
	for _, t := range themes {
		if t.Key == key {
			return t
		}
	}
	return themes[0]
}

func findAccent(key string) Accent {
	for _, a := range accents {
		if a.Key == key {
			return a
		}
	}
	return accents[0]
}

func validTheme(key string) bool  { return findTheme(key).Key == key }
func validAccent(key string) bool { return findAccent(key).Key == key }
func validMode(key string) bool   { return key == "" || key == "light" || key == "dark" }

func merge(ps ...palette) palette {
	out := palette{}
	for _, p := range ps {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}

// palettes returns the effective light and dark palettes.
func palettes(theme, accent string) (palette, palette) {
	t, a := findTheme(theme), findAccent(accent)
	// shape and type come from the light palette unless the dark one says otherwise
	dark := merge(layout(baseLight), baseDark, layout(t.Light), t.Dark, a.Dark)
	if t.DarkOnly {
		return dark, dark
	}
	return merge(baseLight, t.Light, a.Light), dark
}

var layoutKeys = []string{"radius", "radius-sm", "font-head"}

func layout(p palette) palette {
	out := palette{}
	for _, k := range layoutKeys {
		if v, ok := p[k]; ok {
			out[k] = v
		}
	}
	return out
}

func cssVars(p palette) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("--" + k + ":" + p[k] + ";")
	}
	return b.String()
}

var themeCache sync.Map

// themeCSS is the <style> body for a theme, accent and appearance mode.
func themeCSS(theme, accent, mode string) template.CSS {
	key := theme + "|" + accent + "|" + mode
	if v, ok := themeCache.Load(key); ok {
		return v.(template.CSS)
	}
	light, dark := palettes(theme, accent)
	var css string
	switch {
	case findTheme(theme).DarkOnly || mode == "dark":
		css = ":root{color-scheme:dark;" + cssVars(dark) + "}"
	case mode == "light":
		css = ":root{color-scheme:light;" + cssVars(light) + "}"
	default:
		css = ":root{color-scheme:light dark;" + cssVars(light) + "}@media (prefers-color-scheme:dark){:root{" + cssVars(dark) + "}}"
	}
	out := template.CSS(css)
	themeCache.Store(key, out)
	return out
}

// themePreview is what the settings page needs to draw a theme card.
type themePreview struct {
	Key, Label, Note                        string
	Bg, Card, Text, Muted, Line, Accent, AT string
	Radius, Leg, LogoBg, LogoFg, Head       string
}

func themePreviews() []themePreview {
	out := make([]themePreview, 0, len(themes))
	for _, t := range themes {
		p, _ := palettes(t.Key, "")
		out = append(out, themePreview{Key: t.Key, Label: t.Label, Note: t.Note, Bg: p["bg"], Card: p["card"], Text: p["text"],
			Muted: p["muted"], Line: p["line"], Accent: p["accent"], AT: p["accent-text"], Radius: p["radius"],
			Leg: p["logo-leg"], LogoBg: p["logo-bg"], LogoFg: p["logo-fg"], Head: p["font-head"]})
	}
	return out
}

// themePreviewCSS answers the theme <style> for a combination the owner is
// trying on the settings page, before saving.
func (s *Server) themePreviewCSS(c *gin.Context) {
	theme, accent, mode := c.Query("theme"), c.Query("accent"), c.Query("mode")
	if !validTheme(theme) || !validAccent(accent) || !validMode(mode) {
		c.Status(http.StatusBadRequest)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/css; charset=utf-8", []byte(themeCSS(theme, accent, mode)))
}

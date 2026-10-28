package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestThemeCSS(t *testing.T) {
	css := string(themeCSS("ocean", "rose", ""))
	if !strings.Contains(css, "--accent:#e11d48") || !strings.Contains(css, "--accent:#fb7185") || !strings.Contains(css, "@media (prefers-color-scheme:dark)") {
		t.Fatalf("ocean + rose, follow system: %s", css)
	}
	if !strings.Contains(css, "--radius:14px") || strings.Count(css, "--radius:14px") != 2 {
		t.Fatal("the theme's shape must carry into dark mode")
	}
	if css := string(themeCSS("", "", "light")); strings.Contains(css, "@media") || !strings.Contains(css, "color-scheme:light;") {
		t.Fatalf("forced light: %s", css)
	}
	if css := string(themeCSS("midnight", "", "light")); !strings.Contains(css, "color-scheme:dark;") || !strings.Contains(css, "--bg:#0a0e1a") {
		t.Fatalf("midnight is always dark: %s", css)
	}
	if strings.ContainsAny(string(themeCSS("paper", "", "")), "<>") {
		t.Fatal("theme CSS must not contain markup")
	}
}

func TestAppearanceSettings(t *testing.T) {
	e := setup(t)
	c := e.login()
	e.post(c, "/manage/settings", url.Values{"section": {"appearance"}, "theme": {"forest"}, "accent": {"amber"}, "mode": {"dark"}})
	if st := e.st.Settings(); st.Theme != "forest" || st.Accent != "amber" || st.Mode != "dark" {
		t.Fatalf("saved %q %q %q", st.Theme, st.Accent, st.Mode)
	}
	e.post(c, "/manage/settings", url.Values{"section": {"appearance"}, "theme": {"</style><script>"}, "accent": {""}, "mode": {""}})
	if e.st.Settings().Theme != "forest" {
		t.Fatal("unknown theme accepted")
	}
	resp, _ := http.Get(e.ts.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `content="dark"`) || !strings.Contains(string(b), "--accent:#fbbf24") {
		t.Fatal("storefront does not use the saved appearance")
	}
}

func TestAdminEntryLink(t *testing.T) {
	e := setup(t)
	home := func() string {
		resp, _ := http.Get(e.ts.URL + "/")
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	// on by default: in the header and the footer
	if n := strings.Count(home(), `href="/manage/login"`); n != 2 {
		t.Fatalf("expected the entry in the header and the footer, found %d", n)
	}
	e.post(e.login(), "/manage/settings", url.Values{"section": {"login"}})
	if strings.Contains(home(), "/manage/login") {
		t.Fatal("the admin path is shown although the entry is off")
	}
	e.post(e.login(), "/manage/settings", url.Values{"section": {"login"}, "admin_link_in_footer": {"on"}})
	if n := strings.Count(home(), `href="/manage/login"`); n != 2 {
		t.Fatalf("entry not back after turning it on, found %d", n)
	}
}

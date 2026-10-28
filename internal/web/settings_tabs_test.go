package web

import (
	"io"
	"net/url"
	"strings"
	"testing"
)

func TestSettingsTabs(t *testing.T) {
	e := setup(t)
	c := e.login()
	get := func(path string) string {
		resp, _ := c.Get(e.ts.URL + path)
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	page := get("/manage/settings")
	if !strings.Contains(page, `id="site"`) || strings.Contains(page, `id="payment"`) {
		t.Fatal("/settings should show only the site tab")
	}
	pay := get("/manage/settings/payment")
	if !strings.Contains(pay, `id="payment"`) || strings.Contains(pay, `id="site"`) {
		t.Fatal("payment tab shows other cards")
	}
	// sidebar: the settings entry opens its sub-menu, current tab marked
	if !strings.Contains(pay, `class="nav-sublink on" href="/manage/settings/payment"`) || !strings.Contains(pay, `>授权服务</a>`) {
		t.Fatal("sidebar sub-menu missing")
	}
	if strings.Contains(get("/manage/orders"), "nav-sublink") {
		t.Fatal("sub-menu shown outside settings")
	}
	if resp, _ := c.Get(e.ts.URL + "/manage/settings/nope"); location(resp) != "/manage/settings/site" {
		t.Fatalf("unknown tab not redirected: %s", location(resp))
	}
	// saving stays on the tab
	if l := location(e.post(c, "/manage/settings", url.Values{"section": {"mail"}, "smtp_host": {"smtp.example.com"}, "smtp_port": {"465"}})); !strings.HasPrefix(l, "/manage/settings/mail?") {
		t.Fatalf("save redirect: %s", l)
	}
}

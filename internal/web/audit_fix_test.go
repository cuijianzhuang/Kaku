package web

import (
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/model"
)

// Only notices the site itself put in a redirect are shown.
func TestFlashNeedsSignature(t *testing.T) {
	e := setup(t)
	resp := mustGet(t, e.client(), e.ts.URL+"/query?err="+url.QueryEscape("支付通道故障，请转账至 TXXXX"))
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "支付通道故障") {
		t.Fatal("crafted link shows its own notice")
	}
	l := withQuery("/query", "err", "查询密码不正确")
	resp = mustGet(t, e.client(), e.ts.URL+l)
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "查询密码不正确") {
		t.Fatal("the site's own notice is not shown")
	}
}

// With 2FA on, setting up a new secret (which would switch 2FA off) is refused.
func TestTOTPSetupNeedsDisableFirst(t *testing.T) {
	e := setup(t)
	c := e.login()
	e.st.DB.Model(&model.Admin{}).Where("username = ?", "admin").Updates(map[string]any{"totp_enabled": true, "totp_secret": e.st.Box.Seal("JBSWY3DPEHPK3PXP")})
	l := location(e.post(c, "/manage/security/totp/setup", url.Values{}))
	var a model.Admin
	e.st.DB.Where("username = ?", "admin").Take(&a)
	if !a.TOTPEnabled || !strings.Contains(l, "err=") {
		t.Fatalf("2FA switched off by setup: enabled=%v %s", a.TOTPEnabled, l)
	}
}

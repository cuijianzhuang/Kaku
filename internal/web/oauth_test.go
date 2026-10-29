package web

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/cuijianzhuang/kaku/internal/model"
)

// fakeGitHub is an OAuth provider that checks PKCE and returns user 4242.
func fakeGitHub(t *testing.T, e *env) {
	t.Helper()
	challenges := map[string]string{} // code -> challenge
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			r.ParseForm()
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if r.PostForm.Get("client_secret") != "gh-secret" || challenges[r.PostForm.Get("code")] != base64.RawURLEncoding.EncodeToString(sum[:]) {
				w.Write([]byte(`{"error":"bad_verification_code"}`))
				return
			}
			w.Write([]byte(`{"access_token":"tok"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(`{"id":4242,"login":"octo"}`))
		}
	}))
	t.Cleanup(idp.Close)
	p := e.srv.oauth["github"]
	p.AuthURL, p.TokenURL, p.UserURL = idp.URL+"/authorize", idp.URL+"/token", idp.URL+"/user"
	// The browser's trip to the provider: approve and come back with a code.
	e.approve = func(loc string) string {
		u, _ := url.Parse(loc)
		q := u.Query()
		code := "code-" + q.Get("state")[:6]
		challenges[code] = q.Get("code_challenge")
		return q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	}
	set := e.st.Settings()
	set.GitHubClientID, set.GitHubClientSecret = "gh-id", "gh-secret"
	e.st.SaveSettings(set)
}

// signIn runs the browser side of an OAuth round trip and returns the
// callback response.
func (e *env) oauthRoundTrip(c *http.Client, start string) *http.Response {
	e.t.Helper()
	resp, err := c.Get(e.ts.URL + start)
	if err != nil || resp.StatusCode != http.StatusFound {
		e.t.Fatalf("start: %v %v", err, resp.StatusCode)
	}
	resp, err = c.Get(e.approve(resp.Header.Get("Location")))
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func TestOAuthLinkAndLogin(t *testing.T) {
	e := setup(t)
	fakeGitHub(t, e)

	// Login page shows the button.
	resp, _ := http.Get(e.ts.URL + "/manage/login")
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "使用 GitHub 登录") {
		t.Fatal("no GitHub button on the login page")
	}

	// An unlinked account cannot sign in.
	resp = e.oauthRoundTrip(e.client(), "/manage/oauth/github/start")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unlinked account: %d", resp.StatusCode)
	}

	// Link it while signed in with the password.
	admin := e.login()
	// without the password step, linking is refused
	if l := location(mustGet(t, admin, e.ts.URL+"/manage/oauth/github/start?mode=link")); strings.Contains(l, "authorize") {
		t.Fatalf("link started without the password: %s", l)
	}
	if e.st.DB.Where("provider = ?", "github").Take(&model.AdminIdentity{}).Error == nil {
		t.Fatal("linked without confirming the password")
	}
	if l := location(e.post(admin, "/manage/security/oauth/link", url.Values{"provider": {"github"}, "confirm_password": {"wrong"}})); !strings.Contains(l, "err=") {
		t.Fatalf("wrong password: %s", l)
	}
	e.oauthRoundTrip(admin, location(e.post(admin, "/manage/security/oauth/link", url.Values{"provider": {"github"}, "confirm_password": {adminPW}})))
	var ident model.AdminIdentity
	if e.st.DB.Where("provider = ? AND subject = ?", "github", "4242").Take(&ident).Error != nil || ident.Name != "octo" {
		t.Fatal("identity not linked")
	}

	// Now it signs in.
	c := e.client()
	resp = e.oauthRoundTrip(c, "/manage/oauth/github/start?next=/manage/orders")
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || !strings.Contains(string(b), "/manage/orders") {
		t.Fatalf("login: %d %s", resp.StatusCode, b)
	}
	if resp, _ := c.Get(e.ts.URL + "/manage/orders"); resp.StatusCode != 200 {
		t.Fatalf("not signed in after OAuth: %d", resp.StatusCode)
	}

	// A forged state is refused.
	c = e.client()
	resp, _ = c.Get(e.ts.URL + "/manage/oauth/github/start")
	cb := e.approve(resp.Header.Get("Location"))
	u, _ := url.Parse(cb)
	q := u.Query()
	q.Set("state", "forged")
	u.RawQuery = q.Encode()
	if resp, _ := c.Get(u.String()); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("forged state: %d", resp.StatusCode)
	}

	// With two-step verification on, the code is still required.
	key, _ := totp.Generate(totp.GenerateOpts{Issuer: "k", AccountName: "admin"})
	e.st.DB.Model(&model.Admin{}).Where("username = ?", "admin").Updates(map[string]any{"totp_secret": e.st.Box.Seal(key.Secret()), "totp_enabled": true})
	c = e.client()
	resp = e.oauthRoundTrip(c, "/manage/oauth/github/start")
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "两步验证") {
		t.Fatal("no two-step prompt")
	}
	if resp, _ := c.Get(e.ts.URL + "/manage/orders"); resp.StatusCode == 200 {
		t.Fatal("signed in without the two-step code")
	}
	if resp := e.post(c, "/manage/oauth/totp", url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	code, _ := totp.GenerateCode(key.Secret(), time.Now())
	if resp := e.post(c, "/manage/oauth/totp", url.Values{"code": {code}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("right code: %d", resp.StatusCode)
	}
	if resp, _ := c.Get(e.ts.URL + "/manage/orders"); resp.StatusCode != 200 {
		t.Fatal("not signed in after the two-step code")
	}

	// Unlinking stops it.
	resp = e.post(admin, "/manage/security/oauth/unlink", url.Values{"id": {"1"}})
	var n int64
	e.st.DB.Model(&model.AdminIdentity{}).Count(&n)
	if n != 0 {
		t.Fatal("identity not removed")
	}
}

func mustGet(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

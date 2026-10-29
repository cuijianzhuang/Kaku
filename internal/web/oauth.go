package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/store"
)

// Admin sign-in with external accounts (OAuth 2.0 authorization code flow
// with PKCE). An external account only signs in to an admin it was linked
// to by that admin while signed in; it never creates admins. When the
// admin has two-step verification on, the code is still required.

type oauthProvider struct {
	Name, Label       string
	AuthURL, TokenURL string
	UserURL           string
	Scope             string
	Extra             url.Values // extra authorization parameters
	creds             func(store.Settings) (id, secret string)
	parseUser         func(body []byte) (subject, name string, err error)
}

func defaultProviders() map[string]*oauthProvider {
	return map[string]*oauthProvider{
		"github": {
			Name: "github", Label: "GitHub",
			AuthURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token",
			UserURL: "https://api.github.com/user", Scope: "read:user",
			creds: func(st store.Settings) (string, string) { return st.GitHubClientID, st.GitHubClientSecret },
			parseUser: func(b []byte) (string, string, error) {
				var u struct {
					ID    int64  `json:"id"`
					Login string `json:"login"`
				}
				if err := json.Unmarshal(b, &u); err != nil || u.ID == 0 {
					return "", "", errors.New("读取 GitHub 用户信息失败")
				}
				return strconv.FormatInt(u.ID, 10), u.Login, nil
			},
		},
		"google": {
			Name: "google", Label: "Google",
			AuthURL: "https://accounts.google.com/o/oauth2/v2/auth", TokenURL: "https://oauth2.googleapis.com/token",
			UserURL: "https://openidconnect.googleapis.com/v1/userinfo", Scope: "openid email",
			Extra: url.Values{"prompt": {"select_account"}},
			creds: func(st store.Settings) (string, string) { return st.GoogleClientID, st.GoogleClientSecret },
			parseUser: func(b []byte) (string, string, error) {
				var u struct {
					Sub   string `json:"sub"`
					Email string `json:"email"`
				}
				if err := json.Unmarshal(b, &u); err != nil || u.Sub == "" {
					return "", "", errors.New("读取 Google 用户信息失败")
				}
				return u.Sub, u.Email, nil
			},
		},
	}
}

// providerOrder fixes the order of the sign-in buttons.
var providerOrder = []string{"github", "google"}

type providerInfo struct{ Name, Label string }

// enabledProviders lists the providers with credentials configured.
func (s *Server) enabledProviders() []providerInfo {
	st := s.st.Settings()
	var out []providerInfo
	for _, n := range providerOrder {
		p := s.oauth[n]
		if p == nil {
			continue
		}
		if id, secret := p.creds(st); id != "" && secret != "" {
			out = append(out, providerInfo{p.Name, p.Label})
		}
	}
	return out
}

func (s *Server) provider(c *gin.Context) (*oauthProvider, string, string, error) {
	p := s.oauth[c.Param("provider")]
	if p == nil {
		return nil, "", "", errors.New("不支持的登录方式")
	}
	id, secret := p.creds(s.st.Settings())
	if id == "" || secret == "" {
		return nil, "", "", fmt.Errorf("未配置 %s 登录", p.Label)
	}
	return p, id, secret, nil
}

func (s *Server) oauthCallbackURL(c *gin.Context, provider string) string {
	return s.publicURL(c) + s.cfg.AdminPath + "/oauth/" + provider + "/callback"
}

const (
	oauthStateCookie = "kaku_oauth"
	oauth2FACookie   = "kaku_oauth_2fa"
	oauthStateTTL    = 10 * time.Minute
)

// oauthState travels in a signed cookie between start and callback.
type oauthState struct {
	Provider string `json:"p"`
	Mode     string `json:"m"` // login | link
	AdminID  uint   `json:"a,omitempty"`
	Epoch    int    `json:"e,omitempty"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Expires  int64  `json:"x"`
	Next     string `json:"r,omitempty"`
}

func (s *Server) encodeState(st oauthState) string {
	b, _ := json.Marshal(st)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + s.st.Box.Sign("oauth-state", payload)
}

func (s *Server) decodeState(v string) (*oauthState, error) {
	payload, sig, ok := strings.Cut(v, ".")
	if !ok || !s.st.Box.Verify(sig, "oauth-state", payload) {
		return nil, errors.New("登录状态无效，请重试")
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, errors.New("登录状态无效，请重试")
	}
	var st oauthState
	if json.Unmarshal(b, &st) != nil || s.now().Unix() > st.Expires {
		return nil, errors.New("登录已超时，请重试")
	}
	return &st, nil
}

func (s *Server) oauthCookiePath() string { return s.cfg.AdminPath + "/oauth" }

// oauthStart sends the browser to the provider. mode=link (signed in)
// links the account to the current admin; otherwise it signs in.
func (s *Server) oauthStart(c *gin.Context) {
	p, clientID, _, err := s.provider(c)
	if err != nil {
		s.renderLogin(c, http.StatusBadRequest, gin.H{"Error": err.Error()})
		return
	}
	st := oauthState{Provider: p.Name, Mode: "login", Nonce: secure.RandomToken(24), Verifier: secure.RandomToken(48),
		Expires: s.now().Add(oauthStateTTL).Unix(), Next: s.safeNext(c.Query("next"))}
	if c.Query("mode") == "link" {
		a := s.currentAdmin(c)
		if a == nil {
			c.Redirect(http.StatusSeeOther, s.cfg.AdminPath+"/login")
			return
		}
		if !s.checkLinkTicket(c.Query("ticket"), a, p.Name) {
			// linking needs the password first (oauthLink), so a stolen
			// session can't add its own sign-in
			s.bounce(c, withQuery(s.cfg.AdminPath+"/security", "err", "绑定已过期，请重新输入密码后绑定"))
			return
		}
		st.Mode, st.AdminID, st.Epoch, st.Next = "link", a.ID, a.Epoch, s.cfg.AdminPath+"/security"
	} else if s.loginLimit.blocked(c.ClientIP(), s.now()) {
		s.renderLogin(c, http.StatusTooManyRequests, gin.H{"Error": "尝试次数过多，请 15 分钟后再试"})
		return
	}
	c.SetSameSite(http.SameSiteLaxMode) // must come back on the provider's redirect
	c.SetCookie(oauthStateCookie, s.encodeState(st), int(oauthStateTTL.Seconds()), s.oauthCookiePath(), "", s.cfg.SecureCookie, true)
	sum := sha256.Sum256([]byte(st.Verifier))
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {s.oauthCallbackURL(c, p.Name)}, "response_type": {"code"},
		"scope": {p.Scope}, "state": {st.Nonce},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	for k, v := range p.Extra {
		q[k] = v
	}
	c.Redirect(http.StatusFound, p.AuthURL+"?"+q.Encode())
}

func (s *Server) oauthCallback(c *gin.Context) {
	ip, now := c.ClientIP(), s.now()
	raw, _ := c.Cookie(oauthStateCookie)
	c.SetCookie(oauthStateCookie, "", -1, s.oauthCookiePath(), "", s.cfg.SecureCookie, true)
	fail := func(msg string) {
		s.loginLimit.hit(ip, now)
		s.st.Audit("", "login_failed", c.Param("provider"), msg, ip)
		s.renderLogin(c, http.StatusUnauthorized, gin.H{"Error": msg})
	}
	p, clientID, clientSecret, err := s.provider(c)
	if err != nil {
		fail(err.Error())
		return
	}
	st, err := s.decodeState(raw)
	if err != nil {
		fail(err.Error())
		return
	}
	if st.Provider != p.Name || subtle.ConstantTimeCompare([]byte(st.Nonce), []byte(c.Query("state"))) != 1 {
		fail("登录状态不匹配，请重试")
		return
	}
	if e := c.Query("error"); e != "" {
		fail(p.Label + " 未授权：" + trimTo(c.DefaultQuery("error_description", e), 100))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	subject, name, err := s.oauthIdentity(ctx, p, clientID, clientSecret, c.Query("code"), st.Verifier, s.oauthCallbackURL(c, p.Name))
	if err != nil {
		s.log.Warn("oauth failed", "provider", p.Name, "err", err)
		fail(p.Label + " 登录失败，请重试")
		return
	}
	if st.Mode == "link" {
		s.oauthLink(c, p, st, subject, name)
		return
	}
	var ident model.AdminIdentity
	var a model.Admin
	if s.st.DB.Where("provider = ? AND subject = ?", p.Name, subject).Take(&ident).Error != nil ||
		s.st.DB.Where("id = ?", ident.AdminID).Take(&a).Error != nil {
		fail(fmt.Sprintf("%s 账号 %s 没有绑定管理员：请先用密码登录，在「安全」页面绑定", p.Label, name))
		return
	}
	s.st.DB.Model(&ident).Update("last_used", now)
	if a.TOTPEnabled {
		exp := now.Add(5 * time.Minute).Unix()
		payload := fmt.Sprintf("%d.%d.%d.%s", a.ID, a.Epoch, exp, p.Name)
		c.SetSameSite(http.SameSiteLaxMode)
		c.SetCookie(oauth2FACookie, payload+"."+s.st.Box.Sign("oauth-2fa", payload), 300, s.cfg.AdminPath, "", s.cfg.SecureCookie, true)
		s.renderLogin(c, http.StatusOK, gin.H{"TOTPStep": p.Label, "Name": name, "Next": st.Next})
		return
	}
	s.setSession(c, &a)
	s.recordLogin(&a, p.Name, name, ip)
	s.bounce(c, st.Next)
}

// oauthLink adds the external account to the admin who started the flow.
func (s *Server) oauthLink(c *gin.Context, p *oauthProvider, st *oauthState, subject, name string) {
	back := s.cfg.AdminPath + "/security"
	var a model.Admin
	if s.st.DB.Where("id = ?", st.AdminID).Take(&a).Error != nil || a.Epoch != st.Epoch {
		s.bounce(c, withQuery(back, "err", "登录状态已变化，请重新绑定"))
		return
	}
	var existing model.AdminIdentity
	if s.st.DB.Where("provider = ? AND subject = ?", p.Name, subject).Take(&existing).Error == nil {
		msg := "该 " + p.Label + " 账号已绑定"
		if existing.AdminID != a.ID {
			msg += "到其他管理员"
		}
		s.bounce(c, withQuery(back, "err", msg))
		return
	}
	if err := s.st.DB.Create(&model.AdminIdentity{AdminID: a.ID, Provider: p.Name, Subject: subject, Name: name}).Error; err != nil {
		s.bounce(c, withQuery(back, "err", err.Error()))
		return
	}
	s.st.Audit(a.Username, "oauth_link", p.Name, name, c.ClientIP())
	s.bounce(c, withQuery(back, "msg", "已绑定 "+p.Label+" 账号 "+name))
}

// oauthTOTP finishes an external sign-in for an admin with two-step
// verification.
func (s *Server) oauthTOTP(c *gin.Context) {
	ip, now := c.ClientIP(), s.now()
	next := s.safeNext(c.PostForm("next"))
	if s.loginLimit.blocked(ip, now) {
		s.renderLogin(c, http.StatusTooManyRequests, gin.H{"Error": "尝试次数过多，请 15 分钟后再试"})
		return
	}
	raw, _ := c.Cookie(oauth2FACookie)
	parts := strings.Split(raw, ".")
	if len(parts) != 5 || !s.st.Box.Verify(parts[4], "oauth-2fa", strings.Join(parts[:4], ".")) {
		s.renderLogin(c, http.StatusUnauthorized, gin.H{"Error": "登录已失效，请重新登录"})
		return
	}
	exp, _ := strconv.ParseInt(parts[2], 10, 64)
	var a model.Admin
	if now.Unix() > exp || s.st.DB.Where("id = ?", parts[0]).Take(&a).Error != nil || strconv.Itoa(a.Epoch) != parts[1] {
		s.renderLogin(c, http.StatusUnauthorized, gin.H{"Error": "登录已失效，请重新登录"})
		return
	}
	secret, err := s.st.Box.Open(a.TOTPSecret)
	if err != nil || !a.TOTPEnabled || !totp.Validate(strings.TrimSpace(c.PostForm("code")), secret) {
		s.loginLimit.hit(ip, now)
		s.st.Audit(a.Username, "login_failed", parts[3], "两步验证码不正确", ip)
		s.renderLogin(c, http.StatusUnauthorized, gin.H{"Error": "两步验证码不正确", "TOTPStep": parts[3], "Next": next})
		return
	}
	c.SetCookie(oauth2FACookie, "", -1, s.cfg.AdminPath, "", s.cfg.SecureCookie, true)
	s.setSession(c, &a)
	s.recordLogin(&a, parts[3], "", ip)
	c.Redirect(http.StatusSeeOther, next)
}

func (s *Server) oauthUnlink(c *gin.Context) {
	a := admin(c)
	back := s.cfg.AdminPath + "/security"
	var ident model.AdminIdentity
	if s.st.DB.Where("id = ? AND admin_id = ?", c.PostForm("id"), a.ID).Take(&ident).Error != nil {
		redirectErr(c, back, errors.New("绑定不存在"))
		return
	}
	s.st.DB.Delete(&ident)
	s.audit(c, "oauth_unlink", ident.Provider, ident.Name)
	redirectMsg(c, back, "已解除绑定")
}

// oauthIdentity exchanges the code and reads the account's stable id.
func (s *Server) oauthIdentity(ctx context.Context, p *oauthProvider, clientID, clientSecret, code, verifier, redirect string) (string, string, error) {
	if code == "" {
		return "", "", errors.New("missing code")
	}
	form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}, "code": {code},
		"redirect_uri": {redirect}, "grant_type": {"authorization_code"}, "code_verifier": {verifier}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, p.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	body, err := s.oauthDo(req)
	if err != nil {
		return "", "", fmt.Errorf("token: %w", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.AccessToken == "" {
		return "", "", fmt.Errorf("token: %s %s", tok.Error, tok.Description)
	}
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, p.UserURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	body, err = s.oauthDo(req)
	if err != nil {
		return "", "", fmt.Errorf("user: %w", err)
	}
	return p.parseUser(body)
}

func (s *Server) oauthDo(req *http.Request) ([]byte, error) {
	req.Header.Set("User-Agent", "kaku")
	resp, err := s.oauthHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return body, fmt.Errorf("%s", resp.Status)
	}
	return body, nil
}

// bounce redirects through a small page: the session cookie is
// SameSite=Strict, so it is not sent on a redirect chain that started on
// the provider's site, but it is on a navigation started by our own page.
func (s *Server) bounce(c *gin.Context, to string) {
	u := html.EscapeString(to)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=`+u+
		`"><title>正在跳转</title><p style="font-family:sans-serif;padding:24px">正在跳转… <a href="`+u+`">继续</a></p>`))
}

func (s *Server) safeNext(next string) string {
	if !strings.HasPrefix(next, s.cfg.AdminPath) || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return s.cfg.AdminPath
	}
	return next
}

func (s *Server) renderLogin(c *gin.Context, code int, data gin.H) {
	data["Providers"] = s.enabledProviders()
	if _, ok := data["Next"]; !ok {
		data["Next"] = s.safeNext(c.Query("next"))
	}
	s.render(c, code, "admin_login", data)
}

func trimTo(v string, n int) string {
	if r := []rune(v); len(r) > n {
		return string(r[:n])
	}
	return v
}

// oauthLinkStart checks the password, then starts linking a provider account.
func (s *Server) oauthLinkStart(c *gin.Context) {
	a := admin(c)
	back := s.cfg.AdminPath + "/security"
	if err := s.reauth(c); err != nil {
		redirectErr(c, back, err)
		return
	}
	p := c.PostForm("provider")
	exp := s.now().Add(5 * time.Minute).Unix()
	ticket := fmt.Sprintf("%d.%s", exp, linkSig(a, p, exp))
	c.Redirect(http.StatusSeeOther, s.cfg.AdminPath+"/oauth/"+url.PathEscape(p)+"/start?mode=link&ticket="+url.QueryEscape(ticket))
}

func linkSig(a *model.Admin, provider string, exp int64) string {
	return flashSig("link", fmt.Sprintf("%d:%d:%s:%d", a.ID, a.Epoch, provider, exp))
}

func (s *Server) checkLinkTicket(ticket string, a *model.Admin, provider string) bool {
	expStr, sig, ok := strings.Cut(ticket, ".")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if !ok || err != nil || s.now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(linkSig(a, provider, exp)))
}

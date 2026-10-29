package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/secure"
)

const sessionCookie = "kaku_admin"
const sessionTTL = 12 * time.Hour

// session token: adminID.epoch.expiry.signature
func (s *Server) sessionToken(a *model.Admin, exp time.Time) string {
	payload := fmt.Sprintf("%d.%d.%d", a.ID, a.Epoch, exp.Unix())
	return payload + "." + s.st.Box.Sign("admin-session", payload)
}

func (s *Server) currentAdmin(c *gin.Context) *model.Admin {
	v, err := c.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	parts := strings.Split(v, ".")
	if len(parts) != 4 {
		return nil
	}
	payload := strings.Join(parts[:3], ".")
	if !s.st.Box.Verify(parts[3], "admin-session", payload) {
		return nil
	}
	exp, _ := strconv.ParseInt(parts[2], 10, 64)
	if s.now().Unix() > exp {
		return nil
	}
	var a model.Admin
	if s.st.DB.Where("id = ?", parts[0]).Take(&a).Error != nil || strconv.Itoa(a.Epoch) != parts[1] {
		return nil
	}
	return &a
}

func (s *Server) setSession(c *gin.Context, a *model.Admin) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(sessionCookie, s.sessionToken(a, s.now().Add(sessionTTL)), int(sessionTTL.Seconds()), s.cfg.AdminPath, "", s.cfg.SecureCookie, true)
}

// requireAdmin guards admin pages; POSTs must also come from this site.
func (s *Server) requireAdmin(c *gin.Context) {
	a := s.currentAdmin(c)
	if a == nil {
		if c.Request.Method == http.MethodGet {
			c.Redirect(http.StatusSeeOther, s.cfg.AdminPath+"/login?next="+url.QueryEscape(c.Request.URL.RequestURI()))
		} else {
			c.String(http.StatusUnauthorized, "请先登录")
		}
		c.Abort()
		return
	}
	if c.Request.Method != http.MethodGet && !sameOrigin(c) {
		c.String(http.StatusForbidden, "跨站请求被拒绝")
		c.Abort()
		return
	}
	c.Set("admin", a)
	c.Next()
}

// sameOrigin checks Origin (or Referer) against the request host.
func sameOrigin(c *gin.Context) bool {
	src := c.GetHeader("Origin")
	if src == "" || src == "null" {
		src = c.GetHeader("Referer")
	}
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	return err == nil && strings.EqualFold(u.Host, c.Request.Host)
}

func admin(c *gin.Context) *model.Admin {
	a, _ := c.Get("admin")
	m, _ := a.(*model.Admin)
	return m
}

func (s *Server) audit(c *gin.Context, action, target, detail string) {
	name := ""
	if a := admin(c); a != nil {
		name = a.Username
	}
	s.st.Audit(name, action, target, detail, c.ClientIP())
}

func (s *Server) loginPage(c *gin.Context) {
	if s.currentAdmin(c) != nil {
		c.Redirect(http.StatusSeeOther, s.cfg.AdminPath)
		return
	}
	s.renderLogin(c, 200, gin.H{})
}

func (s *Server) login(c *gin.Context) {
	ip, now := c.ClientIP(), s.now()
	next := s.safeNext(c.PostForm("next"))
	fail := func(msg string) {
		s.loginLimit.hit(ip, now)
		s.st.Audit(c.PostForm("username"), "login_failed", "", msg, ip)
		s.renderLogin(c, http.StatusUnauthorized, gin.H{"Error": msg, "Next": next})
	}
	if s.loginLimit.blocked(ip, now) {
		s.renderLogin(c, http.StatusTooManyRequests, gin.H{"Error": "尝试次数过多，请 15 分钟后再试"})
		return
	}
	var a model.Admin
	if s.st.DB.Where("username = ?", strings.TrimSpace(c.PostForm("username"))).Take(&a).Error != nil ||
		!secure.CheckPassword(a.PasswordHash, c.PostForm("password")) {
		fail("用户名、密码或两步验证码不正确")
		return
	}
	if a.TOTPEnabled {
		secret, err := s.st.Box.Open(a.TOTPSecret)
		if err != nil || !totp.Validate(strings.TrimSpace(c.PostForm("code")), secret) {
			// same message as a wrong password: don't confirm the password was right
			fail("用户名、密码或两步验证码不正确")
			return
		}
	}
	s.setSession(c, &a)
	s.recordLogin(&a, "", "", ip)
	c.Redirect(http.StatusSeeOther, next)
}

func (s *Server) logout(c *gin.Context) {
	c.SetCookie(sessionCookie, "", -1, s.cfg.AdminPath, "", s.cfg.SecureCookie, true)
	c.Redirect(http.StatusSeeOther, s.cfg.AdminPath+"/login")
}

// reauth re-checks the password (and TOTP) before sensitive actions such
// as exporting cards.
func (s *Server) reauth(c *gin.Context) error {
	a := admin(c)
	if !secure.CheckPassword(a.PasswordHash, c.PostForm("confirm_password")) {
		return errors.New("密码不正确")
	}
	if a.TOTPEnabled {
		secret, err := s.st.Box.Open(a.TOTPSecret)
		if err != nil || !totp.Validate(strings.TrimSpace(c.PostForm("confirm_code")), secret) {
			return errors.New("两步验证码不正确")
		}
	}
	return nil
}

func (s *Server) securityPage(c *gin.Context) {
	a := admin(c)
	var idents []model.AdminIdentity
	s.st.DB.Where("admin_id = ?", a.ID).Order("id").Find(&idents)
	data := gin.H{"Admin": a, "Identities": idents, "Providers": s.enabledProviders()}
	if !a.TOTPEnabled && a.TOTPSecret != "" {
		if secret, err := s.st.Box.Open(a.TOTPSecret); err == nil {
			data["PendingSecret"] = secret
			data["OTPURL"] = fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s",
				url.PathEscape(s.st.Settings().SiteName), url.PathEscape(a.Username), secret, url.QueryEscape(s.st.Settings().SiteName))
		}
	}
	s.render(c, 200, "admin_security", data)
}

func (s *Server) changePassword(c *gin.Context) {
	a := admin(c)
	back := s.cfg.AdminPath + "/security"
	if !secure.CheckPassword(a.PasswordHash, c.PostForm("old")) {
		redirectErr(c, back, errors.New("原密码不正确"))
		return
	}
	pw := c.PostForm("new")
	if len(pw) < 12 {
		redirectErr(c, back, errors.New("新密码至少 12 位"))
		return
	}
	s.st.DB.Model(a).Updates(map[string]any{"password_hash": secure.HashPassword(pw), "epoch": a.Epoch + 1})
	a.Epoch++
	s.setSession(c, a)
	s.audit(c, "password_change", a.Username, "")
	redirectMsg(c, back, "密码已修改，其他会话已退出")
}

func (s *Server) totpSetup(c *gin.Context) {
	a := admin(c)
	if a.TOTPEnabled {
		// replacing the secret would switch 2FA off without the password
		redirectErr(c, s.cfg.AdminPath+"/security", errors.New("两步验证已开启。要更换验证器，请先输入密码关闭，再重新设置"))
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: s.st.Settings().SiteName, AccountName: a.Username})
	if err != nil {
		redirectErr(c, s.cfg.AdminPath+"/security", err)
		return
	}
	s.st.DB.Model(a).Updates(map[string]any{"totp_secret": s.st.Box.Seal(key.Secret()), "totp_enabled": false})
	c.Redirect(http.StatusSeeOther, s.cfg.AdminPath+"/security")
}

func (s *Server) totpEnable(c *gin.Context) {
	a := admin(c)
	back := s.cfg.AdminPath + "/security"
	secret, err := s.st.Box.Open(a.TOTPSecret)
	if err != nil || secret == "" || !totp.Validate(strings.TrimSpace(c.PostForm("code")), secret) {
		redirectErr(c, back, errors.New("验证码不正确"))
		return
	}
	s.st.DB.Model(a).Update("totp_enabled", true)
	s.audit(c, "totp_enable", a.Username, "")
	redirectMsg(c, back, "两步验证已开启")
}

func (s *Server) totpDisable(c *gin.Context) {
	a := admin(c)
	back := s.cfg.AdminPath + "/security"
	if err := s.reauth(c); err != nil {
		redirectErr(c, back, err)
		return
	}
	s.st.DB.Model(a).Updates(map[string]any{"totp_enabled": false, "totp_secret": ""})
	s.audit(c, "totp_disable", a.Username, "")
	redirectMsg(c, back, "两步验证已关闭")
}

// Package web serves the storefront, the payment callbacks, the Telegram
// webhook and the admin backend.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/store"
)

// Config configures the web server.
type Config struct {
	AdminPath    string // e.g. /admin or a hard-to-guess path
	TrustProxy   bool   // take the client address from X-Forwarded-For
	SecureCookie bool
	Version      string
}

// TGHandler handles Telegram webhook updates.
type TGHandler interface {
	HandleUpdate(body []byte)
}

// Server is the HTTP application.
type Server struct {
	cfg   Config
	shop  *shop.Service
	st    *store.Store
	log   *slog.Logger
	bot   TGHandler
	pages map[string]*template.Template
	now   func() time.Time

	queryLimit *limiter // failed order lookups per address
	loginLimit *limiter // failed admin logins per address
	orderLimit *limiter // orders created per address
}

//go:embed templates static
var assets embed.FS

// New builds the server.
func New(cfg Config, s *shop.Service, bot TGHandler, log *slog.Logger) *Server {
	if cfg.AdminPath == "" {
		cfg.AdminPath = "/admin"
	}
	cfg.AdminPath = "/" + strings.Trim(cfg.AdminPath, "/")
	srv := &Server{cfg: cfg, shop: s, st: s.Store, log: log, bot: bot, now: time.Now,
		queryLimit: newLimiter(10, 15*time.Minute), loginLimit: newLimiter(10, 15*time.Minute), orderLimit: newLimiter(20, 10*time.Minute)}
	srv.pages = srv.parsePages()
	return srv
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery(), securityHeaders)
	if s.cfg.TrustProxy {
		r.SetTrustedProxies([]string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1", "fc00::/7"})
	} else {
		r.SetTrustedProxies(nil)
	}
	r.MaxMultipartMemory = 8 << 20
	static, _ := fs.Sub(assets, "static")
	r.StaticFS("/static", http.FS(static))
	r.GET("/healthz", func(c *gin.Context) { c.String(200, "ok") })
	s.publicRoutes(r)
	s.adminRoutes(r.Group(s.cfg.AdminPath))
	r.NoRoute(func(c *gin.Context) { s.render(c, http.StatusNotFound, "error", gin.H{"Message": "页面不存在"}) })
	return r
}

func securityHeaders(c *gin.Context) {
	h := c.Writer.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	c.Next()
}

// ---- templates ----

var funcs = template.FuncMap{
	"money": shop.FormatMoney,
	"date": func(t any) string {
		switch v := t.(type) {
		case time.Time:
			if v.IsZero() {
				return ""
			}
			return v.Local().Format("2006-01-02 15:04")
		case *time.Time:
			if v == nil || v.IsZero() {
				return ""
			}
			return v.Local().Format("2006-01-02 15:04")
		}
		return ""
	},
	"status":   statusLabel,
	"method":   methodLabel,
	"markdown": renderMarkdown,
	"add":      func(a, b int) int { return a + b },
	"price":    priceInput,
	"contains": strings.Contains,
	"join":     strings.Join,
	"query":    url.QueryEscape,
}

func (s *Server) parsePages() map[string]*template.Template {
	pages := map[string]*template.Template{}
	entries, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		panic(err)
	}
	base := template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html"))
	for _, e := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(e, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		t := template.Must(template.Must(base.Clone()).ParseFS(assets, e))
		pages[name] = t
	}
	return pages
}

func (s *Server) render(c *gin.Context, code int, page string, data gin.H) {
	t, ok := s.pages[page]
	if !ok {
		c.String(500, "missing template "+page)
		return
	}
	if data == nil {
		data = gin.H{}
	}
	st := s.st.Settings()
	data["Site"] = st.SiteName
	data["Settings"] = st
	data["AdminPath"] = s.cfg.AdminPath
	data["Version"] = s.cfg.Version
	data["Path"] = c.Request.URL.Path
	if _, ok := data["Flash"]; !ok {
		data["Flash"] = c.Query("msg")
	}
	if _, ok := data["Error"]; !ok {
		data["Error"] = c.Query("err")
	}
	layout := "public"
	if strings.HasPrefix(page, "admin_") {
		layout = "admin"
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, layout, data); err != nil {
		s.log.Error("render", "page", page, "err", err)
		c.String(500, "render error")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Data(code, "text/html; charset=utf-8", buf.Bytes())
}

func statusLabel(st string) string {
	switch st {
	case model.OrderPending:
		return "待支付"
	case model.OrderPaid:
		return "已支付"
	case model.OrderDelivered:
		return "已发货"
	case model.OrderExpired:
		return "已超时"
	case model.OrderFailed:
		return "发货失败"
	case model.OrderRefunded:
		return "已退款"
	case model.CardAvailable:
		return "可售"
	case model.CardReserved:
		return "已预占"
	case model.CardSold:
		return "已售"
	case model.CardVoid:
		return "已作废"
	case model.LicenseIssued:
		return "未激活"
	case model.LicenseActivated:
		return "已激活"
	case model.LicenseRevoked:
		return "已吊销"
	case model.TicketOpen:
		return "待回复"
	case model.TicketReplied:
		return "已回复"
	case model.TicketClosed:
		return "已关闭"
	}
	return st
}

func methodLabel(m string) string {
	switch m {
	case model.PayEPayAlipay:
		return "支付宝"
	case model.PayEPayWxpay:
		return "微信支付"
	case model.PayUSDT:
		return "USDT"
	case model.PayStars:
		return "Stars"
	case model.PayManual:
		return "人工"
	}
	return m
}

// priceInput renders a stored amount for an input box.
func priceInput(amount int64, cur string) string {
	if amount == 0 {
		return ""
	}
	return shopDecimal(amount, model.CurrencyPlaces(cur))
}

// ---- helpers ----

func (s *Server) publicURL(c *gin.Context) string {
	if u := strings.TrimRight(s.st.Settings().PublicURL, "/"); u != "" {
		return u
	}
	scheme := "http"
	if c.Request.TLS != nil || (s.cfg.TrustProxy && c.GetHeader("X-Forwarded-Proto") == "https") {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}

func redirectMsg(c *gin.Context, to, msg string) {
	c.Redirect(http.StatusSeeOther, withQuery(to, "msg", msg))
}

func redirectErr(c *gin.Context, to string, err error) {
	c.Redirect(http.StatusSeeOther, withQuery(to, "err", err.Error()))
}

// withQuery adds k=v to a path, before any #fragment.
func withQuery(to, k, v string) string {
	path, frag, hasFrag := strings.Cut(to, "#")
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	path += sep + k + "=" + url.QueryEscape(v)
	if hasFrag {
		path += "#" + frag
	}
	return path
}

// limiter counts failures per key in a fixed window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string]*hitWindow
}

type hitWindow struct {
	start time.Time
	n     int
}

func newLimiter(max int, d time.Duration) *limiter {
	return &limiter{max: max, window: d, hits: map[string]*hitWindow{}}
}

func (l *limiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.hits[key]
	return w != nil && now.Sub(w.start) < l.window && w.n >= l.max
}

func (l *limiter) hit(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 20000 {
		for k, w := range l.hits {
			if now.Sub(w.start) >= l.window {
				delete(l.hits, k)
			}
		}
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= l.window {
		w = &hitWindow{start: now}
		l.hits[key] = w
	}
	w.n++
}

func shopDecimal(amount int64, places int) string {
	if places == 0 {
		return fmt.Sprint(amount)
	}
	scale := int64(1)
	for range places {
		scale *= 10
	}
	s := fmt.Sprintf("%d.%0*d", amount/scale, places, amount%scale)
	return strings.TrimRight(strings.TrimRight(s, "0"), ".")
}

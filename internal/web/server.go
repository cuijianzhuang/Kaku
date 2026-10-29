// Package web serves the storefront, the payment callbacks, the Telegram
// webhook and the admin backend.
package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/polaris"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/store"
)

// Config configures the web server.
type Config struct {
	AdminPath    string // e.g. /admin or a hard-to-guess path
	TrustProxy   bool   // take the client address from X-Forwarded-For
	SecureCookie bool
	Version      string
	UploadDir    string // uploaded images ("" = uploads off)
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

	assets    map[string]*asset         // static files, with content hashes
	oauth     map[string]*oauthProvider // admin sign-in providers
	oauthHTTP *http.Client

	queryLimit  *limiter // failed order lookups per address
	loginLimit  *limiter // failed admin logins per address
	orderLimit  *limiter // orders created per address
	ticketLimit *limiter // tickets submitted per address
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
		oauth: defaultProviders(), oauthHTTP: &http.Client{Timeout: 20 * time.Second},
		queryLimit: newLimiter(10, 15*time.Minute), loginLimit: newLimiter(10, 15*time.Minute), orderLimit: newLimiter(20, 10*time.Minute),
		ticketLimit: newLimiter(10, 10*time.Minute)}
	srv.assets = loadAssets()
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
	r.GET("/static/*filepath", s.serveAsset)
	r.HEAD("/static/*filepath", s.serveAsset)
	r.GET("/uploads/:name", s.serveUpload)
	r.GET("/favicon.ico", func(c *gin.Context) { c.FileFromFS("favicon-32.png", http.FS(static)) })
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
	// Pages run only our own script (plus Cloudflare Turnstile when enabled);
	// inline styles are allowed for the theme tokens.
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://challenges.cloudflare.com; "+
		"frame-src https://challenges.cloudflare.com; style-src 'self' 'unsafe-inline'; img-src 'self' https: data: blob:; "+
		"connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
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
	"status":    statusLabel,
	"method":    methodLabel,
	"markdown":  renderMarkdown,
	"add":       func(a, b int) int { return a + b },
	"price":     priceInput,
	"contains":  strings.Contains,
	"join":      strings.Join,
	"query":     url.QueryEscape,
	"kb":        func(n int64) float64 { return float64(n) / 1024 },
	"hasPrefix": strings.HasPrefix,
	"initial":   initial,
	"nav":       navItem,
	// manualOnly: every active spec of the product is delivered by hand
	"manualOnly": func(v any) bool {
		var p model.Product
		switch x := v.(type) {
		case model.Product:
			p = x
		case *model.Product:
			p = *x
		}
		n := 0
		for _, k := range p.SKUs {
			if !k.Active {
				continue
			}
			if k.DelivererKind != "manual" {
				return false
			}
			n++
		}
		return n > 0
	},
	"auditLabel": auditLabel,
	"hours": func() []int {
		h := make([]int, 24)
		for i := range h {
			h[i] = i
		}
		return h
	},
	"settingsTabs": func() []settingsTab { return settingsTabs },
	"hue":          func(id uint) int { return int(id*67+220) % 360 },
	"fromPrice":    fromPrice,
	"expired":      func(t *time.Time) bool { return t != nil && time.Now().After(*t) },
	"plain":        plainText,
	"months":       monthsLabel,
	"days":         daysLabel,
	"featSummary":  featSummary,
	"abs": func(base, u string) string {
		if strings.HasPrefix(u, "/") {
			return base + u
		}
		return u
	},
	"themeCSS":        themeCSS,
	"themePreviews":   themePreviews,
	"accentOptions":   func() []Accent { return accents },
	"appearanceModes": func() [][2]string { return modes },
	"colorScheme": func(theme, mode string) string {
		switch {
		case findTheme(theme).DarkOnly || mode == "dark":
			return "dark"
		case mode == "light":
			return "light"
		}
		return "light dark"
	},
}

// monthsLabel is a template term: 0 = perpetual (or every version).
func monthsLabel(m int, zero string) string {
	switch {
	case m <= 0:
		return zero
	case m%12 == 0:
		return fmt.Sprintf("%d 年", m/12)
	}
	return fmt.Sprintf("%d 个月", m)
}

// daysLabel is a day count in friendly units.
func daysLabel(d int, zero string) string {
	switch {
	case d <= 0:
		return zero
	case d%365 == 0:
		return fmt.Sprintf("%d 年", d/365)
	case d%30 == 0:
		return fmt.Sprintf("%d 个月", d/30)
	}
	return fmt.Sprintf("%d 天", d)
}

// featSummary names a license's features.
func featSummary(cat *polaris.Catalog, ids []string) string {
	if len(ids) == 0 {
		return "无高级功能"
	}
	var names []string
	for _, id := range ids {
		if id == polaris.Every {
			return "全部高级功能"
		}
		name := id
		for _, f := range cat.Features {
			if f.ID == id {
				name = f.Name
			}
		}
		names = append(names, name)
	}
	return strings.Join(names, "、")
}

// plainText turns Markdown into a short line of text for previews.
func plainText(md string, n int) string {
	md = strings.NewReplacer("#", "", "*", "", "`", "", ">", "", "[", "", "]", "", "_", "").Replace(md)
	md = linkTarget.ReplaceAllString(md, "")
	out := strings.Join(strings.Fields(md), " ")
	if r := []rune(out); len(r) > n {
		return string(r[:n]) + "…"
	}
	return out
}

var linkTarget = regexp.MustCompile(`\(https?://[^)]*\)`)

// initial is the first letter of a name, for logos and cover placeholders.
func initial(s string) string {
	for _, r := range strings.TrimSpace(s) {
		return strings.ToUpper(string(r))
	}
	return "K"
}

type navLink struct {
	Label, Icon, Href string
	On                bool
}

// navItem builds a sidebar link; it is active on its page and sub-pages
// (exact matches only for the dashboard).
func navItem(label, icon, href, path string, exact bool) navLink {
	on := path == href
	if !exact && !on {
		on = strings.HasPrefix(path, href+"/") || strings.HasPrefix(path, href+".")
	}
	if !exact && href != "" && strings.HasSuffix(href, "/products") && strings.Contains(path, "/skus") {
		on = true
	}
	return navLink{Label: label, Icon: icon, Href: href, On: on}
}

// fromPrice is a product's lowest price, preferring CNY, for cards.
func fromPrice(v any) string {
	var p model.Product
	switch x := v.(type) {
	case model.Product:
		p = x
	case *model.Product:
		p = *x
	default:
		return ""
	}
	skus := p.SKUs
	var buyable []model.SKU
	for _, k := range p.SKUs {
		if k.DelivererKind != "stock" || k.StockCache > 0 {
			buyable = append(buyable, k)
		}
	}
	if len(buyable) > 0 {
		skus = buyable
	}
	for _, cur := range []string{model.CNY, model.USDT, model.XTR} {
		var low int64
		n := 0
		for _, k := range skus {
			if v := k.Price(cur); v > 0 {
				if n == 0 || v < low {
					low = v
				}
				n++
			}
		}
		if n > 0 {
			out := shop.FormatMoney(low, cur)
			if len(p.SKUs) > 1 {
				out += " 起"
			}
			return out
		}
	}
	return ""
}

func (s *Server) parsePages() map[string]*template.Template {
	pages := map[string]*template.Template{}
	entries, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		panic(err)
	}
	base := template.Must(template.New("").Funcs(funcs).Funcs(template.FuncMap{"asset": s.assetURL}).ParseFS(assets, "templates/layout.html"))
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
	data["BaseURL"] = s.publicURL(c)
	if strings.HasPrefix(page, "admin_") {
		// the license page only matters once Polaris is in use
		var n int64
		if st.PolarisURL == "" {
			s.st.DB.Model(&model.SKU{}).Where("deliverer_kind = ?", "polaris").Count(&n)
		}
		data["ShowLicenses"] = st.PolarisURL != "" || n > 0
	}
	// only messages this site put in the URL are shown, so a crafted link
	// can't make the shop display arbitrary text
	if _, ok := data["Flash"]; !ok {
		data["Flash"] = flashFrom(c, "msg")
	}
	if _, ok := data["Error"]; !ok {
		data["Error"] = flashFrom(c, "err")
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
	writePage(c, code, "text/html; charset=utf-8", buf.Bytes())
}

func statusLabel(st string) string {
	switch st {
	case model.OrderPending:
		return "待支付"
	case model.OrderPaid:
		return "待发货"
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
	if k == "msg" || k == "err" {
		path += "&" + k + "_sig=" + flashSig(k, v)
	}
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

var auditLabels = map[string]string{
	"login": "登录", "login_failed": "登录失败", "oauth_link": "绑定第三方登录", "oauth_unlink": "解绑第三方登录",
	"admin_create": "添加管理员", "admin_delete": "删除管理员", "admin_password_reset": "重置管理员密码",
	"password_change": "修改密码", "totp_enable": "开启两步验证", "totp_disable": "关闭两步验证",
	"backup_download": "下载备份", "backup_run": "立即备份", "backup_send": "发送备份",
	"card_void": "作废卡密", "cards_export": "导出卡密", "cards_import": "导入卡密",
	"category_save": "保存分类", "category_delete": "删除分类",
	"coupon_create": "创建优惠码", "coupon_disable": "停用优惠码", "coupon_enable": "启用优惠码",
	"license_extend": "续期许可证", "license_revoke": "吊销许可证", "license_sync": "同步许可证",
	"order_paid": "补单", "order_redeliver": "重新发货", "order_manual": "手动发货", "order_resend": "重发通知", "order_refund": "退款",
	"orders_export": "导出订单", "product_announce": "推送商品到频道", "product_delete": "删除商品",
	"product_import_polaris": "从授权模板创建商品", "product_import_panel": "从面板套餐创建商品", "product_quick": "从模板创建商品", "product_save": "保存商品",
	"settings_save": "保存设置", "sku_delete": "删除规格", "sku_save": "保存规格",
	"telegram_webhook": "设置 Telegram Webhook", "ticket_reply": "回复工单", "ticket_close": "关闭工单", "upgrade": "在线升级",
}

// auditLabel names an audit action in Chinese (unknown codes as they are).
func auditLabel(code string) string {
	if v, ok := auditLabels[code]; ok {
		return v
	}
	return code
}

// flashKey signs the msg/err texts the site adds to redirect URLs. It only
// has to outlive one redirect, so a new one per process is fine.
var flashKey = secure.RandomBytes(32)

func flashSig(k, v string) string {
	m := hmac.New(sha256.New, flashKey)
	m.Write([]byte(k + "\x00" + v))
	return hex.EncodeToString(m.Sum(nil))[:24]
}

func flashFrom(c *gin.Context, k string) string {
	v := c.Query(k)
	if v == "" || !hmac.Equal([]byte(c.Query(k+"_sig")), []byte(flashSig(k, v))) {
		return ""
	}
	return v
}

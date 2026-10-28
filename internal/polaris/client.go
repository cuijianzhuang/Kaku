// Package polaris calls the polaris-license admin API that issues, extends
// and revokes Polaris licenses, and verifies its callbacks.
//
// Requests are signed with a shared secret:
//
//	X-Kaku-Timestamp: unix seconds
//	X-Kaku-Signature: hex(HMAC-SHA256(secret, ts + "." + METHOD + " " + PATH + "." + body))
//
// The method and path are signed too, so a captured request for one license
// cannot be replayed against another within the 5-minute window.
package polaris

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Window is how far a signed timestamp may drift.
const Window = 5 * time.Minute

// Signature computes the request signature.
func Signature(secret, ts, method, path string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + method + " " + path + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyRequest checks a signed request's timestamp and signature.
func VerifyRequest(secret string, r *http.Request, body []byte, now time.Time) error {
	if len(secret) < 32 {
		return errors.New("未配置授权服务密钥")
	}
	ts := r.Header.Get("X-Kaku-Timestamp")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("缺少时间戳")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > Window || d < -Window {
		return errors.New("签名已过期")
	}
	want := Signature(secret, ts, r.Method, r.URL.EscapedPath(), body)
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(r.Header.Get("X-Kaku-Signature")))) {
		return errors.New("签名不正确")
	}
	return nil
}

// Client talks to one polaris-license server.
type Client struct {
	BaseURL string
	Secret  string
	HTTP    *http.Client
	Now     func() time.Time
}

// Ready reports whether the client is configured.
func (c *Client) Ready() bool {
	u, err := url.Parse(c.BaseURL)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") && len(c.Secret) >= 32
}

// Limits are the license limits (0 = unlimited).
type Limits struct {
	Nodes   int `json:"nodes"`
	Users   int `json:"users"`
	Plugins int `json:"plugins"`
}

// IssueRequest asks for a new activation key.
type IssueRequest struct {
	OrderNo        string   `json:"order_no"`
	Customer       string   `json:"customer,omitempty"`
	Email          string   `json:"email,omitempty"`
	Plan           string   `json:"plan"`
	Days           int      `json:"days"`
	UpdatesDays    int      `json:"updates_days"`
	Limits         Limits   `json:"limits"`
	Features       []string `json:"features"`
	Offline        bool     `json:"offline"`
	MaxActivations int      `json:"max_activations,omitempty"`
	// Template issues from a license template on the server (API 2): its
	// plan, limits, features, activations and month terms are used.
	Template int64 `json:"template,omitempty"`
}

// Term is how much an extension adds: months first, then days.
type Term struct {
	Months, Days, UpdateMonths, UpdatesDays int
}

// Named is an identifier with its display name.
type Named struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Template is a license template defined on the server.
type Template struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Plan           string   `json:"plan"`
	Limits         Limits   `json:"limits"`
	Features       []string `json:"features"`
	MaxActivations int      `json:"max_activations"`
	Offline        bool     `json:"offline"`
	Months         int      `json:"months"`        // 0 = perpetual
	UpdateMonths   int      `json:"update_months"` // 0 = every version
	Note           string   `json:"note"`
}

// Catalog is what the server can issue (API 2).
type Catalog struct {
	API          int        `json:"api"`
	Plans        []Named    `json:"plans"`
	Features     []Named    `json:"features"`
	EveryFeature string     `json:"every_feature"`
	Templates    []Template `json:"templates"`
}

// Template returns a template by id.
func (c *Catalog) Template(id int64) *Template {
	for i := range c.Templates {
		if c.Templates[i].ID == id {
			return &c.Templates[i]
		}
	}
	return nil
}

// Every is the "all features, including future ones" mark.
const Every = "*"

// Builtin is used when the server has no catalogue (older polaris-license).
var Builtin = Catalog{
	Plans: []Named{{"pro", "专业版"}, {"enterprise", "企业版"}},
	Features: []Named{
		{"speed_limit", "客户独立限速"}, {"tg_menu", "Telegram 管理菜单"}, {"backup_push", "备份推送"},
		{"sub_convert", "订阅转换"}, {"ddns", "DDNS 与域名绑定"}, {"branding", "自定义 Logo"}, {"themes", "专属主题"},
		{"plan_addons", "套餐增值（流量包、套餐节点倍率、端口转发）"}, {"shop_payments", "在线收款（Stars、易支付、USDT）"},
	},
	EveryFeature: Every,
}

// HasFeature reports whether id is a feature the catalogue knows (or "*").
func (c *Catalog) HasFeature(id string) bool {
	if id == Every {
		return true
	}
	for _, f := range c.Features {
		if f.ID == id {
			return true
		}
	}
	return false
}

// HasPlan reports whether id is a plan the catalogue knows.
func (c *Catalog) HasPlan(id string) bool {
	for _, p := range c.Plans {
		if p.ID == id {
			return true
		}
	}
	return false
}

// License is the server's view of a license.
type License struct {
	Key          string   `json:"key"`
	LicenseID    string   `json:"license_id"`
	Plan         string   `json:"plan"`
	Status       string   `json:"status"` // valid / activated / expired / revoked
	Revoked      bool     `json:"revoked"`
	ExpiresAt    string   `json:"expires_at"`
	UpdatesUntil string   `json:"updates_until"`
	Instance     string   `json:"instance"`
	Instances    []string `json:"instances"`
	Offline      bool     `json:"offline"`
	License      string   `json:"license"` // signed document, offline licenses only
	Created      bool     `json:"created"`
	// Rights (API 2; empty from older servers).
	Features       []string `json:"features"`
	Limits         *Limits  `json:"limits"`
	MaxActivations int      `json:"max_activations"`
}

// APIError is an error answer from the server.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("授权服务返回 %d：%s", e.Status, e.Message)
}

// Permanent reports whether retrying cannot help (bad request, not found…).
func (e *APIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests && e.Status != http.StatusUnauthorized
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if !c.Ready() {
		return errors.New("未配置授权服务（地址或密钥）")
	}
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	base, _ := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	u := *base
	u.Path = base.Path + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	ts := strconv.FormatInt(now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kaku-Timestamp", ts)
	req.Header.Set("X-Kaku-Signature", Signature(c.Secret, ts, method, u.EscapedPath(), body))
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接授权服务：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("授权服务响应格式不正确：%w", err)
		}
	}
	return nil
}

// Issue creates (or, for a repeated order number, returns) an activation key.
func (c *Client) Issue(ctx context.Context, req IssueRequest) (*License, error) {
	var out License
	return &out, c.do(ctx, http.MethodPost, "/admin/v1/keys", req, &out)
}

// Extend lengthens a license; repeated order numbers are no-ops. Months
// need API 2 (older servers ignore them).
func (c *Client) Extend(ctx context.Context, licenseID, orderNo string, t Term) (*License, error) {
	var out License
	body := map[string]any{"order_no": orderNo, "days": t.Days, "updates_days": t.UpdatesDays}
	if t.Months > 0 || t.UpdateMonths > 0 {
		body["months"], body["update_months"] = t.Months, t.UpdateMonths
	}
	err := c.do(ctx, http.MethodPost, "/admin/v1/licenses/"+url.PathEscape(licenseID)+"/extend", body, &out)
	return &out, err
}

// Catalog fetches plans, features and templates. ErrNoCatalog means the
// server is reachable and the secret is right, but it predates the
// catalogue.
func (c *Client) Catalog(ctx context.Context) (*Catalog, error) {
	var out Catalog
	if err := c.do(ctx, http.MethodGet, "/admin/v1/catalog", nil, &out); err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound && ae.Message != "接口未启用" {
			return nil, ErrNoCatalog
		}
		return nil, err
	}
	if out.EveryFeature == "" {
		out.EveryFeature = Every
	}
	return &out, nil
}

// ErrNoCatalog: the server answers but has no catalogue (polaris-license
// older than the template API).
var ErrNoCatalog = errors.New("授权服务版本较旧，没有商品目录接口；请升级 polaris-license")

// Explain turns a connection or API error into advice for the shop owner.
func Explain(err error) string {
	var ae *APIError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoCatalog):
		return err.Error()
	case errors.As(err, &ae):
		switch {
		case ae.Status == http.StatusNotFound && ae.Message == "接口未启用":
			return "polaris-license 没有开启发卡接口：在它的环境变量里设置 KAKU_API_SECRET（与本页密钥相同，至少 32 位）后重启"
		case ae.Status == http.StatusNotFound:
			return "地址不对：该地址上没有 polaris-license 的发卡接口，检查授权服务地址"
		case ae.Status == http.StatusForbidden:
			return "polaris-license 拒绝了本服务器的地址：把本服务器的出口 IP 加入它的 KAKU_API_ALLOW_IPS，或清空该变量"
		case ae.Message == "签名已过期":
			return "两台服务器的时间相差超过 5 分钟：校准时间（例如开启 NTP / chrony）"
		case ae.Message == "签名不正确":
			return "密钥不一致：本页的 API 密钥要与 polaris-license 的 KAKU_API_SECRET 完全相同；若授权服务挂在反向代理的子路径下，代理不要改写路径"
		case ae.Status == http.StatusTooManyRequests:
			return "失败次数过多被暂时限制，15 分钟后再试"
		}
		return ae.Error()
	}
	return "连不上授权服务：" + err.Error() + "（检查地址、端口、HTTPS 证书和防火墙）"
}

// Revoke revokes a license.
func (c *Client) Revoke(ctx context.Context, licenseID, orderNo, reason string) (*License, error) {
	var out License
	err := c.do(ctx, http.MethodPost, "/admin/v1/licenses/"+url.PathEscape(licenseID)+"/revoke",
		map[string]any{"order_no": orderNo, "reason": reason}, &out)
	return &out, err
}

// Get returns a license's current state.
func (c *Client) Get(ctx context.Context, licenseID string) (*License, error) {
	var out License
	return &out, c.do(ctx, http.MethodGet, "/admin/v1/licenses/"+url.PathEscape(licenseID), nil, &out)
}

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

// Extend adds days to a license; repeated order numbers are no-ops.
func (c *Client) Extend(ctx context.Context, licenseID, orderNo string, days, updatesDays int) (*License, error) {
	var out License
	err := c.do(ctx, http.MethodPost, "/admin/v1/licenses/"+url.PathEscape(licenseID)+"/extend",
		map[string]any{"order_no": orderNo, "days": days, "updates_days": updatesDays}, &out)
	return &out, err
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

// Package panelapi talks to a Polaris panel's admin API with an API token
// holding the redeem:write scope: it reads the panel's plans and traffic
// packs and generates or voids redeem codes for them.
package panelapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Scope is the token scope the shop needs.
const Scope = "redeem:write"

// Client is one panel.
type Client struct {
	BaseURL string // panel address including its admin path, e.g. https://panel.example.com/my-admin
	Token   string // plt_…
	HTTP    *http.Client
}

// Ready reports whether the client is configured.
func (c *Client) Ready() bool {
	u, err := url.Parse(c.BaseURL)
	return err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") && strings.HasPrefix(c.Token, "plt_")
}

// APIError is an error answer from the panel.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("面板返回 %d：%s", e.Status, e.Message) }

// Permanent reports whether retrying cannot help.
func (e *APIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests && e.Status != http.StatusUnauthorized
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	if !c.Ready() {
		return errors.New("未配置 Polaris 面板（地址或 API 令牌）")
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+"/api/v1"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接面板：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
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
			return fmt.Errorf("面板响应格式不正确（地址是否包含后台路径？）：%w", err)
		}
	}
	return nil
}

// Self describes the token.
type Self struct {
	Name    string   `json:"name"`
	Scopes  []string `json:"scopes"`
	Version string   `json:"version"`
}

// HasScope reports whether the token may manage redeem codes.
func (s *Self) HasScope() bool { return slices.Contains(s.Scopes, Scope) }

// Self returns the token's name and scopes.
func (c *Client) Self(ctx context.Context) (*Self, error) {
	var out Self
	return &out, c.do(ctx, http.MethodGet, "/tokens/self", nil, &out)
}

// Plan is a panel plan.
type Plan struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	TrafficLimit int64  `json:"trafficLimit"` // bytes, 0 = unlimited
	Days         int    `json:"days"`         // 0 = no expiry
	DeviceLimit  int    `json:"deviceLimit"`
	Price        string `json:"price"`
	Remark       string `json:"remark"`
}

// Pack is a traffic pack.
type Pack struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Traffic int64  `json:"traffic"` // bytes
}

// Plans lists the panel's plans.
func (c *Client) Plans(ctx context.Context) ([]Plan, error) {
	var out []Plan
	return out, c.do(ctx, http.MethodGet, "/plans", nil, &out)
}

// Packs lists the traffic packs that can be sold as codes: none when the
// panel's license does not include traffic packs (it would refuse codes).
func (c *Client) Packs(ctx context.Context) ([]Pack, error) {
	var out struct {
		Items    []Pack `json:"items"`
		Licensed bool   `json:"licensed"`
	}
	if err := c.do(ctx, http.MethodGet, "/traffic-packs", nil, &out); err != nil {
		return nil, err
	}
	if !out.Licensed {
		return nil, nil
	}
	return out.Items, nil
}

// Code is a redeem code.
type Code struct {
	Code   string     `json:"code"`
	UsedAt *time.Time `json:"usedAt"`
}

// Codes lists the codes of a batch.
func (c *Client) Codes(ctx context.Context, batch string) ([]Code, error) {
	var out struct {
		Items []Code `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/redeem-codes?limit=2000&batch="+url.QueryEscape(batch), nil, &out)
	return out.Items, err
}

// Target is what codes are for: a plan or a traffic pack.
type Target struct {
	PlanID, PackID string
}

// Create generates count codes in a batch.
func (c *Client) Create(ctx context.Context, t Target, count int, batch string, expireDays int) ([]string, error) {
	var out struct {
		Codes []string `json:"codes"`
	}
	in := map[string]any{"count": count, "batch": batch, "expireDays": expireDays}
	if t.PackID != "" {
		in["packId"] = t.PackID
	} else {
		in["planId"] = t.PlanID
	}
	return out.Codes, c.do(ctx, http.MethodPost, "/redeem-codes", in, &out)
}

// Void deletes the named codes that are still unused and reports how many
// were deleted.
func (c *Client) Void(ctx context.Context, codes []string) (int, error) {
	var out struct {
		Deleted int `json:"deleted"`
	}
	return out.Deleted, c.do(ctx, http.MethodDelete, "/redeem-codes", map[string]any{"codes": codes}, &out)
}

// Explain turns an error into advice for the shop owner.
func Explain(err error) string {
	var ae *APIError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &ae):
		switch {
		case ae.Status == http.StatusUnauthorized:
			return "API 令牌无效或已过期：在面板「扩展 → API 令牌」重新创建，勾选「生成和作废兑换码」"
		case ae.Status == http.StatusForbidden:
			return "令牌没有兑换码权限：创建令牌时勾选「生成和作废兑换码」（redeem:write）；面板版本过旧时请先升级面板"
		case ae.Status == http.StatusNotFound:
			return "地址不对：面板地址要包含后台路径，例如 https://panel.example.com/你的后台路径"
		}
		return ae.Error()
	case strings.HasPrefix(err.Error(), "连接面板"):
		return "连不上面板：" + err.Error() + "（检查地址、HTTPS 证书和防火墙）"
	}
	return err.Error()
}

package deliver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/polaris"
)

// Webhook asks an external system for the content: Kaku POSTs the order
// and expects {"items": ["…", …]} back. Requests are signed like the
// polaris-license API (X-Kaku-Timestamp / X-Kaku-Signature) with the SKU's
// secret. The order number is the idempotency key.
type Webhook struct{ env *Env }

type webhookCfg struct {
	URL       string `json:"url"`
	Secret    string `json:"secret"`
	RevokeURL string `json:"revoke_url"`
}

func (*Webhook) Kind() string  { return "webhook" }
func (*Webhook) Label() string { return "Webhook 接口" }

func (*Webhook) Validate(cfg json.RawMessage) error {
	var c webhookCfg
	if err := json.Unmarshal(orEmpty(cfg), &c); err != nil {
		return fmt.Errorf("配置不是有效 JSON：%w", err)
	}
	if c.URL == "" {
		return errors.New("请填写 url")
	}
	for _, raw := range []string{c.URL, c.RevokeURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("url 需为 http(s) 地址")
		}
	}
	if len(c.Secret) < 16 {
		return errors.New("secret 至少 16 位")
	}
	return nil
}

func (*Webhook) Reserve(context.Context, *gorm.DB, *model.Order, *model.SKU) error { return nil }
func (*Webhook) Release(*gorm.DB, *model.Order) error                              { return nil }

func (w *Webhook) call(ctx context.Context, target, secret string, o *model.Order, out any) error {
	body, _ := json.Marshal(map[string]any{
		"order_no": o.OrderNo, "sku_id": o.SKUID, "qty": o.Qty, "email": o.Email,
		"amount": o.Amount, "currency": o.Currency, "renew_of": o.RenewOf,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return Permanent(err)
	}
	ts := strconv.FormatInt(w.env.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Kaku-Timestamp", ts)
	req.Header.Set("X-Kaku-Signature", polaris.Signature(secret, ts, http.MethodPost, req.URL.EscapedPath(), body))
	resp, err := w.env.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 429 {
		return Permanent(fmt.Errorf("接口返回 %d：%s", resp.StatusCode, trimText(string(raw), 200)))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("接口返回 %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("接口响应不是有效 JSON：%w", err)
		}
	}
	return nil
}

func (w *Webhook) Deliver(ctx context.Context, o *model.Order, sku *model.SKU) ([]Item, error) {
	var c webhookCfg
	if err := json.Unmarshal(orEmpty(json.RawMessage(sku.DelivererCfg)), &c); err != nil || c.URL == "" {
		return nil, Permanent(errors.New("Webhook 配置无效"))
	}
	var out struct {
		Items []string `json:"items"`
	}
	if err := w.call(ctx, c.URL, c.Secret, o, &out); err != nil {
		return nil, err
	}
	if len(out.Items) == 0 {
		return nil, errors.New("接口没有返回发货内容")
	}
	items := make([]Item, len(out.Items))
	for i, v := range out.Items {
		items[i] = Item{Content: v, Source: model.SourceWebhook}
	}
	return items, nil
}

func (w *Webhook) Revoke(ctx context.Context, o *model.Order, sku *model.SKU) error {
	var c webhookCfg
	json.Unmarshal(orEmpty(json.RawMessage(sku.DelivererCfg)), &c)
	if c.RevokeURL == "" {
		return ErrRevokeUnsupported
	}
	return w.call(ctx, c.RevokeURL, c.Secret, o, nil)
}

func (*Webhook) Stock(*model.SKU) int { return -1 }

func trimText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/panelapi"
)

// PanelCode sells a Polaris panel's own plans and traffic packs: after
// payment it generates redeem codes on the panel (batch = order number, so
// a retry returns the same codes) and the buyer redeems them in the panel's
// shop or Telegram bot. A refund voids the codes not yet redeemed.
type PanelCode struct{ env *Env }

// PanelCodeCfg is a panel SKU's deliverer_cfg.
type PanelCodeCfg struct {
	PlanID     string `json:"plan_id,omitempty"`
	PackID     string `json:"pack_id,omitempty"`
	Name       string `json:"name"`             // plan or pack name when saved
	Detail     string `json:"detail,omitempty"` // e.g. "30 天 · 100 GB", when saved
	ExpireDays int    `json:"expire_days"`      // code validity, 0 = no expiry
}

// ParsePanelCodeCfg reads and checks a configuration.
func ParsePanelCodeCfg(raw string) (*PanelCodeCfg, error) {
	var c PanelCodeCfg
	if err := json.Unmarshal(orEmpty(json.RawMessage(raw)), &c); err != nil {
		return nil, fmt.Errorf("配置不是有效 JSON：%w", err)
	}
	switch {
	case (c.PlanID == "") == (c.PackID == ""):
		return nil, errors.New("请选择一个面板套餐或流量包")
	case c.ExpireDays < 0 || c.ExpireDays > 3650:
		return nil, errors.New("兑换码有效期需在 0–3650 天之间")
	}
	return &c, nil
}

func (*PanelCode) Kind() string  { return "panelcode" }
func (*PanelCode) Label() string { return "Polaris 面板套餐兑换码" }
func (*PanelCode) Validate(cfg json.RawMessage) error {
	_, err := ParsePanelCodeCfg(string(cfg))
	return err
}
func (*PanelCode) Reserve(context.Context, *gorm.DB, *model.Order, *model.SKU) error { return nil }
func (*PanelCode) Release(*gorm.DB, *model.Order) error                              { return nil }
func (*PanelCode) Stock(*model.SKU) int                                              { return -1 }

func (p *PanelCode) client() (*panelapi.Client, error) {
	var c *panelapi.Client
	if p.env.Panel != nil {
		c = p.env.Panel()
	}
	if c == nil || !c.Ready() {
		return nil, errors.New("未配置 Polaris 面板（后台 → 设置 → Polaris 面板）")
	}
	return c, nil
}

func panelErr(err error) error {
	var ae *panelapi.APIError
	if errors.As(err, &ae) && ae.Permanent() {
		return Permanent(errors.New(panelapi.Explain(err)))
	}
	return err
}

func (p *PanelCode) Deliver(ctx context.Context, o *model.Order, sku *model.SKU) ([]Item, error) {
	cfg, err := ParsePanelCodeCfg(sku.DelivererCfg)
	if err != nil {
		return nil, Permanent(err)
	}
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	// codes made by an earlier attempt for this order are reused
	have, err := c.Codes(ctx, o.OrderNo)
	if err != nil {
		return nil, panelErr(err)
	}
	codes := make([]string, 0, o.Qty)
	for _, x := range have {
		if len(codes) < o.Qty {
			codes = append(codes, x.Code)
		}
	}
	if n := o.Qty - len(codes); n > 0 {
		more, err := c.Create(ctx, panelapi.Target{PlanID: cfg.PlanID, PackID: cfg.PackID}, n, o.OrderNo, cfg.ExpireDays)
		if err != nil {
			return nil, panelErr(err)
		}
		codes = append(codes, more...)
	}
	if len(codes) < o.Qty {
		return nil, errors.New("面板没有返回足够的兑换码")
	}
	items := make([]Item, len(codes))
	for i, code := range codes {
		items[i] = Item{Content: code, Source: model.SourcePanel, RefID: code}
	}
	return items, nil
}

// Revoke voids the order's codes that were not redeemed yet.
func (p *PanelCode) Revoke(ctx context.Context, o *model.Order, _ *model.SKU) error {
	c, err := p.client()
	if err != nil {
		return err
	}
	have, err := c.Codes(ctx, o.OrderNo)
	if err != nil {
		return panelErr(err)
	}
	var unused []string
	used := 0
	for _, x := range have {
		if x.UsedAt != nil {
			used++
		} else {
			unused = append(unused, x.Code)
		}
	}
	if len(unused) > 0 {
		if _, err := c.Void(ctx, unused); err != nil {
			return panelErr(err)
		}
	}
	if used > 0 {
		return fmt.Errorf("%w：%d 个兑换码已被兑换，套餐已开通，需要时请在面板中处理对应用户", ErrRevokeUnsupported, used)
	}
	return nil
}

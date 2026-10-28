package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/polaris"
)

// Polaris issues Polaris license activation keys through polaris-license
// ("实时签发"): unlimited stock, parameters per SKU, the signing key never
// leaves the license server. A renewal order extends its license instead.
// Offline SKUs deliver the signed .lic document.
type Polaris struct{ env *Env }

// PolarisCfg is a Polaris SKU's deliverer_cfg. With Template set the license
// server issues from that template (its rights and month terms); Plan,
// Months and UpdateMonths are then a copy taken when the SKU was saved,
// used for the renewal page and for renewals.
type PolarisCfg struct {
	Template     int64  `json:"template,omitempty"`
	TemplateName string `json:"template_name,omitempty"`
	Months       int    `json:"months,omitempty"`        // template validity, 0 = perpetual
	UpdateMonths int    `json:"update_months,omitempty"` // template update period, 0 = every version

	Plan           string         `json:"plan"`
	Days           int            `json:"days"`         // 0 = perpetual
	UpdatesDays    int            `json:"updates_days"` // 0 = every version
	Limits         polaris.Limits `json:"limits"`
	Features       []string       `json:"features"`
	Offline        bool           `json:"offline"`
	MaxActivations int            `json:"max_activations"`
	// Renewable lists the SKU on the renewal page of licenses with the same
	// plan (needs days > 0).
	Renewable bool `json:"renewable"`
}

var featureRE = regexp.MustCompile(`^(\*|[a-z][a-z0-9_]{1,40})$`)

// ParsePolarisCfg reads and checks a configuration.
func ParsePolarisCfg(raw string) (*PolarisCfg, error) {
	var c PolarisCfg
	if err := json.Unmarshal(orEmpty(json.RawMessage(raw)), &c); err != nil {
		return nil, fmt.Errorf("配置不是有效 JSON：%w", err)
	}
	c.Plan = strings.ToLower(strings.TrimSpace(c.Plan))
	if c.Template < 0 || c.Months < 0 || c.Months > 1200 || c.UpdateMonths < 0 || c.UpdateMonths > 1200 {
		return nil, errors.New("模板配置不正确")
	}
	switch {
	case c.Plan != "pro" && c.Plan != "enterprise":
		return nil, errors.New("plan 只能是 pro 或 enterprise")
	case c.Days < 0 || c.Days > 36500 || c.UpdatesDays < 0 || c.UpdatesDays > 36500:
		return nil, errors.New("days / updates_days 需在 0–36500 之间")
	case c.Limits.Nodes < 0 || c.Limits.Users < 0 || c.Limits.Plugins < 0:
		return nil, errors.New("limits 不能为负数")
	case c.Renewable && !c.Timed():
		return nil, errors.New("永久授权不能作为续费商品")
	case c.Renewable && c.Offline:
		return nil, errors.New("离线许可证不能在线续费")
	}
	for _, f := range c.Features {
		if !featureRE.MatchString(f) {
			return nil, fmt.Errorf("功能标识 %q 不正确", f)
		}
	}
	return &c, nil
}

// Timed reports whether the license expires (only those can be renewed).
func (c *PolarisCfg) Timed() bool {
	if c.Template > 0 {
		return c.Months > 0
	}
	return c.Days > 0
}

// RenewTerm is what one unit of this SKU adds to a license.
func (c *PolarisCfg) RenewTerm(qty int) polaris.Term {
	if c.Template > 0 {
		return polaris.Term{Months: c.Months * qty, UpdateMonths: c.UpdateMonths * qty}
	}
	return polaris.Term{Days: c.Days * qty, UpdatesDays: c.UpdatesDays * qty}
}

// CheckAgainst checks plan and feature names against what the license
// server knows, so a typo is caught when the SKU is saved rather than
// when a paid order fails to deliver.
func (c *PolarisCfg) CheckAgainst(cat *polaris.Catalog) error {
	if c.Template > 0 {
		return nil
	}
	if !cat.HasPlan(c.Plan) {
		return fmt.Errorf("授权服务没有套餐 %q", c.Plan)
	}
	for _, f := range c.Features {
		if !cat.HasFeature(f) {
			return fmt.Errorf("授权服务没有功能 %q", f)
		}
	}
	return nil
}

func (*Polaris) Kind() string  { return "polaris" }
func (*Polaris) Label() string { return "Polaris 激活码" }
func (*Polaris) Validate(cfg json.RawMessage) error {
	_, err := ParsePolarisCfg(string(cfg))
	return err
}
func (*Polaris) Reserve(context.Context, *gorm.DB, *model.Order, *model.SKU) error { return nil }
func (*Polaris) Release(*gorm.DB, *model.Order) error                              { return nil }
func (*Polaris) Stock(*model.SKU) int                                              { return -1 }

func (p *Polaris) client() (*polaris.Client, error) {
	c := p.env.Polaris()
	if c == nil || !c.Ready() {
		return nil, errors.New("未配置授权服务（后台 → 设置 → Polaris 授权服务）")
	}
	return c, nil
}

// apiErr turns permanent API answers into permanent delivery failures.
func apiErr(err error) error {
	var ae *polaris.APIError
	if errors.As(err, &ae) && ae.Permanent() {
		return Permanent(err)
	}
	return err
}

func (p *Polaris) Deliver(ctx context.Context, o *model.Order, sku *model.SKU) ([]Item, error) {
	cfg, err := ParsePolarisCfg(sku.DelivererCfg)
	if err != nil {
		return nil, Permanent(err)
	}
	c, err := p.client()
	if err != nil {
		return nil, err
	}
	db := p.env.Store.DB
	if o.RenewOf != "" {
		l, err := c.Extend(ctx, o.RenewOf, o.OrderNo, cfg.RenewTerm(o.Qty))
		if err != nil {
			return nil, apiErr(err)
		}
		db.Model(&model.LicenseBinding{}).Where("license_id = ?", o.RenewOf).Update("expires_at", l.ExpiresAt)
		until := l.ExpiresAt
		if until == "" {
			until = "永久"
		}
		return []Item{{Content: fmt.Sprintf("许可证 %s 已续期，到期日 %s。面板下次校验时自动生效，无需重新激活。", o.RenewOf, until),
			Source: model.SourcePolaris, RefID: o.RenewOf}}, nil
	}
	var items []Item
	for i := range o.Qty {
		no := o.OrderNo
		if o.Qty > 1 {
			no = fmt.Sprintf("%s-%d", o.OrderNo, i+1)
		}
		req := polaris.IssueRequest{
			OrderNo: no, Customer: o.Email, Email: o.Email, Plan: cfg.Plan, Days: cfg.Days, UpdatesDays: cfg.UpdatesDays,
			Limits: cfg.Limits, Features: cfg.Features, Offline: cfg.Offline, MaxActivations: cfg.MaxActivations,
		}
		if cfg.Template > 0 {
			req = polaris.IssueRequest{OrderNo: no, Customer: o.Email, Email: o.Email, Plan: cfg.Plan, Template: cfg.Template}
		}
		l, err := c.Issue(ctx, req)
		if err != nil {
			return nil, apiErr(err)
		}
		if l.LicenseID == "" || (l.Key == "" && l.License == "") {
			return nil, errors.New("授权服务没有返回激活码")
		}
		b := model.LicenseBinding{
			OrderID: o.ID, LicenseID: l.LicenseID, Plan: l.Plan, ExpiresAt: l.ExpiresAt, Offline: l.Offline,
			Email: o.Email, TGUserID: o.TGUserID, Status: model.LicenseIssued,
		}
		if l.Key != "" {
			b.ActivationKeyHash, b.KeyEnc = p.env.Store.Box.Hash(l.Key), p.env.Store.Box.Seal(l.Key)
		}
		if err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "license_id"}}, DoNothing: true}).Create(&b).Error; err != nil {
			return nil, err
		}
		if l.Offline && l.License != "" {
			items = append(items, Item{Content: l.License, Source: model.SourcePolaris, RefID: l.LicenseID, FileName: "polaris-" + l.LicenseID + ".lic"})
		} else {
			items = append(items, Item{Content: l.Key, Source: model.SourcePolaris, RefID: l.LicenseID})
		}
	}
	return items, nil
}

// Revoke revokes every license the order issued. A renewal cannot be
// undone automatically (the license predates the order), so the admin is
// told instead.
func (p *Polaris) Revoke(ctx context.Context, o *model.Order, _ *model.SKU) error {
	if o.RenewOf != "" {
		return ErrRevokeUnsupported
	}
	var bs []model.LicenseBinding
	if err := p.env.Store.DB.Where("order_id = ?", o.ID).Find(&bs).Error; err != nil {
		return err
	}
	if len(bs) == 0 {
		return nil
	}
	c, err := p.client()
	if err != nil {
		return err
	}
	for _, b := range bs {
		if b.Status == model.LicenseRevoked {
			continue
		}
		if _, err := c.Revoke(ctx, b.LicenseID, o.OrderNo, "订单退款"); err != nil {
			return err
		}
		p.env.Store.DB.Model(&model.LicenseBinding{}).Where("id = ?", b.ID).Update("status", model.LicenseRevoked)
	}
	if len(bs) > 0 && bs[0].Offline {
		return fmt.Errorf("%w：离线许可证已在授权服务吊销，但已下发的 .lic 文件仍可离线使用", ErrRevokeUnsupported)
	}
	return nil
}

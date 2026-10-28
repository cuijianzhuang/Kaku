package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/panelapi"
	"github.com/cuijianzhuang/kaku/internal/shop"
)

// Selling a Polaris panel's plans and traffic packs as redeem codes.

func (s *Server) panelRoutes(a *gin.RouterGroup) {
	a.POST("/settings/panel/test", s.testPanel)
}

func (s *Server) testPanel(c *gin.Context) {
	back := s.settingsURL("panel")
	pc := s.shop.Panel()
	if !pc.Ready() {
		redirectErr(c, back, errors.New("先填写面板地址（含后台路径）和 API 令牌（plt_ 开头）并保存"))
		return
	}
	self, err := pc.Self(c.Request.Context())
	if err != nil {
		redirectErr(c, back, errors.New(panelapi.Explain(err)))
		return
	}
	if !self.HasScope() {
		redirectErr(c, back, fmt.Errorf("令牌「%s」没有兑换码权限：在面板「扩展 → API 令牌」新建令牌并勾选「生成和作废兑换码」；若没有该选项，请先升级面板", self.Name))
		return
	}
	offer, err := s.shop.PanelOffer(c.Request.Context(), true)
	if err != nil {
		redirectErr(c, back, errors.New(panelapi.Explain(err)))
		return
	}
	redirectMsg(c, back, fmt.Sprintf("连接正常（令牌「%s」）：面板有 %d 个套餐、%d 个流量包", self.Name, len(offer.Plans), len(offer.Packs)))
}

func gb(bytes int64) string {
	if bytes <= 0 {
		return "不限流量"
	}
	g := float64(bytes) / (1 << 30)
	if g == float64(int64(g)) {
		return fmt.Sprintf("%d GB", int64(g))
	}
	return fmt.Sprintf("%.1f GB", g)
}

func planDetail(p panelapi.Plan) string {
	parts := []string{}
	if p.Days > 0 {
		parts = append(parts, fmt.Sprintf("%d 天", p.Days))
	} else {
		parts = append(parts, "不限期")
	}
	parts = append(parts, gb(p.TrafficLimit))
	if p.DeviceLimit > 0 {
		parts = append(parts, fmt.Sprintf("%d 台设备", p.DeviceLimit))
	}
	return strings.Join(parts, " · ")
}

// panelForm is what the SKU page needs for a panel-code SKU.
type panelForm struct {
	Offer  *shop.PanelOffer
	Err    string
	Cfg    deliver.PanelCodeCfg
	Target string // "plan:ID" or "pack:ID"
}

func (s *Server) panelFormData(ctx context.Context, sku *model.SKU) panelForm {
	f := panelForm{}
	if sku.DelivererKind == "panelcode" {
		if cfg, err := deliver.ParsePanelCodeCfg(sku.DelivererCfg); err == nil {
			f.Cfg = *cfg
			if cfg.PackID != "" {
				f.Target = "pack:" + cfg.PackID
			} else {
				f.Target = "plan:" + cfg.PlanID
			}
		}
	}
	if !s.shop.Panel().Ready() {
		f.Err = "还没有配置 Polaris 面板：到「设置 → Polaris 面板」填写面板地址和 API 令牌。"
		return f
	}
	offer, err := s.shop.PanelOffer(ctx, false)
	if err != nil {
		f.Err = panelapi.Explain(err)
		return f
	}
	f.Offer = offer
	return f
}

// panelCfgFromForm builds a panel-code configuration from the form,
// keeping the plan's name and terms as they are now for display.
func (s *Server) panelCfgFromForm(c *gin.Context) (string, error) {
	kind, id, _ := strings.Cut(c.PostForm("pk_target"), ":")
	days, _ := strconv.Atoi(strings.TrimSpace(c.PostForm("pk_expire_days")))
	offer, err := s.shop.PanelOffer(c.Request.Context(), true)
	if err != nil {
		return "", errors.New("读取面板套餐失败：" + panelapi.Explain(err))
	}
	cfg := deliver.PanelCodeCfg{ExpireDays: days}
	switch kind {
	case "plan":
		for _, p := range offer.Plans {
			if p.ID == id {
				cfg.PlanID, cfg.Name, cfg.Detail = p.ID, p.Name, planDetail(p)
			}
		}
	case "pack":
		for _, p := range offer.Packs {
			if p.ID == id {
				cfg.PackID, cfg.Name, cfg.Detail = p.ID, p.Name, "流量包 · "+gb(p.Traffic)
			}
		}
	}
	if cfg.PlanID == "" && cfg.PackID == "" {
		return "", errors.New("请选择面板里的一个套餐或流量包")
	}
	b, _ := json.Marshal(cfg)
	return string(b), nil
}

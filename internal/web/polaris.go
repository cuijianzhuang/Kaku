package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/polaris"
)

// Polaris product setup: the connection test, the structured SKU form and
// creating products from the license server's templates.

func (s *Server) polarisRoutes(a *gin.RouterGroup) {
	a.POST("/settings/polaris/test", s.testPolaris)
	a.GET("/products/polaris-import", s.polarisImportPage)
	a.POST("/products/polaris-import", s.polarisImport)
}

func (s *Server) testPolaris(c *gin.Context) {
	back := s.cfg.AdminPath + "/settings#polaris"
	if !s.shop.Polaris().Ready() {
		redirectErr(c, back, errors.New("先填写授权服务地址和 API 密钥（至少 32 位）并保存"))
		return
	}
	cat, err := s.shop.PolarisCatalog(c.Request.Context(), true)
	if errors.Is(err, polaris.ErrNoCatalog) {
		redirectMsg(c, back, "已连接，密钥正确；但 polaris-license 版本较旧，没有商品目录（模板、功能列表）。升级 polaris-license 后可按模板配置商品")
		return
	}
	if err != nil {
		redirectErr(c, back, errors.New(polaris.Explain(err)))
		return
	}
	redirectMsg(c, back, fmt.Sprintf("连接正常：%d 种套餐、%d 项功能、%d 个许可证模板", len(cat.Plans), len(cat.Features), len(cat.Templates)))
}

// polarisForm is what the SKU page needs to draw the Polaris settings.
type polarisForm struct {
	Catalog    *polaris.Catalog
	CatalogErr string
	Templates  bool // the server can issue from templates
	Mode       string
	Cfg        deliver.PolarisCfg
	Checked    map[string]bool
	All        bool
}

func (s *Server) polarisFormData(ctx context.Context, sku *model.SKU) polarisForm {
	cat, err := s.shop.PolarisCatalogOrBuiltin(ctx)
	f := polarisForm{Catalog: cat, Templates: err == nil && cat.API >= 2, Mode: "custom", Checked: map[string]bool{},
		Cfg: deliver.PolarisCfg{Plan: "pro", Days: 365, UpdatesDays: 365, MaxActivations: 1, Renewable: true, Features: []string{polaris.Every}}}
	switch {
	case !s.shop.Polaris().Ready():
		f.CatalogErr = "还没有配置授权服务：到「设置 → Polaris 授权服务」填写地址和密钥。下面使用内置的功能列表。"
	case err != nil:
		f.CatalogErr = polaris.Explain(err) + "。下面使用内置的功能列表。"
	}
	if sku.DelivererKind == "polaris" && sku.DelivererCfg != "" {
		if cfg, err := deliver.ParsePolarisCfg(sku.DelivererCfg); err == nil {
			f.Cfg = *cfg
			if cfg.Template > 0 {
				f.Mode = "template"
				// start the custom fields from the template, should the owner switch
				if t := cat.Template(cfg.Template); t != nil {
					f.Cfg.Days, f.Cfg.UpdatesDays = monthsToDays(t.Months), monthsToDays(t.UpdateMonths)
					f.Cfg.Limits, f.Cfg.Features, f.Cfg.MaxActivations = t.Limits, t.Features, t.MaxActivations
				}
			}
		}
	} else if f.Templates && len(cat.Templates) > 0 {
		f.Mode = "template"
	}
	for _, id := range f.Cfg.Features {
		f.Checked[id] = true
	}
	f.All = f.Checked[polaris.Every]
	return f
}

// monthsToDays turns a template term into days for the custom form.
func monthsToDays(m int) int {
	if m%12 == 0 {
		return m / 12 * 365
	}
	return m * 30
}

// polarisCfgFromForm builds a Polaris SKU configuration from the form.
func (s *Server) polarisCfgFromForm(c *gin.Context) (string, error) {
	num := func(k string) int { n, _ := strconv.Atoi(strings.TrimSpace(c.PostForm(k))); return n }
	on := func(k string) bool { return c.PostForm(k) == "on" }
	var cfg deliver.PolarisCfg
	switch c.PostForm("pc_mode") {
	case "json":
		return strings.TrimSpace(c.PostForm("deliverer_cfg")), nil
	case "template":
		cat, err := s.shop.PolarisCatalog(c.Request.Context(), true)
		if err != nil {
			return "", errors.New("读取授权服务的模板失败：" + polaris.Explain(err))
		}
		id, _ := strconv.ParseInt(c.PostForm("pc_template"), 10, 64)
		t := cat.Template(id)
		if t == nil {
			return "", errors.New("请选择一个许可证模板")
		}
		cfg = deliver.PolarisCfg{Template: t.ID, TemplateName: t.Name, Plan: t.Plan, Months: t.Months, UpdateMonths: t.UpdateMonths,
			Offline: t.Offline, Renewable: on("pc_renewable") && t.Months > 0 && !t.Offline}
	default:
		cfg = deliver.PolarisCfg{Plan: c.PostForm("pc_plan"), Days: num("pc_days"), UpdatesDays: num("pc_updates_days"),
			Limits:  polaris.Limits{Nodes: num("pc_nodes"), Users: num("pc_users"), Plugins: num("pc_plugins")},
			Offline: on("pc_offline"), MaxActivations: max(1, num("pc_max_activations")), Renewable: on("pc_renewable")}
		if on("pc_all") {
			cfg.Features = []string{polaris.Every}
		} else {
			cfg.Features = c.PostFormArray("pc_features")
		}
		// on these errors the form data is still returned, to show it again
		if len(cfg.Features) == 0 {
			b, _ := json.Marshal(cfg)
			return string(b), errors.New("至少选择一项功能，或勾选「全部高级功能」；不选功能的许可证不解锁任何高级功能")
		}
		if cfg.MaxActivations > 1000 {
			b, _ := json.Marshal(cfg)
			return string(b), errors.New("可激活实例数需在 1–1000 之间")
		}
	}
	b, _ := json.Marshal(cfg)
	return string(b), nil
}

// checkPolarisCfg validates a Polaris configuration, including plan and
// feature names against the license server's catalogue when it answers.
func (s *Server) checkPolarisCfg(ctx context.Context, raw string) error {
	cfg, err := deliver.ParsePolarisCfg(raw)
	if err != nil {
		return err
	}
	cat, _ := s.shop.PolarisCatalogOrBuiltin(ctx)
	return cfg.CheckAgainst(cat)
}

func (s *Server) polarisImportPage(c *gin.Context) {
	back := s.cfg.AdminPath + "/products"
	cat, err := s.shop.PolarisCatalog(c.Request.Context(), true)
	if err != nil {
		redirectErr(c, back, errors.New("读取许可证模板失败："+polaris.Explain(err)))
		return
	}
	var cats []model.Category
	s.st.DB.Order("sort, id").Find(&cats)
	s.render(c, 200, "admin_polaris_import", gin.H{"Catalog": cat, "Categories": cats})
}

func (s *Server) polarisImport(c *gin.Context) {
	back := s.cfg.AdminPath + "/products/polaris-import"
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	cat, err := s.shop.PolarisCatalog(ctx, true)
	if err != nil {
		redirectErr(c, back, errors.New(polaris.Explain(err)))
		return
	}
	title := strings.TrimSpace(c.PostForm("title"))
	catID := atoiU(c.PostForm("category_id"))
	var picked []polaris.Template
	for _, v := range c.PostFormArray("template") {
		id, _ := strconv.ParseInt(v, 10, 64)
		if t := cat.Template(id); t != nil && !slices.ContainsFunc(picked, func(p polaris.Template) bool { return p.ID == id }) {
			picked = append(picked, *t)
		}
	}
	switch {
	case title == "" || catID == 0:
		redirectErr(c, back, errors.New("请填写商品名称并选择分类"))
		return
	case len(picked) == 0:
		redirectErr(c, back, errors.New("至少选择一个模板"))
		return
	}
	p := model.Product{CategoryID: catID, Title: title, Status: model.ProductOff,
		DescriptionMD: "付款后自动发放 Polaris 激活码：在面板「许可证」页粘贴激活码即可激活。"}
	if err := s.st.DB.Create(&p).Error; err != nil {
		redirectErr(c, back, err)
		return
	}
	for i, t := range picked {
		cfg, _ := json.Marshal(deliver.PolarisCfg{Template: t.ID, TemplateName: t.Name, Plan: t.Plan, Months: t.Months,
			UpdateMonths: t.UpdateMonths, Offline: t.Offline, Renewable: t.Months > 0 && !t.Offline})
		s.st.DB.Create(&model.SKU{ProductID: p.ID, Name: t.Name, DelivererKind: "polaris", DelivererCfg: string(cfg),
			LimitPerOrder: 10, LowStock: 0, Active: true, Sort: i})
	}
	s.audit(c, "product_import_polaris", fmt.Sprint(p.ID), fmt.Sprintf("%d 个模板", len(picked)))
	redirectMsg(c, fmt.Sprintf("%s/products/%d", s.cfg.AdminPath, p.ID),
		fmt.Sprintf("已创建商品和 %d 个规格（暂未上架）：逐个打开规格填写价格，然后把商品状态改为上架", len(picked)))
}

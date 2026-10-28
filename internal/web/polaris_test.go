package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/polaris"
)

const licSecret = "license-server-secret-0123456789abcdef"

func fakeCatalog(t *testing.T) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := polaris.VerifyRequest(licSecret, r, body, time.Now()); err != nil {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "签名不正确"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"api": 2, "every_feature": "*",
			"plans":     []map[string]string{{"id": "pro", "name": "专业版"}, {"id": "enterprise", "name": "企业版"}},
			"features":  []map[string]string{{"id": "speed_limit", "name": "客户独立限速"}, {"id": "ddns", "name": "DDNS"}},
			"templates": []map[string]any{{"id": 7, "name": "专业版 · 年付", "plan": "pro", "months": 12, "update_months": 12, "max_activations": 1, "features": []string{"*"}}}})
	}))
	t.Cleanup(ts.Close)
	return ts
}

func location(resp *http.Response) string {
	u, _ := url.QueryUnescape(resp.Header.Get("Location"))
	return u
}

func TestPolarisConnectionTestAndSKUForm(t *testing.T) {
	e := setup(t)
	lic := fakeCatalog(t)
	c := e.login()

	e.shop.PolarisClient = &polaris.Client{BaseURL: lic.URL, Secret: strings.Repeat("w", 32)}
	if l := location(e.post(c, "/manage/settings/polaris/test", nil)); !strings.Contains(l, "密钥不一致") {
		t.Fatalf("wrong secret: %s", l)
	}
	e.shop.PolarisClient = &polaris.Client{BaseURL: lic.URL, Secret: licSecret}
	if l := location(e.post(c, "/manage/settings/polaris/test", nil)); !strings.Contains(l, "连接正常：2 种套餐、2 项功能、1 个许可证模板") {
		t.Fatalf("connection test: %s", l)
	}

	cat := model.Category{Name: "c", Visible: true}
	e.st.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "Polaris", Status: model.ProductOn}
	e.st.DB.Create(&p)
	base := url.Values{"product_id": {itoa(int64(p.ID))}, "name": {"1 年"}, "price_cny": {"199"}, "deliverer_kind": {"polaris"}, "limit_per_order": {"1"}, "active": {"on"}}
	with := func(kv ...string) url.Values {
		v := url.Values{}
		for k, x := range base {
			v[k] = x
		}
		for i := 0; i < len(kv); i += 2 {
			v.Add(kv[i], kv[i+1])
		}
		return v
	}
	lastCfg := func() string {
		var k model.SKU
		e.st.DB.Order("id desc").Take(&k)
		return k.DelivererCfg
	}

	// a typo in hand-written JSON is caught on save
	e.post(c, "/manage/skus", with("pc_mode", "json", "deliverer_cfg", `{"plan":"pro","days":365,"features":["speedlimit"]}`))
	var n int64
	e.st.DB.Model(&model.SKU{}).Count(&n)
	if n != 0 {
		t.Fatal("SKU with an unknown feature was saved")
	}
	// no features at all is refused
	e.post(c, "/manage/skus", with("pc_mode", "custom", "pc_plan", "pro", "pc_days", "365"))
	if e.st.DB.Model(&model.SKU{}).Count(&n); n != 0 {
		t.Fatal("SKU without features was saved")
	}
	// the structured form
	e.post(c, "/manage/skus", with("pc_mode", "custom", "pc_plan", "enterprise", "pc_days", "365", "pc_updates_days", "730",
		"pc_nodes", "50", "pc_features", "speed_limit", "pc_features", "ddns", "pc_max_activations", "3", "pc_renewable", "on"))
	cfg, err := deliver.ParsePolarisCfg(lastCfg())
	if err != nil || cfg.Plan != "enterprise" || cfg.Days != 365 || cfg.UpdatesDays != 730 || cfg.Limits.Nodes != 50 ||
		len(cfg.Features) != 2 || cfg.MaxActivations != 3 || !cfg.Renewable {
		t.Fatalf("custom cfg %+v %v", cfg, err)
	}
	// from a template: a copy of its plan and terms is kept for renewals
	e.post(c, "/manage/skus", with("pc_mode", "template", "pc_template", "7", "pc_renewable", "on"))
	cfg, _ = deliver.ParsePolarisCfg(lastCfg())
	if cfg.Template != 7 || cfg.Plan != "pro" || cfg.Months != 12 || !cfg.Renewable || cfg.TemplateName != "专业版 · 年付" {
		t.Fatalf("template cfg %+v", cfg)
	}

	// create a product from templates
	resp := e.post(c, "/manage/products/polaris-import", url.Values{"title": {"Polaris 年付"}, "category_id": {itoa(int64(cat.ID))}, "template": {"7"}})
	if !strings.Contains(location(resp), "已创建商品和 1 个规格") {
		t.Fatalf("import: %s", location(resp))
	}
	var imported model.Product
	e.st.DB.Preload("SKUs").Where("title = ?", "Polaris 年付").Take(&imported)
	if imported.Status != model.ProductOff || len(imported.SKUs) != 1 || !strings.Contains(imported.SKUs[0].DelivererCfg, `"template":7`) {
		t.Fatalf("imported %+v", imported)
	}
}

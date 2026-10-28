package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/panelapi"
)

func fakePanelAPI(t *testing.T, scopes []string) *httptest.Server {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer plt_ok" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		switch r.URL.Path {
		case "/adm/api/v1/tokens/self":
			json.NewEncoder(w).Encode(map[string]any{"name": "kaku", "scopes": scopes})
		case "/adm/api/v1/plans":
			json.NewEncoder(w).Encode([]map[string]any{{"id": "p1", "name": "月付", "days": 30, "trafficLimit": 100 << 30, "deviceLimit": 3}})
		case "/adm/api/v1/traffic-packs":
			json.NewEncoder(w).Encode(map[string]any{"licensed": true, "items": []map[string]any{{"id": "k1", "name": "加油包", "traffic": 50 << 30}}})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestPanelSettingsAndSKUForm(t *testing.T) {
	e := setup(t)
	c := e.login()

	bad := fakePanelAPI(t, []string{"users:read"})
	e.shop.PanelClient = &panelapi.Client{BaseURL: bad.URL + "/adm", Token: "plt_ok"}
	if l := location(e.post(c, "/manage/settings/panel/test", nil)); !strings.Contains(l, "没有兑换码权限") {
		t.Fatalf("token without the scope: %s", l)
	}
	ok := fakePanelAPI(t, []string{"redeem:write"})
	e.shop.PanelClient = &panelapi.Client{BaseURL: ok.URL + "/adm", Token: "plt_ok"}
	if l := location(e.post(c, "/manage/settings/panel/test", nil)); !strings.Contains(l, "面板有 1 个套餐、1 个流量包") {
		t.Fatalf("connection test: %s", l)
	}
	e.shop.PanelClient = &panelapi.Client{BaseURL: ok.URL + "/adm", Token: "plt_wrong"}
	if l := location(e.post(c, "/manage/settings/panel/test", nil)); !strings.Contains(l, "令牌无效") {
		t.Fatalf("wrong token: %s", l)
	}
	e.shop.PanelClient = &panelapi.Client{BaseURL: ok.URL + "/adm", Token: "plt_ok"}

	cat := model.Category{Name: "c", Visible: true}
	e.st.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "机场套餐", Status: model.ProductOn}
	e.st.DB.Create(&p)
	form := url.Values{"product_id": {itoa(int64(p.ID))}, "name": {"月付"}, "price_cny": {"15"}, "deliverer_kind": {"panelcode"},
		"limit_per_order": {"5"}, "active": {"on"}, "pk_target": {"plan:p1"}, "pk_expire_days": {"30"}}
	e.post(c, "/manage/skus", form)
	var k model.SKU
	if err := e.st.DB.Take(&k).Error; err != nil {
		t.Fatal("SKU not saved")
	}
	cfg, err := deliver.ParsePanelCodeCfg(k.DelivererCfg)
	if err != nil || cfg.PlanID != "p1" || cfg.Name != "月付" || cfg.ExpireDays != 30 || cfg.Detail != "30 天 · 100 GB · 3 台设备" {
		t.Fatalf("cfg %+v %v", cfg, err)
	}
	// an unknown plan is refused
	form.Set("pk_target", "plan:nope")
	form.Set("name", "x")
	e.post(c, "/manage/skus", form)
	var n int64
	if e.st.DB.Model(&model.SKU{}).Count(&n); n != 1 {
		t.Fatal("SKU with an unknown plan was saved")
	}
	// the SKU page lists the panel's plans
	resp, _ := c.Get(e.ts.URL + "/manage/skus/" + itoa(int64(k.ID)))
	b := new(strings.Builder)
	buf := make([]byte, 1<<16)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(b.String(), `value="plan:p1" checked`) || !strings.Contains(b.String(), "加油包") {
		t.Fatal("SKU page does not offer the panel's plans")
	}
}

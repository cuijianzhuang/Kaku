package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/model"
)

func TestAdminUIReview(t *testing.T) {
	e := setup(t)
	c := e.login()
	get := func(path string) string {
		resp, _ := c.Get(e.ts.URL + path)
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	// dashboard: setup checklist while the shop is not ready
	dash := get("/manage")
	if !strings.Contains(dash, "开店检查") || !strings.Contains(dash, "配置发件邮箱") {
		t.Fatal("dashboard lacks the setup checklist")
	}
	// license page hidden until Polaris is used
	if strings.Contains(dash, `/manage/licenses"`) {
		t.Fatal("许可证 shown without Polaris")
	}
	set := e.st.Settings()
	set.PolarisURL = "https://license.example.com"
	e.st.SaveSettings(set)
	if !strings.Contains(get("/manage"), `/manage/licenses"`) {
		t.Fatal("许可证 hidden although Polaris is configured")
	}

	// coupons: scope by product and spec names, re-enable
	o := e.stockOrder(500)
	var sku model.SKU
	e.st.DB.Take(&sku, o.SKUID)
	e.post(c, "/manage/coupons", url.Values{"code": {"SAVE10"}, "type": {"percent"}, "value": {"10"}, "max_uses": {"0"}, "sku_scope": {itoa(int64(sku.ID))}})
	var cp model.Coupon
	e.st.DB.Where("code = ?", "SAVE10").Take(&cp)
	if cp.SKUScope != itoa(int64(sku.ID)) {
		t.Fatalf("scope %q", cp.SKUScope)
	}
	page := get("/manage/coupons")
	if !strings.Contains(page, "P · S") || !strings.Contains(page, "减 10%") || !strings.Contains(page, "不过期") {
		t.Fatal("coupon list not readable")
	}
	e.post(c, "/manage/coupons/"+itoa(int64(cp.ID))+"/delete", nil)
	e.post(c, "/manage/coupons/"+itoa(int64(cp.ID))+"/enable", nil)
	e.st.DB.Take(&cp, cp.ID)
	if !cp.Active {
		t.Fatal("coupon not re-enabled")
	}

	// cards: preview start and end, link back to the product
	cards := get("/manage/cards?sku=" + itoa(int64(sku.ID)))
	if !strings.Contains(cards, "CA••••") && !strings.Contains(cards, "CARD") {
		t.Fatal("card preview missing")
	}
	if !strings.Contains(cards, "返回商品") || strings.Contains(cards, "两步验证码") {
		t.Fatal("cards page header / 2FA field")
	}

	// audit: Chinese labels and a clearable filter
	audit := get("/manage/audit?action=coupon_create")
	if !strings.Contains(audit, "创建优惠码") || !strings.Contains(audit, "清除") {
		t.Fatal("audit page not translated")
	}
}

func TestCardPreview(t *testing.T) {
	if got := cardPreview("STEAM-AAAA-1002"); got != "STEA…1002" {
		t.Fatal(got)
	}
	if got := cardPreview("abc"); got != "ab••••" {
		t.Fatal(got)
	}
}

func TestProductListAndPage(t *testing.T) {
	e := setup(t)
	c := e.login()
	o := e.stockOrder(500) // on-sale product "P" with a stock spec "S" (2 cards, one reserved)
	var sku model.SKU
	e.st.DB.Take(&sku, o.SKUID)
	get := func(cl interface {
		Get(string) (*http.Response, error)
	}, path string) string {
		resp, _ := cl.Get(e.ts.URL + path)
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	list := get(c, "/manage/products")
	if !strings.Contains(list, "toggle on") || !strings.Contains(list, "1 个规格") {
		t.Fatal("product list lacks the toggle / summary")
	}
	if !strings.Contains(get(c, "/manage/products?q=nothing"), "没有符合条件的商品") {
		t.Fatal("search filter")
	}
	// toggle off, then on again
	e.post(c, "/manage/products/"+itoa(int64(sku.ProductID))+"/toggle", url.Values{})
	var p model.Product
	e.st.DB.Take(&p, sku.ProductID)
	if p.Status != model.ProductOff {
		t.Fatal("not taken off sale")
	}
	e.post(c, "/manage/products/"+itoa(int64(sku.ProductID))+"/toggle", url.Values{})
	e.st.DB.Take(&p, sku.ProductID)
	if p.Status != model.ProductOn {
		t.Fatal("not put on sale")
	}
	// a product with nothing to sell cannot be switched on
	bare := model.Product{CategoryID: p.CategoryID, Title: "空商品", Status: model.ProductOff}
	e.st.DB.Create(&bare)
	l := location(e.post(c, "/manage/products/"+itoa(int64(bare.ID))+"/toggle", url.Values{}))
	if !strings.Contains(l, "err=") {
		t.Fatalf("empty product went on sale: %s", l)
	}
	// buyer page shows the delivery tag and stock
	page := get(e.client(), "/p/"+itoa(int64(sku.ProductID)))
	if !strings.Contains(page, "自动发货") || !strings.Contains(page, "库存 1") {
		t.Fatal("product page tags")
	}
}

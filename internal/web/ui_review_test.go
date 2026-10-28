package web

import (
	"io"
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

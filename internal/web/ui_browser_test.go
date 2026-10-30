package web

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/model"
)

// TestLiveUIBrowser exercises the real templates, assets, login and form posts.
// Opt-in because Chromium/WebKit and Python Playwright are needed.
func TestLiveUIBrowser(t *testing.T) {
	if os.Getenv("KAKU_UI_BROWSER_TEST") != "1" {
		t.Skip("KAKU_UI_BROWSER_TEST not set")
	}
	e := setup(t)
	st := e.st.Settings()
	st.SiteName = "Kaku 数字商品商店"
	st.MaxUnpaid = 20
	st.Notice = "选购数字商品，付款后在订单页查看发货内容。请妥善保存查询密码。"
	if err := e.st.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	e.stockOrder(1900)
	cat := model.Category{Name: "数字服务与订阅", Visible: true}
	if err := e.st.DB.Create(&cat).Error; err != nil {
		t.Fatal(err)
	}
	titles := []string{"Polaris 专业版 · 月度授权", "设计资源与字体合集 / DesignResources2026WithoutSpacesVeryLongTitle", "云端存储空间 · 年度订阅", "效率工具与工作流", "开发者资源包", "电子阅读与资料库"}
	for i, title := range titles {
		p := model.Product{CategoryID: cat.ID, Title: title, Status: model.ProductOn, DescriptionMD: "## 商品说明\n\n付款后自动发货，请保存订单号。"}
		if err := e.st.DB.Create(&p).Error; err != nil {
			t.Fatal(err)
		}
		sku := model.SKU{ProductID: p.ID, Name: fmt.Sprintf("标准版 · 套餐 %d", i+1), PriceCNY: 1900, DelivererKind: "text", DelivererCfg: `{"text":"synthetic delivery"}`, LimitPerOrder: 10, Active: true}
		if err := e.st.DB.Create(&sku).Error; err != nil {
			t.Fatal(err)
		}
	}
	// Product 2 deliberately starts with a sold-out SKU and includes a SKU
	// with no web payment method. The default must still be the usable one.
	if err := e.st.DB.Model(&model.Product{}).Where("id = ?", 2).Update("description_md", strings.Repeat("商品说明：付款后在订单页领取内容，请保存订单号。\n\n", 18)).Error; err != nil {
		t.Fatal(err)
	}
	if err := e.st.DB.Model(&model.SKU{}).Where("product_id = ?", 2).Update("limit_per_order", 2).Error; err != nil {
		t.Fatal(err)
	}
	for _, sku := range []model.SKU{
		{ProductID: 2, Name: "缺货规格", DelivererKind: "stock", PriceCNY: 100, LimitPerOrder: 1, Active: true, Sort: -1},
		{ProductID: 2, Name: "仅支持未开通的支付方式", DelivererKind: "text", DelivererCfg: `{"text":"synthetic"}`, PriceStars: 10, Active: true, Sort: 1},
	} {
		if err := e.st.DB.Create(&sku).Error; err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("python3", "../../scripts/ui-smoke.py", e.ts.URL)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal("browser regression", err)
	}
}

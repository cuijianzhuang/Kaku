package web

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/model"
)

func TestDeletedProductAndSKUCannotReportSuccessfulSave(t *testing.T) {
	e := setup(t)
	c := e.login()
	cat := model.Category{Name: "test", Visible: true}
	if err := e.st.DB.Create(&cat).Error; err != nil {
		t.Fatal(err)
	}
	product := model.Product{CategoryID: cat.ID, Title: "original", Status: model.ProductOff}
	if err := e.st.DB.Create(&product).Error; err != nil {
		t.Fatal(err)
	}

	resp := e.post(c, "/manage/products", url.Values{
		"id": {"999999"}, "title": {"edited"}, "category_id": {strconv.FormatUint(uint64(cat.ID), 10)}, "status": {"off"},
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "商品不存在") {
		t.Fatalf("missing product claimed saved: %d %s", resp.StatusCode, body)
	}
	resp = e.post(c, "/manage/skus", url.Values{
		"id": {"999999"}, "product_id": {strconv.FormatUint(uint64(product.ID), 10)},
		"name": {"edited"}, "price_cny": {"10"}, "deliverer_kind": {"text"}, "deliverer_cfg": {`{"text":"delivery"}`},
	})
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "规格不存在") {
		t.Fatalf("missing SKU claimed saved: %d %s", resp.StatusCode, body)
	}
}

func TestOrderTicketRequiresPurchaserEmail(t *testing.T) {
	e := setup(t)
	o := e.stockOrder(1900)
	resp := e.post(e.client(), "/ticket", url.Values{
		"order_no": {o.OrderNo}, "email": {"someone-else@example.com"}, "password": {"1234"}, "content": {"Please help"},
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(body), "下单邮箱") {
		t.Fatalf("third-party email accepted for order ticket: %d %s", resp.StatusCode, body)
	}
	var count int64
	if err := e.st.DB.Model(&model.Ticket{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unexpected ticket count: %d %v", count, err)
	}
}

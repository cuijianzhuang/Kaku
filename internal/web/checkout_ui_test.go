package web

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/model"
	"golang.org/x/net/html"
)

type checkoutControls struct {
	selected, max string
	buyDisabled   bool
}

func readCheckout(t *testing.T, r *http.Response) checkoutControls {
	t.Helper()
	defer r.Body.Close()
	z := html.NewTokenizer(r.Body)
	var out checkoutControls
	for {
		kind := z.Next()
		if kind == html.ErrorToken {
			if err := z.Err(); err != io.EOF {
				t.Fatal(err)
			}
			return out
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		token := z.Token()
		attrs := map[string]string{}
		for _, a := range token.Attr {
			attrs[a.Key] = a.Val
		}
		if token.Data == "input" && attrs["name"] == "sku_id" {
			if _, ok := attrs["checked"]; ok {
				out.selected = attrs["value"]
			}
		}
		if token.Data == "input" && attrs["name"] == "qty" {
			out.max = attrs["max"]
		}
		if token.Data == "button" {
			if _, buy := attrs["data-buy-submit"]; buy {
				_, out.buyDisabled = attrs["disabled"]
			}
		}
	}
}

func TestCheckoutSelectsAvailableSKUAndPreservesFailedForm(t *testing.T) {
	e := setup(t)
	cat := model.Category{Name: "checkout", Visible: true}
	e.st.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "checkout", Status: model.ProductOn}
	e.st.DB.Create(&p)
	skus := []model.SKU{
		{ProductID: p.ID, Name: "sold out", DelivererKind: "stock", PriceCNY: 100, Active: true, Sort: -2},
		{ProductID: p.ID, Name: "unavailable payment", DelivererKind: "text", DelivererCfg: `{"text":"test"}`, PriceStars: 10, Active: true, Sort: -1},
		{ProductID: p.ID, Name: "available", DelivererKind: "text", DelivererCfg: `{"text":"test"}`, PriceCNY: 100, LimitPerOrder: 3, Active: true},
		{ProductID: p.ID, Name: "other", DelivererKind: "text", DelivererCfg: `{"text":"test"}`, PriceCNY: 200, LimitPerOrder: 5, Active: true},
	}
	for i := range skus {
		if err := e.st.DB.Create(&skus[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	get := func() checkoutControls {
		r, err := e.client().Get(e.ts.URL + "/p/" + strconv.Itoa(int(p.ID)))
		if err != nil {
			t.Fatal(err)
		}
		return readCheckout(t, r)
	}
	if got := get(); got.selected != strconv.Itoa(int(skus[2].ID)) || got.max != "3" || got.buyDisabled {
		t.Fatalf("unusable first SKU selected: %+v", got)
	}
	// Validation failure returns the buyer's usable selection and its quantity
	// limit, rather than switching back to the first SKU or exposing a password.
	r := e.post(e.client(), "/order", url.Values{"product_id": {strconv.Itoa(int(p.ID))}, "sku_id": {strconv.Itoa(int(skus[3].ID))}, "qty": {"2"}, "email": {"test@example.com"}, "password": {"x"}, "method": {"alipay"}})
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatal(r.StatusCode)
	}
	if got := readCheckout(t, r); got.selected != strconv.Itoa(int(skus[3].ID)) || got.max != "5" {
		t.Fatal("selection lost on validation failure", got)
	}
	if err := e.st.DB.Model(&model.SKU{}).Where("id IN ?", []uint{skus[2].ID, skus[3].ID}).Update("active", false).Error; err != nil {
		t.Fatal(err)
	}
	if got := get(); !got.buyDisabled {
		t.Fatal("purchase enabled without a usable payment method", got)
	}
}

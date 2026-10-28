package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/shop"
)

func TestGenericSKUForms(t *testing.T) {
	e := setup(t)
	c := e.login()
	cat := model.Category{Name: "c", Visible: true}
	e.st.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "资源", Status: model.ProductOn}
	e.st.DB.Create(&p)
	base := url.Values{"product_id": {itoa(int64(p.ID))}, "price_cny": {"9.9"}, "limit_per_order": {"5"}, "active": {"on"}}
	save := func(extra url.Values) (string, model.SKU) {
		f := url.Values{}
		for k, v := range base {
			f[k] = v
		}
		for k, v := range extra {
			f[k] = v
		}
		resp := e.post(c, "/manage/skus", f)
		l := location(resp)
		if resp.StatusCode == http.StatusUnprocessableEntity {
			b, _ := io.ReadAll(resp.Body)
			l = string(b)
		}
		var k model.SKU
		e.st.DB.Where("name = ?", f.Get("name")).Take(&k)
		return l, k
	}

	// fixed text with placeholders and wholesale rows
	_, k := save(url.Values{"name": {"文本"}, "deliverer_kind": {"text"}, "tx_text": {"链接 https://x\r\n订单 {order_no} ×{qty}"},
		"tx_per_unit": {"on"}, "ws_qty": {"5", "10", ""}, "ws_pct": {"90", "80", ""}})
	tc, err := deliver.ParseTextCfg(k.DelivererCfg)
	if err != nil || tc.Text != "链接 https://x\n订单 {order_no} ×{qty}" || !tc.PerUnit {
		t.Fatalf("text cfg %+v %v", tc, err)
	}
	if k.Wholesale != `[{"min_qty":5,"percent":90},{"min_qty":10,"percent":80}]` {
		t.Fatalf("wholesale %q", k.Wholesale)
	}
	items, err := (deliver.Text{}).Deliver(context.Background(), &model.Order{OrderNo: "K1", Qty: 2}, &k)
	if err != nil || len(items) != 2 || items[0].Content != "链接 https://x\n订单 K1 ×2" {
		t.Fatalf("text delivery %+v %v", items, err)
	}
	if l, _ := save(url.Values{"name": {"空文本"}, "deliverer_kind": {"text"}, "tx_text": {"  "}}); !strings.Contains(l, "请填写发货内容") {
		t.Fatalf("empty text: %s", l)
	}
	if l, _ := save(url.Values{"name": {"批发"}, "deliverer_kind": {"text"}, "tx_text": {"x"}, "ws_qty": {"1"}, "ws_pct": {"90"}}); !strings.Contains(l, "至少 2 件") {
		t.Fatalf("bad tier: %s", l)
	}

	// a failed save shows the error with what was typed
	f := url.Values{"product_id": {itoa(int64(p.ID))}, "name": {"保留测试"}, "deliverer_kind": {"text"}, "tx_text": {"保留的发货内容"},
		"ws_qty": {"3"}, "ws_pct": {"95"}}
	resp := e.post(c, "/manage/skus", f)
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(page), "至少填写一种币种的价格") ||
		!strings.Contains(string(page), "保留的发货内容") || !strings.Contains(string(page), `value="保留测试"`) || !strings.Contains(string(page), `value="95"`) {
		t.Fatalf("failed save lost the input: %d", resp.StatusCode)
	}

	// webhook
	if l, _ := save(url.Values{"name": {"接口"}, "deliverer_kind": {"webhook"}, "wh_url": {"https://api.example.com/d"}, "wh_secret": {"short"}}); !strings.Contains(l, "签名密钥至少 16 位") {
		t.Fatalf("short secret: %s", l)
	}
	_, k = save(url.Values{"name": {"接口"}, "deliverer_kind": {"webhook"}, "wh_url": {" https://api.example.com/d "}, "wh_secret": {strings.Repeat("s", 20)}})
	wc, _ := deliver.ParseWebhookCfg(k.DelivererCfg)
	if wc == nil || wc.URL != "https://api.example.com/d" || wc.Secret != strings.Repeat("s", 20) {
		t.Fatalf("webhook cfg %q", k.DelivererCfg)
	}

	// manual with a question for the buyer
	_, k = save(url.Values{"name": {"代充"}, "deliverer_kind": {"manual"}, "mn_ask": {"充值账号"}, "mn_note": {"30 分钟内处理"}})
	mc, _ := deliver.ParseManualCfg(k.DelivererCfg)
	if mc == nil || mc.Ask != "充值账号" || mc.Note != "30 分钟内处理" {
		t.Fatalf("manual cfg %q", k.DelivererCfg)
	}
	resp, _ = e.client().Get(e.ts.URL + "/p/" + itoa(int64(p.ID)))
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `data-ask="充值账号"`) || !strings.Contains(string(body), `name="ask"`) {
		t.Fatal("product page does not ask for the account")
	}
	in := shop.CreateInput{SKUID: k.ID, Qty: 1, Email: "b@example.com", QueryPassword: "1234", PayMethod: model.PayEPayAlipay,
		IP: "9.9.9.9", Channel: model.ChannelWeb, Remark: "尽快"}
	if _, err := e.shop.CreateOrder(context.Background(), in); err == nil || !strings.Contains(err.Error(), "请填写充值账号") {
		t.Fatalf("order without the account: %v", err)
	}
	in.Ask = "13800000000"
	o, err := e.shop.CreateOrder(context.Background(), in)
	if err != nil || o.Remark != "充值账号：13800000000；备注：尽快" {
		t.Fatalf("order remark %q %v", o.Remark, err)
	}
	e.shop.MarkPaid(context.Background(), shop.PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual, TradeNo: "x", Trusted: true})
	e.st.RunDueJobs(context.Background(), e.shop.JobHandlers())
	oc := e.client()
	e.post(oc, "/order/"+o.OrderNo, url.Values{"password": {"1234"}})
	resp, _ = oc.Get(e.ts.URL + "/order/" + o.OrderNo)
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "30 分钟内处理") {
		t.Fatal("waiting page does not show the delivery note")
	}
}

func TestQuickProductTemplates(t *testing.T) {
	e := setup(t)
	c := e.login()
	cat := model.Category{Name: "c", Visible: true}
	e.st.DB.Create(&cat)
	if l := location(e.post(c, "/manage/products/quick", url.Values{"template": {"card"}, "title": {"x"}, "category_id": {itoa(int64(cat.ID))}})); !strings.Contains(l, "请填写价格") {
		t.Fatalf("no price: %s", l)
	}
	for _, tc := range []struct{ tpl, kind, next string }{
		{"card", "stock", "/manage/cards?sku="}, {"download", "text", "/manage/skus/"}, {"recharge", "manual", "/manage/skus/"}, {"api", "webhook", "/manage/skus/"},
	} {
		l := location(e.post(c, "/manage/products/quick", url.Values{"template": {tc.tpl}, "title": {"T-" + tc.tpl},
			"category_id": {itoa(int64(cat.ID))}, "price_cny": {"9.9"}}))
		if !strings.HasPrefix(l, tc.next) {
			t.Fatalf("%s: redirect %s", tc.tpl, l)
		}
		var p model.Product
		e.st.DB.Preload("SKUs").Where("title = ?", "T-"+tc.tpl).Take(&p)
		if p.Status != model.ProductOff || len(p.SKUs) != 1 || p.SKUs[0].DelivererKind != tc.kind || p.SKUs[0].PriceCNY != 990 {
			t.Fatalf("%s: product %+v", tc.tpl, p)
		}
		if tc.kind == "manual" || tc.kind == "text" {
			d, _ := e.shop.Reg.Get(tc.kind)
			if err := d.Validate([]byte(p.SKUs[0].DelivererCfg)); err != nil {
				t.Fatalf("%s: template config invalid: %v", tc.tpl, err)
			}
		}
	}
	resp, _ := c.Get(e.ts.URL + "/manage/products/quick")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "人工代充") {
		t.Fatal("template page")
	}
}

package web

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/shop"
)

func TestNetdiskAndArticleDelivery(t *testing.T) {
	e := setup(t)
	c := e.login()
	cat := model.Category{Name: "资源", Visible: true}
	e.st.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "合集", Status: model.ProductOn}
	e.st.DB.Create(&p)
	save := func(name string, extra url.Values) model.SKU {
		f := url.Values{"product_id": {itoa(int64(p.ID))}, "name": {name}, "price_cny": {"5"}, "limit_per_order": {"5"}, "active": {"on"}, "deliverer_kind": {"text"}}
		for k, v := range extra {
			f[k] = v
		}
		e.post(c, "/manage/skus", f)
		var k model.SKU
		e.st.DB.Where("name = ?", name).Take(&k)
		return k
	}
	// pasting the whole share text fills the link and the code
	nd := save("网盘", url.Values{"tx_format": {"netdisk"}, "tx_url": {"链接: https://pan.baidu.com/s/1AbC 提取码: x7k2 复制这段内容后打开百度网盘App"},
		"tx_code": {""}, "tx_unzip": {"zip!"}, "tx_text": {"失效请提交工单"}})
	cfg, _ := deliver.ParseTextCfg(nd.DelivererCfg)
	if cfg.Format != deliver.FormatNetdisk || cfg.URL != "https://pan.baidu.com/s/1AbC" || cfg.Code != "x7k2" || cfg.Unzip != "zip!" {
		t.Fatalf("netdisk cfg %+v", cfg)
	}
	art := save("文章", url.Values{"tx_format": {"article"}, "tx_text": {"# 第一章\n\n**重点**：内容"}})

	buy := func(sku model.SKU) string {
		o, err := e.shop.CreateOrder(context.Background(), shop.CreateInput{SKUID: sku.ID, Qty: 1, Email: "b@example.com", QueryPassword: "1234",
			PayMethod: model.PayEPayAlipay, IP: "9.9.9.9", Channel: model.ChannelWeb})
		if err != nil {
			t.Fatal(err)
		}
		e.shop.MarkPaid(context.Background(), shop.PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual, TradeNo: o.OrderNo, Trusted: true})
		e.st.RunDueJobs(context.Background(), e.shop.JobHandlers())
		oc := e.client()
		e.post(oc, "/order/"+o.OrderNo, url.Values{"password": {"1234"}})
		resp, _ := oc.Get(e.ts.URL + "/order/" + o.OrderNo)
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	page := buy(nd)
	for _, want := range []string{"百度网盘", "打开网盘", `href="https://pan.baidu.com/s/1AbC"`, "x7k2", "zip!", "复制链接和提取码", "失效请提交工单"} {
		if !strings.Contains(page, want) {
			t.Fatalf("netdisk order page lacks %q", want)
		}
	}
	page = buy(art)
	if !strings.Contains(page, "<strong>重点</strong>") || strings.Contains(page, "**重点**") || !strings.Contains(page, "rich article") {
		t.Fatal("article not rendered on the order page")
	}
	// the email gets plain text
	var o model.Order
	e.st.DB.Where("sku_id = ?", nd.ID).Take(&o)
	items, _ := e.shop.Items(o.ID)
	if len(items) != 1 || !strings.HasPrefix(items[0].Content, "百度网盘链接：https://pan.baidu.com/s/1AbC\n提取码：x7k2") {
		t.Fatalf("plain %+v", items)
	}
}

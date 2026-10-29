package deliver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/model"
)

func TestSplitShare(t *testing.T) {
	for _, c := range []struct{ in, code, link, wantCode string }{
		{"链接: https://pan.baidu.com/s/1AbC-x 提取码: x7k2 复制这段内容后打开百度网盘手机App", "", "https://pan.baidu.com/s/1AbC-x", "x7k2"},
		{"https://pan.baidu.com/s/1AbC?pwd=ab12", "", "https://pan.baidu.com/s/1AbC?pwd=ab12", "ab12"},
		{"「阿里云盘」分享 https://www.alipan.com/s/xyz 点击链接保存", "", "https://www.alipan.com/s/xyz", ""},
		{"https://pan.quark.cn/s/abc", "9999", "https://pan.quark.cn/s/abc", "9999"},
	} {
		link, code := SplitShare(c.in, c.code)
		if link != c.link || code != c.wantCode {
			t.Fatalf("%q → %q %q", c.in, link, code)
		}
	}
	if NetdiskName("https://pan.baidu.com/s/1") != "百度网盘" || NetdiskName("https://www.alipan.com/s/x") != "阿里云盘" ||
		NetdiskName("https://wwi.lanzoup.com/abc") != "蓝奏云" || NetdiskName("https://example.com/x") != "" {
		t.Fatal("provider names")
	}
}

func TestTextFormats(t *testing.T) {
	cfg := func(c TextCfg) string { b, _ := json.Marshal(c); return string(b) }
	for _, c := range []struct {
		cfg TextCfg
		err bool
	}{
		{TextCfg{Format: FormatNetdisk, URL: "https://pan.baidu.com/s/1", Code: "abcd"}, false},
		{TextCfg{Format: FormatNetdisk, URL: "pan.baidu.com/s/1"}, true},
		{TextCfg{Format: FormatNetdisk, URL: Placeholder + "链接】"}, true},
		{TextCfg{Format: FormatLink, URL: "https://t.me/+abc"}, false},
		{TextCfg{Format: FormatLink, URL: "javascript:alert(1)"}, true},
		{TextCfg{Format: FormatArticle, Text: "# 标题"}, false},
		{TextCfg{Format: FormatArticle}, true},
		{TextCfg{Text: "纯文本"}, false},
		{TextCfg{Format: "nope", Text: "x"}, true},
	} {
		if err := (Text{}).Validate(json.RawMessage(cfg(c.cfg))); (err != nil) != c.err {
			t.Fatalf("%+v: %v", c.cfg, err)
		}
	}
	sku := &model.SKU{DelivererCfg: cfg(TextCfg{Format: FormatNetdisk, URL: "https://pan.baidu.com/s/1", Code: "abcd", Unzip: "zip!", Text: "订单 {order_no}"})}
	items, err := (Text{}).Deliver(context.Background(), &model.Order{OrderNo: "K9", Qty: 1}, sku)
	if err != nil || len(items) != 1 || items[0].Source != FormatNetdisk {
		t.Fatalf("deliver %+v %v", items, err)
	}
	r, ok := ParseRich(items[0].Source, items[0].Content)
	if !ok || r.Body != "订单 K9" || r.Provider() != "百度网盘" {
		t.Fatalf("rich %+v", r)
	}
	if p := r.Plain(); p != "百度网盘链接：https://pan.baidu.com/s/1\n提取码：abcd\n解压密码：zip!\n\n订单 K9" {
		t.Fatalf("plain %q", p)
	}
	if r.ShareText() != "链接：https://pan.baidu.com/s/1 提取码：abcd" {
		t.Fatal(r.ShareText())
	}
}

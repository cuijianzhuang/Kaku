package notify

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestBuildMessage(t *testing.T) {
	body := "您好，\n\n订单号：K123\n商品：Steam <卡> ×1\n\n发货内容：\n1. CARD-0001\n2. CARD-0002\n\n订单页面：https://shop.example.com/order/K123（凭查询密码查看）"
	raw := buildMessage("Kaku <a@example.com>", "b@example.com", "【Kaku】订单 K123 已发货", "Kaku", body, time.Unix(1, 0))
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mt != "multipart/alternative" {
		t.Fatalf("type %s", mt)
	}
	r := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p) // the reader decodes nothing: decode base64 ourselves
		dec, err := decodeB64(string(b))
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p.Header.Get("Content-Type")+"\n"+dec)
	}
	if len(parts) != 2 || !strings.Contains(parts[0], body) {
		t.Fatalf("text part: %v", parts)
	}
	h := parts[1]
	for _, want := range []string{"text/html", "Steam &lt;卡&gt;", `<a href="https://shop.example.com/order/K123"`, "CARD-0002", "订单 K123 已发货"} {
		if !strings.Contains(h, want) {
			t.Fatalf("html part lacks %q", want)
		}
	}
	if strings.Contains(h, "<卡>") {
		t.Fatal("html part not escaped")
	}
}

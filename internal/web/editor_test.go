package web

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
)

func TestMarkdownImagesListsQuotes(t *testing.T) {
	out := string(renderMarkdown("![截图](/uploads/abc.png)\n\n1. 第一步\n2. 第二步\n\n> 注意事项\n> 第二行\n\n![x](javascript:alert(1)) ![y](http://plain.example/a.png)\n![\"onerror=alert(1)](https://ok.example/a.png)"))
	for _, want := range []string{
		`<img src="/uploads/abc.png" alt="截图" loading="lazy">`,
		`<ol><li>第一步</li><li>第二步</li></ol>`,
		`<blockquote>注意事项<br>第二行</blockquote>`,
		`alt="&#34;onerror=alert(1)"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Contains(out, `src="javascript`) || strings.Contains(out, `src="http://`) {
		t.Errorf("unsafe image source rendered:\n%s", out)
	}
}

func TestEditorUploadAndPreview(t *testing.T) {
	e := setup(t)
	c := e.login()
	resp := e.upload(c, "/manage/uploads/image", nil, "file", tinyPNG)
	var j map[string]string
	json.NewDecoder(resp.Body).Decode(&j)
	if resp.StatusCode != 200 || !strings.HasPrefix(j["url"], "/uploads/") {
		t.Fatalf("upload: %d %v", resp.StatusCode, j)
	}
	if r, _ := c.Get(e.ts.URL + j["url"]); r.StatusCode != 200 {
		t.Fatalf("uploaded image not served: %d", r.StatusCode)
	}
	bad := e.upload(c, "/manage/uploads/image", nil, "file", []byte("<svg onload=alert(1)>"))
	if bad.StatusCode != 400 {
		t.Fatalf("non-image accepted: %d", bad.StatusCode)
	}
	if anon := e.upload(e.client(), "/manage/uploads/image", nil, "file", tinyPNG); anon.StatusCode == 200 {
		t.Fatal("upload without login")
	}

	pv := e.post(c, "/manage/markdown/preview", url.Values{"text": {"## 标题\n**粗** <script>x</script>"}})
	b, _ := io.ReadAll(pv.Body)
	if !strings.Contains(string(b), "<h4>标题</h4>") || !strings.Contains(string(b), "<strong>粗</strong>") || strings.Contains(string(b), "<script>") {
		t.Fatalf("preview: %s", b)
	}

	page, _ := c.Get(e.ts.URL + "/manage/products/new")
	pb, _ := io.ReadAll(page.Body)
	for _, want := range []string{"data-cover-file", "data-md-input", `data-md-cmd="img"`} {
		if !strings.Contains(string(pb), want) {
			t.Fatalf("product page lacks %s", want)
		}
	}
}

func TestBuyerFormsKeepInput(t *testing.T) {
	e := setup(t)
	o := e.stockOrder(500) // creates an on-sale product with a stock SKU
	_ = o
	var sku model.SKU
	e.st.DB.First(&sku)
	c := e.client()
	f := url.Values{"product_id": {itoa(int64(sku.ProductID))}, "sku_id": {itoa(int64(sku.ID))}, "qty": {"1"}, "email": {"keep@example.com"},
		"password": {"1234"}, "method": {"epay_alipay"}, "coupon": {"NOPE"}}
	resp := e.post(c, "/order", f)
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 422 || !strings.Contains(string(b), `value="keep@example.com"`) || !strings.Contains(string(b), `value="NOPE"`) {
		t.Fatalf("order error lost the input: %d", resp.StatusCode)
	}
	// sold out: the buy button says so
	e.st.DB.Model(&model.Card{}).Where("1 = 1").Update("status", model.CardVoid)
	deliver.RefreshStock(e.st.DB, sku.ID)
	page, _ := c.Get(e.ts.URL + "/p/" + itoa(int64(sku.ProductID)))
	pb, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(pb), "暂时缺货") {
		t.Fatal("sold-out product still offers 立即购买")
	}
	q := e.post(c, "/query", url.Values{"email": {"keep@example.com"}, "password": {"wrong"}})
	qb, _ := io.ReadAll(q.Body)
	if !strings.Contains(string(qb), `value="keep@example.com"`) || !strings.Contains(string(qb), "忘记查询密码") {
		t.Fatal("query page lost the email or lacks help")
	}
}

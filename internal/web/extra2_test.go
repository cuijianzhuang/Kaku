package web

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/model"
)

const adminPW = "correct horse battery"

func TestAdminAccounts(t *testing.T) {
	e := setup(t)
	c := e.login()
	// the current password is required
	e.post(c, "/manage/admins", url.Values{"username": {"alice"}, "password": {"alice-password-1"}, "confirm_password": {"wrong"}})
	var n int64
	e.st.DB.Model(&model.Admin{}).Count(&n)
	if n != 1 {
		t.Fatal("admin created without the current password")
	}
	e.post(c, "/manage/admins", url.Values{"username": {"alice"}, "password": {"alice-password-1"}, "confirm_password": {adminPW}})
	e.post(c, "/manage/admins", url.Values{"username": {"alice"}, "password": {"another-password"}, "confirm_password": {adminPW}})
	e.st.DB.Model(&model.Admin{}).Count(&n)
	if n != 2 {
		t.Fatalf("admins = %d", n)
	}
	// alice can sign in, and shows a last login
	alice := e.client()
	if resp, _ := alice.PostForm(e.ts.URL+"/manage/login", url.Values{"username": {"alice"}, "password": {"alice-password-1"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("alice login: %d", resp.StatusCode)
	}
	var a model.Admin
	e.st.DB.Where("username = ?", "alice").Take(&a)
	if a.LastLogin == nil {
		t.Fatal("last login not recorded")
	}
	// resetting her password signs her out
	e.post(c, "/manage/admins/"+itoa(int64(a.ID))+"/password", url.Values{"password": {"alice-password-2"}, "confirm_password": {adminPW}})
	if resp, _ := alice.Get(e.ts.URL + "/manage/orders"); resp.StatusCode == 200 {
		t.Fatal("old session still valid after a password reset")
	}
	// nobody deletes themselves; the last admin stays
	var me model.Admin
	e.st.DB.Where("username = ?", "admin").Take(&me)
	e.post(c, "/manage/admins/"+itoa(int64(me.ID))+"/delete", url.Values{"confirm_password": {adminPW}})
	e.post(c, "/manage/admins/"+itoa(int64(a.ID))+"/delete", url.Values{"confirm_password": {adminPW}})
	e.st.DB.Model(&model.Admin{}).Count(&n)
	if n != 1 || e.st.DB.Where("username = ?", "admin").Take(&me).Error != nil {
		t.Fatalf("after deletes: %d admins", n)
	}
}

func (e *env) upload(c *http.Client, path string, fields map[string]string, file string, data []byte) *http.Response {
	e.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	fw, _ := w.CreateFormFile(file, "x.png")
	fw.Write(data)
	w.Close()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", e.ts.URL)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

// a 1×1 PNG
var tinyPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func TestCoverUploadAndShareMeta(t *testing.T) {
	e := setup(t)
	c := e.login()
	cat := model.Category{Name: "c", Visible: true}
	e.st.DB.Create(&cat)
	fields := map[string]string{"title": "Steam 卡", "category_id": itoa(int64(cat.ID)), "status": "on", "description": "# 说明\n**秒发** [官网](https://example.com)"}

	// a script disguised as a PNG is refused
	e.upload(c, "/manage/products", fields, "cover_file", []byte("<svg onload=alert(1)>"))
	var n int64
	e.st.DB.Model(&model.Product{}).Count(&n)
	if n != 0 {
		t.Fatal("product saved with a non-image upload")
	}
	e.upload(c, "/manage/products", fields, "cover_file", tinyPNG)
	var p model.Product
	if e.st.DB.Take(&p).Error != nil || !strings.HasPrefix(p.Cover, "/uploads/") || !strings.HasSuffix(p.Cover, ".png") {
		t.Fatalf("cover = %q", p.Cover)
	}
	resp, _ := http.Get(e.ts.URL + p.Cover)
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || !bytes.Equal(b, tinyPNG) || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("serve upload: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := http.Get(e.ts.URL + "/uploads/..%2fk.db"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("path escape: %d", resp.StatusCode)
	}

	// share preview and sitemap
	resp, _ = http.Get(e.ts.URL + "/p/" + itoa(int64(p.ID)))
	b, _ := io.ReadAll(resp.Body)
	page := string(b)
	for _, want := range []string{`property="og:title" content="Steam 卡`, `content="说明 秒发 官网"`, `property="og:image" content="` + e.ts.URL + p.Cover} {
		if !strings.Contains(page, want) {
			t.Fatalf("product page lacks %s", want)
		}
	}
	e.st.DB.Create(&model.SKU{ProductID: p.ID, Name: "月卡", PriceCNY: 1000, DelivererKind: "text", DelivererCfg: `{"text":"x"}`, LimitPerOrder: 1, Active: true})
	resp, _ = http.Get(e.ts.URL + "/sitemap.xml")
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "/p/"+itoa(int64(p.ID))+"</loc>") {
		t.Fatalf("sitemap: %s", b)
	}
	resp, _ = http.Get(e.ts.URL + "/robots.txt")
	b, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "Disallow: /order/") || strings.Contains(string(b), "/manage") {
		t.Fatalf("robots: %s", b)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestStaticCachingAndCompression(t *testing.T) {
	e := setup(t)
	resp, _ := http.Get(e.ts.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("no CSP on pages")
	}
	i := strings.Index(string(b), "/static/style.css?v=")
	if i < 0 {
		t.Fatal("stylesheet link has no version")
	}
	link := string(b)[i : i+len("/static/style.css?v=")+12]
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+link, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, _ = http.DefaultTransport.RoundTrip(req) // no transparent decompression
	if resp.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset headers: %v", resp.Header)
	}
	req, _ = http.NewRequest(http.MethodGet, e.ts.URL+"/static/style.css", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusNotModified {
		t.Fatalf("revalidation: %d", resp.StatusCode)
	}
	if resp, _ := http.Get(e.ts.URL + "/static/../web_test.go"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("path escape: %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, e.ts.URL+"/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	if resp, _ := http.DefaultTransport.RoundTrip(req); resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("pages are not compressed")
	}
}

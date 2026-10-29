package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/notify"
	"github.com/cuijianzhuang/kaku/internal/payment"
	"github.com/cuijianzhuang/kaku/internal/polaris"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/store"
)

type env struct {
	t    *testing.T
	shop *shop.Service
	st   *store.Store
	ts   *httptest.Server
	srv  *Server
	// approve plays the OAuth provider's consent page (oauth_test.go)
	approve func(authorizeURL string) string
	bot     *fakeBot
	// fake EPay gateway state
	gwPaid  atomic.Bool
	gwMoney atomic.Value
	gwRaw   atomic.Value // when set, the whole api.php response
}

type fakeBot struct{ n atomic.Int32 }

func (b *fakeBot) HandleUpdate([]byte) { b.n.Add(1) }

const epayKey = "merchant-key"

func setup(t *testing.T) *env {
	t.Helper()
	box, _ := secure.NewBox(secure.RandomBytes(32))
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "k.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := shop.New(st, &notify.Notifier{Store: st, Log: log, Send: func(string, string, string) error { return nil }}, log)
	e := &env{t: t, shop: svc, st: st, bot: &fakeBot{}}
	e.gwMoney.Store("0.00")
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api.php" || r.URL.Query().Get("key") != epayKey {
			w.WriteHeader(404)
			return
		}
		if raw, _ := e.gwRaw.Load().(string); raw != "" {
			fmt.Fprint(w, raw)
			return
		}
		status := "0"
		if e.gwPaid.Load() {
			status = "1"
		}
		fmt.Fprintf(w, `{"code":1,"status":%s,"money":"%s","trade_no":"GW123","type":"alipay"}`, status, e.gwMoney.Load())
	}))
	t.Cleanup(gw.Close)
	svc.EPayClient = &payment.EPay{BaseURL: gw.URL, PID: "1000", Key: epayKey, AllowHTTP: true}
	set := st.Settings()
	set.EPayURL, set.EPayPID, set.EPayKey = "https://pay.example.com", "1000", epayKey
	set.PolarisSecret = strings.Repeat("p", 32)
	set.TGWebhookSecret = "hooksecret123"
	st.SaveSettings(set)
	st.DB.Create(&model.Admin{Username: "admin", PasswordHash: secure.HashPassword("correct horse battery"), Epoch: 1})
	srv := New(Config{AdminPath: "/manage", UploadDir: filepath.Join(t.TempDir(), "uploads")}, svc, e.bot, log)
	e.srv = srv
	e.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *env) stockOrder(amountFen int64) *model.Order {
	e.t.Helper()
	cat := model.Category{Name: "c", Visible: true}
	e.st.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "P", Status: model.ProductOn}
	e.st.DB.Create(&p)
	sku := model.SKU{ProductID: p.ID, Name: "S", PriceCNY: amountFen, DelivererKind: "stock", LimitPerOrder: 5, Active: true}
	e.st.DB.Create(&sku)
	deliver.ImportCards(&deliver.Env{Store: e.st}, sku.ID, "CARD-1\nCARD-2")
	o, err := e.shop.CreateOrder(context.Background(), shop.CreateInput{SKUID: sku.ID, Qty: 1, Email: "b@example.com",
		QueryPassword: "1234", PayMethod: model.PayEPayAlipay, IP: "9.9.9.9"})
	if err != nil {
		e.t.Fatal(err)
	}
	return o
}

func (e *env) notify(v url.Values) (int, string) {
	resp, err := http.PostForm(e.ts.URL+"/notify/epay", v)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func signed(v url.Values) url.Values {
	v.Set("sign", payment.Sign(v, epayKey))
	v.Set("sign_type", "MD5")
	return v
}

func TestEPayNotify(t *testing.T) {
	e := setup(t)
	o := e.stockOrder(1990)
	base := func() url.Values {
		return url.Values{"pid": {"1000"}, "trade_no": {"GW123"}, "out_trade_no": {o.OrderNo}, "type": {"alipay"},
			"name": {"P"}, "money": {"19.90"}, "trade_status": {"TRADE_SUCCESS"}}
	}
	// Forged signature.
	v := signed(base())
	v.Set("sign", strings.Repeat("0", 32))
	if code, _ := e.notify(v); code == 200 {
		t.Fatal("forged signature accepted")
	}
	// Valid signature but the gateway does not confirm the payment.
	if code, _ := e.notify(signed(base())); code == 200 {
		t.Fatal("unconfirmed payment accepted")
	}
	// Wrong amount, even if the gateway reports that amount.
	e.gwPaid.Store(true)
	e.gwMoney.Store("0.01")
	low := base()
	low.Set("money", "0.01")
	if code, _ := e.notify(signed(low)); code == 200 {
		t.Fatal("wrong amount accepted")
	}
	// Correct.
	e.gwMoney.Store("19.90")
	for range 2 {
		if code, body := e.notify(signed(base())); code != 200 || body != "success" {
			t.Fatalf("notify %d %s", code, body)
		}
	}
	got, _ := e.shop.OrderByNo(o.OrderNo)
	if got.Status != model.OrderPaid {
		t.Fatalf("status %s", got.Status)
	}
	var n int64
	e.st.DB.Model(&model.Payment{}).Count(&n)
	if n != 1 {
		t.Fatalf("payments %d", n)
	}
}

func (e *env) hook(body string, secret string, ts time.Time) int {
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/hooks/polaris", strings.NewReader(body))
	tsText := strconv.FormatInt(ts.Unix(), 10)
	req.Header.Set("X-Kaku-Timestamp", tsText)
	req.Header.Set("X-Kaku-Signature", polaris.Signature(secret, tsText, http.MethodPost, "/hooks/polaris", []byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestPolarisHook(t *testing.T) {
	e := setup(t)
	e.st.DB.Create(&model.LicenseBinding{OrderID: 1, LicenseID: "lic_abc", Status: model.LicenseIssued})
	body := `{"event":"activated","license_id":"lic_abc","instance":"inst_12345678"}`
	if code := e.hook(body, "wrong-secret-wrong-secret-wrong-secret", time.Now()); code != 401 {
		t.Fatalf("bad signature: %d", code)
	}
	if code := e.hook(body, strings.Repeat("p", 32), time.Now().Add(-10*time.Minute)); code != 401 {
		t.Fatalf("stale timestamp: %d", code)
	}
	if code := e.hook(body, strings.Repeat("p", 32), time.Now()); code != 200 {
		t.Fatalf("valid hook: %d", code)
	}
	var b model.LicenseBinding
	e.st.DB.Where("license_id = ?", "lic_abc").Take(&b)
	if b.Status != model.LicenseActivated || b.Instance != "inst_12345678" {
		t.Fatalf("binding %+v", b)
	}
}

func TestTelegramWebhookSecret(t *testing.T) {
	e := setup(t)
	post := func(path, header string) int {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, bytes.NewReader([]byte(`{}`)))
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if post("/tg/webhook/hooksecret123", "nope") != 404 || post("/tg/webhook/nope", "hooksecret123") != 404 {
		t.Fatal("webhook accepted without both secrets")
	}
	if post("/tg/webhook/hooksecret123", "hooksecret123") != 200 || e.bot.n.Load() != 1 {
		t.Fatal("valid webhook rejected")
	}
}

func (e *env) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestAdminAuthAndCSRF(t *testing.T) {
	e := setup(t)
	c := e.client()
	resp, _ := c.Get(e.ts.URL + "/manage/orders")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/manage/login") {
		t.Fatalf("unauthenticated: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = c.PostForm(e.ts.URL+"/manage/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, _ = c.PostForm(e.ts.URL+"/manage/login", url.Values{"username": {"admin"}, "password": {"correct horse battery"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	resp, _ = c.Get(e.ts.URL + "/manage/orders")
	if resp.StatusCode != 200 {
		t.Fatalf("orders: %d", resp.StatusCode)
	}
	// A POST without a same-site Origin is refused.
	resp, _ = c.PostForm(e.ts.URL+"/manage/categories", url.Values{"name": {"x"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("csrf: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/manage/categories", strings.NewReader("name=x&visible=on"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, _ = c.Do(req)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin post: %d", resp.StatusCode)
	}
	var n int64
	e.st.DB.Model(&model.Category{}).Count(&n)
	if n != 1 {
		t.Fatalf("categories %d", n)
	}
}

func TestOrderPageNeedsAccess(t *testing.T) {
	e := setup(t)
	o := e.stockOrder(500)
	e.shop.MarkPaid(context.Background(), shop.PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual, TradeNo: "x", Trusted: true})
	e.st.RunDueJobs(context.Background(), e.shop.JobHandlers())
	c := e.client()
	resp, _ := c.Get(e.ts.URL + "/order/" + o.OrderNo)
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "CARD-1") {
		t.Fatal("secret shown without access")
	}
	resp, _ = c.PostForm(e.ts.URL+"/order/"+o.OrderNo, url.Values{"password": {"1234"}})
	resp, _ = c.Get(e.ts.URL + "/order/" + o.OrderNo)
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "CARD-1") {
		t.Fatal("secret not shown after password")
	}
	resp, _ = http.Get(e.ts.URL + "/order/" + o.OrderNo + "/status")
	var st map[string]string
	json.NewDecoder(resp.Body).Decode(&st)
	if st["status"] != model.OrderDelivered {
		t.Fatalf("status %v", st)
	}
}

func TestMarkdownEscapes(t *testing.T) {
	out := string(renderMarkdown("# T\n<script>alert(1)</script>\n[x](javascript:alert(1)) [ok](https://a.example/?q=1&b=2)"))
	if strings.Contains(out, "<script>") || strings.Contains(out, `href="javascript`) || !strings.Contains(out, `href="https://a.example/?q=1&amp;b=2"`) {
		t.Fatalf("markdown output: %s", out)
	}
}

func (e *env) login() *http.Client {
	e.t.Helper()
	c := e.client()
	resp, err := c.PostForm(e.ts.URL+"/manage/login", url.Values{"username": {"admin"}, "password": {"correct horse battery"}})
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("login: %v", err)
	}
	return c
}

func (e *env) post(c *http.Client, path string, form url.Values) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", e.ts.URL)
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func TestExportStatsAndBackups(t *testing.T) {
	e := setup(t)
	o := e.stockOrder(500)
	e.st.DB.Model(o).Update("remark", "=HYPERLINK(\"http://evil\")")
	e.shop.MarkPaid(context.Background(), shop.PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual, TradeNo: "x", Trusted: true})
	e.st.RunDueJobs(context.Background(), e.shop.JobHandlers())

	if resp, _ := e.client().Get(e.ts.URL + "/manage/orders.csv"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("export without login: %d", resp.StatusCode)
	}
	c := e.login()
	resp, _ := c.Get(e.ts.URL + "/manage/orders.csv")
	body, _ := io.ReadAll(resp.Body)
	csvText := string(body)
	if !strings.HasPrefix(csvText, "\uFEFF订单号") || !strings.Contains(csvText, o.OrderNo) || !strings.Contains(csvText, "'=HYPERLINK") {
		t.Fatalf("csv: %s", csvText)
	}
	if strings.Contains(csvText, "CARD-1") {
		t.Fatal("csv leaks delivered content")
	}
	for _, p := range []string{"/manage/stats", "/manage/stats?range=year&currency=CNY", "/manage/backups"} {
		resp, _ := c.Get(e.ts.URL + p)
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(b), "<h1>") {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
	}

	e.shop.BackupDir = filepath.Join(t.TempDir(), "backups")
	resp = e.post(c, "/manage/backups/run", url.Values{})
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "msg=") {
		t.Fatalf("backup run: %s", loc)
	}
	files, _ := e.shop.Backups()
	if len(files) != 1 {
		t.Fatalf("backups %v", files)
	}
	resp = e.post(c, "/manage/backups/download", url.Values{"name": {files[0].Name}, "confirm_password": {"wrong"}})
	if resp.Header.Get("Content-Disposition") != "" {
		t.Fatal("backup downloaded without the password")
	}
	resp = e.post(c, "/manage/backups/download", url.Values{"name": {files[0].Name}, "confirm_password": {"correct horse battery"}})
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(resp.Header.Get("Content-Disposition"), files[0].Name) || len(b) < 100 || b[0] != 0x1f {
		t.Fatalf("download: %v %d", resp.Header, len(b))
	}
}

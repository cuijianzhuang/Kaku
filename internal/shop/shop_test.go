package shop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/notify"
	"github.com/cuijianzhuang/kaku/internal/polaris"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/store"
)

type mailbox struct {
	mu   sync.Mutex
	sent []string
}

func (m *mailbox) send(to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, to+"|"+subject+"|"+body)
	return nil
}

func setup(t *testing.T) (*Service, *mailbox) {
	t.Helper()
	box, _ := secure.NewBox(secure.RandomBytes(32))
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "k.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	set := st.Settings()
	set.EPayURL, set.EPayPID, set.EPayKey = "https://pay.example.com", "1000", "secretkey"
	set.PublicURL = "https://shop.example.com"
	if err := st.SaveSettings(set); err != nil {
		t.Fatal(err)
	}
	mb := &mailbox{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(st, &notify.Notifier{Store: st, Log: log, Send: mb.send}, log)
	return s, mb
}

func mkSKU(t *testing.T, s *Service, kind, cfg string, price int64) *model.SKU {
	t.Helper()
	cat := model.Category{Name: "c", Visible: true}
	s.Store.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "商品", Status: model.ProductOn}
	s.Store.DB.Create(&p)
	sku := model.SKU{ProductID: p.ID, Name: "月卡", PriceCNY: price, DelivererKind: kind, DelivererCfg: cfg, LimitPerOrder: 10, Active: true}
	if err := s.Store.DB.Create(&sku).Error; err != nil {
		t.Fatal(err)
	}
	return &sku
}

func order(t *testing.T, s *Service, sku *model.SKU, qty int, coupon string) *model.Order {
	t.Helper()
	o, err := s.CreateOrder(context.Background(), CreateInput{SKUID: sku.ID, Qty: qty, Email: "Buyer@Example.com",
		QueryPassword: "1234", PayMethod: model.PayEPayAlipay, IP: "1.2.3.4", Coupon: coupon})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func runJobs(s *Service) { s.Store.RunDueJobs(context.Background(), s.JobHandlers()) }

func TestStockOrderFlow(t *testing.T) {
	s, mb := setup(t)
	sku := mkSKU(t, s, "stock", "", 1000)
	env := &deliver.Env{Store: s.Store}
	r, err := deliver.ImportCards(env, sku.ID, "AAA\nBBB\n\nAAA\nCCC\n")
	if err != nil || r.Added != 3 || r.Duplicate != 1 {
		t.Fatalf("import %+v %v", r, err)
	}
	r, _ = deliver.ImportCards(env, sku.ID, "CCC\nDDD")
	if r.Added != 1 || r.Duplicate != 1 {
		t.Fatalf("second import %+v", r)
	}

	o := order(t, s, sku, 2, "")
	if o.Email != "buyer@example.com" || o.Amount != 2000 || o.Status != model.OrderPending {
		t.Fatalf("order %+v", o)
	}
	s.Store.DB.First(sku, sku.ID)
	if sku.StockCache != 2 {
		t.Fatalf("stock after reserve = %d", sku.StockCache)
	}
	ctx := context.Background()
	if _, err := s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: "alipay", TradeNo: "T1", Amount: 1999, Currency: model.CNY}); !errors.Is(err, ErrAmountMismatch) {
		t.Fatalf("wrong amount accepted: %v", err)
	}
	for range 2 { // duplicate callback
		if _, err := s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: "alipay", TradeNo: "T1", Amount: 2000, Currency: model.CNY}); err != nil {
			t.Fatal(err)
		}
	}
	var pays int64
	s.Store.DB.Model(&model.Payment{}).Count(&pays)
	if pays != 1 {
		t.Fatalf("payments = %d", pays)
	}
	runJobs(s)
	runJobs(s)
	got, _ := s.OrderByNo(o.OrderNo)
	if got.Status != model.OrderDelivered {
		t.Fatalf("status %s (%s)", got.Status, got.LastError)
	}
	items, _ := s.Items(got.ID)
	if len(items) != 2 || items[0].Content != "AAA" || items[1].Content != "BBB" {
		t.Fatalf("items %+v", items)
	}
	if len(mb.sent) != 1 || !strings.Contains(mb.sent[0], "AAA") || !strings.HasPrefix(mb.sent[0], "buyer@example.com") {
		t.Fatalf("mail %v", mb.sent)
	}
	// Card secrets are not stored in clear text.
	var c model.Card
	s.Store.DB.First(&c)
	if strings.Contains(c.SecretEnc, "AAA") {
		t.Fatal("card stored in clear")
	}
	found, _ := s.FindOrders("", "BUYER@example.com", "1234")
	if len(found) != 1 {
		t.Fatalf("query found %d", len(found))
	}
	if found, _ := s.FindOrders(o.OrderNo, "", "wrong"); len(found) != 0 {
		t.Fatal("wrong password matched")
	}
}

func TestOutOfStockAndExpiry(t *testing.T) {
	s, _ := setup(t)
	sku := mkSKU(t, s, "stock", "", 500)
	deliver.ImportCards(&deliver.Env{Store: s.Store}, sku.ID, "X1")
	o := order(t, s, sku, 1, "")
	if _, err := s.CreateOrder(context.Background(), CreateInput{SKUID: sku.ID, Qty: 1, Email: "b@example.com", QueryPassword: "1234", PayMethod: model.PayEPayAlipay}); err == nil || !strings.Contains(err.Error(), "库存不足") {
		t.Fatalf("oversold: %v", err)
	}
	base := s.Now()
	s.Now = func() time.Time { return base.Add(time.Hour) }
	s.ExpireOrders(context.Background())
	got, _ := s.OrderByNo(o.OrderNo)
	if got.Status != model.OrderExpired {
		t.Fatalf("status %s", got.Status)
	}
	s.Store.DB.First(sku, sku.ID)
	if sku.StockCache != 1 {
		t.Fatalf("stock not released: %d", sku.StockCache)
	}
	// A late payment still delivers while a card is left.
	s.Now = time.Now
	if _, err := s.MarkPaid(context.Background(), PaidInput{OrderNo: o.OrderNo, Channel: "alipay", TradeNo: "late", Amount: 500, Currency: model.CNY}); err != nil {
		t.Fatal(err)
	}
	runJobs(s)
	got, _ = s.OrderByNo(o.OrderNo)
	if got.Status != model.OrderDelivered {
		t.Fatalf("late payment status %s %s", got.Status, got.LastError)
	}
}

func TestUnpaidLimit(t *testing.T) {
	s, _ := setup(t)
	sku := mkSKU(t, s, "text", `{"text":"hello"}`, 100)
	for range 3 {
		order(t, s, sku, 1, "")
	}
	_, err := s.CreateOrder(context.Background(), CreateInput{SKUID: sku.ID, Qty: 1, Email: "other@example.com", QueryPassword: "1234", PayMethod: model.PayEPayAlipay, IP: "1.2.3.4"})
	if err == nil || !strings.Contains(err.Error(), "未支付订单过多") {
		t.Fatalf("limit not enforced: %v", err)
	}
}

func TestCouponAndWholesale(t *testing.T) {
	s, _ := setup(t)
	sku := mkSKU(t, s, "text", `{"text":"hello"}`, 1000)
	s.Store.DB.Model(sku).Update("wholesale", `[{"min_qty":5,"percent":90}]`)
	s.Store.DB.Create(&model.Coupon{Code: "OFF10", Type: model.CouponPercent, Value: 10, MaxUses: 1, Active: true})
	o := order(t, s, sku, 5, "OFF10")
	if o.Amount != 4050 || o.Discount != 450 {
		t.Fatalf("amount %d discount %d", o.Amount, o.Discount)
	}
	if _, err := s.CreateOrder(context.Background(), CreateInput{SKUID: sku.ID, Qty: 1, Email: "x@example.com", QueryPassword: "1234", PayMethod: model.PayEPayAlipay, Coupon: "OFF10"}); err == nil {
		t.Fatal("coupon reused beyond max uses")
	}
	// Free order with a 100% coupon is delivered without payment.
	s.Store.DB.Create(&model.Coupon{Code: "FREE", Type: model.CouponPercent, Value: 100, Active: true})
	free := order(t, s, sku, 1, "FREE")
	if free.Status != model.OrderPaid {
		t.Fatalf("free order status %s", free.Status)
	}
	runJobs(s)
	got, _ := s.OrderByNo(free.OrderNo)
	if got.Status != model.OrderDelivered {
		t.Fatalf("free order not delivered: %s", got.Status)
	}
}

// fakeLicense is a minimal polaris-license admin API.
type fakeLicense struct {
	mu      sync.Mutex
	secret  string
	issued  map[string]string // order_no → license id
	revoked map[string]bool
	extend  map[string]int
	months  map[string]int
	issues  []map[string]any
	fail    int
}

func (f *fakeLicense) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if err := polaris.VerifyRequest(f.secret, r, body, time.Now()); err != nil {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail > 0 {
		f.fail--
		w.WriteHeader(502)
		return
	}
	var req map[string]any
	json.Unmarshal(body, &req)
	switch {
	case r.URL.Path == "/admin/v1/catalog":
		json.NewEncoder(w).Encode(map[string]any{"api": 2, "every_feature": "*",
			"plans":     []map[string]string{{"id": "pro", "name": "专业版"}, {"id": "enterprise", "name": "企业版"}},
			"features":  []map[string]string{{"id": "speed_limit", "name": "客户独立限速"}, {"id": "ddns", "name": "DDNS"}},
			"templates": []map[string]any{{"id": 7, "name": "专业版 · 季付", "plan": "pro", "months": 3, "update_months": 3, "max_activations": 1, "features": []string{"*"}}}})
	case r.URL.Path == "/admin/v1/keys":
		f.issues = append(f.issues, req)
		no := req["order_no"].(string)
		id, ok := f.issued[no]
		if !ok {
			id = "lic_" + strings.ToLower(no)
			f.issued[no] = id
		}
		json.NewEncoder(w).Encode(map[string]any{"key": "KEY-" + no, "license_id": id, "plan": req["plan"], "expires_at": "2027-01-01"})
	case strings.HasSuffix(r.URL.Path, "/extend"):
		id := strings.Split(r.URL.Path, "/")[4]
		f.extend[id] += int(req["days"].(float64))
		if m, ok := req["months"].(float64); ok {
			if f.months == nil {
				f.months = map[string]int{}
			}
			f.months[id] += int(m)
		}
		json.NewEncoder(w).Encode(map[string]any{"license_id": id, "expires_at": "2028-01-01"})
	case strings.HasSuffix(r.URL.Path, "/revoke"):
		f.revoked[strings.Split(r.URL.Path, "/")[4]] = true
		json.NewEncoder(w).Encode(map[string]any{"revoked": true})
	default:
		w.WriteHeader(404)
	}
}

func TestPolarisIssueRenewRefund(t *testing.T) {
	s, _ := setup(t)
	fake := &fakeLicense{secret: strings.Repeat("s", 32), issued: map[string]string{}, revoked: map[string]bool{}, extend: map[string]int{}, fail: 1}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	s.PolarisClient = &polaris.Client{BaseURL: ts.URL, Secret: fake.secret}
	sku := mkSKU(t, s, "polaris", `{"plan":"pro","days":365,"updates_days":365,"features":["*"],"renewable":true}`, 19900)

	o := order(t, s, sku, 2, "")
	ctx := context.Background()
	if _, err := s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: "alipay", TradeNo: "P1", Amount: o.PayAmount, Currency: model.CNY}); err != nil {
		t.Fatal(err)
	}
	runJobs(s) // first call fails with 502 → retry scheduled
	got, _ := s.OrderByNo(o.OrderNo)
	if got.Status != model.OrderPaid || got.LastError == "" {
		t.Fatalf("expected retry, got %s %q", got.Status, got.LastError)
	}
	s.Store.DB.Model(&model.Job{}).Where("1=1").Update("run_at", time.Now().Add(-time.Second))
	runJobs(s)
	got, _ = s.OrderByNo(o.OrderNo)
	if got.Status != model.OrderDelivered {
		t.Fatalf("status %s %s", got.Status, got.LastError)
	}
	items, _ := s.Items(got.ID)
	if len(items) != 2 || items[0].Content != "KEY-"+o.OrderNo+"-1" {
		t.Fatalf("items %+v", items)
	}
	var bs []model.LicenseBinding
	s.Store.DB.Find(&bs)
	if len(bs) != 2 || bs[0].Email != "buyer@example.com" {
		t.Fatalf("bindings %+v", bs)
	}

	// Renewal: only the owner, only renewable SKUs, extends the same license.
	b, _ := s.Binding(bs[0].LicenseID)
	if !OwnsLicense(b, "BUYER@example.com", 0) || OwnsLicense(b, "x@example.com", 0) {
		t.Fatal("ownership check wrong")
	}
	skus, _, _ := s.RenewSKUs(b)
	if len(skus) != 1 {
		t.Fatalf("renew skus %d", len(skus))
	}
	ro, err := s.CreateOrder(ctx, CreateInput{SKUID: sku.ID, Qty: 1, Email: "buyer@example.com", QueryPassword: "1234", PayMethod: model.PayEPayWxpay, RenewOf: b.LicenseID})
	if err != nil {
		t.Fatal(err)
	}
	s.MarkPaid(ctx, PaidInput{OrderNo: ro.OrderNo, Channel: "wxpay", TradeNo: "P2", Amount: ro.PayAmount, Currency: model.CNY})
	runJobs(s)
	if fake.extend[b.LicenseID] != 365 {
		t.Fatalf("extend %v", fake.extend)
	}
	s.Store.DB.First(b, b.ID)
	if b.ExpiresAt != "2028-01-01" {
		t.Fatalf("binding expiry %s", b.ExpiresAt)
	}

	// Refund revokes both licenses of the first order.
	warn, err := s.Refund(ctx, got.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.revoked) != 2 || len(warn) != 1 { // gateway refund is manual
		t.Fatalf("revoked %v warnings %v", fake.revoked, warn)
	}
	got, _ = s.OrderByNo(o.OrderNo)
	if got.Status != model.OrderRefunded {
		t.Fatalf("status %s", got.Status)
	}
}

func TestManualDeliverAndRedeliver(t *testing.T) {
	s, _ := setup(t)
	sku := mkSKU(t, s, "manual", "", 100)
	o := order(t, s, sku, 1, "")
	s.MarkPaid(context.Background(), PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual, TradeNo: "admin", Trusted: true})
	runJobs(s)
	got, _ := s.OrderByNo(o.OrderNo)
	if got.Status != model.OrderPaid || got.LastError != "等待人工发货" {
		t.Fatalf("status %s %q", got.Status, got.LastError)
	}
	if err := s.ManualDeliver(got.ID, "acc1\nacc2", true); err != nil {
		t.Fatal(err)
	}
	items, _ := s.Items(got.ID)
	if len(items) != 2 {
		t.Fatalf("items %d", len(items))
	}
	if err := s.Redeliver(got.ID); err == nil {
		t.Fatal("redeliver of delivered order allowed")
	}
}

func TestUniqueUSDTAmounts(t *testing.T) {
	s, _ := setup(t)
	set := s.Store.Settings()
	set.USDTEnabled, set.USDTAddress = true, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	s.Store.SaveSettings(set)
	sku := mkSKU(t, s, "text", `{"text":"x"}`, 100)
	s.Store.DB.Model(sku).Update("price_usdt", 10_000_000)
	seen := map[int64]bool{}
	for i := range 3 {
		o, err := s.CreateOrder(context.Background(), CreateInput{SKUID: sku.ID, Qty: 1, Email: "u@example.com", QueryPassword: "1234",
			PayMethod: model.PayUSDT, IP: "10.0.0." + string(rune('1'+i))})
		if err != nil {
			t.Fatal(err)
		}
		if o.Amount != 10_000_000 || seen[o.PayAmount] {
			t.Fatalf("pay amount %d repeated", o.PayAmount)
		}
		seen[o.PayAmount] = true
		// Different emails so the unpaid limit doesn't trip.
		s.Store.DB.Model(o).Update("email", "u"+o.OrderNo+"@example.com")
	}
}

func TestRefundRevokesPartialPolarisDelivery(t *testing.T) {
	s, _ := setup(t)
	fake := &fakeLicense{secret: strings.Repeat("s", 32), issued: map[string]string{}, revoked: map[string]bool{}, extend: map[string]int{}}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	s.PolarisClient = &polaris.Client{BaseURL: ts.URL, Secret: fake.secret}
	sku := mkSKU(t, s, "polaris", `{"plan":"pro","days":30}`, 100)
	o := order(t, s, sku, 2, "")
	ctx := context.Background()
	s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual, TradeNo: "m", Trusted: true})
	// Simulate a crash after the first key: one binding exists, order still paid.
	s.Store.DB.Create(&model.LicenseBinding{OrderID: o.ID, LicenseID: "lic_partial", Status: model.LicenseIssued})
	if _, err := s.Refund(ctx, o.ID, "partial"); err != nil {
		t.Fatal(err)
	}
	if !fake.revoked["lic_partial"] {
		t.Fatalf("partial license not revoked: %v", fake.revoked)
	}
}

func TestPolarisTemplateSKU(t *testing.T) {
	s, _ := setup(t)
	fake := &fakeLicense{secret: strings.Repeat("s", 32), issued: map[string]string{}, revoked: map[string]bool{}, extend: map[string]int{}}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	s.PolarisClient = &polaris.Client{BaseURL: ts.URL, Secret: fake.secret}
	ctx := context.Background()

	cat, err := s.PolarisCatalog(ctx, true)
	if err != nil || cat.Template(7) == nil || !cat.HasFeature("ddns") || cat.HasFeature("speedlimit") {
		t.Fatalf("catalog %+v %v", cat, err)
	}
	sku := mkSKU(t, s, "polaris", `{"template":7,"template_name":"专业版 · 季付","plan":"pro","months":3,"update_months":3,"renewable":true}`, 9900)
	o := order(t, s, sku, 1, "")
	s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: "alipay", TradeNo: "T1", Amount: o.PayAmount, Currency: model.CNY})
	runJobs(s)
	if len(fake.issues) != 1 || fake.issues[0]["template"].(float64) != 7 {
		t.Fatalf("issue request %v", fake.issues)
	}
	// the template SKU renews by its months
	var b model.LicenseBinding
	s.Store.DB.First(&b)
	ro, err := s.CreateOrder(ctx, CreateInput{SKUID: sku.ID, Qty: 2, Email: "buyer@example.com", QueryPassword: "1234", PayMethod: model.PayEPayWxpay, RenewOf: b.LicenseID})
	if err != nil {
		t.Fatal(err)
	}
	s.MarkPaid(ctx, PaidInput{OrderNo: ro.OrderNo, Channel: "wxpay", TradeNo: "T2", Amount: ro.PayAmount, Currency: model.CNY})
	runJobs(s)
	if fake.months[b.LicenseID] != 6 || fake.extend[b.LicenseID] != 0 {
		t.Fatalf("renewal months %v days %v", fake.months, fake.extend)
	}
}

package tgbot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/notify"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/store"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
)

type call struct {
	Method string
	Params map[string]any
}

type fakeTG struct {
	mu    sync.Mutex
	calls []call
}

func (f *fakeTG) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var p map[string]any
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		json.NewDecoder(r.Body).Decode(&p)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{method, p})
	f.mu.Unlock()
	w.Write([]byte(`{"ok":true,"result":true}`))
}

func (f *fakeTG) find(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func TestStarsPurchase(t *testing.T) {
	box, _ := secure.NewBox(secure.RandomBytes(32))
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "k.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	set := st.Settings()
	set.TGToken, set.StarsEnabled = "123:abc", true
	st.SaveSettings(set)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := shop.New(st, &notify.Notifier{Store: st, Log: log, Send: func(string, string, string) error { return nil }}, log)
	tg := &fakeTG{}
	srv := httptest.NewServer(tg)
	defer srv.Close()
	svc.Bot = func() *tgapi.Client { return &tgapi.Client{Token: "123:abc", BaseURL: srv.URL} }
	bot := New(svc, log)

	cat := model.Category{Name: "c", Visible: true}
	st.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "VPN", Status: model.ProductOn}
	st.DB.Create(&p)
	sku := model.SKU{ProductID: p.ID, Name: "月卡", PriceStars: 50, DelivererKind: "stock", LimitPerOrder: 5, Active: true}
	st.DB.Create(&sku)
	deliver.ImportCards(&deliver.Env{Store: st}, sku.ID, "VPN-KEY-1")

	ctx := context.Background()
	user := tgapi.User{ID: 42, FirstName: "A"}
	msg := &tgapi.Message{MessageID: 7, From: &user}
	msg.Chat.ID, msg.Chat.Type = 42, "private"
	bot.Handle(ctx, &tgapi.Update{CallbackQuery: &tgapi.CallbackQuery{ID: "1", From: user, Message: msg, Data: "m:1:1:stars"}})
	inv := tg.find("sendInvoice")
	if len(inv) != 1 || inv[0].Params["currency"] != "XTR" {
		t.Fatalf("invoice %+v", tg.calls)
	}
	orderNo := inv[0].Params["payload"].(string)

	// Another user's pre-checkout for this order is refused.
	bot.Handle(ctx, &tgapi.Update{PreCheckoutQuery: &tgapi.PreCheckoutQuery{ID: "x", From: tgapi.User{ID: 99}, Currency: "XTR", TotalAmount: 50, InvoicePayload: orderNo}})
	bot.Handle(ctx, &tgapi.Update{PreCheckoutQuery: &tgapi.PreCheckoutQuery{ID: "y", From: user, Currency: "XTR", TotalAmount: 50, InvoicePayload: orderNo}})
	pc := tg.find("answerPreCheckoutQuery")
	if len(pc) != 2 || pc[0].Params["ok"] != false || pc[1].Params["ok"] != true {
		t.Fatalf("pre-checkout %+v", pc)
	}

	paidMsg := &tgapi.Message{From: &user, SuccessfulPayment: &tgapi.SuccessfulPayment{Currency: "XTR", TotalAmount: 50,
		InvoicePayload: orderNo, TelegramPaymentChargeID: "charge-1"}}
	paidMsg.Chat.ID = 42
	bot.Handle(ctx, &tgapi.Update{Message: paidMsg})
	st.RunDueJobs(ctx, svc.JobHandlers())
	st.RunDueJobs(ctx, svc.JobHandlers())
	o, _ := svc.OrderByNo(orderNo)
	if o.Status != model.OrderDelivered {
		t.Fatalf("status %s %s", o.Status, o.LastError)
	}
	delivered := false
	for _, c := range tg.find("sendMessage") {
		if strings.Contains(c.Params["text"].(string), "VPN-KEY-1") {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("card not sent by private message")
	}

	// Refund goes back through refundStarPayment.
	if _, err := svc.Refund(ctx, o.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if r := tg.find("refundStarPayment"); len(r) != 1 || r[0].Params["telegram_payment_charge_id"] != "charge-1" {
		t.Fatalf("refund %+v", r)
	}
}

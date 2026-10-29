package tgbot

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/notify"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/store"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
)

type menuEnv struct {
	t    *testing.T
	st   *store.Store
	svc  *shop.Service
	tg   *fakeTG
	bot  *Bot
	user tgapi.User
}

func newMenuEnv(t *testing.T) *menuEnv {
	box, _ := secure.NewBox(secure.RandomBytes(32))
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "k.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	set := st.Settings()
	set.TGToken, set.StarsEnabled = "123:abc", true
	st.SaveSettings(set)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := shop.New(st, &notify.Notifier{Store: st, Log: log, Send: func(string, string, string) error { return nil }}, log)
	tg := &fakeTG{}
	srv := httptest.NewServer(tg)
	t.Cleanup(srv.Close)
	svc.Bot = func() *tgapi.Client { return &tgapi.Client{Token: "123:abc", BaseURL: srv.URL} }
	return &menuEnv{t: t, st: st, svc: svc, tg: tg, bot: New(svc, log), user: tgapi.User{ID: 42, FirstName: "A"}}
}

func (e *menuEnv) tap(data string) {
	msg := &tgapi.Message{MessageID: 7, From: &e.user}
	msg.Chat.ID, msg.Chat.Type = 42, "private"
	e.bot.Handle(context.Background(), &tgapi.Update{CallbackQuery: &tgapi.CallbackQuery{ID: "1", From: e.user, Message: msg, Data: data}})
}

func (e *menuEnv) say(text string) {
	m := &tgapi.Message{MessageID: 9, From: &e.user, Text: text}
	m.Chat.ID, m.Chat.Type = 42, "private"
	e.bot.Handle(context.Background(), &tgapi.Update{Message: m})
}

// last returns the text and button labels/data of the latest screen.
func (e *menuEnv) last() (string, []string) {
	e.tg.mu.Lock()
	defer e.tg.mu.Unlock()
	for i := len(e.tg.calls) - 1; i >= 0; i-- {
		c := e.tg.calls[i]
		if c.Method != "editMessageText" && c.Method != "sendMessage" {
			continue
		}
		var btns []string
		if rm, ok := c.Params["reply_markup"].(map[string]any); ok {
			rows, _ := rm["inline_keyboard"].([]any)
			for _, r := range rows {
				for _, b := range r.([]any) {
					m := b.(map[string]any)
					d, _ := m["callback_data"].(string)
					btns = append(btns, m["text"].(string)+"|"+d)
				}
			}
		}
		return c.Params["text"].(string), btns
	}
	return "", nil
}

func has(btns []string, sub string) bool {
	for _, b := range btns {
		if strings.Contains(b, sub) {
			return true
		}
	}
	return false
}

func TestMenuBrowseAndOrder(t *testing.T) {
	e := newMenuEnv(t)
	cat := model.Category{Name: "游戏点卡", Visible: true}
	e.st.DB.Create(&cat)
	p := model.Product{CategoryID: cat.ID, Title: "Steam 卡", Status: model.ProductOn, DescriptionMD: "# 说明\n**秒发** [官网](https://example.com)\n- 第一条"}
	e.st.DB.Create(&p)
	sku := model.SKU{ProductID: p.ID, Name: "50 元", PriceStars: 100, DelivererKind: "stock", LimitPerOrder: 10, Active: true,
		Wholesale: `[{"min_qty":3,"percent":80}]`}
	e.st.DB.Create(&sku)
	deliver.ImportCards(&deliver.Env{Store: e.st}, sku.ID, "A1\nA2\nA3\nA4\nA5")

	// home has icons and the main entries
	e.say("/start")
	text, btns := e.last()
	if !strings.Contains(text, "欢迎") || !has(btns, "🛒 选购商品|cats") || !has(btns, "📦 我的订单|orders:0") || !has(btns, "❓ 帮助|help") {
		t.Fatalf("home: %s %v", text, btns)
	}
	// /help answers (it used to fall back to the home screen)
	e.say("/help")
	if text, _ = e.last(); !strings.Contains(text, "使用帮助") {
		t.Fatalf("help: %s", text)
	}
	// one category: straight to its products, with icon and price
	e.tap("cats")
	text, btns = e.last()
	if !strings.Contains(text, "🎮") || !has(btns, "Steam 卡 · 100 ⭐") || !has(btns, "主菜单") {
		t.Fatalf("products: %s %v", text, btns)
	}
	// product page renders the description, no raw Markdown
	e.tap("p:1")
	text, btns = e.last()
	if strings.Contains(text, "**") || strings.Contains(text, "# ") || !strings.Contains(text, "<b>秒发</b>") ||
		!strings.Contains(text, `<a href="https://example.com">官网</a>`) || !strings.Contains(text, "• 第一条") || !has(btns, "s:1:1") {
		t.Fatalf("product: %s %v", text, btns)
	}
	// confirm screen: stepper and the wholesale total
	e.tap("s:1:3")
	text, btns = e.last()
	if !strings.Contains(text, "数量：<b>3</b>") || !strings.Contains(text, "240 ⭐") || !strings.Contains(text, "已享批发价") ||
		!has(btns, "➖|s:1:2") || !has(btns, "➕|s:1:4") || !has(btns, "m:1:3:stars") {
		t.Fatalf("confirm: %s %v", text, btns)
	}
	// quantity can't go past the stock
	e.tap("s:1:50")
	if text, btns = e.last(); !strings.Contains(text, "数量：<b>5</b>") || !has(btns, "➕|noop:qty") {
		t.Fatalf("clamped: %s %v", text, btns)
	}
	// order, then the order list and detail offer to pay or cancel
	e.tap("m:1:3:stars")
	text, btns = e.last()
	if !strings.Contains(text, "订单已创建") || !has(btns, "❌ 取消订单") || len(e.tg.find("sendInvoice")) != 1 {
		t.Fatalf("order: %s %v", text, btns)
	}
	var o model.Order
	e.st.DB.Take(&o)
	if o.Amount != 240 {
		t.Fatalf("amount %d", o.Amount)
	}
	e.tap("orders:0")
	if text, btns = e.last(); !has(btns, "⏳ Steam 卡 ×3") {
		t.Fatalf("orders: %s %v", text, btns)
	}
	e.tap("o:" + o.OrderNo)
	if text, btns = e.last(); !has(btns, "💳 继续付款|op:") || !has(btns, "oc:"+o.OrderNo) {
		t.Fatalf("detail: %s %v", text, btns)
	}
	// cancelling gives the cards back
	e.tap("oc:" + o.OrderNo)
	e.st.DB.Take(&o)
	var k model.SKU
	e.st.DB.Take(&k, sku.ID)
	if o.Status != model.OrderExpired || k.StockCache != 5 {
		t.Fatalf("cancel: %s stock %d", o.Status, k.StockCache)
	}
	if text, btns = e.last(); !has(btns, "重新下单") {
		t.Fatalf("after cancel: %s %v", text, btns)
	}
	// someone else can't cancel or view it
	other := tgapi.User{ID: 7}
	if err := e.svc.CancelUnpaid(o.OrderNo, other.ID); err == nil {
		t.Fatal("cancelled another user's order")
	}
}

func TestCatIcon(t *testing.T) {
	for name, want := range map[string]string{"会员订阅": "👑 ", "Steam 游戏": "🎮 ", "其他": "🗂️ ", "🔥 热卖": ""} {
		if got := catIcon(name); got != want {
			t.Fatalf("%s → %q", name, got)
		}
	}
}

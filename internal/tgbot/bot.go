// Package tgbot is the Telegram sales channel: browse with inline
// keyboards, pay with Stars (native invoices), EPay links or USDT, receive
// the content by private message, list orders and renew Polaris licenses
// through a /start renew_{license_id} deep link.
package tgbot

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
)

// Bot handles webhook updates.
type Bot struct {
	shop *shop.Service
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[int64]*session
}

// session is a short conversation state: the renewal email check, or the
// answer a manual SKU asks for before ordering.
type session struct {
	Step      string
	LicenseID string
	// Step "ask": the order waiting for the buyer's answer
	SKUID   uint
	Qty     int
	Method  string
	Ask     string
	Expires time.Time
}

const sessionTTL = 30 * time.Minute

// New returns a bot.
func New(s *shop.Service, log *slog.Logger) *Bot {
	return &Bot{shop: s, log: log, sessions: map[int64]*session{}}
}

func (b *Bot) api() *tgapi.Client { return b.shop.Bot() }

func (b *Bot) getSession(uid int64) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sessions[uid]
	if s == nil || time.Now().After(s.Expires) {
		delete(b.sessions, uid)
		return nil
	}
	return s
}

func (b *Bot) setSession(uid int64, s *session) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sessions) > 10000 {
		now := time.Now()
		for k, v := range b.sessions {
			if now.After(v.Expires) {
				delete(b.sessions, k)
			}
		}
	}
	if s == nil {
		delete(b.sessions, uid)
		return
	}
	s.Expires = time.Now().Add(sessionTTL)
	b.sessions[uid] = s
}

// HandleUpdate processes one update in the background (the webhook answers
// Telegram right away).
func (b *Bot) HandleUpdate(body []byte) {
	var u tgapi.Update
	if err := json.Unmarshal(body, &u); err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				b.log.Error("telegram handler panic", "err", r)
			}
		}()
		b.Handle(ctx, &u)
	}()
}

// Handle processes one update synchronously.
func (b *Bot) Handle(ctx context.Context, u *tgapi.Update) {
	switch {
	case u.PreCheckoutQuery != nil:
		b.preCheckout(ctx, u.PreCheckoutQuery)
	case u.Message != nil && u.Message.SuccessfulPayment != nil:
		b.paid(ctx, u.Message)
	case u.CallbackQuery != nil:
		b.callback(ctx, u.CallbackQuery)
	case u.Message != nil && u.Message.From != nil && u.Message.Chat.Type == "private":
		b.message(ctx, u.Message)
	}
}

func (b *Bot) send(ctx context.Context, chat int64, text string, kb *tgapi.Keyboard) {
	if err := b.api().SendMessage(ctx, chat, text, true, kb); err != nil {
		b.log.Warn("telegram send failed", "err", err)
	}
}

func (b *Bot) preCheckout(ctx context.Context, q *tgapi.PreCheckoutQuery) {
	ok, msg := true, ""
	o, err := b.shop.OrderByNo(q.InvoicePayload)
	switch {
	case err != nil:
		ok, msg = false, "订单不存在"
	case o.Status != model.OrderPending:
		ok, msg = false, "订单已关闭，请重新下单"
	case o.TGUserID != q.From.ID:
		ok, msg = false, "这不是你的订单"
	case q.Currency != model.XTR || q.TotalAmount != o.PayAmount:
		ok, msg = false, "金额不一致，请重新下单"
	case time.Now().After(o.ExpiresAt):
		ok, msg = false, "订单已超时，请重新下单"
	}
	p := map[string]any{"pre_checkout_query_id": q.ID, "ok": ok}
	if !ok {
		p["error_message"] = msg
	}
	if err := b.api().Call(ctx, "answerPreCheckoutQuery", p, nil); err != nil {
		b.log.Warn("answer pre-checkout failed", "err", err)
	}
}

func (b *Bot) paid(ctx context.Context, m *tgapi.Message) {
	sp := m.SuccessfulPayment
	raw, _ := json.Marshal(sp)
	o, err := b.shop.MarkPaid(ctx, shop.PaidInput{OrderNo: sp.InvoicePayload, Channel: model.PayStars, TradeNo: sp.TelegramPaymentChargeID,
		Amount: sp.TotalAmount, Currency: sp.Currency, Raw: string(raw)})
	if err != nil {
		b.log.Error("stars payment not applied", "order", sp.InvoicePayload, "charge", sp.TelegramPaymentChargeID, "err", err)
		b.shop.Notify.Admin(fmt.Sprintf("⚠️ Stars 付款未能入账：订单 %s，charge %s：%v", sp.InvoicePayload, sp.TelegramPaymentChargeID, err))
		b.send(ctx, m.Chat.ID, "付款已收到，但订单处理出错，管理员已收到通知，会尽快处理。", nil)
		return
	}
	b.send(ctx, m.Chat.ID, fmt.Sprintf("💰 已收到付款，订单 <code>%s</code> 正在发货，发货内容会私信发给你。", o.OrderNo),
		kbRows(btnRow(btn("🧾 查看订单", "o:"+o.OrderNo))))
}

// ---- renewals ----

// startRenew begins a renewal. The license id only locates the license:
// the buyer must be the Telegram account that bought it, or prove the
// purchase email.
func (b *Bot) startRenew(ctx context.Context, chat, uid int64, lid string) {
	bd, err := b.shop.Binding(lid)
	if err != nil {
		b.send(ctx, chat, "⚠️ "+html.EscapeString(err.Error()), b.homeKB())
		return
	}
	if shop.OwnsLicense(bd, "", uid) {
		b.setSession(uid, &session{Step: "renew_ready", LicenseID: lid})
		b.renewOptions(ctx, chat, uid, bd)
		return
	}
	b.setSession(uid, &session{Step: "renew_email", LicenseID: lid})
	b.api().Call(ctx, "sendMessage", map[string]any{
		"chat_id": chat, "text": "为确认身份，请回复购买该许可证时使用的邮箱：",
		"reply_markup": map[string]any{"force_reply": true, "input_field_placeholder": "you@example.com"},
	}, nil)
}

func (b *Bot) renewOptions(ctx context.Context, chat, uid int64, bd *model.LicenseBinding) {
	skus, titles, err := b.shop.RenewSKUs(bd)
	if err != nil || len(skus) == 0 {
		b.send(ctx, chat, "暂无可用于续费的商品，请联系客服。", b.homeKB())
		return
	}
	exp := bd.ExpiresAt
	if exp == "" {
		exp = "永久"
	}
	kb := &tgapi.Keyboard{}
	for i := range skus {
		k := &skus[i]
		kb.InlineKeyboard = append(kb.InlineKeyboard, []tgapi.InlineButton{{Text: titles[k.ID] + " · " + k.Name + " · " + priceLine(k), CallbackData: fmt.Sprintf("r:%d", k.ID)}})
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, btnRow(btn("❌ 取消", "x")))
	b.send(ctx, chat, fmt.Sprintf("🔁 <b>续费许可证</b>\n\n许可证 <code>%s</code>\n套餐 %s\n当前到期 %s\n\n续费后面板会在下次校验时自动生效，无需重新激活。请选择续费时长：", bd.LicenseID, html.EscapeString(bd.Plan), exp), kb)
}

// emailTries counts failed renewal email checks per user (15-minute window).
type emailTries struct {
	n     int
	start time.Time
}

var attempts sync.Map // uid → emailTries

// askFor returns what a manual SKU asks the buyer, if anything.
func (b *Bot) askFor(skuID uint) string {
	k, err := b.loadSKU(skuID)
	if err != nil || k.DelivererKind != "manual" {
		return ""
	}
	cfg, err := deliver.ParseManualCfg(k.DelivererCfg)
	if err != nil {
		return ""
	}
	return cfg.Ask
}

func (b *Bot) continueSession(ctx context.Context, m *tgapi.Message, s *session) {
	uid := m.From.ID
	if s.Step == "ask" {
		ans := strings.TrimSpace(m.Text)
		if ans == "" || len([]rune(ans)) > 100 {
			b.send(ctx, m.Chat.ID, fmt.Sprintf("请发送%s（1–100 字）。", html.EscapeString(s.Ask)), kbRows(btnRow(btn("❌ 取消", "x"))))
			return
		}
		b.setSession(uid, nil)
		b.order(ctx, m.Chat.ID, 0, *m.From, s.SKUID, s.Qty, s.Method, "", ans)
		return
	}
	if s.Step != "renew_email" {
		b.home(ctx, m.Chat.ID, 0, uid)
		return
	}
	bd, err := b.shop.Binding(s.LicenseID)
	if err != nil {
		b.setSession(uid, nil)
		return
	}
	v, _ := attempts.Load(uid)
	tries, _ := v.(emailTries)
	if time.Since(tries.start) > 15*time.Minute {
		tries = emailTries{start: time.Now()}
	}
	if tries.n >= 5 {
		b.setSession(uid, nil)
		b.send(ctx, m.Chat.ID, "尝试次数过多，请稍后再试或联系客服。", nil)
		return
	}
	if !shop.OwnsLicense(bd, m.Text, 0) {
		tries.n++
		attempts.Store(uid, tries)
		b.send(ctx, m.Chat.ID, "邮箱与购买时不一致，请重新输入。", kbRows(btnRow(btn("❌ 取消", "x"))))
		return
	}
	attempts.Delete(uid)
	b.setSession(uid, &session{Step: "renew_ready", LicenseID: s.LicenseID})
	b.renewOptions(ctx, m.Chat.ID, uid, bd)
}

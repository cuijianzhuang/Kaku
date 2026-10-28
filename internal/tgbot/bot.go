// Package tgbot is the Telegram sales channel: browse with inline
// keyboards, pay with Stars (native invoices), EPay links or USDT, receive
// the content by private message, list orders and renew Polaris licenses
// through a /start renew_{license_id} deep link.
package tgbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/payment"
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

func (b *Bot) message(ctx context.Context, m *tgapi.Message) {
	uid := m.From.ID
	text := strings.TrimSpace(m.Text)
	if s := b.getSession(uid); s != nil && !strings.HasPrefix(text, "/") {
		b.continueSession(ctx, m, s)
		return
	}
	cmd, arg, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	switch cmd {
	case "/start":
		if lid, ok := strings.CutPrefix(strings.TrimSpace(arg), "renew_"); ok && lid != "" {
			b.startRenew(ctx, m.Chat.ID, uid, lid)
			return
		}
		b.home(ctx, m.Chat.ID, 0)
	case "/shop":
		b.categories(ctx, m.Chat.ID, 0)
	case "/orders":
		b.orders(ctx, m.Chat.ID, uid)
	case "/stats":
		b.stats(ctx, m.Chat.ID, uid)
	case "/renew":
		if arg = strings.TrimSpace(arg); arg != "" {
			b.startRenew(ctx, m.Chat.ID, uid, arg)
			return
		}
		b.send(ctx, m.Chat.ID, "用法：/renew lic_xxxx（许可证编号在面板「许可证」页）", nil)
	default:
		b.home(ctx, m.Chat.ID, 0)
	}
}

func (b *Bot) home(ctx context.Context, chat, msgID int64) {
	st := b.shop.Store.Settings()
	text := fmt.Sprintf("👋 欢迎来到 <b>%s</b>\n\n选购商品后可直接在这里付款，付款成功后机器人会把卡密私信发给你。", html.EscapeString(st.SiteName))
	if st.Notice != "" {
		text += "\n\n📢 " + html.EscapeString(st.Notice)
	}
	kb := &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{
		{{Text: "🛒 选购商品", CallbackData: "cats"}},
		{{Text: "📦 我的订单", CallbackData: "orders"}},
	}}
	if st.SupportURL != "" {
		kb.InlineKeyboard = append(kb.InlineKeyboard, []tgapi.InlineButton{{Text: "💬 联系客服", URL: st.SupportURL}})
	}
	b.show(ctx, chat, msgID, text, kb)
}

// show edits the message a button belongs to, or sends a new one.
func (b *Bot) show(ctx context.Context, chat, msgID int64, text string, kb *tgapi.Keyboard) {
	if msgID != 0 {
		if err := b.api().EditMessage(ctx, chat, msgID, text, kb); err == nil {
			return
		}
	}
	b.send(ctx, chat, text, kb)
}

func back(data string) []tgapi.InlineButton {
	return []tgapi.InlineButton{{Text: "« 返回", CallbackData: data}}
}

func (b *Bot) categories(ctx context.Context, chat, msgID int64) {
	cats, err := b.shop.Catalog()
	if err != nil || len(cats) == 0 {
		b.show(ctx, chat, msgID, "暂无在售商品。", &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{back("home")}})
		return
	}
	kb := &tgapi.Keyboard{}
	for _, c := range cats {
		kb.InlineKeyboard = append(kb.InlineKeyboard, []tgapi.InlineButton{{Text: c.Name, CallbackData: fmt.Sprintf("c:%d", c.ID)}})
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, back("home"))
	b.show(ctx, chat, msgID, "请选择分类：", kb)
}

func (b *Bot) products(ctx context.Context, chat, msgID int64, catID uint) {
	cats, _ := b.shop.Catalog()
	kb := &tgapi.Keyboard{}
	for _, c := range cats {
		if c.ID != catID {
			continue
		}
		for _, p := range c.Products {
			kb.InlineKeyboard = append(kb.InlineKeyboard, []tgapi.InlineButton{{Text: p.Title, CallbackData: fmt.Sprintf("p:%d", p.ID)}})
		}
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, back("cats"))
	b.show(ctx, chat, msgID, "请选择商品：", kb)
}

func priceLine(k *model.SKU) string {
	var ps []string
	for _, cur := range []string{model.XTR, model.CNY, model.USDT} {
		if v := k.Price(cur); v > 0 {
			ps = append(ps, shop.FormatMoney(v, cur))
		}
	}
	return strings.Join(ps, " / ")
}

func (b *Bot) skus(ctx context.Context, chat, msgID int64, prodID uint) {
	p, err := b.shop.Product(prodID)
	if err != nil {
		b.show(ctx, chat, msgID, "商品不存在或已下架。", &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{back("cats")}})
		return
	}
	var t strings.Builder
	fmt.Fprintf(&t, "<b>%s</b>\n", html.EscapeString(p.Title))
	if d := strings.TrimSpace(p.DescriptionMD); d != "" {
		if r := []rune(d); len(r) > 600 {
			d = string(r[:600]) + "…"
		}
		t.WriteString("\n" + html.EscapeString(d) + "\n")
	}
	t.WriteString("\n请选择规格：")
	kb := &tgapi.Keyboard{}
	for i := range p.SKUs {
		k := &p.SKUs[i]
		label := k.Name + " · " + priceLine(k)
		n := b.shop.Stock(k)
		if n == 0 {
			label += "（缺货）"
		}
		data := fmt.Sprintf("s:%d", k.ID)
		if n == 0 {
			data = "noop"
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, []tgapi.InlineButton{{Text: label, CallbackData: data}})
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, back(fmt.Sprintf("c:%d", p.CategoryID)))
	b.show(ctx, chat, msgID, t.String(), kb)
}

func (b *Bot) loadSKU(id uint) (*model.SKU, error) {
	var k model.SKU
	if err := b.shop.Store.DB.Where("id = ? AND active = ?", id, true).Take(&k).Error; err != nil {
		return nil, errors.New("规格不存在或已下架")
	}
	return &k, nil
}

func (b *Bot) quantity(ctx context.Context, chat, msgID int64, skuID uint) {
	k, err := b.loadSKU(skuID)
	if err != nil {
		b.show(ctx, chat, msgID, err.Error(), nil)
		return
	}
	limit := max(k.LimitPerOrder, 1)
	if n := b.shop.Stock(k); n >= 0 {
		limit = min(limit, n)
	}
	var row []tgapi.InlineButton
	kb := &tgapi.Keyboard{}
	for _, q := range []int{1, 2, 3, 5, 10} {
		if q > limit {
			break
		}
		row = append(row, tgapi.InlineButton{Text: strconv.Itoa(q), CallbackData: fmt.Sprintf("q:%d:%d", skuID, q)})
	}
	if len(row) == 0 {
		b.show(ctx, chat, msgID, "该规格已缺货。", &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{back(fmt.Sprintf("p:%d", k.ProductID))}})
		return
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, row, back(fmt.Sprintf("p:%d", k.ProductID)))
	b.show(ctx, chat, msgID, fmt.Sprintf("<b>%s</b> · %s\n请选择购买数量：", html.EscapeString(k.Name), priceLine(k)), kb)
}

func (b *Bot) methods(ctx context.Context, chat, msgID int64, skuID uint, qty int, renew bool) {
	k, err := b.loadSKU(skuID)
	if err != nil {
		b.show(ctx, chat, msgID, err.Error(), nil)
		return
	}
	ms := b.shop.MethodsFor(model.ChannelTG, k)
	if len(ms) == 0 {
		b.show(ctx, chat, msgID, "该规格暂无可用的支付方式。", nil)
		return
	}
	kb := &tgapi.Keyboard{}
	for _, m := range ms {
		data := fmt.Sprintf("m:%d:%d:%s", skuID, qty, m.ID)
		if renew {
			data = fmt.Sprintf("rm:%d:%d:%s", skuID, qty, m.ID)
		}
		kb.InlineKeyboard = append(kb.InlineKeyboard, []tgapi.InlineButton{{Text: m.Label + " · " + shop.FormatMoney(k.Price(m.Currency)*int64(qty), m.Currency), CallbackData: data}})
	}
	if !renew {
		kb.InlineKeyboard = append(kb.InlineKeyboard, back(fmt.Sprintf("s:%d", skuID)))
	}
	b.show(ctx, chat, msgID, fmt.Sprintf("<b>%s</b> ×%d\n请选择支付方式：", html.EscapeString(k.Name), qty), kb)
}

func (b *Bot) callback(ctx context.Context, q *tgapi.CallbackQuery) {
	answer := ""
	defer func() {
		b.api().Call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": q.ID, "text": answer}, nil)
	}()
	if q.Message == nil {
		return
	}
	chat, msg, uid := q.Message.Chat.ID, q.Message.MessageID, q.From.ID
	parts := strings.Split(q.Data, ":")
	num := func(i int) int {
		if i >= len(parts) {
			return 0
		}
		n, _ := strconv.Atoi(parts[i])
		return n
	}
	switch parts[0] {
	case "home":
		b.home(ctx, chat, msg)
	case "cats":
		b.categories(ctx, chat, msg)
	case "orders":
		b.orders(ctx, chat, uid)
	case "c":
		b.products(ctx, chat, msg, uint(num(1)))
	case "p":
		b.skus(ctx, chat, msg, uint(num(1)))
	case "s":
		b.quantity(ctx, chat, msg, uint(num(1)))
	case "q":
		b.methods(ctx, chat, msg, uint(num(1)), num(2), false)
	case "m", "rm":
		if len(parts) != 4 {
			return
		}
		renewOf := ""
		if parts[0] == "rm" {
			s := b.getSession(uid)
			if s == nil || s.Step != "renew_ready" {
				answer = "会话已过期，请重新打开续费链接"
				return
			}
			renewOf = s.LicenseID
		}
		if renewOf == "" {
			if ask := b.askFor(uint(num(1))); ask != "" {
				b.setSession(uid, &session{Step: "ask", SKUID: uint(num(1)), Qty: num(2), Method: parts[3], Ask: ask})
				b.show(ctx, chat, msg, fmt.Sprintf("请发送<b>%s</b>（发送 /start 取消）：", html.EscapeString(ask)), nil)
				return
			}
		}
		answer = b.order(ctx, chat, msg, q.From, uint(num(1)), num(2), parts[3], renewOf, "")
	case "o":
		if len(parts) == 2 {
			b.orderDetail(ctx, chat, uid, parts[1])
		}
	case "r":
		s := b.getSession(uid)
		if s == nil || s.Step != "renew_ready" {
			answer = "会话已过期，请重新打开续费链接"
			return
		}
		b.methods(ctx, chat, msg, uint(num(1)), 1, true)
	case "noop":
		answer = "该规格已缺货"
	}
}

// order creates an order and shows how to pay. It returns a toast for
// errors.
func (b *Bot) order(ctx context.Context, chat, msgID int64, from tgapi.User, skuID uint, qty int, method, renewOf, ask string) string {
	// called from a text message (msgID 0) there is no toast: say it in the chat
	fail := func(t string) string {
		if msgID == 0 {
			b.send(ctx, chat, "⚠️ "+html.EscapeString(t), nil)
		}
		return t
	}
	var email string
	if renewOf != "" {
		if bd, err := b.shop.Binding(renewOf); err == nil {
			email = bd.Email
		}
	}
	o, err := b.shop.CreateOrder(ctx, shop.CreateInput{SKUID: skuID, Qty: qty, PayMethod: method, Channel: model.ChannelTG,
		TGUserID: from.ID, Email: email, RenewOf: renewOf, Ask: ask, IP: fmt.Sprintf("tg:%d", from.ID)})
	if err != nil {
		var ue *shop.UserError
		if errors.As(err, &ue) {
			b.send(ctx, chat, "⚠️ "+html.EscapeString(ue.Msg), nil)
			return ue.Msg
		}
		b.log.Error("telegram order failed", "err", err)
		return fail("下单失败，请稍后再试")
	}
	if renewOf != "" {
		b.setSession(from.ID, nil)
	}
	if o.Status != model.OrderPending {
		b.show(ctx, chat, msgID, fmt.Sprintf("✅ 订单 <code>%s</code> 已创建，正在发货…", o.OrderNo), nil)
		return ""
	}
	head := fmt.Sprintf("🧾 订单 <code>%s</code>\n%s %s ×%d\n应付 <b>%s</b>\n请在 %d 分钟内完成支付。",
		o.OrderNo, html.EscapeString(o.ProductTitle), html.EscapeString(o.SKUName), o.Qty, shop.FormatMoney(o.Amount, o.Currency),
		max(b.shop.Store.Settings().OrderTTL, 5))
	switch method {
	case model.PayStars:
		b.show(ctx, chat, msgID, head, nil)
		title := o.ProductTitle
		if r := []rune(title); len(r) > 32 {
			title = string(r[:32])
		}
		desc := o.SKUName + " ×" + strconv.Itoa(o.Qty)
		if err := b.api().Call(ctx, "sendInvoice", map[string]any{
			"chat_id": chat, "title": title, "description": desc, "payload": o.OrderNo, "currency": "XTR",
			"prices": []map[string]any{{"label": desc, "amount": o.PayAmount}},
		}, nil); err != nil {
			b.log.Warn("send invoice failed", "err", err)
			return fail("无法创建 Stars 账单")
		}
	case model.PayUSDT:
		addr := b.shop.Store.Settings().USDTAddress
		amount := payment.Decimal(o.PayAmount, 6)
		text := head + fmt.Sprintf("\n\n请向下面的地址转账 <b>恰好 %s USDT</b>（TRC20）：\n<code>%s</code>\n\n金额尾数用于识别订单，请勿修改。链上确认后自动发货。", amount, addr)
		b.show(ctx, chat, msgID, text, &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{
			{tgapi.CopyButton("复制金额", amount), tgapi.CopyButton("复制地址", addr)},
		}})
	default:
		base := strings.TrimRight(b.shop.Store.Settings().PublicURL, "/")
		if !strings.HasPrefix(base, "https://") {
			return fail("站点地址未配置，无法使用该支付方式")
		}
		b.show(ctx, chat, msgID, head, &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{
			{{Text: "前往付款", URL: base + "/pay/" + o.OrderNo}},
		}})
	}
	return ""
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
	b.send(ctx, m.Chat.ID, fmt.Sprintf("💰 已收到付款，订单 <code>%s</code> 正在发货…", o.OrderNo), nil)
}

func (b *Bot) orders(ctx context.Context, chat, uid int64) {
	var orders []model.Order
	b.shop.Store.DB.Where("tg_user_id = ?", uid).Order("id DESC").Limit(10).Find(&orders)
	if len(orders) == 0 {
		b.send(ctx, chat, "你还没有订单。", &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{{{Text: "🛒 选购商品", CallbackData: "cats"}}}})
		return
	}
	var t strings.Builder
	t.WriteString("📦 <b>最近订单</b>\n")
	kb := &tgapi.Keyboard{}
	for _, o := range orders {
		fmt.Fprintf(&t, "\n<code>%s</code> · %s ×%d · %s · %s", o.OrderNo, html.EscapeString(o.ProductTitle), o.Qty, shop.FormatMoney(o.Amount, o.Currency), statusText(o.Status))
		if o.Status == model.OrderDelivered {
			kb.InlineKeyboard = append(kb.InlineKeyboard, []tgapi.InlineButton{{Text: "查看 " + o.OrderNo, CallbackData: "o:" + o.OrderNo}})
		}
	}
	var markup *tgapi.Keyboard
	if len(kb.InlineKeyboard) > 0 {
		markup = kb
	}
	b.send(ctx, chat, t.String(), markup)
}

func (b *Bot) orderDetail(ctx context.Context, chat, uid int64, no string) {
	o, err := b.shop.OrderByNo(no)
	if err != nil || o.TGUserID != uid || o.Status != model.OrderDelivered {
		b.send(ctx, chat, "订单不存在。", nil)
		return
	}
	items, err := b.shop.Items(o.ID)
	if err != nil {
		return
	}
	if err := b.shop.SendItemsTG(ctx, chat, o, items); err != nil {
		b.log.Warn("send items failed", "err", err)
	}
}

func statusText(s string) string {
	switch s {
	case model.OrderPending:
		return "待支付"
	case model.OrderPaid:
		return "发货中"
	case model.OrderDelivered:
		return "已发货"
	case model.OrderExpired:
		return "已超时"
	case model.OrderFailed:
		return "处理中"
	case model.OrderRefunded:
		return "已退款"
	}
	return s
}

func (b *Bot) stats(ctx context.Context, chat, uid int64) {
	if b.shop.Store.Settings().TGAdminChat != uid {
		b.home(ctx, chat, 0)
		return
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rev, _ := b.shop.RevenueSince(today)
	var t strings.Builder
	t.WriteString("📊 <b>今日销售</b>\n")
	if len(rev) == 0 {
		t.WriteString("暂无")
	}
	for _, r := range rev {
		fmt.Fprintf(&t, "%s：%d 单 · %s\n", r.Currency, r.Orders, shop.FormatMoney(r.Amount, r.Currency))
	}
	var failed, waiting int64
	b.shop.Store.DB.Model(&model.Order{}).Where("status = ?", model.OrderFailed).Count(&failed)
	b.shop.Store.DB.Model(&model.Order{}).Where("status = ?", model.OrderPaid).Count(&waiting)
	fmt.Fprintf(&t, "\n待发货 %d · 发货失败 %d", waiting, failed)
	b.send(ctx, chat, t.String(), nil)
}

// ---- renewals ----

// startRenew begins a renewal. The license id only locates the license:
// the buyer must be the Telegram account that bought it, or prove the
// purchase email.
func (b *Bot) startRenew(ctx context.Context, chat, uid int64, lid string) {
	bd, err := b.shop.Binding(lid)
	if err != nil {
		b.send(ctx, chat, "⚠️ "+html.EscapeString(err.Error()), nil)
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
		b.send(ctx, chat, "暂无可用于续费的商品，请联系客服。", nil)
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
	b.send(ctx, chat, fmt.Sprintf("🔁 续费许可证 <code>%s</code>\n套餐 %s · 当前到期 %s\n\n请选择续费时长：", bd.LicenseID, bd.Plan, exp), kb)
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
			b.send(ctx, m.Chat.ID, fmt.Sprintf("请发送%s（1–100 字，发送 /start 取消）。", html.EscapeString(s.Ask)), nil)
			return
		}
		b.setSession(uid, nil)
		b.order(ctx, m.Chat.ID, 0, *m.From, s.SKUID, s.Qty, s.Method, "", ans)
		return
	}
	if s.Step != "renew_email" {
		b.home(ctx, m.Chat.ID, 0)
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
		b.send(ctx, m.Chat.ID, "邮箱与购买时不一致，请重新输入（或发送 /start 取消）。", nil)
		return
	}
	attempts.Delete(uid)
	b.setSession(uid, &session{Step: "renew_ready", LicenseID: s.LicenseID})
	b.renewOptions(ctx, m.Chat.ID, uid, bd)
}

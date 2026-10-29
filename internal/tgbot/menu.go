package tgbot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/payment"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
)

// The menus: home → categories → products → product (pick a spec) →
// confirm (quantity and total, pick how to pay) → payment. Every screen
// edits the message it was opened from and has a way back.

const (
	pageSize     = 8
	orderPage    = 6
	maxQtyButton = 99
)

// ---- keyboard helpers ----

func btn(text, data string) tgapi.InlineButton { return tgapi.InlineButton{Text: text, CallbackData: data} }

func btnRow(bs ...tgapi.InlineButton) []tgapi.InlineButton { return bs }

func kbRows(rows ...[]tgapi.InlineButton) *tgapi.Keyboard {
	return &tgapi.Keyboard{InlineKeyboard: rows}
}

// grid lays buttons out n per row.
func grid(bs []tgapi.InlineButton, n int) [][]tgapi.InlineButton {
	var rows [][]tgapi.InlineButton
	for i := 0; i < len(bs); i += n {
		rows = append(rows, bs[i:min(i+n, len(bs))])
	}
	return rows
}

func navRow(backData string) []tgapi.InlineButton {
	if backData == "" || backData == "home" {
		return btnRow(btn("🏠 主菜单", "home"))
	}
	return btnRow(btn("⬅️ 返回", backData), btn("🏠 主菜单", "home"))
}

// pager adds ◀ 1/3 ▶ when a list has more than one page.
func pager(prefix string, page, pages int) []tgapi.InlineButton {
	if pages <= 1 {
		return nil
	}
	row := []tgapi.InlineButton{}
	if page > 0 {
		row = append(row, btn("◀️ 上一页", fmt.Sprintf("%s:%d", prefix, page-1)))
	}
	row = append(row, btn(fmt.Sprintf("%d / %d", page+1, pages), "noop:page"))
	if page < pages-1 {
		row = append(row, btn("下一页 ▶️", fmt.Sprintf("%s:%d", prefix, page+1)))
	}
	return row
}

// ---- icons ----

var catIcons = []struct{ words, icon string }{
	{"会员 vip 订阅 plus pro", "👑"}, {"游戏 game steam 点卡", "🎮"}, {"账号 帐号 account", "👤"},
	{"软件 激活 license key 序列号", "💿"}, {"教程 资源 课程 网盘 下载 书", "📚"}, {"充值 话费 代充 流量", "💳"},
	{"面板 polaris 许可 授权", "🛡️"}, {"vpn 网络 节点 加速 代理 机场", "🌐"}, {"ai gpt claude 智能", "🤖"},
	{"音乐 music", "🎵"}, {"视频 影视 netflix video", "🎬"}, {"服务 定制 代办", "🛠️"}, {"礼品 礼物 卡券 gift", "🎁"},
}

// startsWithIcon reports whether a name already begins with an emoji.
func startsWithIcon(s string) bool {
	for _, r := range s {
		return r > 0x2000 && !unicode.Is(unicode.Han, r) && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}
	return false
}

func catIcon(name string) string {
	if startsWithIcon(name) {
		return ""
	}
	low := strings.ToLower(name)
	for _, c := range catIcons {
		for _, w := range strings.Fields(c.words) {
			if strings.Contains(low, w) {
				return c.icon + " "
			}
		}
	}
	return "🗂️ "
}

var kindIcons = map[string]string{"stock": "🔑", "text": "📄", "manual": "🙋", "polaris": "🛡️", "panelcode": "🎟️", "webhook": "⚡"}

func productIcon(p *model.Product) string {
	if startsWithIcon(p.Title) {
		return ""
	}
	if len(p.SKUs) > 0 {
		if i, ok := kindIcons[p.SKUs[0].DelivererKind]; ok {
			return i + " "
		}
	}
	return "📦 "
}

func methodIcon(id string) string {
	switch id {
	case model.PayStars:
		return "⭐"
	case model.PayUSDT:
		return "💵"
	case model.PayEPayAlipay:
		return "🔵"
	case model.PayEPayWxpay:
		return "🟢"
	}
	return "💳"
}

func statusIcon(s string) string {
	switch s {
	case model.OrderPending:
		return "⏳"
	case model.OrderPaid:
		return "🚚"
	case model.OrderDelivered:
		return "✅"
	case model.OrderExpired:
		return "⌛"
	case model.OrderFailed:
		return "🛠️"
	case model.OrderRefunded:
		return "↩️"
	}
	return "•"
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
		return "已关闭"
	case model.OrderFailed:
		return "处理中"
	case model.OrderRefunded:
		return "已退款"
	}
	return s
}

// ---- text helpers ----

func priceLine(k *model.SKU) string {
	var ps []string
	for _, cur := range []string{model.XTR, model.CNY, model.USDT} {
		if v := k.Price(cur); v > 0 {
			ps = append(ps, shop.FormatMoney(v, cur))
		}
	}
	return strings.Join(ps, " / ")
}

// lowPrice is a product's cheapest buyable price, "¥9.9 起".
func (b *Bot) lowPrice(p *model.Product) string {
	skus := p.SKUs
	var buyable []model.SKU
	for i := range p.SKUs {
		if b.shop.Stock(&p.SKUs[i]) != 0 {
			buyable = append(buyable, p.SKUs[i])
		}
	}
	if len(buyable) > 0 {
		skus = buyable
	}
	for _, cur := range []string{model.CNY, model.XTR, model.USDT} {
		var low int64
		for _, k := range skus {
			if v := k.Price(cur); v > 0 && (low == 0 || v < low) {
				low = v
			}
		}
		if low > 0 {
			out := shop.FormatMoney(low, cur)
			if len(skus) > 1 {
				out += " 起"
			}
			return out
		}
	}
	return ""
}

func tierTip(k *model.SKU) string {
	var t []string
	for _, x := range k.Tiers() {
		if x.MinQty >= 2 && x.Percent > 0 && x.Percent < 100 {
			t = append(t, fmt.Sprintf("满 %d 件 %s 折", x.MinQty, strconv.FormatFloat(float64(x.Percent)/10, 'f', -1, 64)))
		}
	}
	return strings.Join(t, " · ")
}

var (
	mdBold = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdCode = regexp.MustCompile("`([^`\n]+)`")
	mdLink = regexp.MustCompile(`\[([^\]\n]+)\]\((https?://[^)\s"]+)\)`)
	mdImg  = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
)

// tgText turns a product description (Markdown) into Telegram HTML,
// shortened to about n characters.
func tgText(md string, n int) string {
	md = mdImg.ReplaceAllString(md, "")
	if r := []rune(strings.TrimSpace(md)); len(r) > n {
		md = string(r[:n]) + "…"
	}
	var out []string
	blank := false
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		bold := false
		switch {
		case strings.HasPrefix(line, "#"):
			line, bold = strings.TrimSpace(strings.TrimLeft(line, "#")), true
		case strings.HasPrefix(line, "- "), strings.HasPrefix(line, "* "):
			line = "• " + line[2:]
		case strings.HasPrefix(line, "> "):
			line = "▎" + line[2:]
		}
		line = html.EscapeString(line)
		line = mdLink.ReplaceAllStringFunc(line, func(m string) string {
			p := mdLink.FindStringSubmatch(m)
			return `<a href="` + p[2] + `">` + p[1] + `</a>`
		})
		line = mdBold.ReplaceAllString(line, "<b>$1</b>")
		line = mdCode.ReplaceAllString(line, "<code>$1</code>")
		if bold {
			line = "<b>" + line + "</b>"
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// ---- screens ----

// show edits the message a button belongs to, or sends a new one. With a
// preview URL (a product cover) the picture is shown above the text.
func (b *Bot) show(ctx context.Context, chat, msgID int64, text string, kb *tgapi.Keyboard, preview ...string) {
	p := map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML", "link_preview_options": map[string]any{"is_disabled": true}}
	if len(preview) > 0 && preview[0] != "" {
		p["link_preview_options"] = map[string]any{"url": preview[0], "prefer_large_media": true, "show_above_text": true}
	}
	if kb != nil {
		p["reply_markup"] = kb
	}
	if msgID != 0 {
		p["message_id"] = msgID
		if err := b.api().Call(ctx, "editMessageText", p, nil); err == nil {
			return
		} else if strings.Contains(err.Error(), "message is not modified") {
			return
		}
		delete(p, "message_id")
	}
	if err := b.api().Call(ctx, "sendMessage", p, nil); err != nil {
		b.log.Warn("telegram send failed", "err", err)
	}
}

func (b *Bot) isAdmin(uid int64) bool {
	return uid != 0 && b.shop.Store.Settings().TGAdminChat == uid
}

func (b *Bot) homeKB() *tgapi.Keyboard { return kbRows(btnRow(btn("🏠 主菜单", "home"))) }

func (b *Bot) home(ctx context.Context, chat, msgID, uid int64) {
	st := b.shop.Store.Settings()
	var t strings.Builder
	fmt.Fprintf(&t, "👋 欢迎来到 <b>%s</b>\n\n", html.EscapeString(st.SiteName))
	t.WriteString("🛒 在这里直接选购、付款\n⚡ 付款后自动发货，内容私信发给你\n📦 随时在「我的订单」里查看")
	if st.Notice != "" {
		t.WriteString("\n\n📢 " + html.EscapeString(st.Notice))
	}
	var pending int64
	if uid != 0 {
		b.shop.Store.DB.Model(&model.Order{}).Where("tg_user_id = ? AND status = ?", uid, model.OrderPending).Count(&pending)
	}
	if pending > 0 {
		fmt.Fprintf(&t, "\n\n⏳ 你有 %d 个待支付订单，可在「我的订单」继续付款。", pending)
	}
	buttons := []tgapi.InlineButton{btn("🛒 选购商品", "cats"), btn("📦 我的订单", "orders:0")}
	if st.PolarisURL != "" {
		buttons = append(buttons, btn("🔁 续费许可证", "renewhelp"))
	}
	buttons = append(buttons, btn("❓ 帮助", "help"))
	rows := grid(buttons, 2)
	if st.SupportURL != "" {
		rows = append(rows, btnRow(tgapi.InlineButton{Text: "💬 联系客服", URL: st.SupportURL}))
	}
	if b.isAdmin(uid) {
		rows = append(rows, btnRow(btn("📊 今日销售（管理员）", "stats")))
	}
	b.show(ctx, chat, msgID, t.String(), kbRows(rows...))
}

func (b *Bot) help(ctx context.Context, chat, msgID int64) {
	st := b.shop.Store.Settings()
	var t strings.Builder
	t.WriteString("❓ <b>使用帮助</b>\n\n")
	t.WriteString("<b>如何购买</b>\n1️⃣ 🛒 选购商品 → 选分类和商品\n2️⃣ 选规格，用 ➖ ➕ 调整数量\n3️⃣ 选支付方式并付款\n4️⃣ 付款后机器人自动把内容私信发给你\n\n")
	var ms []string
	for _, m := range b.shop.Methods(model.ChannelTG) {
		ms = append(ms, methodIcon(m.ID)+" "+m.Label)
	}
	if len(ms) > 0 {
		t.WriteString("<b>支付方式</b>\n" + strings.Join(ms, "\n") + "\n\n")
	}
	t.WriteString("<b>常用命令</b>\n/start 主菜单\n/shop 选购商品\n/orders 我的订单\n/cancel 取消当前操作\n")
	if st.PolarisURL != "" {
		t.WriteString("/renew 许可证编号 — 续费许可证\n")
	}
	t.WriteString("\n未付款的订单 " + strconv.Itoa(max(st.OrderTTL, 5)) + " 分钟后自动关闭，可在「我的订单」里继续付款或取消。")
	rows := [][]tgapi.InlineButton{btnRow(btn("🛒 选购商品", "cats"), btn("📦 我的订单", "orders:0"))}
	if st.SupportURL != "" {
		rows = append(rows, btnRow(tgapi.InlineButton{Text: "💬 联系客服", URL: st.SupportURL}))
	}
	rows = append(rows, navRow("home"))
	b.show(ctx, chat, msgID, t.String(), kbRows(rows...))
}

func (b *Bot) renewHelp(ctx context.Context, chat, msgID int64) {
	b.show(ctx, chat, msgID, "🔁 <b>续费许可证</b>\n\n在 Polaris 面板「许可证」页点击「续费」，会直接打开这里。\n\n也可以发送：\n<code>/renew 许可证编号</code>\n例如 <code>/renew lic_xxxx</code>",
		kbRows(navRow("home")))
}

func (b *Bot) categories(ctx context.Context, chat, msgID int64) {
	cats, err := b.shop.Catalog()
	var shown []shop.CategoryView
	for _, c := range cats {
		if len(c.Products) > 0 {
			shown = append(shown, c)
		}
	}
	if err != nil || len(shown) == 0 {
		b.show(ctx, chat, msgID, "🛒 暂无在售商品，晚点再来看看。", kbRows(navRow("home")))
		return
	}
	if len(shown) == 1 {
		b.products(ctx, chat, msgID, shown[0].ID, 0)
		return
	}
	var bs []tgapi.InlineButton
	for _, c := range shown {
		bs = append(bs, btn(fmt.Sprintf("%s%s (%d)", catIcon(c.Name), c.Name, len(c.Products)), fmt.Sprintf("c:%d:0", c.ID)))
	}
	rows := grid(bs, 2)
	rows = append(rows, navRow("home"))
	b.show(ctx, chat, msgID, "🛒 <b>选购商品</b>\n\n请选择分类：", kbRows(rows...))
}

func (b *Bot) products(ctx context.Context, chat, msgID int64, catID uint, page int) {
	cats, _ := b.shop.Catalog()
	var cat *shop.CategoryView
	multi := 0
	for i := range cats {
		if len(cats[i].Products) > 0 {
			multi++
		}
		if cats[i].ID == catID {
			cat = &cats[i]
		}
	}
	if cat == nil || len(cat.Products) == 0 {
		b.show(ctx, chat, msgID, "这个分类暂无在售商品。", kbRows(navRow("cats")))
		return
	}
	pages := (len(cat.Products) + pageSize - 1) / pageSize
	page = min(max(page, 0), pages-1)
	var rows [][]tgapi.InlineButton
	for _, p := range cat.Products[page*pageSize : min((page+1)*pageSize, len(cat.Products))] {
		full, err := b.shop.Product(p.ID)
		if err != nil {
			continue
		}
		label := productIcon(full) + full.Title
		if lp := b.lowPrice(full); lp != "" {
			label += " · " + lp
		}
		if b.soldOut(full) {
			label += " · 售罄"
		}
		rows = append(rows, btnRow(btn(label, fmt.Sprintf("p:%d", p.ID))))
	}
	if pg := pager(fmt.Sprintf("c:%d", catID), page, pages); pg != nil {
		rows = append(rows, pg)
	}
	backTo := "cats"
	if multi <= 1 {
		backTo = "home"
	}
	rows = append(rows, navRow(backTo))
	b.show(ctx, chat, msgID, fmt.Sprintf("%s<b>%s</b>\n\n共 %d 件商品，点击查看详情：", catIcon(cat.Name), html.EscapeString(cat.Name), len(cat.Products)), kbRows(rows...))
}

func (b *Bot) soldOut(p *model.Product) bool {
	for i := range p.SKUs {
		if b.shop.Stock(&p.SKUs[i]) != 0 {
			return false
		}
	}
	return true
}

func manualOnly(p *model.Product) bool {
	for _, k := range p.SKUs {
		if k.DelivererKind != "manual" {
			return false
		}
	}
	return len(p.SKUs) > 0
}

func (b *Bot) coverURL(p *model.Product) string {
	c := strings.TrimSpace(p.Cover)
	if strings.HasPrefix(c, "/") {
		base := strings.TrimRight(b.shop.Store.Settings().PublicURL, "/")
		if !strings.HasPrefix(base, "https://") {
			return ""
		}
		c = base + c
	}
	if !strings.HasPrefix(c, "https://") {
		return ""
	}
	return c
}

// product shows a product and its specs.
func (b *Bot) product(ctx context.Context, chat, msgID int64, prodID uint) {
	p, err := b.shop.Product(prodID)
	if err != nil {
		b.show(ctx, chat, msgID, "😕 商品不存在或已下架。", kbRows(navRow("cats")))
		return
	}
	var t strings.Builder
	fmt.Fprintf(&t, "%s<b>%s</b>\n\n", productIcon(p), html.EscapeString(p.Title))
	var facts []string
	if lp := b.lowPrice(p); lp != "" {
		facts = append(facts, "💰 "+lp)
	}
	if manualOnly(p) {
		facts = append(facts, "🙋 人工发货")
	} else {
		facts = append(facts, "⚡ 自动发货")
	}
	ids := make([]uint, len(p.SKUs))
	for i, k := range p.SKUs {
		ids[i] = k.ID
	}
	if n := b.shop.Sold(ids); n > 0 {
		facts = append(facts, fmt.Sprintf("🔥 已售 %d", n))
	}
	t.WriteString(strings.Join(facts, "  ·  "))
	if d := tgText(p.DescriptionMD, 700); d != "" {
		t.WriteString("\n\n" + d)
	}
	var rows [][]tgapi.InlineButton
	if len(p.SKUs) == 0 {
		t.WriteString("\n\n暂时没有可购买的规格。")
	} else {
		t.WriteString("\n\n👇 请选择规格：")
	}
	for i := range p.SKUs {
		k := &p.SKUs[i]
		n := b.shop.Stock(k)
		label := k.Name + " · " + priceLine(k)
		data := fmt.Sprintf("s:%d:1", k.ID)
		switch {
		case n == 0:
			label, data = "🚫 "+label+" · 缺货", "noop:out"
		case n > 0 && n <= 10:
			label += fmt.Sprintf(" · 剩 %d", n)
		}
		rows = append(rows, btnRow(btn(label, data)))
	}
	if b.soldOut(p) && len(p.SKUs) > 0 {
		if st := b.shop.Store.Settings(); st.SupportURL != "" {
			rows = append(rows, btnRow(tgapi.InlineButton{Text: "💬 询问补货", URL: st.SupportURL}))
		}
	}
	rows = append(rows, navRow(fmt.Sprintf("c:%d:0", p.CategoryID)))
	b.show(ctx, chat, msgID, t.String(), kbRows(rows...), b.coverURL(p))
}

func (b *Bot) loadSKU(id uint) (*model.SKU, error) {
	var k model.SKU
	if err := b.shop.Store.DB.Where("id = ? AND active = ?", id, true).Take(&k).Error; err != nil {
		return nil, errors.New("规格不存在或已下架")
	}
	return &k, nil
}

// confirm is the order screen: quantity stepper, totals per payment
// method, and the buttons to pay.
func (b *Bot) confirm(ctx context.Context, chat, msgID int64, skuID uint, qty int) {
	k, err := b.loadSKU(skuID)
	if err != nil {
		b.show(ctx, chat, msgID, "😕 "+err.Error(), kbRows(navRow("cats")))
		return
	}
	var p model.Product
	b.shop.Store.DB.Take(&p, k.ProductID)
	limit := min(max(k.LimitPerOrder, 1), maxQtyButton)
	stock := b.shop.Stock(k)
	if stock >= 0 {
		limit = min(limit, stock)
	}
	if limit <= 0 {
		b.show(ctx, chat, msgID, "🚫 该规格已缺货。", kbRows(navRow(fmt.Sprintf("p:%d", k.ProductID))))
		return
	}
	qty = min(max(qty, 1), limit)
	var t strings.Builder
	fmt.Fprintf(&t, "🧾 <b>确认订单</b>\n\n📦 %s\n🏷️ 规格：%s\n💰 单价：%s\n🔢 数量：<b>%d</b>", html.EscapeString(p.Title), html.EscapeString(k.Name), priceLine(k), qty)
	if tip := tierTip(k); tip != "" {
		t.WriteString("\n🎁 批发价：" + tip)
	}
	if stock > 0 {
		fmt.Fprintf(&t, "\n📊 库存：%d", stock)
	}
	ms := b.shop.MethodsFor(model.ChannelTG, k)
	var payRows [][]tgapi.InlineButton
	if len(ms) > 0 {
		t.WriteString("\n\n<b>应付金额</b>")
	}
	for _, m := range ms {
		q, err := b.shop.Preview(k, qty, m.Currency)
		if err != nil {
			continue
		}
		amount := shop.FormatMoney(q.Amount, m.Currency)
		line := fmt.Sprintf("\n%s %s：<b>%s</b>", methodIcon(m.ID), m.Label, amount)
		if q.Amount < q.Unit*int64(qty) {
			line += "（已享批发价）"
		}
		t.WriteString(line)
		payRows = append(payRows, btnRow(btn(fmt.Sprintf("%s %s 支付 %s", methodIcon(m.ID), m.Label, amount), fmt.Sprintf("m:%d:%d:%s", k.ID, qty, m.ID))))
	}
	if len(payRows) == 0 {
		t.WriteString("\n\n⚠️ 该规格暂无可用的支付方式，请联系客服。")
	}
	if ask := b.askFor(k.ID); ask != "" && len(payRows) > 0 {
		t.WriteString("\n\n✍️ 选择支付方式后，需要填写「" + html.EscapeString(ask) + "」。")
	}
	rows := [][]tgapi.InlineButton{}
	if limit > 1 {
		step := func(label string, to int) tgapi.InlineButton {
			if to < 1 || to > limit || to == qty {
				return btn(label, "noop:qty")
			}
			return btn(label, fmt.Sprintf("s:%d:%d", k.ID, to))
		}
		rows = append(rows, btnRow(step("➖", qty-1), btn(fmt.Sprintf("🔢 %d 件", qty), "noop:qty"), step("➕", qty+1)))
		if limit >= 5 {
			var quick []tgapi.InlineButton
			for _, n := range []int{1, 5, 10, 20} {
				if n <= limit {
					quick = append(quick, step(fmt.Sprintf("%d 件", n), n))
				}
			}
			if limit > 1 && limit != 5 && limit != 10 && limit != 20 {
				quick = append(quick, step(fmt.Sprintf("最多 %d", limit), limit))
			}
			rows = append(rows, quick)
		}
	}
	rows = append(rows, payRows...)
	rows = append(rows, navRow(fmt.Sprintf("p:%d", k.ProductID)))
	b.show(ctx, chat, msgID, t.String(), kbRows(rows...))
}

// ---- orders ----

// payView is how to pay an order, with its buttons.
func (b *Bot) payView(o *model.Order) (string, *tgapi.Keyboard, error) {
	left := time.Until(o.ExpiresAt).Round(time.Minute)
	head := fmt.Sprintf("🧾 <b>订单已创建</b>\n\n🔖 订单号：<code>%s</code>\n📦 %s · %s ×%d\n💰 应付：<b>%s</b>\n⏰ 请在 %d 分钟内完成支付",
		o.OrderNo, html.EscapeString(o.ProductTitle), html.EscapeString(o.SKUName), o.Qty, shop.FormatMoney(o.Amount, o.Currency), max(int(left.Minutes()), 1))
	manage := btnRow(btn("🔄 刷新状态", "o:"+o.OrderNo), btn("❌ 取消订单", "oc:"+o.OrderNo))
	switch o.PayMethod {
	case model.PayStars:
		return head + "\n\n⭐ 请点击下方的 Stars 账单完成支付。", kbRows(manage, navRow("orders:0")), nil
	case model.PayUSDT:
		addr := b.shop.Store.Settings().USDTAddress
		amount := payment.Decimal(o.PayAmount, 6)
		text := head + fmt.Sprintf("\n\n💵 请向下面的地址转账 <b>恰好 %s USDT</b>（TRC20）：\n<code>%s</code>\n\n⚠️ 金额尾数用于识别订单，请勿修改。链上确认后自动发货。", amount, addr)
		return text, kbRows(btnRow(tgapi.CopyButton("📋 复制金额", amount), tgapi.CopyButton("📋 复制地址", addr)), manage, navRow("orders:0")), nil
	default:
		base := strings.TrimRight(b.shop.Store.Settings().PublicURL, "/")
		if !strings.HasPrefix(base, "https://") {
			return "", nil, errors.New("站点地址未配置，无法使用该支付方式")
		}
		return head + "\n\n" + methodIcon(o.PayMethod) + " 点击「前往付款」完成支付，付款后回到这里点「刷新状态」。",
			kbRows(btnRow(tgapi.InlineButton{Text: methodIcon(o.PayMethod) + " 前往付款", URL: base + "/pay/" + o.OrderNo}), manage, navRow("orders:0")), nil
	}
}

func (b *Bot) sendInvoice(ctx context.Context, chat int64, o *model.Order) error {
	title := o.ProductTitle
	if r := []rune(title); len(r) > 32 {
		title = string(r[:32])
	}
	desc := o.SKUName + " ×" + strconv.Itoa(o.Qty)
	return b.api().Call(ctx, "sendInvoice", map[string]any{
		"chat_id": chat, "title": title, "description": desc, "payload": o.OrderNo, "currency": "XTR",
		"prices": []map[string]any{{"label": desc, "amount": o.PayAmount}},
	}, nil)
}

// order creates an order and shows how to pay. It returns a toast for
// errors.
func (b *Bot) order(ctx context.Context, chat, msgID int64, from tgapi.User, skuID uint, qty int, method, renewOf, ask string) string {
	fail := func(t string) string {
		b.show(ctx, chat, msgID, "⚠️ "+html.EscapeString(t), kbRows(navRow(fmt.Sprintf("s:%d:%d", skuID, qty))))
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
			return fail(ue.Msg)
		}
		b.log.Error("telegram order failed", "err", err)
		return fail("下单失败，请稍后再试")
	}
	if renewOf != "" {
		b.setSession(from.ID, nil)
	}
	if o.Status != model.OrderPending {
		b.show(ctx, chat, msgID, fmt.Sprintf("✅ 订单 <code>%s</code> 已创建，正在发货，内容会私信发给你。", o.OrderNo),
			kbRows(btnRow(btn("🧾 查看订单", "o:"+o.OrderNo)), navRow("home")))
		return ""
	}
	text, kb, err := b.payView(o)
	if err != nil {
		return fail(err.Error())
	}
	b.show(ctx, chat, msgID, text, kb)
	if method == model.PayStars {
		if err := b.sendInvoice(ctx, chat, o); err != nil {
			b.log.Warn("send invoice failed", "err", err)
			return "无法创建 Stars 账单"
		}
	}
	return ""
}

func (b *Bot) orders(ctx context.Context, chat, msgID, uid int64, page int) {
	var total int64
	q := b.shop.Store.DB.Model(&model.Order{}).Where("tg_user_id = ?", uid)
	q.Count(&total)
	if total == 0 {
		b.show(ctx, chat, msgID, "📦 <b>我的订单</b>\n\n你还没有订单。", kbRows(btnRow(btn("🛒 去选购", "cats")), navRow("home")))
		return
	}
	pages := int((total + orderPage - 1) / orderPage)
	page = min(max(page, 0), pages-1)
	var list []model.Order
	q.Order("id DESC").Offset(page * orderPage).Limit(orderPage).Find(&list)
	var rows [][]tgapi.InlineButton
	for _, o := range list {
		rows = append(rows, btnRow(btn(fmt.Sprintf("%s %s ×%d · %s · %s", statusIcon(o.Status), o.ProductTitle, o.Qty, shop.FormatMoney(o.Amount, o.Currency), statusText(o.Status)), "o:"+o.OrderNo)))
	}
	if pg := pager("orders", page, pages); pg != nil {
		rows = append(rows, pg)
	}
	rows = append(rows, navRow("home"))
	b.show(ctx, chat, msgID, fmt.Sprintf("📦 <b>我的订单</b>（共 %d 个）\n\n点击订单查看详情、继续付款或查看发货内容。", total), kbRows(rows...))
}

// orderView shows one order with what can be done with it.
func (b *Bot) orderView(ctx context.Context, chat, msgID, uid int64, no string) string {
	o, err := b.shop.OrderByNo(no)
	if err != nil || o.TGUserID != uid {
		b.show(ctx, chat, msgID, "😕 订单不存在。", kbRows(navRow("orders:0")))
		return ""
	}
	toast := ""
	if o.Status == model.OrderPending {
		if paid, _ := b.shop.CheckEPay(ctx, o, 5*time.Second); paid {
			o, _ = b.shop.OrderByNo(no)
			toast = "已确认付款，正在发货"
		}
	}
	var t strings.Builder
	fmt.Fprintf(&t, "🧾 <b>订单详情</b>\n\n🔖 订单号：<code>%s</code>\n📦 %s · %s ×%d\n💰 金额：%s\n%s 状态：<b>%s</b>\n🕐 下单：%s",
		o.OrderNo, html.EscapeString(o.ProductTitle), html.EscapeString(o.SKUName), o.Qty, shop.FormatMoney(o.Amount, o.Currency),
		statusIcon(o.Status), statusText(o.Status), o.CreatedAt.Local().Format("2006-01-02 15:04"))
	if o.Remark != "" {
		t.WriteString("\n📝 " + html.EscapeString(o.Remark))
	}
	var rows [][]tgapi.InlineButton
	switch o.Status {
	case model.OrderPending:
		fmt.Fprintf(&t, "\n⏰ %s 前未付款将自动关闭", o.ExpiresAt.Local().Format("15:04"))
		rows = append(rows, btnRow(btn("💳 继续付款", "op:"+o.OrderNo), btn("🔄 刷新", "o:"+o.OrderNo)), btnRow(btn("❌ 取消订单", "oc:"+o.OrderNo)))
	case model.OrderPaid, model.OrderFailed:
		t.WriteString("\n\n🚚 已付款，正在处理，发货后会私信通知你。")
		rows = append(rows, btnRow(btn("🔄 刷新", "o:"+o.OrderNo)))
	case model.OrderDelivered:
		rows = append(rows, btnRow(btn("📄 查看发货内容", "oi:"+o.OrderNo)), btnRow(btn("🛒 再来一单", fmt.Sprintf("s:%d:%d", o.SKUID, o.Qty))))
	case model.OrderExpired:
		rows = append(rows, btnRow(btn("🛒 重新下单", fmt.Sprintf("s:%d:%d", o.SKUID, o.Qty))))
	}
	if st := b.shop.Store.Settings(); st.SupportURL != "" && o.Status != model.OrderPending {
		rows = append(rows, btnRow(tgapi.InlineButton{Text: "💬 售后客服", URL: st.SupportURL}))
	}
	rows = append(rows, navRow("orders:0"))
	b.show(ctx, chat, msgID, t.String(), kbRows(rows...))
	return toast
}

func (b *Bot) continuePay(ctx context.Context, chat, msgID, uid int64, no string) string {
	o, err := b.shop.OrderByNo(no)
	if err != nil || o.TGUserID != uid {
		return "订单不存在"
	}
	if o.Status != model.OrderPending {
		b.orderView(ctx, chat, msgID, uid, no)
		return "订单已付款或已关闭"
	}
	text, kb, err := b.payView(o)
	if err != nil {
		return err.Error()
	}
	b.show(ctx, chat, msgID, text, kb)
	if o.PayMethod == model.PayStars {
		if err := b.sendInvoice(ctx, chat, o); err != nil {
			return "无法创建 Stars 账单"
		}
	}
	return ""
}

func (b *Bot) orderItems(ctx context.Context, chat, uid int64, no string) string {
	o, err := b.shop.OrderByNo(no)
	if err != nil || o.TGUserID != uid || o.Status != model.OrderDelivered {
		return "订单不存在或还没有发货"
	}
	items, err := b.shop.Items(o.ID)
	if err != nil {
		return "读取失败，请稍后再试"
	}
	if err := b.shop.SendItemsTG(ctx, chat, o, items); err != nil {
		b.log.Warn("send items failed", "err", err)
		return "发送失败，请稍后再试"
	}
	return "已发送"
}

func (b *Bot) stats(ctx context.Context, chat, msgID, uid int64) {
	if !b.isAdmin(uid) {
		b.home(ctx, chat, msgID, uid)
		return
	}
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rev, _ := b.shop.RevenueSince(today)
	var t strings.Builder
	t.WriteString("📊 <b>今日销售</b>\n\n")
	if len(rev) == 0 {
		t.WriteString("暂无成交")
	}
	for _, r := range rev {
		fmt.Fprintf(&t, "💰 %s：%d 单 · %s\n", r.Currency, r.Orders, shop.FormatMoney(r.Amount, r.Currency))
	}
	var failed, waiting, pending int64
	b.shop.Store.DB.Model(&model.Order{}).Where("status = ?", model.OrderFailed).Count(&failed)
	b.shop.Store.DB.Model(&model.Order{}).Where("status = ?", model.OrderPaid).Count(&waiting)
	b.shop.Store.DB.Model(&model.Order{}).Where("status = ?", model.OrderPending).Count(&pending)
	fmt.Fprintf(&t, "\n🚚 待发货 %d\n🛠️ 发货失败 %d\n⏳ 待支付 %d", waiting, failed, pending)
	b.show(ctx, chat, msgID, t.String(), kbRows(btnRow(btn("🔄 刷新", "stats")), navRow("home")))
}

// ---- input ----

func (b *Bot) message(ctx context.Context, m *tgapi.Message) {
	uid := m.From.ID
	text := strings.TrimSpace(m.Text)
	if s := b.getSession(uid); s != nil && !strings.HasPrefix(text, "/") {
		b.continueSession(ctx, m, s)
		return
	}
	cmd, arg, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(cmd, "@")
	chat := m.Chat.ID
	switch cmd {
	case "/start":
		b.setSession(uid, nil)
		if lid, ok := strings.CutPrefix(strings.TrimSpace(arg), "renew_"); ok && lid != "" {
			b.startRenew(ctx, chat, uid, lid)
			return
		}
		b.home(ctx, chat, 0, uid)
	case "/shop":
		b.categories(ctx, chat, 0)
	case "/orders":
		b.orders(ctx, chat, 0, uid, 0)
	case "/help":
		b.help(ctx, chat, 0)
	case "/cancel":
		b.setSession(uid, nil)
		b.send(ctx, chat, "✅ 已取消当前操作。", b.homeKB())
	case "/stats":
		b.stats(ctx, chat, 0, uid)
	case "/renew":
		if arg = strings.TrimSpace(arg); arg != "" {
			b.startRenew(ctx, chat, uid, arg)
			return
		}
		b.renewHelp(ctx, chat, 0)
	default:
		b.home(ctx, chat, 0, uid)
	}
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
	arg := func(i int) string {
		if i >= len(parts) {
			return ""
		}
		return parts[i]
	}
	switch parts[0] {
	case "home":
		b.home(ctx, chat, msg, uid)
	case "help":
		b.help(ctx, chat, msg)
	case "renewhelp":
		b.renewHelp(ctx, chat, msg)
	case "stats":
		b.stats(ctx, chat, msg, uid)
	case "cats":
		b.categories(ctx, chat, msg)
	case "c":
		b.products(ctx, chat, msg, uint(num(1)), num(2))
	case "p":
		b.product(ctx, chat, msg, uint(num(1)))
	case "s":
		b.confirm(ctx, chat, msg, uint(num(1)), max(num(2), 1))
	case "orders":
		b.orders(ctx, chat, msg, uid, num(1))
	case "o":
		answer = b.orderView(ctx, chat, msg, uid, arg(1))
	case "op":
		answer = b.continuePay(ctx, chat, msg, uid, arg(1))
	case "oi":
		answer = b.orderItems(ctx, chat, uid, arg(1))
	case "oc":
		if err := b.shop.CancelUnpaid(arg(1), uid); err != nil {
			answer = userMsg(err)
		} else {
			answer = "订单已取消"
		}
		b.orderView(ctx, chat, msg, uid, arg(1))
	case "x":
		b.setSession(uid, nil)
		answer = "已取消"
		b.home(ctx, chat, msg, uid)
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
				b.show(ctx, chat, msg, fmt.Sprintf("✍️ 请直接发送<b>%s</b>：\n\n（1–100 字，发送后自动下单）", html.EscapeString(ask)),
					kbRows(btnRow(btn("❌ 取消", fmt.Sprintf("s:%d:%d", num(1), num(2))))))
				return
			}
		}
		answer = b.order(ctx, chat, msg, q.From, uint(num(1)), num(2), parts[3], renewOf, "")
	case "r":
		s := b.getSession(uid)
		if s == nil || s.Step != "renew_ready" {
			answer = "会话已过期，请重新打开续费链接"
			return
		}
		b.methods(ctx, chat, msg, uint(num(1)), 1)
	case "noop":
		switch arg(1) {
		case "out":
			answer = "该规格已缺货"
		case "qty":
			answer = "已到数量上限或下限"
		}
	}
	// leaving the order screen ends a pending "send your account" step
	if parts[0] == "s" {
		if s := b.getSession(uid); s != nil && s.Step == "ask" {
			b.setSession(uid, nil)
		}
	}
}

func userMsg(err error) string {
	var ue *shop.UserError
	if errors.As(err, &ue) {
		return ue.Msg
	}
	return "操作失败，请稍后再试"
}

// methods lists the payment methods for a renewal.
func (b *Bot) methods(ctx context.Context, chat, msgID int64, skuID uint, qty int) {
	k, err := b.loadSKU(skuID)
	if err != nil {
		b.show(ctx, chat, msgID, "😕 "+err.Error(), b.homeKB())
		return
	}
	ms := b.shop.MethodsFor(model.ChannelTG, k)
	if len(ms) == 0 {
		b.show(ctx, chat, msgID, "⚠️ 该规格暂无可用的支付方式，请联系客服。", b.homeKB())
		return
	}
	var rows [][]tgapi.InlineButton
	for _, m := range ms {
		rows = append(rows, btnRow(btn(fmt.Sprintf("%s %s 支付 %s", methodIcon(m.ID), m.Label, shop.FormatMoney(k.Price(m.Currency)*int64(qty), m.Currency)),
			fmt.Sprintf("rm:%d:%d:%s", skuID, qty, m.ID))))
	}
	rows = append(rows, btnRow(btn("❌ 取消", "x")))
	b.show(ctx, chat, msgID, fmt.Sprintf("🔁 <b>续费</b> · %s\n\n💳 请选择支付方式：", html.EscapeString(k.Name)), kbRows(rows...))
}

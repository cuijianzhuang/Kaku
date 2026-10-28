package shop

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/store"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
)

// deliveredJob tells the buyer: an email with the content, and for
// Telegram orders a private message (files as documents).
func (s *Service) deliveredJob(ctx context.Context, j *model.Job) error {
	var o model.Order
	if err := s.Store.DB.Where("id = ?", strings.TrimSpace(j.Payload)).Take(&o).Error; err != nil {
		return err
	}
	if o.Status != model.OrderDelivered {
		return nil
	}
	items, err := s.Items(o.ID)
	if err != nil {
		return err
	}
	var errs []string
	if o.Email != "" && s.Notify.MailReady() {
		if err := s.Notify.Mail(o.Email, fmt.Sprintf("【%s】订单 %s 已发货", s.Store.Settings().SiteName, o.OrderNo), s.mailBody(&o, items)); err != nil {
			errs = append(errs, "邮件："+err.Error())
		}
	}
	if o.TGUserID != 0 && s.Bot().Ready() {
		if err := s.SendItemsTG(ctx, o.TGUserID, &o, items); err != nil {
			errs = append(errs, "Telegram："+err.Error())
		}
	}
	if len(errs) > 0 {
		err := fmt.Errorf("%s", strings.Join(errs, "；"))
		if j.Attempts < 3 {
			return &store.RetryError{Err: err, After: time.Duration(j.Attempts+1) * 2 * time.Minute}
		}
		return err
	}
	return nil
}

func (s *Service) orderURL(o *model.Order) string {
	base := strings.TrimRight(s.Store.Settings().PublicURL, "/")
	if base == "" {
		return ""
	}
	return base + "/order/" + o.OrderNo
}

func (s *Service) mailBody(o *model.Order, items []ItemView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "您好，\n\n您的订单已发货。\n\n订单号：%s\n商品：%s %s ×%d\n金额：%s\n\n", o.OrderNo, o.ProductTitle, o.SKUName, o.Qty, FormatMoney(o.Amount, o.Currency))
	b.WriteString("发货内容：\n")
	for i, it := range items {
		if it.FileName != "" {
			fmt.Fprintf(&b, "%d. 文件 %s（请在订单页下载）\n", i+1, it.FileName)
			continue
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, it.Content)
	}
	for _, it := range items {
		if it.Source == model.SourcePolaris && it.FileName == "" && o.RenewOf == "" {
			b.WriteString("\n激活方法：打开 Polaris 面板「许可证」页，粘贴激活码并点击激活。\n")
			break
		}
	}
	for _, it := range items {
		if it.Source == model.SourcePanel {
			b.WriteString("\n兑换方法：" + s.PanelRedeemHint() + "\n")
			break
		}
	}
	if u := s.orderURL(o); u != "" {
		fmt.Fprintf(&b, "\n订单页面：%s（凭下单时设置的查询密码查看）\n", u)
	}
	b.WriteString("\n请妥善保管，此邮件请勿转发。\n")
	return b.String()
}

// SendItemsTG sends delivered content to a Telegram user.
func (s *Service) SendItemsTG(ctx context.Context, chatID int64, o *model.Order, items []ItemView) error {
	bot := s.Bot()
	var b strings.Builder
	fmt.Fprintf(&b, "✅ <b>订单已发货</b>\n%s · %s ×%d\n订单号 <code>%s</code>\n", html.EscapeString(o.ProductTitle), html.EscapeString(o.SKUName), o.Qty, o.OrderNo)
	var kb tgapi.Keyboard
	var files []ItemView
	for i, it := range items {
		if it.FileName != "" {
			files = append(files, it)
			continue
		}
		fmt.Fprintf(&b, "\n%d. <code>%s</code>", i+1, html.EscapeString(it.Content))
		if len(it.Content) <= 256 && len(kb.InlineKeyboard) < 10 {
			kb.InlineKeyboard = append(kb.InlineKeyboard, []tgapi.InlineButton{tgapi.CopyButton(fmt.Sprintf("复制 %d", i+1), it.Content)})
		}
	}
	for _, it := range items {
		if it.Source == model.SourcePanel {
			b.WriteString("\n\n兑换方法：" + html.EscapeString(s.PanelRedeemHint()))
			break
		}
	}
	text := b.String()
	if len(text) > 4000 {
		text = text[:4000] + "…\n（内容过长，请在订单页查看）"
	}
	var markup *tgapi.Keyboard
	if len(kb.InlineKeyboard) > 0 {
		markup = &kb
	}
	if err := bot.SendMessage(ctx, chatID, text, true, markup); err != nil {
		return err
	}
	for _, f := range files {
		if err := bot.SendDocument(ctx, chatID, f.FileName, []byte(f.Content), "离线许可证文件"); err != nil {
			return err
		}
	}
	return nil
}

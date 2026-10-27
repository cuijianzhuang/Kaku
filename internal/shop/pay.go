package shop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/payment"
	"github.com/cuijianzhuang/kaku/internal/store"
)

// PaidInput is a confirmed payment.
type PaidInput struct {
	OrderNo  string
	Channel  string // payment method that collected it
	TradeNo  string // gateway transaction id; made unique per channel
	Amount   int64
	Currency string
	Raw      string
	// Trusted skips the amount check (admin 补单, free orders).
	Trusted bool
}

// ErrAmountMismatch means the gateway reported a different amount.
var ErrAmountMismatch = errors.New("支付金额与订单不一致")

// MarkPaid records a payment and queues delivery. It is idempotent: the
// trade number is unique, and only a pending (or lapsed) order moves to
// paid, so duplicate callbacks never deliver twice.
func (s *Service) MarkPaid(ctx context.Context, in PaidInput) (*model.Order, error) {
	var (
		o         model.Order
		duplicate bool
		extra     bool
	)
	tradeNo := in.Channel + ":" + in.TradeNo
	err := s.Store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("order_no = ?", in.OrderNo).Take(&o).Error; err != nil {
			return userErr("订单不存在")
		}
		if !in.Trusted {
			if in.Currency != o.Currency || in.Amount != o.PayAmount {
				return fmt.Errorf("%w：订单 %s 应付 %d %s，实付 %d %s", ErrAmountMismatch, o.OrderNo, o.PayAmount, o.Currency, in.Amount, in.Currency)
			}
		}
		var n int64
		tx.Model(&model.Payment{}).Where("trade_no = ?", tradeNo).Count(&n)
		if n > 0 {
			duplicate = true
			return nil
		}
		amount, cur := in.Amount, in.Currency
		if in.Trusted {
			amount, cur = o.PayAmount, o.Currency
		}
		if err := tx.Create(&model.Payment{OrderID: o.ID, Channel: in.Channel, TradeNo: tradeNo, Amount: amount,
			Currency: cur, RawNotify: in.Raw, Status: model.PaymentPaid}).Error; err != nil {
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				duplicate = true
				return nil
			}
			return err
		}
		now := s.Now()
		res := tx.Model(&model.Order{}).Where("id = ? AND status IN ?", o.ID, []string{model.OrderPending, model.OrderExpired}).
			Updates(map[string]any{"status": model.OrderPaid, "paid_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// Paid twice (second channel or a second transfer): keep the
			// payment for a refund, deliver nothing more.
			extra = true
			return nil
		}
		o.Status, o.PaidAt = model.OrderPaid, &now
		return store.Enqueue(tx, "deliver", fmt.Sprintf("deliver:%d", o.ID), o.ID, now)
	})
	if err != nil {
		return nil, err
	}
	switch {
	case duplicate:
	case extra:
		s.Notify.Admin(fmt.Sprintf("⚠️ 订单 %s 收到重复付款（%s %s），请核对后退款", o.OrderNo, in.Channel, in.TradeNo))
	default:
		s.Log.Info("order paid", "order", o.OrderNo, "channel", in.Channel)
		go s.Notify.Admin(fmt.Sprintf("💰 新订单已支付\n%s · %s ×%d\n%s · %s", o.OrderNo, o.ProductTitle+" "+o.SKUName, o.Qty,
			FormatMoney(o.Amount, o.Currency), o.PayMethod))
	}
	return &o, nil
}

// backoff is the delay before each delivery retry; when it runs out the
// order fails over to an admin.
var backoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}

// JobHandlers returns the background job handlers.
func (s *Service) JobHandlers() map[string]store.JobFunc {
	return map[string]store.JobFunc{
		"deliver":   s.deliverJob,
		"delivered": s.deliveredJob,
	}
}

func (s *Service) deliverJob(ctx context.Context, j *model.Job) error {
	var o model.Order
	if err := s.Store.DB.Where("id = ?", strings.TrimSpace(j.Payload)).Take(&o).Error; err != nil {
		return err
	}
	if o.Status != model.OrderPaid {
		return nil
	}
	var sku model.SKU
	if err := s.Store.DB.Where("id = ?", o.SKUID).Take(&sku).Error; err != nil {
		return s.failOrder(&o, "商品规格已删除")
	}
	d, err := s.Reg.Get(sku.DelivererKind)
	if err != nil {
		return s.failOrder(&o, err.Error())
	}
	s.Store.DB.Model(&model.Order{}).Where("id = ?", o.ID).Update("deliver_attempts", gorm.Expr("deliver_attempts + 1"))
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	items, err := d.Deliver(cctx, &o, &sku)
	var perm *deliver.PermanentError
	switch {
	case errors.Is(err, deliver.ErrManual):
		s.Store.DB.Model(&model.Order{}).Where("id = ?", o.ID).Update("last_error", "等待人工发货")
		go s.Notify.Admin(fmt.Sprintf("✋ 订单 %s 等待人工发货：%s %s ×%d", o.OrderNo, o.ProductTitle, o.SKUName, o.Qty))
		return nil
	case errors.As(err, &perm):
		return s.failOrder(&o, err.Error())
	case err != nil:
		s.Store.DB.Model(&model.Order{}).Where("id = ?", o.ID).Update("last_error", trim(err.Error(), 500))
		if j.Attempts < len(backoff) {
			s.Log.Warn("delivery failed, will retry", "order", o.OrderNo, "attempt", j.Attempts+1, "err", err)
			return &store.RetryError{Err: err, After: backoff[j.Attempts]}
		}
		return s.failOrder(&o, err.Error())
	}
	return s.complete(&o, items)
}

// complete stores delivered items and marks the order delivered, once.
func (s *Service) complete(o *model.Order, items []deliver.Item) error {
	now := s.Now()
	err := s.Store.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Order{}).Where("id = ? AND status IN ?", o.ID, []string{model.OrderPaid, model.OrderFailed}).
			Updates(map[string]any{"status": model.OrderDelivered, "delivered_at": now, "last_error": ""})
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		for _, it := range items {
			if err := tx.Create(&model.OrderItem{OrderID: o.ID, ContentEnc: s.Store.Box.Seal(it.Content), Source: it.Source,
				RefID: it.RefID, FileName: it.FileName}).Error; err != nil {
				return err
			}
		}
		return store.Enqueue(tx, "delivered", fmt.Sprintf("delivered:%d", o.ID), o.ID, now)
	})
	if err == nil {
		s.Log.Info("order delivered", "order", o.OrderNo, "items", len(items))
		s.checkLowStock(o.SKUID)
	}
	return err
}

func (s *Service) failOrder(o *model.Order, reason string) error {
	s.Store.DB.Model(&model.Order{}).Where("id = ? AND status = ?", o.ID, model.OrderPaid).
		Updates(map[string]any{"status": model.OrderFailed, "last_error": trim(reason, 500)})
	s.Log.Error("delivery failed", "order", o.OrderNo, "reason", reason)
	go s.Notify.Admin(fmt.Sprintf("❌ 订单 %s 发货失败，需要处理：%s", o.OrderNo, reason))
	return nil
}

func (s *Service) checkLowStock(skuID uint) {
	var sku model.SKU
	if s.Store.DB.Where("id = ?", skuID).Take(&sku).Error != nil || sku.DelivererKind != "stock" || sku.LowStock <= 0 {
		return
	}
	if sku.StockCache <= sku.LowStock {
		go s.Notify.Admin(fmt.Sprintf("📦 库存不足：%s（SKU #%d）剩余 %d", sku.Name, sku.ID, sku.StockCache))
	}
}

// ManualDeliver delivers typed content (one item per non-empty line, or
// the whole text as one item).
func (s *Service) ManualDeliver(orderID uint, content string, perLine bool) error {
	var o model.Order
	if err := s.Store.DB.Where("id = ?", orderID).Take(&o).Error; err != nil {
		return err
	}
	if o.Status != model.OrderPaid && o.Status != model.OrderFailed {
		return userErr("只有已支付未发货的订单可以手动发货")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return userErr("请填写发货内容")
	}
	var items []deliver.Item
	if perLine {
		for _, l := range strings.Split(content, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				items = append(items, deliver.Item{Content: l, Source: model.SourceManual})
			}
		}
	} else {
		items = []deliver.Item{{Content: content, Source: model.SourceManual}}
	}
	return s.complete(&o, items)
}

// Redeliver retries a failed order's delivery now.
func (s *Service) Redeliver(orderID uint) error {
	res := s.Store.DB.Model(&model.Order{}).Where("id = ? AND status IN ?", orderID, []string{model.OrderFailed, model.OrderPaid}).
		Updates(map[string]any{"status": model.OrderPaid, "last_error": ""})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return userErr("只有已支付或发货失败的订单可以重新发货")
	}
	key := fmt.Sprintf("deliver:%d", orderID)
	if err := store.Enqueue(s.Store.DB, "deliver", key, orderID, s.Now()); err != nil {
		return err
	}
	return s.Store.Requeue(key, s.Now())
}

// ResendNotice sends the delivery email / Telegram message again.
func (s *Service) ResendNotice(orderID uint) error {
	var o model.Order
	if err := s.Store.DB.Where("id = ?", orderID).Take(&o).Error; err != nil {
		return err
	}
	if o.Status != model.OrderDelivered {
		return userErr("订单尚未发货")
	}
	return store.Enqueue(s.Store.DB, "delivered", fmt.Sprintf("delivered:%d:%d", o.ID, s.Now().UnixNano()), o.ID, s.Now())
}

// Refund refunds an order: the deliverer takes back what it can (Polaris
// licenses are revoked), Stars payments are refunded through Telegram, and
// the order is marked refunded. Warnings list what the admin must still do
// by hand (gateway refunds, cards already seen).
func (s *Service) Refund(ctx context.Context, orderID uint, reason string) ([]string, error) {
	var o model.Order
	if err := s.Store.DB.Where("id = ?", orderID).Take(&o).Error; err != nil {
		return nil, err
	}
	if o.Status != model.OrderPaid && o.Status != model.OrderDelivered && o.Status != model.OrderFailed {
		return nil, userErr("该订单状态不能退款")
	}
	var warnings []string
	var sku model.SKU
	// Polaris orders can hold issued licenses even when delivery failed
	// part-way (several keys per order), so they are always revoked.
	if s.Store.DB.Where("id = ?", o.SKUID).Take(&sku).Error == nil && (o.Status == model.OrderDelivered || sku.DelivererKind == "polaris") {
		if d, err := s.Reg.Get(sku.DelivererKind); err == nil {
			if err := d.Revoke(ctx, &o, &sku); err != nil {
				if !errors.Is(err, deliver.ErrRevokeUnsupported) {
					return nil, fmt.Errorf("回收失败，未退款：%w", err)
				}
				warnings = append(warnings, err.Error())
			}
		}
	}
	var pays []model.Payment
	s.Store.DB.Where("order_id = ? AND status = ?", o.ID, model.PaymentPaid).Find(&pays)
	for _, p := range pays {
		switch p.Channel {
		case model.PayStars:
			charge := strings.TrimPrefix(p.TradeNo, model.PayStars+":")
			if err := s.Bot().Call(ctx, "refundStarPayment", map[string]any{"user_id": o.TGUserID, "telegram_payment_charge_id": charge}, nil); err != nil {
				warnings = append(warnings, "Stars 退款失败，请在 Telegram 手动处理："+err.Error())
				continue
			}
		case model.PayManual:
		default:
			warnings = append(warnings, fmt.Sprintf("请在支付渠道（%s）手动退款 %s，流水号 %s", p.Channel, FormatMoney(p.Amount, p.Currency), p.TradeNo))
		}
		s.Store.DB.Model(&model.Payment{}).Where("id = ?", p.ID).Update("status", model.PaymentRefunded)
	}
	now := s.Now()
	err := s.Store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Order{}).Where("id = ?", o.ID).
			Updates(map[string]any{"status": model.OrderRefunded, "refunded_at": now, "last_error": trim("退款："+reason, 500)}).Error; err != nil {
			return err
		}
		if d, err := s.Reg.Get(sku.DelivererKind); err == nil {
			return d.Release(tx, &o)
		}
		return nil
	})
	return warnings, err
}

// ExpireOrders closes unpaid orders past their deadline and frees their
// stock and coupon use.
func (s *Service) ExpireOrders(ctx context.Context) {
	now := s.Now()
	var orders []model.Order
	s.Store.DB.Where("status = ? AND expires_at < ?", model.OrderPending, now).Limit(200).Find(&orders)
	for _, o := range orders {
		s.Store.DB.Transaction(func(tx *gorm.DB) error {
			res := tx.Model(&model.Order{}).Where("id = ? AND status = ?", o.ID, model.OrderPending).Update("status", model.OrderExpired)
			if res.Error != nil || res.RowsAffected == 0 {
				return res.Error
			}
			if o.CouponID != nil {
				tx.Model(&model.Coupon{}).Where("id = ? AND used > 0", *o.CouponID).Update("used", gorm.Expr("used - 1"))
			}
			var sku model.SKU
			if tx.Where("id = ?", o.SKUID).Take(&sku).Error == nil {
				if d, err := s.Reg.Get(sku.DelivererKind); err == nil {
					return d.Release(tx, &o)
				}
			}
			return nil
		})
	}
	deliver.ReleaseExpired(s.Store.DB, now)
}

// PollUSDT scans the receiving address for new USDT transfers and matches
// them to pending orders by exact amount.
func (s *Service) PollUSDT(ctx context.Context) error {
	st := s.Store.Settings()
	if !st.USDTEnabled || !payment.ValidTRONAddress(st.USDTAddress) {
		return nil
	}
	var oldest model.Order
	err := s.Store.DB.Where("pay_method = ? AND status = ?", model.PayUSDT, model.OrderPending).Order("created_at").Take(&oldest).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	tron := s.TRON()
	since := oldest.CreatedAt.Add(-time.Minute).Unix()
	return tron.Scan(ctx, st.USDTAddress, since, func(txid string) error {
		var n int64
		s.Store.DB.Model(&model.Payment{}).Where("trade_no LIKE ?", model.PayUSDT+":"+txid+"%").Count(&n)
		if n > 0 {
			return nil
		}
		transfers, err := tron.Verify(ctx, txid, st.USDTAddress)
		if errors.Is(err, payment.ErrPending) {
			return nil
		}
		if err != nil {
			s.Log.Warn("usdt verify failed", "tx", txid, "err", err)
			return nil
		}
		for _, t := range transfers {
			var o model.Order
			at := time.Unix(t.At, 0)
			err := s.Store.DB.Where("pay_method = ? AND pay_amount = ? AND status IN ? AND created_at <= ?",
				model.PayUSDT, t.Amount, []string{model.OrderPending, model.OrderExpired}, at.Add(time.Minute)).
				Where("expires_at >= ?", at.Add(-time.Hour)).Order("id DESC").Take(&o).Error
			if err != nil {
				continue
			}
			if _, err := s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: model.PayUSDT, TradeNo: fmt.Sprintf("%s:%d", t.TXID, t.Index),
				Amount: t.Amount, Currency: model.USDT}); err != nil {
				s.Log.Warn("usdt mark paid failed", "order", o.OrderNo, "err", err)
			}
		}
		return nil
	})
}

// Run starts the background loops (jobs, expiry, USDT polling).
func (s *Service) Run(ctx context.Context) {
	go s.Store.RunJobs(ctx, s.JobHandlers(), 2*time.Second)
	expire := time.NewTicker(30 * time.Second)
	usdt := time.NewTicker(20 * time.Second)
	defer expire.Stop()
	defer usdt.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-expire.C:
			s.ExpireOrders(ctx)
		case <-usdt.C:
			if err := s.PollUSDT(ctx); err != nil {
				s.Log.Warn("usdt poll failed", "err", err)
			}
		}
	}
}

func trim(v string, n int) string {
	if r := []rune(v); len(r) > n {
		return string(r[:n])
	}
	return v
}

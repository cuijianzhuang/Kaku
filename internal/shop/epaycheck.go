package shop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/payment"
)

// EPay payments are normally confirmed by the gateway's notification. If
// it never arrives (wrong site address, firewall, a gateway that only
// retries a few times) the buyer has paid but the order stays unpaid, so
// open orders are also checked against the gateway directly: when the
// buyer's page polls, when they come back from the cashier, and in the
// background.

// EPayEvent is the last thing that happened with EPay confirmation, shown
// in the admin so a broken callback can be diagnosed.
type EPayEvent struct {
	OK  bool      `json:"ok"`
	Msg string    `json:"msg"`
	At  time.Time `json:"at"`
}

const epayEventKey = "epay:last"

// RecordEPay stores the latest EPay confirmation result.
func (s *Service) RecordEPay(ok bool, msg string) {
	if b, err := json.Marshal(EPayEvent{OK: ok, Msg: msg, At: s.Now()}); err == nil {
		s.Store.SetValue(epayEventKey, string(b))
	}
}

// LastEPay returns the latest EPay confirmation result, if any.
func (s *Service) LastEPay() *EPayEvent {
	var ev EPayEvent
	if v := s.Store.Value(epayEventKey); v == "" || json.Unmarshal([]byte(v), &ev) != nil {
		return nil
	}
	return &ev
}

// EPayChannel is the payment method recorded for an EPay payment: what
// the buyer actually used at the cashier when the gateway says so.
func EPayChannel(o *model.Order, gwType string) string {
	if gwType == model.PayEPayAlipay || gwType == model.PayEPayWxpay {
		return gwType
	}
	return o.PayMethod
}

func isEPay(o *model.Order) bool {
	return o.PayMethod == model.PayEPayAlipay || o.PayMethod == model.PayEPayWxpay
}

var epayChecked sync.Map // order no → time.Time of the last query

// CheckEPay asks the gateway whether an open EPay order has been paid and,
// if so, marks it paid. Queries for one order are spaced at least every
// seconds apart unless every is 0. It reports whether the order is paid now.
func (s *Service) CheckEPay(ctx context.Context, o *model.Order, every time.Duration) (bool, error) {
	if !isEPay(o) || (o.Status != model.OrderPending && o.Status != model.OrderExpired) {
		return false, nil
	}
	ep := s.EPay()
	if !ep.Ready() {
		return false, errors.New("易支付尚未配置")
	}
	now := s.Now()
	if every > 0 {
		if last, ok := epayChecked.Load(o.OrderNo); ok && now.Sub(last.(time.Time)) < every {
			return false, nil
		}
	}
	epayChecked.Store(o.OrderNo, now)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := ep.Query(ctx, o.OrderNo)
	if err != nil {
		s.RecordEPay(false, fmt.Sprintf("查询订单 %s 失败：%v", o.OrderNo, err))
		return false, err
	}
	if !r.Paid {
		return false, nil
	}
	if r.Amount != o.PayAmount {
		msg := fmt.Sprintf("订单 %s 支付平台显示已付 %s 元，与应付 %s 元不一致，未自动发货", o.OrderNo,
			payment.Decimal(r.Amount, 2), payment.Decimal(o.PayAmount, 2))
		s.RecordEPay(false, msg)
		return false, errors.New(msg)
	}
	if r.TradeNo == "" {
		err := fmt.Errorf("订单 %s 支付平台显示已付，但没有返回交易号，请在后台手动补单", o.OrderNo)
		s.RecordEPay(false, err.Error())
		return false, err
	}
	if _, err := s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: EPayChannel(o, r.Type), TradeNo: r.TradeNo,
		Amount: r.Amount, Currency: model.CNY, Raw: "query"}); err != nil {
		return false, err
	}
	s.RecordEPay(true, fmt.Sprintf("订单 %s 已通过主动查单确认付款（没有收到支付平台的回调）", o.OrderNo))
	epayChecked.Delete(o.OrderNo)
	return true, nil
}

// CheckOpenEPay checks recent unpaid EPay orders, including ones that just
// expired (a late payment still counts).
func (s *Service) CheckOpenEPay(ctx context.Context) {
	if !s.EPay().Ready() {
		return
	}
	ttl := time.Duration(s.Store.Settings().OrderTTL) * time.Minute
	var orders []model.Order
	s.Store.DB.Where("pay_method IN ? AND status IN ? AND created_at > ?",
		[]string{model.PayEPayAlipay, model.PayEPayWxpay}, []string{model.OrderPending, model.OrderExpired},
		s.Now().Add(-ttl-30*time.Minute)).Order("created_at desc").Limit(20).Find(&orders)
	for i := range orders {
		if ctx.Err() != nil {
			return
		}
		s.CheckEPay(ctx, &orders[i], 25*time.Second)
	}
}

// ForgetEPayCheck lets the next CheckEPay for an order query at once.
func (s *Service) ForgetEPayCheck(orderNo string) { epayChecked.Delete(orderNo) }

package web

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/shop"
)

func (e *env) orderStatus(no string) string {
	e.t.Helper()
	resp, err := e.client().Get(e.ts.URL + "/order/" + no + "/status")
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var r struct{ Status string }
	json.NewDecoder(resp.Body).Decode(&r)
	return r.Status
}

// The buyer paid but the gateway's notification never arrived: the order
// page's polling asks the gateway and confirms the payment.
func TestEPayLostNotify(t *testing.T) {
	e := setup(t)
	o := e.stockOrder(100)
	if s := e.orderStatus(o.OrderNo); s != model.OrderPending {
		t.Fatalf("unpaid order: %s", s)
	}
	e.gwPaid.Store(true)
	e.gwMoney.Store("1.00")
	// the check is spaced 5 s per order; the first poll above used it up
	if s := e.orderStatus(o.OrderNo); s != model.OrderPending {
		t.Fatalf("checked again within 5 s: %s", s)
	}
	e.st.DB.Model(&model.Order{}).Where("id = ?", o.ID).Update("created_at", time.Now().Add(-time.Minute))
	epayChecked(e, o.OrderNo)
	resp, _ := e.client().Get(e.ts.URL + "/pay/" + o.OrderNo)
	if l := location(resp); l != "/order/"+o.OrderNo {
		t.Fatalf("pay page still offers to pay a paid order: %d %s", resp.StatusCode, l)
	}
	e.st.RunDueJobs(context.Background(), e.shop.JobHandlers())
	var after model.Order
	e.st.DB.Take(&after, o.ID)
	if after.Status != model.OrderDelivered {
		t.Fatalf("status %s", after.Status)
	}
	var pays []model.Payment
	e.st.DB.Where("order_id = ?", o.ID).Find(&pays)
	if len(pays) != 1 || pays[0].TradeNo != "alipay:GW123" {
		t.Fatalf("payments %+v", pays)
	}
	// the notification arriving late does not count twice
	v := url.Values{"pid": {"1000"}, "trade_no": {"GW123"}, "out_trade_no": {o.OrderNo}, "type": {"alipay"},
		"money": {"1.00"}, "trade_status": {"TRADE_SUCCESS"}}
	if code, body := e.notify(signed(v)); code != 200 || body != "success" {
		t.Fatalf("late notify %d %s", code, body)
	}
	e.st.DB.Where("order_id = ?", o.ID).Find(&pays)
	if len(pays) != 1 {
		t.Fatalf("late notify recorded a second payment: %d", len(pays))
	}
	if ev := e.shop.LastEPay(); ev == nil || !ev.OK {
		t.Fatalf("last event %+v", ev)
	}
}

// epayChecked forgets when an order was last queried.
func epayChecked(e *env, no string) {
	e.shop.ForgetEPayCheck(no)
}

// A gateway whose query API does not work still confirms through the
// signed notification; the admin sees why the query failed.
func TestEPayQueryUnavailable(t *testing.T) {
	e := setup(t)
	o := e.stockOrder(990)
	e.gwRaw.Store(`{"code":-1,"msg":"商户未开通查询接口"}`)
	if code, body := e.notify(signed(url.Values{"pid": {"1000"}, "trade_no": {"T9"}, "out_trade_no": {o.OrderNo}, "type": {"wxpay"},
		"money": {"9.90"}, "trade_status": {"TRADE_SUCCESS"}})); code != 200 || body != "success" {
		t.Fatalf("signed notify with broken query: %d %s", code, body)
	}
	var after model.Order
	e.st.DB.Take(&after, o.ID)
	if after.Status != model.OrderPaid {
		t.Fatalf("status %s", after.Status)
	}
	// a forged notification is still refused, and the reason is shown
	o2, err := e.shop.CreateOrder(context.Background(), shop.CreateInput{SKUID: o.SKUID, Qty: 1, Email: "c@example.com",
		QueryPassword: "1234", PayMethod: model.PayEPayWxpay, IP: "9.9.9.8"})
	if err != nil {
		t.Fatal(err)
	}
	v := signed(url.Values{"pid": {"1000"}, "trade_no": {"T10"}, "out_trade_no": {o2.OrderNo}, "money": {"9.90"}, "trade_status": {"TRADE_SUCCESS"}})
	v.Set("sign", strings.Repeat("0", 32))
	if code, _ := e.notify(v); code == 200 {
		t.Fatal("forged notify accepted")
	}
	c := e.login()
	resp, _ := c.Get(e.ts.URL + "/manage/settings/payment")
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "最近一次支付确认") || !strings.Contains(string(b), "签名不正确") {
		t.Fatal("payment settings do not show the last confirmation problem")
	}
	// the admin's query button reports the gateway's error
	l := location(e.post(c, "/manage/orders/"+o2.OrderNo+"/epay-check", url.Values{}))
	if !strings.Contains(l, "未开通查询接口") {
		t.Fatalf("query button: %s", l)
	}
	e.gwRaw.Store(`{"code":"1","status":"1","money":9.9,"trade_no":"T10","type":"wxpay"}`)
	if l := location(e.post(c, "/manage/orders/"+o2.OrderNo+"/epay-check", url.Values{})); !strings.Contains(l, "msg=") {
		t.Fatalf("query button after payment: %s", l)
	}
	e.st.DB.Take(&after, o2.ID)
	if after.Status != model.OrderPaid {
		t.Fatalf("admin query did not confirm: %s", after.Status)
	}
}

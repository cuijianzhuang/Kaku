// Package payment implements the payment channels: EPay (易支付, Alipay /
// WeChat Pay through an aggregator) and USDT-TRC20 with unique amounts.
// Telegram Stars payments arrive through the bot (package tgbot).
package payment

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EPay is a traditional 易支付 merchant (submit.php / api.php, MD5 sign).
type EPay struct {
	BaseURL   string // gateway base, e.g. https://pay.example.com
	PID       string
	Key       string
	AllowHTTP bool // tests only
	Client    *http.Client
}

func validURL(raw string, allowHTTP bool) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" &&
		(u.Scheme == "https" || (allowHTTP && u.Scheme == "http"))
}

func (e *EPay) base() (*url.URL, error) {
	raw := strings.TrimSpace(e.BaseURL)
	if !validURL(raw, e.AllowHTTP) {
		return nil, errors.New("易支付地址需为 HTTPS，且不能带查询参数")
	}
	u, _ := url.Parse(raw)
	u.Path = strings.TrimRight(strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/submit.php"), "/")
	u.RawPath = ""
	return u, nil
}

// Ready reports whether the merchant is fully configured.
func (e *EPay) Ready() bool {
	_, err := e.base()
	return err == nil && e.PID != "" && e.Key != ""
}

// Sign computes the legacy EPay MD5 signature: sorted non-empty k=v pairs
// (excluding sign and sign_type), joined by &, followed by the key.
func Sign(values url.Values, key string) string {
	keys := []string{}
	for k, v := range values {
		if k != "sign" && k != "sign_type" && len(v) == 1 && v[0] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+values.Get(k))
	}
	h := md5.Sum([]byte(strings.Join(parts, "&") + key))
	return hex.EncodeToString(h[:])
}

// Verify checks a notification's signature.
func Verify(values url.Values, key string) bool {
	for _, v := range values {
		if len(v) != 1 {
			return false
		}
	}
	if t := values.Get("sign_type"); t != "" && !strings.EqualFold(t, "MD5") {
		return false
	}
	want := Sign(values, key)
	return len(values.Get("sign")) == 32 && subtle.ConstantTimeCompare([]byte(strings.ToLower(values.Get("sign"))), []byte(want)) == 1
}

// CheckoutURL builds the cashier redirect for an order.
func (e *EPay) CheckoutURL(method, orderNo, name string, amountFen int64, notifyURL, returnURL string) (string, error) {
	if !e.Ready() {
		return "", errors.New("易支付尚未配置")
	}
	u, err := e.base()
	if err != nil {
		return "", err
	}
	u.Path += "/submit.php"
	v := url.Values{
		"pid": {e.PID}, "type": {method}, "out_trade_no": {orderNo}, "notify_url": {notifyURL},
		"return_url": {returnURL}, "name": {name}, "money": {Decimal(amountFen, 2)},
	}
	v.Set("sign", Sign(v, e.Key))
	v.Set("sign_type", "MD5")
	u.RawQuery = v.Encode()
	return u.String(), nil
}

// Notification is a verified EPay payment notification.
type Notification struct {
	OrderNo string
	TradeNo string
	Amount  int64 // fen
	Method  string
}

// ParseNotify verifies a notification's signature and status.
func (e *EPay) ParseNotify(values url.Values) (*Notification, error) {
	if !e.Ready() {
		return nil, errors.New("易支付尚未配置")
	}
	if !Verify(values, e.Key) {
		return nil, errors.New("签名不正确")
	}
	if values.Get("pid") != "" && values.Get("pid") != e.PID {
		return nil, errors.New("商户号不匹配")
	}
	if values.Get("trade_status") != "TRADE_SUCCESS" {
		return nil, errors.New("交易未成功")
	}
	amount, err := ParseDecimal(values.Get("money"), 2)
	if err != nil {
		return nil, err
	}
	n := &Notification{OrderNo: values.Get("out_trade_no"), TradeNo: values.Get("trade_no"), Amount: amount, Method: values.Get("type")}
	if n.OrderNo == "" || n.TradeNo == "" {
		return nil, errors.New("通知缺少订单号")
	}
	return n, nil
}

// QueryPaid asks the gateway whether an order was paid (api.php?act=order),
// the second confirmation against forged notifications. It returns the
// paid amount in fen.
func (e *EPay) QueryPaid(ctx context.Context, orderNo string) (bool, int64, error) {
	u, err := e.base()
	if err != nil {
		return false, 0, err
	}
	u.Path += "/api.php"
	u.RawQuery = url.Values{"act": {"order"}, "pid": {e.PID}, "key": {e.Key}, "out_trade_no": {orderNo}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, 0, err
	}
	c := e.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return false, 0, fmt.Errorf("查询易支付订单：%w", err)
	}
	defer resp.Body.Close()
	var r struct {
		Code   json.Number `json:"code"`
		Status json.Number `json:"status"`
		Money  json.RawMessage
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(body, &r); err != nil {
		return false, 0, errors.New("易支付查单响应格式不正确")
	}
	if r.Code.String() != "1" {
		return false, 0, nil
	}
	money := strings.Trim(string(r.Money), `"`)
	amount, err := ParseDecimal(money, 2)
	if err != nil {
		return false, 0, err
	}
	return r.Status.String() == "1", amount, nil
}

// Decimal formats an integer amount with places decimals.
func Decimal(amount int64, places int) string {
	if places == 0 {
		return strconv.FormatInt(amount, 10)
	}
	scale := int64(1)
	for range places {
		scale *= 10
	}
	sign := ""
	if amount < 0 {
		sign, amount = "-", -amount
	}
	return fmt.Sprintf("%s%d.%0*d", sign, amount/scale, places, amount%scale)
}

// Trimmed formats an amount without trailing zeros (10.50 → 10.5, 3.00 → 3).
func Trimmed(amount int64, places int) string {
	s := Decimal(amount, places)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// ParseDecimal parses a non-negative decimal amount with at most places
// decimals into an integer of the smallest unit.
func ParseDecimal(s string, places int) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 24 {
		return 0, errors.New("无效金额")
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && (frac == "" || len(frac) > places)) {
		return 0, errors.New("金额格式不正确")
	}
	for _, c := range whole + frac {
		if c < '0' || c > '9' {
			return 0, errors.New("无效金额")
		}
	}
	n, err := strconv.ParseInt(whole+frac+strings.Repeat("0", places-len(frac)), 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("金额超出范围")
	}
	return n, nil
}

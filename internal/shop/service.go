// Package shop is the order lifecycle: pricing, order creation with stock
// reservation, payment confirmation, delivery with retries, refunds and
// renewals. Web and Telegram are just two front doors to it.
package shop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/notify"
	"github.com/cuijianzhuang/kaku/internal/payment"
	"github.com/cuijianzhuang/kaku/internal/polaris"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/store"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
	"github.com/cuijianzhuang/kaku/internal/update"
)

// Service runs the shop.
type Service struct {
	Store  *store.Store
	Reg    *deliver.Registry
	Notify *notify.Notifier
	Log    *slog.Logger
	Now    func() time.Time
	// BackupDir holds automatic backups ("" = backups off).
	BackupDir string
	// Updater checks for and installs new releases (nil = off).
	Updater *update.Updater

	backupMu sync.Mutex

	catMu  sync.Mutex
	cat    *polaris.Catalog
	catErr error
	catAt  time.Time
	catKey string

	// Overridable in tests.
	PolarisClient *polaris.Client
	EPayClient    *payment.EPay
	TRON          func() *payment.TRON
	Bot           func() *tgapi.Client
}

// New wires a service.
func New(st *store.Store, n *notify.Notifier, log *slog.Logger) *Service {
	s := &Service{Store: st, Notify: n, Log: log, Now: time.Now}
	s.Reg = deliver.NewRegistry(&deliver.Env{Store: st, Polaris: s.Polaris, Now: func() time.Time { return s.Now() }})
	s.TRON = func() *payment.TRON {
		return payment.NewTRON(payment.TRONConfig{TRONAPIKey: st.Settings().TronGridKey})
	}
	s.Bot = func() *tgapi.Client { return &tgapi.Client{Token: st.Settings().TGToken} }
	return s
}

// Polaris returns the license server client from the current settings.
func (s *Service) Polaris() *polaris.Client {
	if s.PolarisClient != nil {
		return s.PolarisClient
	}
	st := s.Store.Settings()
	return &polaris.Client{BaseURL: st.PolarisURL, Secret: st.PolarisSecret}
}

// EPay returns the EPay merchant from the current settings.
func (s *Service) EPay() *payment.EPay {
	if s.EPayClient != nil {
		return s.EPayClient
	}
	st := s.Store.Settings()
	return &payment.EPay{BaseURL: st.EPayURL, PID: st.EPayPID, Key: st.EPayKey}
}

// UserError is a message safe to show the buyer.
type UserError struct{ Msg string }

func (e *UserError) Error() string { return e.Msg }

func userErr(format string, a ...any) error { return &UserError{Msg: fmt.Sprintf(format, a...)} }

// ---- payment methods ----

// Method is a payment method a buyer can pick.
type Method struct {
	ID, Label, Currency string
}

// Methods lists the payment methods enabled in settings for a channel
// (Stars only through Telegram).
func (s *Service) Methods(channel string) []Method {
	st := s.Store.Settings()
	var out []Method
	if s.EPay().Ready() {
		if st.EPayAlipay {
			out = append(out, Method{model.PayEPayAlipay, "支付宝", model.CNY})
		}
		if st.EPayWxpay {
			out = append(out, Method{model.PayEPayWxpay, "微信支付", model.CNY})
		}
	}
	if st.USDTEnabled && payment.ValidTRONAddress(st.USDTAddress) {
		out = append(out, Method{model.PayUSDT, "USDT (TRC20)", model.USDT})
	}
	if channel == model.ChannelTG && st.StarsEnabled && st.TGToken != "" {
		out = append(out, Method{model.PayStars, "Telegram Stars", model.XTR})
	}
	return out
}

// MethodsFor filters methods to those the SKU has a price for.
func (s *Service) MethodsFor(channel string, sku *model.SKU) []Method {
	var out []Method
	for _, m := range s.Methods(channel) {
		if sku.Price(m.Currency) > 0 {
			out = append(out, m)
		}
	}
	return out
}

func (s *Service) methodEnabled(channel, id string) bool {
	for _, m := range s.Methods(channel) {
		if m.ID == id {
			return true
		}
	}
	return false
}

// ---- catalog ----

// Stock returns a SKU's available units (-1 = unlimited).
func (s *Service) Stock(sku *model.SKU) int {
	d, err := s.Reg.Get(sku.DelivererKind)
	if err != nil {
		return 0
	}
	return d.Stock(sku)
}

// CategoryView is a category with its visible products.
type CategoryView struct {
	model.Category
	Products []model.Product
}

// Catalog returns visible categories with products on sale and their
// active SKUs.
func (s *Service) Catalog() ([]CategoryView, error) {
	var cats []model.Category
	if err := s.Store.DB.Where("visible = ?", true).Order("sort, id").Find(&cats).Error; err != nil {
		return nil, err
	}
	var prods []model.Product
	if err := s.Store.DB.Where("status = ?", model.ProductOn).Order("sort, id").
		Preload("SKUs", func(db *gorm.DB) *gorm.DB { return db.Where("active = ?", true).Order("sort, id") }).
		Find(&prods).Error; err != nil {
		return nil, err
	}
	byCat := map[uint][]model.Product{}
	for _, p := range prods {
		if len(p.SKUs) > 0 {
			byCat[p.CategoryID] = append(byCat[p.CategoryID], p)
		}
	}
	var out []CategoryView
	for _, c := range cats {
		if len(byCat[c.ID]) > 0 {
			out = append(out, CategoryView{Category: c, Products: byCat[c.ID]})
		}
	}
	return out, nil
}

// Product returns a product on sale with its active SKUs.
func (s *Service) Product(id uint) (*model.Product, error) {
	var p model.Product
	err := s.Store.DB.Where("id = ? AND status = ?", id, model.ProductOn).
		Preload("SKUs", func(db *gorm.DB) *gorm.DB { return db.Where("active = ?", true).Order("sort, id") }).Take(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, userErr("商品不存在或已下架")
	}
	return &p, err
}

// ---- pricing ----

// Quote is a computed price.
type Quote struct {
	Unit, Subtotal, Discount, Amount int64
	Currency                         string
	Coupon                           *model.Coupon
}

func (s *Service) quote(tx *gorm.DB, sku *model.SKU, qty int, currency, code string) (*Quote, error) {
	unit := sku.Price(currency)
	if unit <= 0 {
		return nil, userErr("该规格不支持此币种付款")
	}
	percent := int64(100)
	best := 0
	for _, t := range sku.Tiers() {
		if t.MinQty <= qty && t.MinQty > best && t.Percent > 0 && t.Percent <= 100 {
			best, percent = t.MinQty, int64(t.Percent)
		}
	}
	q := &Quote{Unit: unit, Currency: currency}
	q.Subtotal = (unit*int64(qty)*percent + 50) / 100
	if code = strings.TrimSpace(code); code != "" {
		var c model.Coupon
		err := tx.Where("code = ? AND active = ?", code, true).Take(&c).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, userErr("优惠码无效")
		}
		if err != nil {
			return nil, err
		}
		if c.ExpiresAt != nil && s.Now().After(*c.ExpiresAt) {
			return nil, userErr("优惠码已过期")
		}
		if c.MaxUses > 0 && c.Used >= c.MaxUses {
			return nil, userErr("优惠码已用完")
		}
		if c.SKUScope != "" {
			ok := false
			for _, id := range strings.Split(c.SKUScope, ",") {
				ok = ok || strings.TrimSpace(id) == strconv.Itoa(int(sku.ID))
			}
			if !ok {
				return nil, userErr("优惠码不适用于该商品")
			}
		}
		switch c.Type {
		case model.CouponPercent:
			q.Discount = q.Subtotal * c.Value / 100
		case model.CouponFixed:
			switch {
			case currency == model.CNY:
				q.Discount = c.Value
			case sku.PriceCNY > 0:
				// Convert by the SKU's own price ratio between currencies.
				q.Discount = c.Value * unit / sku.PriceCNY
			default:
				return nil, userErr("该优惠码只能用于人民币付款")
			}
		}
		q.Discount = min(max(q.Discount, 0), q.Subtotal)
		q.Coupon = &c
	}
	q.Amount = q.Subtotal - q.Discount
	if currency == model.XTR && q.Amount < 1 && q.Subtotal > 0 {
		q.Amount = 1
	}
	return q, nil
}

// ---- orders ----

// CreateInput is a buyer's order.
type CreateInput struct {
	SKUID         uint
	Qty           int
	Email         string
	QueryPassword string
	PayMethod     string
	Coupon        string
	Remark        string
	IP            string
	Channel       string
	TGUserID      int64
	RenewOf       string // license_id; caller has verified the buyer owns it
}

// NewOrderNo returns an order number: K + yymmddHHMMSS + 6 random digits.
func NewOrderNo(now time.Time) string {
	return "K" + now.UTC().Format("060102150405") + secure.RandomDigits(6)
}

// CreateOrder validates the input, prices it, reserves stock and stores a
// pending order.
func (s *Service) CreateOrder(ctx context.Context, in CreateInput) (*model.Order, error) {
	st := s.Store.Settings()
	if in.Channel == "" {
		in.Channel = model.ChannelWeb
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	in.Remark = strings.TrimSpace(in.Remark)
	switch {
	case in.Qty < 1:
		return nil, userErr("购买数量不正确")
	case in.Channel == model.ChannelWeb && in.Email == "":
		return nil, userErr("请填写邮箱")
	case in.Channel == model.ChannelWeb && len(in.QueryPassword) < 4:
		return nil, userErr("查询密码至少 4 位")
	case len(in.QueryPassword) > 64:
		return nil, userErr("查询密码最多 64 位")
	case len([]rune(in.Remark)) > 200:
		return nil, userErr("备注最多 200 字")
	}
	if in.Email != "" {
		if a, err := mail.ParseAddress(in.Email); err != nil || a.Address != in.Email || len(in.Email) > 200 {
			return nil, userErr("邮箱格式不正确")
		}
	}
	if !s.methodEnabled(in.Channel, in.PayMethod) {
		return nil, userErr("请选择可用的支付方式")
	}
	var sku model.SKU
	if err := s.Store.DB.Where("id = ? AND active = ?", in.SKUID, true).Take(&sku).Error; err != nil {
		return nil, userErr("商品规格不存在或已下架")
	}
	var prod model.Product
	if err := s.Store.DB.Where("id = ? AND status = ?", sku.ProductID, model.ProductOn).Take(&prod).Error; err != nil {
		return nil, userErr("商品不存在或已下架")
	}
	if sku.LimitPerOrder > 0 && in.Qty > sku.LimitPerOrder {
		return nil, userErr("每单最多购买 %d 件", sku.LimitPerOrder)
	}
	if in.RenewOf != "" {
		if err := s.checkRenewable(in.RenewOf, &sku); err != nil {
			return nil, err
		}
	}
	if err := s.checkUnpaid(in, st.MaxUnpaid); err != nil {
		return nil, err
	}
	d, err := s.Reg.Get(sku.DelivererKind)
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(max(st.OrderTTL, 5)) * time.Minute
	now := s.Now()
	o := &model.Order{
		OrderNo: NewOrderNo(now), Email: in.Email, SKUID: sku.ID, ProductTitle: prod.Title, SKUName: sku.Name,
		Qty: in.Qty, Currency: model.MethodCurrency(in.PayMethod), PayMethod: in.PayMethod,
		Status: model.OrderPending, Channel: in.Channel, TGUserID: in.TGUserID, ClientIP: in.IP,
		RenewOf: in.RenewOf, Remark: in.Remark, ExpiresAt: now.Add(ttl),
	}
	if in.QueryPassword != "" {
		o.QueryPwdHash = secure.HashPassword(in.QueryPassword)
	}
	err = s.Store.DB.Transaction(func(tx *gorm.DB) error {
		q, err := s.quote(tx, &sku, in.Qty, o.Currency, in.Coupon)
		if err != nil {
			return err
		}
		o.Amount, o.Discount, o.PayAmount = q.Amount, q.Discount, q.Amount
		if q.Coupon != nil {
			res := tx.Model(&model.Coupon{}).Where("id = ? AND (max_uses = 0 OR used < max_uses)", q.Coupon.ID).
				Update("used", gorm.Expr("used + 1"))
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				return userErr("优惠码已用完")
			}
			o.CouponID = &q.Coupon.ID
		}
		if o.PayMethod == model.PayUSDT && o.Amount > 0 {
			if o.PayAmount, err = uniqueUSDT(tx, o.Amount); err != nil {
				return err
			}
		}
		if err := tx.Create(o).Error; err != nil {
			return err
		}
		if err := d.Reserve(ctx, tx, o, &sku); err != nil {
			if errors.Is(err, deliver.ErrOutOfStock) {
				return userErr("库存不足")
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if o.Amount == 0 {
		// Free (100% coupon): no payment step.
		return s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual, TradeNo: "free:" + o.OrderNo, Trusted: true})
	}
	return o, nil
}

func (s *Service) checkUnpaid(in CreateInput, limit int) error {
	if limit <= 0 {
		limit = 3
	}
	count := func(col string, v any) int64 {
		var n int64
		s.Store.DB.Model(&model.Order{}).Where(col+" = ? AND status = ?", v, model.OrderPending).Count(&n)
		return n
	}
	if (in.IP != "" && count("client_ip", in.IP) >= int64(limit)) ||
		(in.Email != "" && count("email", in.Email) >= int64(limit)) ||
		(in.TGUserID != 0 && count("tg_user_id", in.TGUserID) >= int64(limit)) {
		return userErr("未支付订单过多，请先完成或等待之前的订单超时（%d 分钟）", max(s.Store.Settings().OrderTTL, 5))
	}
	return nil
}

// uniqueUSDT adds a tail of 0.0001 steps so every pending USDT order has a
// distinct amount; the chain transfer is matched on the exact amount.
func uniqueUSDT(tx *gorm.DB, amount int64) (int64, error) {
	var taken []int64
	if err := tx.Model(&model.Order{}).Where("pay_method = ? AND status IN ? AND pay_amount BETWEEN ? AND ?",
		model.PayUSDT, []string{model.OrderPending, model.OrderExpired}, amount, amount+100*999).
		Where("status = ? OR updated_at > ?", model.OrderPending, time.Now().Add(-time.Hour)).
		Pluck("pay_amount", &taken).Error; err != nil {
		return 0, err
	}
	used := map[int64]bool{}
	for _, v := range taken {
		used[v] = true
	}
	for k := int64(0); k < 1000; k++ {
		if v := amount + k*100; !used[v] {
			return v, nil
		}
	}
	return 0, userErr("当前 USDT 订单过多，请稍后再试")
}

// OrderByNo loads an order.
func (s *Service) OrderByNo(no string) (*model.Order, error) {
	var o model.Order
	err := s.Store.DB.Where("order_no = ?", no).Take(&o).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, userErr("订单不存在")
	}
	return &o, err
}

// AccessToken lets the browser that placed an order see its content.
func (s *Service) AccessToken(orderNo string) string { return s.Store.Box.Sign("order", orderNo) }

// CheckAccess verifies an access token.
func (s *Service) CheckAccess(orderNo, token string) bool {
	return token != "" && s.Store.Box.Verify(token, "order", orderNo)
}

// ItemView is a decrypted delivered item.
type ItemView struct {
	ID       uint
	Content  string
	Source   string
	RefID    string
	FileName string
}

// Items returns an order's delivered content, decrypted.
func (s *Service) Items(orderID uint) ([]ItemView, error) {
	var items []model.OrderItem
	if err := s.Store.DB.Where("order_id = ?", orderID).Order("id").Find(&items).Error; err != nil {
		return nil, err
	}
	out := make([]ItemView, len(items))
	for i, it := range items {
		out[i] = ItemView{ID: it.ID, Content: s.Store.Box.MustOpen(it.ContentEnc), Source: it.Source, RefID: it.RefID, FileName: it.FileName}
	}
	return out, nil
}

// FindOrders looks orders up for the buyer: by order number, or every
// order of an email, keeping only those the query password opens.
func (s *Service) FindOrders(orderNo, email, password string) ([]model.Order, error) {
	var orders []model.Order
	q := s.Store.DB.Order("id DESC").Limit(20)
	switch {
	case strings.TrimSpace(orderNo) != "":
		q = q.Where("order_no = ?", strings.TrimSpace(orderNo))
	case strings.TrimSpace(email) != "":
		q = q.Where("email = ?", strings.ToLower(strings.TrimSpace(email)))
	default:
		return nil, userErr("请填写订单号或邮箱")
	}
	if err := q.Find(&orders).Error; err != nil {
		return nil, err
	}
	out := orders[:0]
	for _, o := range orders {
		if o.QueryPwdHash != "" && secure.CheckPassword(o.QueryPwdHash, password) {
			out = append(out, o)
		}
	}
	return out, nil
}

// ---- renewals ----

func (s *Service) checkRenewable(licenseID string, sku *model.SKU) error {
	var b model.LicenseBinding
	if err := s.Store.DB.Where("license_id = ?", licenseID).Take(&b).Error; err != nil {
		return userErr("许可证不存在（只能续费本店售出的许可证）")
	}
	if b.Status == model.LicenseRevoked || b.Offline {
		return userErr("该许可证不能在线续费")
	}
	if sku.DelivererKind != "polaris" {
		return userErr("该商品不是续费商品")
	}
	cfg, err := deliver.ParsePolarisCfg(sku.DelivererCfg)
	if err != nil || !cfg.Renewable || cfg.Plan != b.Plan {
		return userErr("该规格不能用于续费此许可证")
	}
	return nil
}

// Binding returns the license binding of a license id.
func (s *Service) Binding(licenseID string) (*model.LicenseBinding, error) {
	var b model.LicenseBinding
	err := s.Store.DB.Where("license_id = ?", licenseID).Take(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, userErr("许可证不存在（只能续费本店售出的许可证）")
	}
	return &b, err
}

// RenewSKUs lists SKUs that can renew a binding.
func (s *Service) RenewSKUs(b *model.LicenseBinding) ([]model.SKU, map[uint]string, error) {
	var skus []model.SKU
	if err := s.Store.DB.Where("active = ? AND deliverer_kind = ?", true, "polaris").Order("sort, id").Find(&skus).Error; err != nil {
		return nil, nil, err
	}
	titles := map[uint]string{}
	out := skus[:0]
	for _, k := range skus {
		if s.checkRenewable(b.LicenseID, &k) == nil {
			var p model.Product
			if s.Store.DB.Where("id = ? AND status = ?", k.ProductID, model.ProductOn).Take(&p).Error == nil {
				titles[k.ID] = p.Title
				out = append(out, k)
			}
		}
	}
	return out, titles, nil
}

// OwnsLicense reports whether a buyer identity may renew a license: the
// purchase email (case-insensitive) or the Telegram account that bought it.
func OwnsLicense(b *model.LicenseBinding, email string, tgUser int64) bool {
	if tgUser != 0 && b.TGUserID == tgUser {
		return true
	}
	email = strings.ToLower(strings.TrimSpace(email))
	return email != "" && email == strings.ToLower(b.Email)
}

// PolarisEvent applies an activated / deactivated callback.
func (s *Service) PolarisEvent(event, licenseID, instance string) error {
	upd := map[string]any{}
	switch event {
	case "activated":
		upd["status"], upd["instance"] = model.LicenseActivated, instance
	case "deactivated":
		upd["status"], upd["instance"] = model.LicenseIssued, ""
	default:
		return fmt.Errorf("unknown event %q", event)
	}
	return s.Store.DB.Model(&model.LicenseBinding{}).
		Where("license_id = ? AND status <> ?", licenseID, model.LicenseRevoked).Updates(upd).Error
}

// ---- stats ----

// Revenue is paid revenue in one currency.
type Revenue struct {
	Currency string
	Orders   int64
	Amount   int64
}

// RevenueSince sums paid orders since t (refunds excluded).
func (s *Service) RevenueSince(t time.Time) ([]Revenue, error) {
	var out []Revenue
	err := s.Store.DB.Model(&model.Order{}).Select("currency, COUNT(*) AS orders, COALESCE(SUM(amount),0) AS amount").
		Where("paid_at >= ? AND status IN ?", t, []string{model.OrderPaid, model.OrderDelivered, model.OrderFailed}).
		Group("currency").Scan(&out).Error
	return out, err
}

// FormatMoney renders an amount with its currency.
func FormatMoney(amount int64, currency string) string {
	switch currency {
	case model.CNY:
		return "¥" + payment.Trimmed(amount, 2)
	case model.USDT:
		return payment.Trimmed(amount, 6) + " USDT"
	case model.XTR:
		return strconv.FormatInt(amount, 10) + " ⭐"
	}
	return strconv.FormatInt(amount, 10) + " " + currency
}

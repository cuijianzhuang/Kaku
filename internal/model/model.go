// Package model defines Kaku's database tables.
//
// Money is stored as integers in the smallest unit of its currency:
// CNY in fen (0.01), USDT in micro-USDT (0.000001, the TRC20 precision) and
// Telegram Stars (XTR) as whole stars.
package model

import (
	"encoding/json"
	"time"
)

// Currencies.
const (
	CNY  = "CNY"
	USDT = "USDT"
	XTR  = "XTR"
)

// CurrencyPlaces is the number of decimal places stored for each currency.
func CurrencyPlaces(cur string) int {
	switch cur {
	case CNY:
		return 2
	case USDT:
		return 6
	}
	return 0
}

// Payment methods and the currency each one charges in.
const (
	PayEPayAlipay = "alipay"
	PayEPayWxpay  = "wxpay"
	PayUSDT       = "usdt"
	PayStars      = "stars"
	PayManual     = "manual" // admin marked paid (补单) or free order
)

// MethodCurrency returns the currency a payment method charges in.
func MethodCurrency(method string) string {
	switch method {
	case PayUSDT:
		return USDT
	case PayStars:
		return XTR
	}
	return CNY
}

type Category struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"size:100;not null"`
	Sort      int    `gorm:"not null;default:0"`
	Visible   bool   `gorm:"not null;default:true"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

const (
	ProductOn  = "on"
	ProductOff = "off"
)

type Product struct {
	ID            uint   `gorm:"primaryKey"`
	CategoryID    uint   `gorm:"index;not null"`
	Title         string `gorm:"size:200;not null"`
	DescriptionMD string `gorm:"type:text"`
	Cover         string `gorm:"size:500"`
	Status        string `gorm:"size:10;not null;default:on"`
	Sort          int    `gorm:"not null;default:0"`
	CreatedAt     time.Time
	UpdatedAt     time.Time

	SKUs []SKU `gorm:"foreignKey:ProductID"`
}

// WholesaleTier gives a discount (percent of the unit price) from MinQty up.
type WholesaleTier struct {
	MinQty  int `json:"min_qty"`
	Percent int `json:"percent"` // 90 = 9 折
}

type SKU struct {
	ID            uint   `gorm:"primaryKey"`
	ProductID     uint   `gorm:"index;not null"`
	Name          string `gorm:"size:100;not null"`
	PriceCNY      int64  `gorm:"not null;default:0"` // fen; 0 = not sold in CNY
	PriceUSDT     int64  `gorm:"not null;default:0"` // micro-USDT
	PriceStars    int64  `gorm:"not null;default:0"`
	DelivererKind string `gorm:"size:20;not null"`
	DelivererCfg  string `gorm:"type:text"`
	Wholesale     string `gorm:"type:text"` // JSON []WholesaleTier
	LimitPerOrder int    `gorm:"not null;default:10"`
	StockCache    int    `gorm:"not null;default:0"`
	LowStock      int    `gorm:"not null;default:5"` // alert threshold for stock SKUs
	Active        bool   `gorm:"not null;default:true"`
	Sort          int    `gorm:"not null;default:0"`
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Price returns the unit price in a currency (0 = not offered).
func (s *SKU) Price(cur string) int64 {
	switch cur {
	case CNY:
		return s.PriceCNY
	case USDT:
		return s.PriceUSDT
	case XTR:
		return s.PriceStars
	}
	return 0
}

// Tiers parses the wholesale tiers.
func (s *SKU) Tiers() []WholesaleTier {
	var t []WholesaleTier
	if s.Wholesale != "" {
		json.Unmarshal([]byte(s.Wholesale), &t)
	}
	return t
}

// Card states.
const (
	CardAvailable = "available"
	CardReserved  = "reserved"
	CardSold      = "sold"
	CardVoid      = "void"
)

type Card struct {
	ID            uint   `gorm:"primaryKey"`
	SKUID         uint   `gorm:"column:sku_id;index:idx_card_pick;not null"`
	SecretEnc     string `gorm:"type:text;not null"`
	SecretHash    string `gorm:"size:64;uniqueIndex;not null"`
	Status        string `gorm:"size:10;index:idx_card_pick;not null;default:available"`
	OrderID       *uint  `gorm:"index"`
	ReservedUntil *time.Time
	SoldAt        *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Order states.
const (
	OrderPending   = "pending"
	OrderPaid      = "paid" // paid, delivery running or waiting for manual delivery
	OrderDelivered = "delivered"
	OrderExpired   = "expired"
	OrderFailed    = "failed" // delivery retries used up; needs an admin
	OrderRefunded  = "refunded"
)

// Sales channels.
const (
	ChannelWeb = "web"
	ChannelTG  = "tg"
)

type Order struct {
	ID              uint   `gorm:"primaryKey"`
	OrderNo         string `gorm:"size:32;uniqueIndex;not null"`
	Email           string `gorm:"size:200;index"`
	QueryPwdHash    string `gorm:"size:200"`
	SKUID           uint   `gorm:"column:sku_id;index;not null"`
	ProductTitle    string `gorm:"size:200"`
	SKUName         string `gorm:"column:sku_name;size:100"`
	Qty             int    `gorm:"not null"`
	Amount          int64  `gorm:"not null"` // amount due, after discounts
	PayAmount       int64  `gorm:"not null"` // amount the gateway must report (USDT adds a unique tail)
	Discount        int64  `gorm:"not null;default:0"`
	Currency        string `gorm:"size:8;not null"`
	PayMethod       string `gorm:"size:16;not null"`
	Status          string `gorm:"size:12;index;not null"`
	Channel         string `gorm:"size:8;not null;default:web"`
	TGUserID        int64  `gorm:"column:tg_user_id;index"`
	ClientIP        string `gorm:"size:64;index"`
	CouponID        *uint
	RenewOf         string `gorm:"size:64;index"` // license_id being renewed
	Remark          string `gorm:"size:500"`
	DeliverAttempts int    `gorm:"not null;default:0"`
	LastError       string `gorm:"size:500"`
	ExpiresAt       time.Time
	PaidAt          *time.Time
	DeliveredAt     *time.Time
	RefundedAt      *time.Time
	CreatedAt       time.Time `gorm:"index"`
	UpdatedAt       time.Time
}

// Payment states.
const (
	PaymentPaid     = "paid"
	PaymentRefunded = "refunded"
)

type Payment struct {
	ID        uint   `gorm:"primaryKey"`
	OrderID   uint   `gorm:"index;not null"`
	Channel   string `gorm:"size:16;not null"`
	TradeNo   string `gorm:"size:128;uniqueIndex;not null"` // channel-prefixed; the replay guard
	Amount    int64  `gorm:"not null"`
	Currency  string `gorm:"size:8;not null"`
	RawNotify string `gorm:"type:text"`
	Status    string `gorm:"size:12;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Delivered item sources.
const (
	SourceCard    = "card"
	SourcePolaris = "polaris"
	SourceText    = "text"
	SourceWebhook = "webhook"
	SourceManual  = "manual"
)

type OrderItem struct {
	ID         uint   `gorm:"primaryKey"`
	OrderID    uint   `gorm:"index;not null"`
	ContentEnc string `gorm:"type:text;not null"`
	Source     string `gorm:"size:12;not null"`
	RefID      string `gorm:"size:64"`
	FileName   string `gorm:"size:100"` // set for content delivered as a file (.lic)
	CreatedAt  time.Time
}

// License binding states.
const (
	LicenseIssued    = "issued"
	LicenseActivated = "activated"
	LicenseRevoked   = "revoked"
)

type LicenseBinding struct {
	ID                uint   `gorm:"primaryKey"`
	OrderID           uint   `gorm:"index;not null"`
	ActivationKeyHash string `gorm:"size:64;index"`
	KeyEnc            string `gorm:"type:text"`
	LicenseID         string `gorm:"size:64;uniqueIndex;not null"`
	Plan              string `gorm:"size:20"`
	ExpiresAt         string `gorm:"size:10"` // YYYY-MM-DD, "" = perpetual
	Instance          string `gorm:"size:100"`
	Offline           bool
	Email             string `gorm:"size:200;index"`
	TGUserID          int64  `gorm:"column:tg_user_id;index"`
	Status            string `gorm:"size:12;not null"`
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

const (
	CouponFixed   = "fixed"   // value in the order currency's smallest unit... see Coupon.Value
	CouponPercent = "percent" // value = percent off (1-100)
)

type Coupon struct {
	ID        uint   `gorm:"primaryKey"`
	Code      string `gorm:"size:64;uniqueIndex;not null"`
	Type      string `gorm:"size:10;not null"`
	Value     int64  `gorm:"not null"`                  // percent, or CNY fen off (converted for other currencies by ratio)
	SKUScope  string `gorm:"column:sku_scope;size:500"` // comma separated SKU ids, "" = all
	MaxUses   int    `gorm:"not null;default:0"`        // 0 = unlimited
	Used      int    `gorm:"not null;default:0"`
	ExpiresAt *time.Time
	Active    bool `gorm:"not null;default:true"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Admin struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"size:64;uniqueIndex;not null"`
	PasswordHash string `gorm:"size:200;not null"`
	TOTPSecret   string `gorm:"size:200"` // encrypted
	TOTPEnabled  bool
	Epoch        int `gorm:"not null;default:1"` // bump to sign out every session
	LastLogin    *time.Time
	LastIP       string `gorm:"size:64"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// AdminIdentity links an external account (GitHub, Google) to an admin
// for sign-in. Identities are only added by a signed-in admin.
type AdminIdentity struct {
	ID        uint   `gorm:"primaryKey"`
	AdminID   uint   `gorm:"index;not null"`
	Provider  string `gorm:"size:20;not null;uniqueIndex:idx_identity"`
	Subject   string `gorm:"size:100;not null;uniqueIndex:idx_identity"` // the provider's stable user id
	Name      string `gorm:"size:200"`                                   // login or email, for display
	CreatedAt time.Time
	LastUsed  *time.Time
}

type AuditLog struct {
	ID        uint      `gorm:"primaryKey"`
	Admin     string    `gorm:"size:64"`
	Action    string    `gorm:"size:64;index"`
	Target    string    `gorm:"size:200"`
	Detail    string    `gorm:"type:text"`
	IP        string    `gorm:"size:64"`
	CreatedAt time.Time `gorm:"index"`
}

type Setting struct {
	Key       string `gorm:"primaryKey;size:64"`
	Value     string `gorm:"type:text"`
	UpdatedAt time.Time
}

// Job states.
const (
	JobPending = "pending"
	JobRunning = "running"
	JobDone    = "done"
	JobDead    = "dead"
)

// Job is a durable background task (delivery, mail, Telegram message).
type Job struct {
	ID        uint      `gorm:"primaryKey"`
	Kind      string    `gorm:"size:20;index;not null"`
	Key       string    `gorm:"size:100;uniqueIndex;not null"` // de-duplicates (deliver:{order_id})
	Payload   string    `gorm:"type:text"`
	Status    string    `gorm:"size:10;index;not null"`
	RunAt     time.Time `gorm:"index"`
	Attempts  int
	LastError string `gorm:"size:500"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Ticket states.
const (
	TicketOpen    = "open"
	TicketReplied = "replied"
	TicketClosed  = "closed"
)

// Ticket is an after-sales request from a buyer.
type Ticket struct {
	ID        uint   `gorm:"primaryKey"`
	OrderNo   string `gorm:"size:32;index"`
	Email     string `gorm:"size:200"`
	Content   string `gorm:"type:text"`
	Reply     string `gorm:"type:text"`
	Status    string `gorm:"size:10;index;not null"`
	ClientIP  string `gorm:"size:64"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// All lists every table for migration.
func All() []any {
	return []any{&Category{}, &Product{}, &SKU{}, &Card{}, &Order{}, &Payment{}, &OrderItem{},
		&LicenseBinding{}, &Coupon{}, &Admin{}, &AdminIdentity{}, &AuditLog{}, &Setting{}, &Job{}, &Ticket{}}
}

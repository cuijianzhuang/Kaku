// Package deliver implements the delivery methods. Every SKU names one
// deliverer; adding a delivery method never touches order code.
package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/panelapi"
	"github.com/cuijianzhuang/kaku/internal/polaris"
	"github.com/cuijianzhuang/kaku/internal/store"
)

// Item is one delivered piece of content.
type Item struct {
	Content  string
	Source   string
	RefID    string
	FileName string // deliver as a file (offline .lic)
}

// Deliverer is a delivery method.
type Deliverer interface {
	Kind() string
	Label() string
	// Validate checks a SKU's configuration when it is saved.
	Validate(cfg json.RawMessage) error
	// Reserve holds stock for a new order inside its transaction.
	Reserve(ctx context.Context, tx *gorm.DB, o *model.Order, sku *model.SKU) error
	// Release frees what Reserve held (order expired or cancelled).
	Release(tx *gorm.DB, o *model.Order) error
	// Deliver produces the order's content after payment. It must be
	// idempotent: a retry after a crash returns the same content.
	Deliver(ctx context.Context, o *model.Order, sku *model.SKU) ([]Item, error)
	// Revoke takes back what was delivered (refund), where possible.
	Revoke(ctx context.Context, o *model.Order, sku *model.SKU) error
	// Stock is the number of units available (-1 = unlimited).
	Stock(sku *model.SKU) int
}

var (
	// ErrManual means the order waits for an admin to deliver it by hand.
	ErrManual = errors.New("等待人工发货")
	// ErrOutOfStock means not enough stock to reserve.
	ErrOutOfStock = errors.New("库存不足")
	// ErrRevokeUnsupported means what was delivered cannot be taken back
	// automatically; the refund proceeds and the admin is told.
	ErrRevokeUnsupported = errors.New("该发货方式无法自动回收")
)

// PermanentError marks a delivery failure that retrying cannot fix.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent wraps err as permanent.
func Permanent(err error) error { return &PermanentError{Err: err} }

// Env is what deliverers need from the rest of the system.
type Env struct {
	Store   *store.Store
	Polaris func() *polaris.Client
	Panel   func() *panelapi.Client
	HTTP    *http.Client
	Now     func() time.Time
}

// Registry holds the deliverers by kind.
type Registry struct {
	m map[string]Deliverer
}

// NewRegistry registers the built-in deliverers.
func NewRegistry(env *Env) *Registry {
	if env.HTTP == nil {
		env.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if env.Now == nil {
		env.Now = time.Now
	}
	r := &Registry{m: map[string]Deliverer{}}
	for _, d := range []Deliverer{&Stock{env}, &Text{}, &Manual{}, &Webhook{env}, &Polaris{env}, &PanelCode{env}} {
		r.m[d.Kind()] = d
	}
	return r
}

// Get returns a deliverer by kind.
func (r *Registry) Get(kind string) (Deliverer, error) {
	d, ok := r.m[kind]
	if !ok {
		return nil, fmt.Errorf("未知发货方式 %q", kind)
	}
	return d, nil
}

// Kinds lists the deliverers for forms, in a stable order.
func (r *Registry) Kinds() []Deliverer {
	order := map[string]int{"stock": 0, "polaris": 1, "panelcode": 2, "text": 3, "webhook": 4, "manual": 5}
	out := make([]Deliverer, 0, len(r.m))
	for _, d := range r.m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return order[out[i].Kind()] < order[out[j].Kind()] })
	return out
}

// Placeholder marks text a product template left for the owner to fill in.
const Placeholder = "【请填写"

// ---- fixed text ----

// Text delivers the same text to every buyer (download links, guides).
// {order_no}, {email} and {qty} in the text are filled in per order.
type Text struct{}

// TextCfg is a fixed-text SKU's configuration.
type TextCfg struct {
	Text    string `json:"text"`
	PerUnit bool   `json:"per_unit"` // repeat once per unit bought
}

// ParseTextCfg reads a fixed-text configuration.
func ParseTextCfg(raw string) (*TextCfg, error) {
	var c TextCfg
	if err := json.Unmarshal(orEmpty(json.RawMessage(raw)), &c); err != nil {
		return nil, fmt.Errorf("配置不是有效 JSON：%w", err)
	}
	return &c, nil
}

func (Text) Kind() string  { return "text" }
func (Text) Label() string { return "固定文本" }
func (Text) Validate(cfg json.RawMessage) error {
	c, err := ParseTextCfg(string(cfg))
	if err != nil {
		return err
	}
	if strings.TrimSpace(c.Text) == "" {
		return errors.New("请填写发货内容")
	}
	if strings.Contains(c.Text, Placeholder) {
		return errors.New("发货内容里还有「" + Placeholder + "…】」，请换成实际内容")
	}
	if len(c.Text) > 20000 {
		return errors.New("发货内容最多 20000 字节")
	}
	return nil
}
func (Text) Reserve(context.Context, *gorm.DB, *model.Order, *model.SKU) error { return nil }
func (Text) Release(*gorm.DB, *model.Order) error                              { return nil }
func (Text) Deliver(_ context.Context, o *model.Order, sku *model.SKU) ([]Item, error) {
	c, err := ParseTextCfg(sku.DelivererCfg)
	if err != nil || c.Text == "" {
		return nil, Permanent(errors.New("固定文本配置无效"))
	}
	text := strings.NewReplacer("{order_no}", o.OrderNo, "{email}", o.Email, "{qty}", strconv.Itoa(o.Qty)).Replace(c.Text)
	n := 1
	if c.PerUnit {
		n = o.Qty
	}
	out := make([]Item, n)
	for i := range out {
		out[i] = Item{Content: text, Source: model.SourceText}
	}
	return out, nil
}
func (Text) Revoke(context.Context, *model.Order, *model.SKU) error { return ErrRevokeUnsupported }
func (Text) Stock(*model.SKU) int                                   { return -1 }

// ---- manual ----

// Manual waits for an admin to type the content in the backend.
type Manual struct{}

// ManualCfg is a manual SKU's configuration, all optional.
type ManualCfg struct {
	Ask  string `json:"ask"`  // what the buyer must fill in when ordering, e.g. 充值账号
	Note string `json:"note"` // shown to the buyer while the order waits, e.g. 30 分钟内处理
}

// ParseManualCfg reads a manual configuration.
func ParseManualCfg(raw string) (*ManualCfg, error) {
	var c ManualCfg
	if err := json.Unmarshal(orEmpty(json.RawMessage(raw)), &c); err != nil {
		return nil, fmt.Errorf("配置不是有效 JSON：%w", err)
	}
	c.Ask, c.Note = strings.TrimSpace(c.Ask), strings.TrimSpace(c.Note)
	return &c, nil
}

func (Manual) Kind() string  { return "manual" }
func (Manual) Label() string { return "人工发货" }
func (Manual) Validate(cfg json.RawMessage) error {
	c, err := ParseManualCfg(string(cfg))
	switch {
	case err != nil:
		return err
	case len([]rune(c.Ask)) > 20:
		return errors.New("买家填写项的名称最多 20 字")
	case len([]rune(c.Note)) > 300:
		return errors.New("发货说明最多 300 字")
	}
	return nil
}
func (Manual) Reserve(context.Context, *gorm.DB, *model.Order, *model.SKU) error { return nil }
func (Manual) Release(*gorm.DB, *model.Order) error                              { return nil }
func (Manual) Deliver(context.Context, *model.Order, *model.SKU) ([]Item, error) {
	return nil, ErrManual
}
func (Manual) Revoke(context.Context, *model.Order, *model.SKU) error { return ErrRevokeUnsupported }
func (Manual) Stock(*model.SKU) int                                   { return -1 }

func orEmpty(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return b
}

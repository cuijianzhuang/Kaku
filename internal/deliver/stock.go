package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/cuijianzhuang/kaku/internal/model"
)

// Stock sells imported cards (卡密). Polaris activation keys generated in
// the license backend can be imported here too ("预生成导入").
type Stock struct{ env *Env }

func (*Stock) Kind() string                   { return "stock" }
func (*Stock) Label() string                  { return "卡密库存" }
func (*Stock) Validate(json.RawMessage) error { return nil }

// Reserve picks qty available cards for the order. On PostgreSQL the rows
// are locked with FOR UPDATE SKIP LOCKED so concurrent buyers never pick the
// same card; SQLite runs one writer at a time.
func (s *Stock) Reserve(_ context.Context, tx *gorm.DB, o *model.Order, sku *model.SKU) error {
	var ids []uint
	q := tx.Model(&model.Card{}).Where("sku_id = ? AND status = ?", sku.ID, model.CardAvailable).Order("id").Limit(o.Qty)
	if err := s.env.Store.ForUpdate(q).Pluck("id", &ids).Error; err != nil {
		return err
	}
	if len(ids) < o.Qty {
		return ErrOutOfStock
	}
	until := o.ExpiresAt
	res := tx.Model(&model.Card{}).Where("id IN ? AND status = ?", ids, model.CardAvailable).
		Updates(map[string]any{"status": model.CardReserved, "order_id": o.ID, "reserved_until": until})
	if res.Error != nil {
		return res.Error
	}
	if int(res.RowsAffected) != len(ids) {
		return ErrOutOfStock
	}
	return refreshStock(tx, sku.ID)
}

// Release returns reserved cards to stock.
func (s *Stock) Release(tx *gorm.DB, o *model.Order) error {
	var c model.Card
	tx.Where("order_id = ?", o.ID).Take(&c)
	if err := tx.Model(&model.Card{}).Where("order_id = ? AND status = ?", o.ID, model.CardReserved).
		Updates(map[string]any{"status": model.CardAvailable, "order_id": nil, "reserved_until": nil}).Error; err != nil {
		return err
	}
	if c.SKUID != 0 {
		return refreshStock(tx, c.SKUID)
	}
	return nil
}

// Deliver marks the order's cards sold and returns them. Cards already
// sold to this order are returned again, so retries are harmless. An order
// paid after its reservation lapsed picks fresh cards if any are left.
func (s *Stock) Deliver(_ context.Context, o *model.Order, sku *model.SKU) ([]Item, error) {
	var items []Item
	err := s.env.Store.DB.Transaction(func(tx *gorm.DB) error {
		var cards []model.Card
		if err := tx.Where("order_id = ? AND status IN ?", o.ID, []string{model.CardReserved, model.CardSold}).Order("id").Find(&cards).Error; err != nil {
			return err
		}
		if missing := o.Qty - len(cards); missing > 0 {
			var more []model.Card
			q := tx.Where("sku_id = ? AND status = ?", sku.ID, model.CardAvailable).Order("id").Limit(missing)
			if err := s.env.Store.ForUpdate(q).Find(&more).Error; err != nil {
				return err
			}
			if len(more) < missing {
				return Permanent(fmt.Errorf("%w：还差 %d 张卡密，请补货后重新发货或退款", ErrOutOfStock, missing-len(more)))
			}
			cards = append(cards, more...)
		}
		now := s.env.Now()
		ids := make([]uint, len(cards))
		for i, c := range cards {
			ids[i] = c.ID
		}
		if err := tx.Model(&model.Card{}).Where("id IN ?", ids).
			Updates(map[string]any{"status": model.CardSold, "order_id": o.ID, "sold_at": now, "reserved_until": nil}).Error; err != nil {
			return err
		}
		for _, c := range cards {
			secret, err := s.env.Store.Box.Open(c.SecretEnc)
			if err != nil {
				return Permanent(err)
			}
			items = append(items, Item{Content: secret, Source: model.SourceCard, RefID: fmt.Sprint(c.ID)})
		}
		return refreshStock(tx, sku.ID)
	})
	return items, err
}

// Revoke cannot take back a card the buyer has seen; the cards stay sold.
func (*Stock) Revoke(context.Context, *model.Order, *model.SKU) error { return ErrRevokeUnsupported }

func (s *Stock) Stock(sku *model.SKU) int { return sku.StockCache }

func refreshStock(tx *gorm.DB, skuID uint) error {
	var n int64
	if err := tx.Model(&model.Card{}).Where("sku_id = ? AND status = ?", skuID, model.CardAvailable).Count(&n).Error; err != nil {
		return err
	}
	return tx.Model(&model.SKU{}).Where("id = ?", skuID).Update("stock_cache", n).Error
}

// RefreshStock recounts a SKU's available cards.
func RefreshStock(tx *gorm.DB, skuID uint) error { return refreshStock(tx, skuID) }

// ImportResult reports a card import.
type ImportResult struct {
	Added, Duplicate, Blank int
}

// ImportCards adds one card per non-empty line. Cards are de-duplicated by
// a keyed hash across every SKU (a card can only be sold once).
func ImportCards(env *Env, skuID uint, text string) (ImportResult, error) {
	var r ImportResult
	seen := map[string]bool{}
	var cards []model.Card
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			r.Blank++
			continue
		}
		if len(line) > 4000 {
			return r, errors.New("单条卡密不能超过 4000 字符")
		}
		h := env.Store.Box.Hash(line)
		if seen[h] {
			r.Duplicate++
			continue
		}
		seen[h] = true
		cards = append(cards, model.Card{SKUID: skuID, SecretEnc: env.Store.Box.Seal(line), SecretHash: h, Status: model.CardAvailable})
	}
	err := env.Store.DB.Transaction(func(tx *gorm.DB) error {
		hashes := make([]string, len(cards))
		for i, c := range cards {
			hashes[i] = c.SecretHash
		}
		existing := map[string]bool{}
		for i := 0; i < len(hashes); i += 500 {
			var found []string
			if err := tx.Model(&model.Card{}).Where("secret_hash IN ?", hashes[i:min(i+500, len(hashes))]).Pluck("secret_hash", &found).Error; err != nil {
				return err
			}
			for _, f := range found {
				existing[f] = true
			}
		}
		fresh := cards[:0]
		for _, c := range cards {
			if existing[c.SecretHash] {
				r.Duplicate++
				continue
			}
			fresh = append(fresh, c)
		}
		if len(fresh) > 0 {
			if err := tx.CreateInBatches(fresh, 200).Error; err != nil {
				return err
			}
		}
		r.Added = len(fresh)
		return refreshStock(tx, skuID)
	})
	return r, err
}

// ReleaseExpired frees cards whose reservation lapsed without an order to
// hold them (belt and braces next to order expiry).
func ReleaseExpired(tx *gorm.DB, now time.Time) error {
	return tx.Model(&model.Card{}).
		Where("status = ? AND reserved_until < ? AND order_id IN (?)", model.CardReserved, now,
			tx.Model(&model.Order{}).Select("id").Where("status IN ?", []string{model.OrderExpired, model.OrderRefunded})).
		Updates(map[string]any{"status": model.CardAvailable, "order_id": nil, "reserved_until": nil}).Error
}

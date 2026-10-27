package shop

import (
	"sort"
	"strconv"
	"time"

	"github.com/cuijianzhuang/kaku/internal/model"
)

// Sales statistics count orders by payment time. Revenue is net of
// refunds; refunded orders are reported separately.

// CurrencyTotal sums one currency over a period.
type CurrencyTotal struct {
	Currency       string
	Orders         int
	Amount         int64
	NewOrders      int
	RenewOrders    int
	Refunds        int
	RefundedAmount int64
}

// PeriodRow is one day (or month) of a period.
type PeriodRow struct {
	Label  string
	Orders int
	Amount map[string]int64
}

// ProductRow is one product/SKU's sales over a period.
type ProductRow struct {
	Title  string
	SKU    string
	Orders int
	Qty    int
	Amount map[string]int64
}

// SalesStats is a period's report.
type SalesStats struct {
	From, To   time.Time
	ByMonth    bool
	Totals     []CurrencyTotal
	Rows       []PeriodRow
	Products   []ProductRow
	Buyers     int
	Currencies []string
}

// Stats builds the report for [from, to). Periods longer than 62 days are
// grouped by month.
func (s *Service) Stats(from, to time.Time) (*SalesStats, error) {
	var orders []model.Order
	if err := s.Store.DB.Where("paid_at >= ? AND paid_at < ? AND status IN ?", from, to,
		[]string{model.OrderPaid, model.OrderDelivered, model.OrderFailed, model.OrderRefunded}).
		Order("paid_at").Find(&orders).Error; err != nil {
		return nil, err
	}
	st := &SalesStats{From: from, To: to, ByMonth: to.Sub(from) > 62*24*time.Hour}
	label := func(t time.Time) string {
		if st.ByMonth {
			return t.Local().Format("2006-01")
		}
		return t.Local().Format("01-02")
	}
	// Every period in range gets a row, so gaps show as empty bars.
	rows := map[string]*PeriodRow{}
	for t := from.Local(); t.Before(to); {
		l := label(t)
		if rows[l] == nil {
			rows[l] = &PeriodRow{Label: l, Amount: map[string]int64{}}
			st.Rows = append(st.Rows, PeriodRow{Label: l})
		}
		if st.ByMonth {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
		} else {
			t = t.AddDate(0, 0, 1)
		}
		if len(st.Rows) > 400 {
			break
		}
	}
	totals := map[string]*CurrencyTotal{}
	products := map[uint]*ProductRow{}
	buyers := map[string]bool{}
	for _, o := range orders {
		t := totals[o.Currency]
		if t == nil {
			t = &CurrencyTotal{Currency: o.Currency}
			totals[o.Currency] = t
		}
		if o.Status == model.OrderRefunded {
			t.Refunds++
			t.RefundedAmount += o.Amount
			continue
		}
		t.Orders++
		t.Amount += o.Amount
		if o.RenewOf != "" {
			t.RenewOrders++
		} else {
			t.NewOrders++
		}
		if r := rows[label(*o.PaidAt)]; r != nil {
			r.Orders++
			r.Amount[o.Currency] += o.Amount
		}
		p := products[o.SKUID]
		if p == nil {
			p = &ProductRow{Title: o.ProductTitle, SKU: o.SKUName, Amount: map[string]int64{}}
			products[o.SKUID] = p
		}
		p.Orders++
		p.Qty += o.Qty
		p.Amount[o.Currency] += o.Amount
		switch {
		case o.Email != "":
			buyers["e:"+o.Email] = true
		case o.TGUserID != 0:
			buyers["t:"+strconv.FormatInt(o.TGUserID, 10)] = true
		}
	}
	for i := range st.Rows {
		st.Rows[i] = *rows[st.Rows[i].Label]
	}
	for _, cur := range []string{model.CNY, model.USDT, model.XTR} {
		if t := totals[cur]; t != nil {
			st.Totals = append(st.Totals, *t)
			st.Currencies = append(st.Currencies, cur)
		}
	}
	for _, p := range products {
		st.Products = append(st.Products, *p)
	}
	sort.Slice(st.Products, func(i, j int) bool { return st.Products[i].Orders > st.Products[j].Orders })
	st.Buyers = len(buyers)
	return st, nil
}

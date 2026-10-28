package shop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/panelapi"
)

// fakePanel is a Polaris panel's redeem-code API.
type fakePanel struct {
	mu     sync.Mutex
	token  string
	codes  map[string][]string // batch -> codes
	used   map[string]bool
	voided []string
	n      int
}

func (f *fakePanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(401)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && r.URL.Path == "/adm/api/v1/redeem-codes":
		var items []map[string]any
		for _, c := range f.codes[r.URL.Query().Get("batch")] {
			it := map[string]any{"code": c}
			if f.used[c] {
				it["usedAt"] = time.Now()
			}
			items = append(items, it)
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	case r.Method == "POST" && r.URL.Path == "/adm/api/v1/redeem-codes":
		var req struct {
			PlanID string `json:"planId"`
			Count  int    `json:"count"`
			Batch  string `json:"batch"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var out []string
		for range req.Count {
			f.n++
			out = append(out, fmt.Sprintf("CODE-%04d", f.n))
		}
		f.codes[req.Batch] = append(f.codes[req.Batch], out...)
		json.NewEncoder(w).Encode(map[string]any{"codes": out})
	case r.Method == "DELETE" && r.URL.Path == "/adm/api/v1/redeem-codes":
		var req struct {
			Codes []string `json:"codes"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		f.voided = append(f.voided, req.Codes...)
		json.NewEncoder(w).Encode(map[string]any{"deleted": len(req.Codes)})
	default:
		w.WriteHeader(404)
	}
}

func TestPanelCodeDeliverRetryRefund(t *testing.T) {
	s, mb := setup(t)
	fake := &fakePanel{token: "plt_test", codes: map[string][]string{}, used: map[string]bool{}}
	ts := httptest.NewServer(fake)
	defer ts.Close()
	s.PanelClient = &panelapi.Client{BaseURL: ts.URL + "/adm", Token: "plt_test"}
	sku := mkSKU(t, s, "panelcode", `{"plan_id":"p1","name":"月付","expire_days":30}`, 1500)
	ctx := context.Background()

	o := order(t, s, sku, 2, "")
	// an earlier attempt already made one code for this order
	fake.codes[o.OrderNo] = []string{"EARLY-0001"}
	s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: "alipay", TradeNo: "Q1", Amount: o.PayAmount, Currency: model.CNY})
	runJobs(s)
	got, _ := s.OrderByNo(o.OrderNo)
	items, _ := s.Items(got.ID)
	if got.Status != model.OrderDelivered || len(items) != 2 || items[0].Content != "EARLY-0001" || len(fake.codes[o.OrderNo]) != 2 {
		t.Fatalf("status %s items %+v codes %v", got.Status, items, fake.codes)
	}
	runJobs(s) // the delivery notice is its own job
	if mail := strings.Join(mb.sent, "\n"); !strings.Contains(mail, "兑换方法") {
		t.Fatal("delivery mail lacks the redemption hint")
	}

	// refund: unused codes are voided, a redeemed one is reported
	fake.used["EARLY-0001"] = true
	warn, err := s.Refund(ctx, got.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.voided) != 1 || fake.voided[0] == "EARLY-0001" {
		t.Fatalf("voided %v", fake.voided)
	}
	if !strings.Contains(strings.Join(warn, ";"), "1 个兑换码已被兑换") {
		t.Fatalf("warnings %v", warn)
	}
}

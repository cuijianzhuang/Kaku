package shop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/kaku/internal/model"
)

func TestBackupCreatesListsAndPrunes(t *testing.T) {
	s, _ := setup(t)
	s.BackupDir = filepath.Join(t.TempDir(), "backups")
	set := s.Store.Settings()
	set.BackupKeep = 2
	s.Store.SaveSettings(set)
	base := time.Date(2026, 10, 1, 4, 0, 0, 0, time.Local)
	for i := range 3 {
		s.Now = func() time.Time { return base.Add(time.Duration(i) * time.Hour) }
		if _, err := s.Backup(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := s.Backups()
	if len(files) != 2 || files[0].Name != "kaku-20261001-060000.db.gz" {
		t.Fatalf("files %+v", files)
	}
	if info, _ := os.Stat(filepath.Join(s.BackupDir, files[0].Name)); info.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", info.Mode())
	}
	for _, bad := range []string{"../k.db", "kaku.db", "kaku-20261001-060000.db.gz/../../x"} {
		if _, err := s.BackupPath(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	// The daily backup runs once per day, after the configured hour.
	set.BackupEnabled, set.BackupHour = true, 5
	s.Store.SaveSettings(set)
	s.Now = func() time.Time { return base.Add(24 * time.Hour) } // 04:00 next day
	s.maybeBackup(context.Background())
	if s.Store.Value("backup_day") != "" {
		t.Fatal("backup ran before its hour")
	}
	s.Now = func() time.Time { return base.Add(26 * time.Hour) }
	s.maybeBackup(context.Background())
	s.maybeBackup(context.Background())
	files, _ = s.Backups()
	if s.Store.Value("backup_day") != "2026-10-02" || files[0].Name != "kaku-20261002-060000.db.gz" {
		t.Fatalf("daily backup: day %q files %+v", s.Store.Value("backup_day"), files)
	}
}

func TestReminders(t *testing.T) {
	s, mb := setup(t)
	sku := mkSKU(t, s, "polaris", `{"plan":"pro","days":365,"renewable":true}`, 100)
	_ = sku
	now := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	mk := func(id, exp string, status string) {
		s.Store.DB.Create(&model.LicenseBinding{OrderID: 1, LicenseID: id, Plan: "pro", ExpiresAt: exp, Email: "o@example.com", Status: status})
	}
	mk("lic_in7", "2026-11-03", model.LicenseActivated)  // 7 days
	mk("lic_in1", "2026-10-28", model.LicenseActivated)  // 1 day
	mk("lic_in20", "2026-11-16", model.LicenseActivated) // too far
	mk("lic_rev", "2026-10-28", model.LicenseRevoked)
	n, err := s.QueueReminders()
	if err != nil || n != 2 {
		t.Fatalf("queued %d %v", n, err)
	}
	if n, _ := s.QueueReminders(); n != 0 {
		t.Fatalf("queued twice: %d", n)
	}
	// One license gets renewed before its reminder goes out.
	s.Store.DB.Model(&model.LicenseBinding{}).Where("license_id = ?", "lic_in7").Update("expires_at", "2027-11-03")
	s.Store.RunDueJobs(context.Background(), s.JobHandlers())
	if len(mb.sent) != 1 || !strings.Contains(mb.sent[0], "lic_in1") || !strings.Contains(mb.sent[0], "https://shop.example.com/renew?lid=lic_in1") {
		t.Fatalf("mail %v", mb.sent)
	}
	// Turned off: nothing queued.
	set := s.Store.Settings()
	set.RemindEnabled = false
	s.Store.SaveSettings(set)
	s.Now = func() time.Time { return now.AddDate(0, 0, 6) }
	if n, _ := s.QueueReminders(); n != 0 {
		t.Fatalf("queued while off: %d", n)
	}
}

func TestStatsAndLateCoupon(t *testing.T) {
	s, _ := setup(t)
	sku := mkSKU(t, s, "text", `{"text":"x"}`, 1000)
	s.Store.DB.Create(&model.Coupon{Code: "HALF", Type: model.CouponPercent, Value: 50, Active: true})
	ctx := context.Background()
	paid := func(code string) *model.Order {
		o := order(t, s, sku, 1, code)
		if _, err := s.MarkPaid(ctx, PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual, TradeNo: o.OrderNo, Trusted: true}); err != nil {
			t.Fatal(err)
		}
		return o
	}
	paid("")
	refunded := paid("")
	s.Store.RunDueJobs(ctx, s.JobHandlers())
	if _, err := s.Refund(ctx, refunded.ID, "x"); err != nil {
		t.Fatal(err)
	}
	// Coupon order that expires, then pays late: the coupon use counts again.
	late := order(t, s, sku, 1, "HALF")
	s.Now = func() time.Time { return time.Now().Add(time.Hour) }
	s.ExpireOrders(ctx)
	var c model.Coupon
	s.Store.DB.Where("code = ?", "HALF").Take(&c)
	if c.Used != 0 {
		t.Fatalf("expiry kept coupon use: %d", c.Used)
	}
	s.Now = time.Now
	if _, err := s.MarkPaid(ctx, PaidInput{OrderNo: late.OrderNo, Channel: model.PayManual, TradeNo: "late", Trusted: true}); err != nil {
		t.Fatal(err)
	}
	s.Store.DB.Where("code = ?", "HALF").Take(&c)
	if c.Used != 1 {
		t.Fatalf("late payment coupon use: %d", c.Used)
	}

	st, err := s.Stats(time.Now().Add(-24*time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Totals) != 1 {
		t.Fatalf("totals %+v", st.Totals)
	}
	tot := st.Totals[0]
	if tot.Orders != 2 || tot.Amount != 1500 || tot.Refunds != 1 || tot.RefundedAmount != 1000 || st.Buyers != 1 {
		t.Fatalf("totals %+v buyers %d", tot, st.Buyers)
	}
	if len(st.Products) != 1 || st.Products[0].Qty != 2 {
		t.Fatalf("products %+v", st.Products)
	}
}

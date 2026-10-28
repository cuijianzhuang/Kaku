package shop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cuijianzhuang/kaku/internal/update"
)

func TestAutomaticUpgrade(t *testing.T) {
	s, _ := setup(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v0.0.4","html_url":"https://github.com/x"}`))
	}))
	defer ts.Close()
	u := update.New("v0.0.3", t.TempDir())
	u.UpdaterDir, u.API = t.TempDir(), ts.URL
	os.WriteFile(filepath.Join(u.UpdaterDir, "enabled"), nil, 0o644)
	s.Updater = u
	s.BackupDir = filepath.Join(t.TempDir(), "backups")

	base := time.Date(2026, 10, 28, 3, 0, 0, 0, time.Local)
	s.Now = func() time.Time { return base }
	u.Now = s.Now
	s.maybeUpdate(context.Background())
	if s.Store.Value("update_notified") != "v0.0.4" {
		t.Fatal("new release not noticed")
	}
	if u.Pending() {
		t.Fatal("upgraded although automatic upgrades are off")
	}

	set := s.Store.Settings()
	set.AutoUpdate, set.AutoUpdateHour = true, 5
	s.Store.SaveSettings(set)
	s.Now = func() time.Time { return base.Add(time.Hour) } // 04:00
	s.maybeUpdate(context.Background())
	if u.Pending() {
		t.Fatal("upgraded before the configured hour")
	}
	s.Now = func() time.Time { return base.Add(2 * time.Hour) } // 05:00
	s.maybeUpdate(context.Background())
	if !u.Pending() {
		t.Fatal("automatic upgrade not requested at the configured hour")
	}
	if files, _ := s.Backups(); len(files) != 1 {
		t.Fatalf("expected a backup before upgrading, got %d", len(files))
	}

	// The updater ran and failed: the same release is not retried.
	os.Remove(u.RequestFile())
	os.WriteFile(filepath.Join(u.UpdaterDir, "status"), []byte("state=failed\nfrom=v0.0.3\nto=v0.0.4\ntime=1793000000\n"), 0o644)
	s.Now = func() time.Time { return base.Add(26 * time.Hour) } // 05:00 next day
	s.maybeUpdate(context.Background())
	if u.Pending() {
		t.Fatal("a failed release was retried")
	}
	if s.Store.Value("update_reported") != "1793000000" {
		t.Fatal("upgrade result not reported")
	}
}

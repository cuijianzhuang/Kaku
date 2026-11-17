package store

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cuijianzhuang/kaku/internal/secure"
)

func TestUpdateSettingsPreservesConcurrentChanges(t *testing.T) {
	box, err := secure.NewBox(secure.RandomBytes(32))
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open("sqlite", filepath.Join(t.TempDir(), "settings.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	start := st.Settings().MaxUnpaid
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.UpdateSettings(func(s *Settings) error { s.MaxUnpaid++; return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got := st.Settings()
	if got.MaxUnpaid != start+40 {
		t.Fatalf("concurrent saves lost updates: max unpaid = %d", got.MaxUnpaid)
	}
	if err := st.UpdateSettings(func(s *Settings) error { s.SiteName = "invalid"; return errors.New("reject") }); err == nil {
		t.Fatal("invalid change was accepted")
	}
	if st.Settings().SiteName == "invalid" {
		t.Fatal("rejected change altered settings")
	}
}

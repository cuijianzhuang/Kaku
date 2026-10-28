package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.0.3", "v0.0.4", true},
		{"v0.0.9", "v0.0.10", true},
		{"v0.1.0", "v0.0.10", false},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2", "v1.2.1", true},
		{"dev", "v9.9.9", false},
		{"v1.0.0", "garbage", false},
		{"v1.0.0-rc1", "v1.0.0", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func testUpdater(t *testing.T, api string) *Updater {
	t.Helper()
	u := New("v0.0.3", t.TempDir())
	u.UpdaterDir = t.TempDir()
	u.API = api
	return u
}

func TestLatestCachesAndValidates(t *testing.T) {
	hits, tag := 0, "v0.0.4"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/repos/"+Repo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"tag_name":"` + tag + `","html_url":"https://github.com/x","published_at":"2026-10-01T00:00:00Z"}`))
	}))
	defer ts.Close()
	u := testUpdater(t, ts.URL)
	now := time.Now()
	u.Now = func() time.Time { return now }
	r, err := u.Latest(context.Background(), false)
	if err != nil || r.Tag != "v0.0.4" {
		t.Fatalf("latest = %+v, %v", r, err)
	}
	u.Latest(context.Background(), false)
	if hits != 1 {
		t.Fatalf("expected cached result, hits = %d", hits)
	}
	tag = "v0.0.5; rm -rf /"
	if _, err := u.Latest(context.Background(), true); err == nil {
		t.Fatal("invalid tag accepted")
	}
	if r, _ := u.Cached(); r.Tag != "v0.0.4" {
		t.Fatalf("a failed check must keep the last good release, got %q", r.Tag)
	}
}

func TestRequestAndStatus(t *testing.T) {
	u := testUpdater(t, "")
	if err := u.Request("v0.0.4"); err != ErrUnsupported {
		t.Fatalf("request without the updater: %v", err)
	}
	os.WriteFile(filepath.Join(u.UpdaterDir, "enabled"), nil, 0o644)
	if u.Mode() != ModeSystemd {
		t.Fatalf("mode = %s", u.Mode())
	}
	if err := u.Request("v0.0.4\nmalicious"); err == nil {
		t.Fatal("invalid tag accepted")
	}
	if err := u.Request("v0.0.4"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(u.RequestFile()); string(b) != "v0.0.4\n" {
		t.Fatalf("request = %q", b)
	}
	if !u.Pending() || u.Request("v0.0.4") == nil {
		t.Fatal("a second request must wait for the first")
	}
	os.Remove(u.RequestFile())

	stamp := time.Now().Unix()
	os.WriteFile(filepath.Join(u.UpdaterDir, "status"), []byte("state=running\nfrom=v0.0.3\nto=v0.0.4\ntime="+itoa(stamp)+"\n"), 0o644)
	if !u.Pending() {
		t.Fatal("a running upgrade is pending")
	}
	os.WriteFile(filepath.Join(u.UpdaterDir, "status"), []byte("state=failed\nfrom=v0.0.3\nto=v0.0.4\ntime="+itoa(stamp)+"\n"), 0o644)
	os.WriteFile(filepath.Join(u.UpdaterDir, "last.log"), []byte("\x1b[1m==>\x1b[0m 下载\n"), 0o644)
	st := u.LastStatus()
	if st == nil || st.State != "failed" || st.From != "v0.0.3" || st.To != "v0.0.4" || st.Time.Unix() != stamp || st.Log != "==> 下载\n" {
		t.Fatalf("status = %+v", st)
	}
	if u.Pending() {
		t.Fatal("finished upgrade reported as pending")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

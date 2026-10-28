// Package update checks GitHub Releases for new versions and hands
// upgrades to the system updater.
//
// The shop runs as an unprivileged user and cannot replace its own binary.
// Installs made with scripts/install.sh get a root-owned systemd path unit
// (kaku-update.path) that watches a request file in the data directory and
// runs `kaku-ctl apply-request`: it downloads the release, checks its
// SHA-256, restarts the service and rolls back if the new version does not
// come up. The result is written to a root-owned status directory that the
// shop can read but not change.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is the GitHub repository releases come from.
const Repo = "cuijianzhuang/Kaku"

// Release is a published version.
type Release struct {
	Tag       string    `json:"tag_name"`
	URL       string    `json:"html_url"`
	Notes     string    `json:"body"`
	Published time.Time `json:"published_at"`
}

// Status is the result of the last upgrade the updater ran.
type Status struct {
	State string // ok | failed | uptodate | running
	From  string
	To    string
	Time  time.Time
	Log   string // tail of the updater output
}

// Mode says how this instance can be upgraded.
type Mode string

const (
	ModeSystemd Mode = "systemd" // installed by install.sh with the updater unit
	ModeLegacy  Mode = "legacy"  // installed by install.sh before the updater existed
	ModeDocker  Mode = "docker"
	ModeManual  Mode = "manual" // a binary started some other way
)

// Updater checks for releases and files upgrade requests.
type Updater struct {
	Current    string // running version, e.g. v0.0.3 ("dev" for local builds)
	DataDir    string // request file goes here (writable by the shop)
	UpdaterDir string // status written here by root (read only for the shop)
	API        string // releases API base, overridable in tests
	HTTP       *http.Client
	Now        func() time.Time

	mu      sync.Mutex
	latest  *Release
	checked time.Time
	lastErr error
}

// New returns an updater with the default locations.
func New(current, dataDir string) *Updater {
	dir := os.Getenv("KAKU_UPDATER_DIR")
	if dir == "" {
		dir = "/var/lib/kaku-updater"
	}
	return &Updater{Current: current, DataDir: dataDir, UpdaterDir: dir,
		API: "https://api.github.com", HTTP: &http.Client{Timeout: 20 * time.Second}, Now: time.Now}
}

// RequestFile is where upgrade requests are written.
func (u *Updater) RequestFile() string { return filepath.Join(u.DataDir, "update-request") }

// Mode reports how this instance can be upgraded.
func (u *Updater) Mode() Mode {
	if _, err := os.Stat(filepath.Join(u.UpdaterDir, "enabled")); err == nil {
		return ModeSystemd
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return ModeDocker
	}
	if exe, err := os.Executable(); err == nil && exe == "/usr/local/bin/kaku" {
		if _, err := os.Stat("/usr/local/bin/kaku-ctl"); err == nil {
			return ModeLegacy
		}
	}
	return ModeManual
}

// Latest returns the newest release, cached for an hour unless force is set.
func (u *Updater) Latest(ctx context.Context, force bool) (*Release, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !force && !u.checked.IsZero() && u.Now().Sub(u.checked) < time.Hour {
		return u.latest, u.lastErr
	}
	r, err := u.fetch(ctx)
	u.checked = u.Now()
	u.lastErr = err
	if err == nil {
		u.latest = r
	}
	return u.latest, err
}

// Cached returns the last known release without network access.
func (u *Updater) Cached() (*Release, time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.latest, u.checked
}

func (u *Updater) fetch(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.API+"/repos/"+Repo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "kaku/"+u.Current)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("检查更新失败（服务器能否访问 api.github.com？）：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("检查更新失败：GitHub 返回 %s", resp.Status)
	}
	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("检查更新失败：%w", err)
	}
	if !ValidTag(r.Tag) {
		return nil, fmt.Errorf("检查更新失败：版本号 %q 格式不正确", r.Tag)
	}
	return &r, nil
}

var tagRe = regexp.MustCompile(`^v[0-9][0-9A-Za-z.+-]{0,39}$`)

// ValidTag reports whether t looks like a release tag (same rule as install.sh).
func ValidTag(t string) bool { return tagRe.MatchString(t) }

// Newer reports whether release tag b is newer than a. Unparseable
// versions (such as "dev") are never considered older than a release,
// so local builds are not upgraded automatically.
func Newer(a, b string) bool {
	pa, okA := parse(a)
	pb, okB := parse(b)
	if !okA || !okB {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pb[i] > pa[i]
		}
	}
	return false
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ErrUnsupported means this instance cannot be upgraded from the backend.
var ErrUnsupported = errors.New("当前安装方式不支持在线升级")

// Request asks the system updater to install tag.
func (u *Updater) Request(tag string) error {
	if u.Mode() != ModeSystemd {
		return ErrUnsupported
	}
	if !ValidTag(tag) {
		return fmt.Errorf("版本号格式不正确：%q", tag)
	}
	if u.Pending() {
		return errors.New("已有升级在进行中，请稍候")
	}
	tmp := u.RequestFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte(tag+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, u.RequestFile())
}

// Pending reports whether an upgrade is waiting or running (a "running"
// status older than 20 minutes is treated as stale).
func (u *Updater) Pending() bool {
	if _, err := os.Stat(u.RequestFile()); err == nil {
		return true
	}
	st := u.LastStatus()
	return st != nil && st.State == "running" && u.Now().Sub(st.Time) < 20*time.Minute
}

// LastStatus reads the result of the last upgrade (nil when none).
func (u *Updater) LastStatus() *Status {
	b, err := os.ReadFile(filepath.Join(u.UpdaterDir, "status"))
	if err != nil {
		return nil
	}
	st := &Status{}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "state":
			st.State = v
		case "from":
			st.From = v
		case "to":
			st.To = v
		case "time":
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				st.Time = time.Unix(n, 0)
			}
		}
	}
	if st.State == "" {
		return nil
	}
	if log, err := os.ReadFile(filepath.Join(u.UpdaterDir, "last.log")); err == nil {
		if len(log) > 8<<10 {
			log = log[len(log)-8<<10:]
		}
		st.Log = stripANSI(string(log))
	}
	return st
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*[A-Za-z]")

func stripANSI(s string) string { return ansi.ReplaceAllString(s, "") }

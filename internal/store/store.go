// Package store opens the database and keeps settings, the audit log and
// the durable job queue.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/secure"
)

// Store wraps the database.
type Store struct {
	DB  *gorm.DB
	Box *secure.Box
	// Postgres reports whether row locks (FOR UPDATE SKIP LOCKED) are
	// available. SQLite serializes writers instead.
	Postgres bool

	mu       sync.RWMutex
	settings *Settings
}

// Open connects to the database ("sqlite" with a file path, or "postgres"
// with a DSN) and migrates it.
func Open(driver, dsn string, box *secure.Box) (*Store, error) {
	cfg := &gorm.Config{TranslateError: true, Logger: logger.New(log.New(os.Stderr, "", log.LstdFlags),
		logger.Config{SlowThreshold: time.Second, LogLevel: logger.Warn, IgnoreRecordNotFoundError: true})}
	var (
		db  *gorm.DB
		err error
		pg  bool
	)
	switch driver {
	case "postgres", "pg", "postgresql":
		db, err = gorm.Open(postgres.Open(dsn), cfg)
		pg = true
	case "sqlite", "":
		if dir := filepath.Dir(dsn); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
		}
		db, err = gorm.Open(sqlite.Open(dsn+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"), cfg)
		if err == nil {
			sqlDB, _ := db.DB()
			// One writer: makes check-then-update sequences race free on SQLite.
			sqlDB.SetMaxOpenConns(1)
		}
	default:
		return nil, fmt.Errorf("不支持的数据库类型 %q（sqlite / postgres）", driver)
	}
	if err != nil {
		return nil, fmt.Errorf("连接数据库：%w", err)
	}
	if err := db.AutoMigrate(model.All()...); err != nil {
		return nil, fmt.Errorf("迁移数据库：%w", err)
	}
	s := &Store{DB: db, Box: box, Postgres: pg}
	if err := s.loadSettings(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ForUpdate adds a row lock that skips rows other transactions hold, where
// the database supports it.
func (s *Store) ForUpdate(tx *gorm.DB) *gorm.DB {
	if s.Postgres {
		return tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
	}
	return tx
}

// ---- settings ----

// Settings is everything configurable from the admin backend. Secret
// fields are encrypted at rest.
type Settings struct {
	SiteName   string `json:"site_name"`
	PublicURL  string `json:"public_url"` // https://shop.example.com
	Notice     string `json:"notice"`     // storefront announcement
	OrderTTL   int    `json:"order_ttl"`  // minutes an unpaid order holds stock
	MaxUnpaid  int    `json:"max_unpaid"` // unpaid orders per IP / email / Telegram user
	SupportURL string `json:"support_url"`

	EPayURL      string `json:"epay_url"`
	EPayPID      string `json:"epay_pid"`
	EPayKey      string `json:"epay_key" secret:"1"`
	EPayAlipay   bool   `json:"epay_alipay"`
	EPayWxpay    bool   `json:"epay_wxpay"`
	USDTAddress  string `json:"usdt_address"`
	TronGridKey  string `json:"trongrid_key" secret:"1"`
	USDTEnabled  bool   `json:"usdt_enabled"`
	StarsEnabled bool   `json:"stars_enabled"`

	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	SMTPUser string `json:"smtp_user"`
	SMTPPass string `json:"smtp_pass" secret:"1"`
	SMTPFrom string `json:"smtp_from"`

	TGToken         string `json:"tg_token" secret:"1"`
	TGWebhookSecret string `json:"tg_webhook_secret" secret:"1"`
	TGAdminChat     int64  `json:"tg_admin_chat"`
	TGChannel       string `json:"tg_channel"` // @channel or -100… for announcements
	TGBotUsername   string `json:"tg_bot_username"`

	PolarisURL    string `json:"polaris_url"` // https://license.example.com
	PolarisSecret string `json:"polaris_secret" secret:"1"`

	TurnstileSite   string `json:"turnstile_site"`
	TurnstileSecret string `json:"turnstile_secret" secret:"1"`

	BackupEnabled  bool `json:"backup_enabled"`
	BackupHour     int  `json:"backup_hour"`     // local hour of the daily backup
	BackupKeep     int  `json:"backup_keep"`     // backups kept on disk
	BackupTelegram bool `json:"backup_telegram"` // also send each backup to the admin chat

	RemindEnabled bool `json:"remind_enabled"` // license expiry reminders (7 days and 1 day before)

	GitHubClientID     string `json:"github_client_id"` // admin sign-in with GitHub
	GitHubClientSecret string `json:"github_client_secret" secret:"1"`
	GoogleClientID     string `json:"google_client_id"` // admin sign-in with Google
	GoogleClientSecret string `json:"google_client_secret" secret:"1"`
	AdminEntry         bool   `json:"admin_link_in_footer"` // show a 后台 link in the shop header and footer

	AutoUpdate     bool `json:"auto_update"`      // install new releases automatically
	AutoUpdateHour int  `json:"auto_update_hour"` // local hour of the daily check
	UpdateNotify   bool `json:"update_notify"`    // tell the admin chat about new releases

	Theme   string `json:"theme"`    // colour scheme: "" (简约), paper, ocean, forest, sakura, midnight
	Mode    string `json:"mode"`     // appearance: "" (follow the system), light, dark
	Accent  string `json:"accent"`   // accent override: "" (theme's own), ink, indigo, emerald, amber, rose, blue
	LogoURL string `json:"logo_url"` // shop logo image; empty = the Kaku mark
}

func defaults() *Settings {
	return &Settings{SiteName: "Kaku 发卡", OrderTTL: 15, MaxUnpaid: 3, SMTPPort: 465, EPayAlipay: true, EPayWxpay: true,
		BackupHour: 4, BackupKeep: 7, RemindEnabled: true, AutoUpdateHour: 5, UpdateNotify: true}
}

const settingsKey = "settings"

func (s *Store) loadSettings() error {
	st := defaults()
	var row model.Setting
	err := s.DB.Where("key = ?", settingsKey).Take(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
	case err != nil:
		return err
	default:
		var raw map[string]any
		if err := json.Unmarshal([]byte(row.Value), &raw); err != nil {
			return fmt.Errorf("读取设置：%w", err)
		}
		for k, v := range raw {
			if str, ok := v.(string); ok && strings.HasPrefix(str, "enc:") {
				plain, err := s.Box.Open(strings.TrimPrefix(str, "enc:"))
				if err != nil {
					return fmt.Errorf("解密设置 %s：%w", k, err)
				}
				raw[k] = plain
			}
		}
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, st); err != nil {
			return fmt.Errorf("读取设置：%w", err)
		}
	}
	s.mu.Lock()
	s.settings = st
	s.mu.Unlock()
	return nil
}

// Settings returns a copy of the current settings.
func (s *Store) Settings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return *s.settings
}

// SaveSettings stores settings, encrypting the secret fields.
func (s *Store) SaveSettings(st Settings) error {
	b, _ := json.Marshal(st)
	var raw map[string]any
	json.Unmarshal(b, &raw)
	for _, k := range secretFields {
		if v, ok := raw[k].(string); ok && v != "" {
			raw[k] = "enc:" + s.Box.Seal(v)
		}
	}
	b, _ = json.Marshal(raw)
	row := model.Setting{Key: settingsKey, Value: string(b), UpdatedAt: time.Now()}
	if err := s.DB.Save(&row).Error; err != nil {
		return err
	}
	s.mu.Lock()
	s.settings = &st
	s.mu.Unlock()
	return nil
}

var secretFields = []string{"epay_key", "trongrid_key", "smtp_pass", "tg_token", "tg_webhook_secret", "polaris_secret", "turnstile_secret", "github_client_secret", "google_client_secret"}

// ---- small internal values (not admin settings) ----

// Value reads an internal value ("" when unset).
func (s *Store) Value(key string) string {
	var row model.Setting
	if s.DB.Where("key = ?", "v:"+key).Take(&row).Error != nil {
		return ""
	}
	return row.Value
}

// SetValue stores an internal value.
func (s *Store) SetValue(key, value string) error {
	return s.DB.Save(&model.Setting{Key: "v:" + key, Value: value, UpdatedAt: time.Now()}).Error
}

// ---- audit ----

// Audit appends to the audit log; failures are ignored.
func (s *Store) Audit(admin, action, target, detail, ip string) {
	s.DB.Create(&model.AuditLog{Admin: admin, Action: action, Target: target, Detail: detail, IP: ip})
}

// ---- jobs ----

// Enqueue adds a job unless one with the same key exists. tx may be a
// transaction.
func Enqueue(tx *gorm.DB, kind, key string, payload any, at time.Time) error {
	var body string
	switch p := payload.(type) {
	case string:
		body = p
	case nil:
	default:
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		body = string(b)
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.Job{
		Kind: kind, Key: key, Payload: body, Status: model.JobPending, RunAt: at,
	}).Error
}

// Requeue resets a finished or dead job so it runs again (redeliver).
func (s *Store) Requeue(key string, at time.Time) error {
	res := s.DB.Model(&model.Job{}).Where("key = ?", key).
		Updates(map[string]any{"status": model.JobPending, "run_at": at, "attempts": 0, "last_error": ""})
	return res.Error
}

// JobFunc runs one job. A nil error finishes it; a RetryError schedules
// another attempt; any other error buries it.
type JobFunc func(ctx context.Context, j *model.Job) error

// RetryError asks for another attempt at After.
type RetryError struct {
	Err   error
	After time.Duration
}

func (e *RetryError) Error() string { return e.Err.Error() }
func (e *RetryError) Unwrap() error { return e.Err }

// RunJobs polls for due jobs until ctx ends.
func (s *Store) RunJobs(ctx context.Context, handlers map[string]JobFunc, every time.Duration) {
	// Jobs left running by a crash are picked up again.
	s.DB.Model(&model.Job{}).Where("status = ?", model.JobRunning).Update("status", model.JobPending)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.RunDueJobs(ctx, handlers)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunDueJobs runs every job that is due now, once.
func (s *Store) RunDueJobs(ctx context.Context, handlers map[string]JobFunc) {
	var jobs []model.Job
	s.DB.Where("status = ? AND run_at <= ?", model.JobPending, time.Now()).Order("run_at").Limit(50).Find(&jobs)
	for i := range jobs {
		j := &jobs[i]
		res := s.DB.Model(&model.Job{}).Where("id = ? AND status = ?", j.ID, model.JobPending).Update("status", model.JobRunning)
		if res.Error != nil || res.RowsAffected == 0 {
			continue
		}
		h := handlers[j.Kind]
		var err error
		if h == nil {
			err = fmt.Errorf("unknown job kind %q", j.Kind)
		} else {
			err = safeRun(ctx, h, j)
		}
		j.Attempts++
		upd := map[string]any{"attempts": j.Attempts}
		var retry *RetryError
		switch {
		case err == nil:
			upd["status"], upd["last_error"] = model.JobDone, ""
		case errors.As(err, &retry):
			upd["status"], upd["run_at"], upd["last_error"] = model.JobPending, time.Now().Add(retry.After), trim(err.Error(), 500)
		default:
			upd["status"], upd["last_error"] = model.JobDead, trim(err.Error(), 500)
		}
		s.DB.Model(&model.Job{}).Where("id = ?", j.ID).Updates(upd)
		if ctx.Err() != nil {
			return
		}
	}
}

func safeRun(ctx context.Context, h JobFunc, j *model.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return h(ctx, j)
}

func trim(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

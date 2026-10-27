package shop

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Backups are gzipped SQLite copies in BackupDir. They hold card secrets
// and payment keys only in encrypted form: restoring one needs the same
// KAKU_MASTER_KEY.

// BackupFile is one backup on disk.
type BackupFile struct {
	Name    string
	Size    int64
	ModTime time.Time
}

var backupName = regexp.MustCompile(`^kaku-\d{8}-\d{6}\.db\.gz$`)

// ErrBackupUnavailable means backups cannot run here (PostgreSQL, or no
// backup directory configured).
var ErrBackupUnavailable = errors.New("自动备份只支持 SQLite；PostgreSQL 请使用 pg_dump")

func (s *Service) backupReady() error {
	if s.BackupDir == "" || s.Store.Postgres {
		return ErrBackupUnavailable
	}
	return os.MkdirAll(s.BackupDir, 0o700)
}

// Backup writes a new backup and prunes old ones. It returns the file name.
func (s *Service) Backup(ctx context.Context) (string, error) {
	if err := s.backupReady(); err != nil {
		return "", err
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	now := s.Now()
	name := "kaku-" + now.Format("20060102-150405") + ".db.gz"
	raw := filepath.Join(s.BackupDir, ".tmp-"+now.Format("150405.000000")+".db")
	defer os.Remove(raw)
	if err := s.Store.DB.WithContext(ctx).Exec("VACUUM INTO ?", raw).Error; err != nil {
		return "", fmt.Errorf("复制数据库：%w", err)
	}
	dst := filepath.Join(s.BackupDir, name)
	if err := gzipFile(raw, dst+".part"); err != nil {
		os.Remove(dst + ".part")
		return "", err
	}
	if err := os.Rename(dst+".part", dst); err != nil {
		return "", err
	}
	s.pruneBackups()
	s.Log.Info("backup written", "file", name)
	return name, nil
}

func gzipFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(out)
	if _, err := io.Copy(zw, in); err != nil {
		out.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Backups lists backups, newest first.
func (s *Service) Backups() ([]BackupFile, error) {
	if s.BackupDir == "" || s.Store.Postgres {
		return nil, nil
	}
	entries, err := os.ReadDir(s.BackupDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []BackupFile
	for _, e := range entries {
		if !backupName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupFile{Name: e.Name(), Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// BackupPath resolves a backup name safely (only names Backup creates).
func (s *Service) BackupPath(name string) (string, error) {
	if s.BackupDir == "" || !backupName.MatchString(name) {
		return "", errors.New("备份不存在")
	}
	p := filepath.Join(s.BackupDir, name)
	if _, err := os.Stat(p); err != nil {
		return "", errors.New("备份不存在")
	}
	return p, nil
}

func (s *Service) pruneBackups() {
	keep := s.Store.Settings().BackupKeep
	if keep < 1 {
		keep = 7
	}
	files, _ := s.Backups()
	for i, f := range files {
		if i >= keep {
			os.Remove(filepath.Join(s.BackupDir, f.Name))
		}
	}
}

// telegramMaxFile is the Bot API upload limit for documents.
const telegramMaxFile = 50 << 20

// sendBackup sends a backup to the admin chat.
func (s *Service) sendBackup(ctx context.Context, name string) error {
	st := s.Store.Settings()
	if st.TGAdminChat == 0 || st.TGToken == "" {
		return errors.New("未配置 Telegram 管理员 Chat ID")
	}
	p, err := s.BackupPath(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if len(data) > telegramMaxFile {
		return fmt.Errorf("备份 %d MB，超过 Telegram 50 MB 限制，只保存在服务器", len(data)>>20)
	}
	caption := fmt.Sprintf("%s 数据库备份 %s（恢复需要同一个 KAKU_MASTER_KEY）", st.SiteName, strings.TrimSuffix(name, ".db.gz"))
	return s.Bot().SendDocument(ctx, st.TGAdminChat, name, data, caption)
}

// maybeBackup runs the daily backup once the configured hour arrives.
func (s *Service) maybeBackup(ctx context.Context) {
	st := s.Store.Settings()
	now := s.Now()
	if !st.BackupEnabled || now.Hour() < st.BackupHour || s.backupReady() != nil {
		return
	}
	day := now.Format("2006-01-02")
	if s.Store.Value("backup_day") == day {
		return
	}
	s.Store.SetValue("backup_day", day)
	name, err := s.Backup(ctx)
	if err != nil {
		s.Log.Error("daily backup failed", "err", err)
		s.Notify.Admin("❌ 每日备份失败：" + err.Error())
		return
	}
	if st.BackupTelegram {
		if err := s.sendBackup(ctx, name); err != nil {
			s.Log.Warn("backup not sent to telegram", "err", err)
			s.Notify.Admin("⚠️ 备份 " + name + " 已保存，但未能发送到 Telegram：" + err.Error())
		}
	}
}

// SendBackup sends an existing backup to the admin chat (backend button).
func (s *Service) SendBackup(ctx context.Context, name string) error { return s.sendBackup(ctx, name) }

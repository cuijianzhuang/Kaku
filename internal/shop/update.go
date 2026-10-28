package shop

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/cuijianzhuang/kaku/internal/update"
)

// maybeUpdate runs once a minute: it reports the result of the last
// upgrade, checks for new releases every six hours (telling the admin
// chat once per release) and, when automatic upgrades are on, installs a
// new release at the configured hour. A release that failed to install
// is not retried automatically.
func (s *Service) maybeUpdate(ctx context.Context) {
	u := s.Updater
	if u == nil {
		return
	}
	s.reportUpgrade()
	st := s.Store.Settings()
	now := s.Now()
	day := now.Format("2006-01-02")
	upgradeTime := st.AutoUpdate && now.Hour() == st.AutoUpdateHour && s.Store.Value("update_day") != day
	_, checked := u.Cached()
	if !upgradeTime && !checked.IsZero() && now.Sub(checked) < 6*time.Hour {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rel, err := u.Latest(cctx, upgradeTime)
	if err != nil || rel == nil || !update.Newer(u.Current, rel.Tag) {
		if upgradeTime {
			s.Store.SetValue("update_day", day)
		}
		return
	}
	if st.UpdateNotify && s.Store.Value("update_notified") != rel.Tag {
		s.Store.SetValue("update_notified", rel.Tag)
		msg := fmt.Sprintf("🆕 Kaku 有新版本 %s（当前 %s）\n%s", rel.Tag, u.Current, rel.URL)
		if st.AutoUpdate && u.Mode() == update.ModeSystemd {
			msg += fmt.Sprintf("\n将在 %d:00 自动升级。", st.AutoUpdateHour)
		} else {
			msg += "\n在后台「升级」页面安装。"
		}
		s.Notify.Admin(msg)
	}
	if !upgradeTime || u.Mode() != update.ModeSystemd {
		return
	}
	s.Store.SetValue("update_day", day)
	if s.Store.Value("update_tried") == rel.Tag {
		return
	}
	s.Store.SetValue("update_tried", rel.Tag)
	if err := s.StartUpgrade(ctx, rel.Tag); err != nil {
		s.Log.Error("automatic upgrade not started", "err", err)
		s.Notify.Admin("❌ 自动升级到 " + rel.Tag + " 未能开始：" + err.Error())
		return
	}
	s.Log.Info("automatic upgrade requested", "to", rel.Tag)
}

// StartUpgrade backs up the database (SQLite) and asks the system
// updater to install tag. The service restarts when it is done.
func (s *Service) StartUpgrade(ctx context.Context, tag string) error {
	if s.Updater == nil {
		return update.ErrUnsupported
	}
	if s.backupReady() == nil {
		bctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if _, err := s.Backup(bctx); err != nil {
			return fmt.Errorf("升级前备份失败：%w", err)
		}
	}
	return s.Updater.Request(tag)
}

// reportUpgrade tells the admin chat how the last upgrade went, once.
func (s *Service) reportUpgrade() {
	st := s.Updater.LastStatus()
	if st == nil || st.Time.IsZero() || st.State == "running" {
		return
	}
	stamp := strconv.FormatInt(st.Time.Unix(), 10)
	if s.Store.Value("update_reported") == stamp {
		return
	}
	s.Store.SetValue("update_reported", stamp)
	switch st.State {
	case "ok":
		s.Notify.Admin(fmt.Sprintf("✅ Kaku 已升级：%s → %s", st.From, st.To))
	case "failed":
		s.Notify.Admin(fmt.Sprintf("❌ Kaku 升级到 %s 失败，已回滚到 %s。详情见后台「升级」页面。", st.To, st.From))
	}
}

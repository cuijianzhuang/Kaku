package shop

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/store"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
)

// License expiry reminders: Polaris licenses sold here get an email and a
// Telegram message 7 days and 1 day before they expire, with a renewal
// link. The job key carries the expiry date and the tier, so each reminder
// is sent once per term; renewing moves the expiry and makes room for the
// next term's reminders.

type reminder struct {
	LicenseID string `json:"license_id"`
	ExpiresAt string `json:"expires_at"`
	Days      int    `json:"days"`
}

// remindTier is 7 or 1 for a license due for a reminder, else 0.
func remindTier(daysLeft int) int {
	switch {
	case daysLeft < 0:
		return 0
	case daysLeft <= 1:
		return 1
	case daysLeft <= 7:
		return 7
	}
	return 0
}

// QueueReminders queues the reminders that are due now. It returns how
// many new reminders were queued.
func (s *Service) QueueReminders() (int, error) {
	if !s.Store.Settings().RemindEnabled {
		return 0, nil
	}
	now := s.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from, until := today.Format("2006-01-02"), today.AddDate(0, 0, 7).Format("2006-01-02")
	var bs []model.LicenseBinding
	if err := s.Store.DB.Where("status <> ? AND offline = ? AND expires_at <> '' AND expires_at >= ? AND expires_at <= ?",
		model.LicenseRevoked, false, from, until).Find(&bs).Error; err != nil {
		return 0, err
	}
	queued := 0
	for _, b := range bs {
		exp, err := time.Parse("2006-01-02", b.ExpiresAt)
		if err != nil || (b.Email == "" && b.TGUserID == 0) {
			continue
		}
		tier := remindTier(int(exp.Sub(today).Hours() / 24))
		if tier == 0 {
			continue
		}
		var before int64
		key := fmt.Sprintf("remind:%s:%s:%d", b.LicenseID, b.ExpiresAt, tier)
		s.Store.DB.Model(&model.Job{}).Where("key = ?", key).Count(&before)
		if before > 0 {
			continue
		}
		if err := store.Enqueue(s.Store.DB, "remind", key, reminder{LicenseID: b.LicenseID, ExpiresAt: b.ExpiresAt, Days: tier}, now); err != nil {
			return queued, err
		}
		queued++
	}
	return queued, nil
}

func (s *Service) remindJob(ctx context.Context, j *model.Job) error {
	var r reminder
	if err := json.Unmarshal([]byte(j.Payload), &r); err != nil {
		return err
	}
	b, err := s.Binding(r.LicenseID)
	if err != nil {
		return nil
	}
	// Renewed, revoked or changed since it was queued: nothing to say.
	if b.ExpiresAt != r.ExpiresAt || b.Status == model.LicenseRevoked {
		return nil
	}
	skus, _, err := s.RenewSKUs(b)
	if err != nil || len(skus) == 0 {
		return nil // nothing on sale to renew with
	}
	st := s.Store.Settings()
	base := strings.TrimRight(st.PublicURL, "/")
	renewURL := ""
	if strings.HasPrefix(base, "https://") || strings.HasPrefix(base, "http://") {
		renewURL = base + "/renew?lid=" + url.QueryEscape(b.LicenseID)
	}
	when := "即将"
	if exp, err := time.Parse("2006-01-02", b.ExpiresAt); err == nil {
		now := s.Now()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		switch days := int(exp.Sub(today).Hours() / 24); {
		case days <= 0:
			when = "今天"
		case days == 1:
			when = "明天"
		default:
			when = fmt.Sprintf("%d 天后", days)
		}
	}
	var errs []string
	if b.Email != "" && s.Notify.MailReady() && renewURL != "" {
		body := fmt.Sprintf("您好，\n\n您的 Polaris %s 许可证 %s 将于 %s（%s）到期。\n\n续费链接：%s\n（用购买时的邮箱验证；续费后面板会在下次校验时自动生效，无需重新激活。）\n\n到期后面板会回落到社区版，已生效的配置保持不变。\n",
			b.Plan, b.LicenseID, b.ExpiresAt, when, renewURL)
		if err := s.Notify.Mail(b.Email, fmt.Sprintf("【%s】许可证将于 %s 到期", st.SiteName, b.ExpiresAt), body); err != nil {
			errs = append(errs, "邮件："+err.Error())
		}
	}
	if b.TGUserID != 0 && s.Bot().Ready() {
		text := fmt.Sprintf("⏰ 你的 Polaris %s 许可证 <code>%s</code> 将于 <b>%s</b>（%s）到期。\n续费后面板自动生效，无需重新激活。",
			html.EscapeString(b.Plan), b.LicenseID, b.ExpiresAt, when)
		var kb *tgapi.Keyboard
		if st.TGBotUsername != "" {
			kb = &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{{{Text: "立即续费", URL: "https://t.me/" + st.TGBotUsername + "?start=renew_" + b.LicenseID}}}}
		} else if strings.HasPrefix(renewURL, "https://") {
			kb = &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{{{Text: "立即续费", URL: renewURL}}}}
		}
		if err := s.Bot().SendMessage(ctx, b.TGUserID, text, true, kb); err != nil {
			errs = append(errs, "Telegram："+err.Error())
		}
	}
	if len(errs) > 0 {
		err := fmt.Errorf("%s", strings.Join(errs, "；"))
		if j.Attempts < 3 {
			return &store.RetryError{Err: err, After: 30 * time.Minute}
		}
		return err
	}
	return nil
}

// maybeRemind queues reminders once a day.
func (s *Service) maybeRemind() {
	now := s.Now()
	day := now.Format("2006-01-02")
	if now.Hour() < 10 || s.Store.Value("remind_day") == day {
		return
	}
	s.Store.SetValue("remind_day", day)
	if n, err := s.QueueReminders(); err != nil {
		s.Log.Error("queue reminders failed", "err", err)
	} else if n > 0 {
		s.Log.Info("license reminders queued", "count", n)
	}
}

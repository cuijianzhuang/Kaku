// Package notify sends mail over SMTP and admin alerts over Telegram.
package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	"github.com/cuijianzhuang/kaku/internal/store"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
)

// Notifier sends mail and Telegram alerts using the current settings.
type Notifier struct {
	Store *store.Store
	Log   *slog.Logger
	// Send overrides SMTP delivery (tests).
	Send func(to, subject, body string) error
}

// MailReady reports whether SMTP is configured.
func (n *Notifier) MailReady() bool {
	st := n.Store.Settings()
	return n.Send != nil || (st.SMTPHost != "" && st.SMTPFrom != "")
}

// Mail sends an email: the text as given plus an HTML version of it.
func (n *Notifier) Mail(to, subject, body string) error {
	if n.Send != nil {
		return n.Send(to, subject, body)
	}
	st := n.Store.Settings()
	if st.SMTPHost == "" || st.SMTPFrom == "" {
		return errors.New("未配置 SMTP")
	}
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("收件地址无效：%w", err)
	}
	from, err := mail.ParseAddress(st.SMTPFrom)
	if err != nil {
		return fmt.Errorf("发件地址无效：%w", err)
	}
	port := st.SMTPPort
	if port == 0 {
		port = 465
	}
	addr := net.JoinHostPort(st.SMTPHost, fmt.Sprint(port))
	msg := buildMessage(from.String(), to, subject, st.SiteName, body, time.Now())
	var auth smtp.Auth
	if st.SMTPUser != "" {
		auth = smtp.PlainAuth("", st.SMTPUser, st.SMTPPass, st.SMTPHost)
	}
	if port != 465 {
		// STARTTLS (smtp.SendMail upgrades when offered).
		return smtp.SendMail(addr, auth, from.Address, []string{to}, []byte(msg))
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{ServerName: st.SMTPHost})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, st.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Bot returns a Telegram client for the configured token.
func (n *Notifier) Bot() *tgapi.Client {
	return &tgapi.Client{Token: n.Store.Settings().TGToken}
}

// Admin sends an alert to the admin chat (best effort).
func (n *Notifier) Admin(text string) {
	st := n.Store.Settings()
	if st.TGToken == "" || st.TGAdminChat == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := n.Bot().SendMessage(ctx, st.TGAdminChat, text, false, nil); err != nil && n.Log != nil {
		n.Log.Warn("admin alert failed", "err", err)
	}
}

func buildMessage(from, to, subject, site, body string, now time.Time) string {
	boundary := "kaku-" + fmt.Sprintf("%x", now.UnixNano())
	return strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + mime.BEncoding.Encode("utf-8", subject),
		"Date: " + now.Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		`Content-Type: multipart/alternative; boundary="` + boundary + `"`,
		"",
		"--" + boundary,
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: base64",
		"", b64(body),
		"--" + boundary,
		"Content-Type: text/html; charset=utf-8",
		"Content-Transfer-Encoding: base64",
		"", b64(mailHTML(site, subject, body)),
		"--" + boundary + "--", "",
	}, "\r\n")
}

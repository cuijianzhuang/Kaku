package web

import (
	"context"
	"encoding/csv"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/payment"
)

func (s *Server) extraRoutes(a *gin.RouterGroup) {
	a.GET("/stats", s.statsPage)
	a.GET("/orders.csv", s.exportOrders)
	a.GET("/backups", s.backupsPage)
	a.POST("/backups/run", s.runBackup)
	a.POST("/backups/settings", s.saveBackupSettings)
	a.POST("/backups/download", s.downloadBackup)
	a.POST("/backups/send", s.sendBackup)
}

// statsRange turns a range name into [from, to) in local time.
func statsRange(name string, now time.Time) (time.Time, time.Time, string) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	tomorrow := day.AddDate(0, 0, 1)
	switch name {
	case "today":
		return day, tomorrow, "今天"
	case "7d":
		return day.AddDate(0, 0, -6), tomorrow, "近 7 天"
	case "month":
		return month, tomorrow, "本月"
	case "lastmonth":
		return month.AddDate(0, -1, 0), month, "上月"
	case "year":
		return time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location()), tomorrow, "今年"
	}
	return day.AddDate(0, 0, -29), tomorrow, "近 30 天"
}

type bar struct {
	Label   string
	Orders  int
	Amount  int64
	Percent int
}

func (s *Server) statsPage(c *gin.Context) {
	rng := c.DefaultQuery("range", "30d")
	from, to, label := statsRange(rng, s.now())
	st, err := s.shop.Stats(from, to)
	if err != nil {
		redirectErr(c, s.cfg.AdminPath, err)
		return
	}
	cur := c.Query("currency")
	if cur == "" && len(st.Currencies) > 0 {
		cur = st.Currencies[0]
	}
	if cur == "" {
		cur = model.CNY
	}
	var maxAmount int64
	for _, r := range st.Rows {
		maxAmount = max(maxAmount, r.Amount[cur])
	}
	bars := make([]bar, len(st.Rows))
	for i, r := range st.Rows {
		b := bar{Label: r.Label, Orders: r.Orders, Amount: r.Amount[cur]}
		if maxAmount > 0 {
			b.Percent = int(b.Amount * 100 / maxAmount)
		}
		bars[i] = b
	}
	s.render(c, 200, "admin_stats", gin.H{"Stats": st, "Range": rng, "RangeLabel": label, "Currency": cur, "Bars": bars,
		"Ranges": [][2]string{{"today", "今天"}, {"7d", "近 7 天"}, {"30d", "近 30 天"}, {"month", "本月"}, {"lastmonth", "上月"}, {"year", "今年"}}})
}

// exportOrders downloads the orders matching the list filters as CSV.
// Delivered content is never exported.
func (s *Server) exportOrders(c *gin.Context) {
	q := s.st.DB.Model(&model.Order{})
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	if ch := c.Query("channel"); ch != "" {
		q = q.Where("channel = ?", ch)
	}
	if kw := strings.TrimSpace(c.Query("q")); kw != "" {
		like := "%" + strings.ToLower(kw) + "%"
		q = q.Where("order_no = ? OR email LIKE ? OR renew_of = ? OR client_ip = ?", kw, like, kw, kw)
	}
	var orders []model.Order
	if err := q.Order("id DESC").Limit(50000).Find(&orders).Error; err != nil {
		redirectErr(c, s.cfg.AdminPath+"/orders", err)
		return
	}
	s.audit(c, "orders_export", "", fmt.Sprintf("%d 单", len(orders)))
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="orders-%s.csv"`, s.now().Format("20060102-1504")))
	c.Header("Cache-Control", "no-store")
	c.Writer.WriteString("\uFEFF") // Excel reads UTF-8 with a BOM
	w := csv.NewWriter(c.Writer)
	w.Write([]string{"订单号", "下单时间", "支付时间", "发货时间", "状态", "渠道", "支付方式", "商品", "规格", "数量",
		"金额", "币种", "优惠", "邮箱", "Telegram", "续费许可证", "IP", "备注"})
	for _, o := range orders {
		places := model.CurrencyPlaces(o.Currency)
		tg := ""
		if o.TGUserID != 0 {
			tg = strconv.FormatInt(o.TGUserID, 10)
		}
		w.Write([]string{o.OrderNo, csvTime(&o.CreatedAt), csvTime(o.PaidAt), csvTime(o.DeliveredAt), statusLabel(o.Status),
			o.Channel, methodLabel(o.PayMethod), csvSafe(o.ProductTitle), csvSafe(o.SKUName), strconv.Itoa(o.Qty),
			payment.Decimal(o.Amount, places), o.Currency, payment.Decimal(o.Discount, places), csvSafe(o.Email), tg,
			o.RenewOf, o.ClientIP, csvSafe(o.Remark)})
	}
	w.Flush()
}

func csvTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// csvSafe stops spreadsheet formula injection from buyer-typed fields.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func (s *Server) backupsPage(c *gin.Context) {
	files, err := s.shop.Backups()
	data := gin.H{"Files": files, "S": s.st.Settings(), "Postgres": s.st.Postgres, "LastDay": s.st.Value("backup_day")}
	if err != nil {
		data["Error"] = err.Error()
	}
	s.render(c, 200, "admin_backups", data)
}

func (s *Server) runBackup(c *gin.Context) {
	back := s.cfg.AdminPath + "/backups"
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Minute)
	defer cancel()
	name, err := s.shop.Backup(ctx)
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "backup_run", name, "")
	msg := "已备份：" + name
	if c.PostForm("send") == "on" {
		if err := s.shop.SendBackup(ctx, name); err != nil {
			msg += "（发送到 Telegram 失败：" + err.Error() + "）"
		} else {
			msg += "，已发送到 Telegram"
		}
	}
	redirectMsg(c, back, msg)
}

func (s *Server) saveBackupSettings(c *gin.Context) {
	back := s.cfg.AdminPath + "/backups"
	st := s.st.Settings()
	st.BackupEnabled = c.PostForm("backup_enabled") == "on"
	st.BackupTelegram = c.PostForm("backup_telegram") == "on"
	hour, _ := strconv.Atoi(c.PostForm("backup_hour"))
	keep, _ := strconv.Atoi(c.PostForm("backup_keep"))
	st.BackupHour, st.BackupKeep = min(max(hour, 0), 23), min(max(keep, 1), 90)
	if err := s.st.SaveSettings(st); err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "settings_save", "backup", "")
	redirectMsg(c, back, "备份设置已保存")
}

// downloadBackup needs the password again: a backup holds every order.
func (s *Server) downloadBackup(c *gin.Context) {
	back := s.cfg.AdminPath + "/backups"
	if err := s.reauth(c); err != nil {
		redirectErr(c, back, err)
		return
	}
	name := c.PostForm("name")
	p, err := s.shop.BackupPath(name)
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "backup_download", name, "")
	c.Header("Cache-Control", "no-store")
	c.FileAttachment(p, name)
}

func (s *Server) sendBackup(c *gin.Context) {
	back := s.cfg.AdminPath + "/backups"
	name := c.PostForm("name")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
	defer cancel()
	if err := s.shop.SendBackup(ctx, name); err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "backup_send", name, "")
	redirectMsg(c, back, "已发送到 Telegram")
}

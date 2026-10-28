package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/payment"
	"github.com/cuijianzhuang/kaku/internal/polaris"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/tgapi"
)

func (s *Server) adminRoutes(g *gin.RouterGroup) {
	g.GET("/login", s.loginPage)
	g.POST("/login", s.login)
	g.GET("/oauth/:provider/start", s.oauthStart)
	g.GET("/oauth/:provider/callback", s.oauthCallback)
	g.POST("/oauth/totp", s.oauthTOTP)
	a := g.Group("", s.requireAdmin)
	a.POST("/logout", s.logout)
	a.GET("", s.dashboard)
	a.GET("/categories", s.categories)
	a.POST("/categories", s.saveCategory)
	a.POST("/categories/:id/delete", s.deleteCategory)
	a.GET("/products", s.products)
	a.GET("/products/new", s.productForm)
	a.GET("/products/:id", s.productForm)
	a.POST("/products", s.saveProduct)
	a.POST("/products/:id/delete", s.deleteProduct)
	a.POST("/products/:id/announce", s.announceProduct)
	a.GET("/skus/new", s.skuForm)
	a.GET("/skus/:id", s.skuForm)
	a.POST("/skus", s.saveSKU)
	a.POST("/skus/:id/delete", s.deleteSKU)
	a.GET("/cards", s.cards)
	a.POST("/cards/import", s.importCards)
	a.POST("/cards/:id/void", s.voidCard)
	a.POST("/cards/export", s.exportCards)
	a.GET("/orders", s.orders)
	a.GET("/orders/:no", s.adminOrder)
	a.POST("/orders/:no/:action", s.orderAction)
	a.GET("/licenses", s.licenses)
	a.POST("/licenses/:id/:action", s.licenseAction)
	a.GET("/coupons", s.coupons)
	a.POST("/coupons", s.saveCoupon)
	a.POST("/coupons/:id/delete", s.deleteCoupon)
	a.GET("/tickets", s.tickets)
	a.POST("/tickets/:id/reply", s.replyTicket)
	a.POST("/tickets/:id/close", s.closeTicket)
	a.GET("/settings", s.settingsPage)
	a.POST("/settings", s.saveSettings)
	a.POST("/settings/telegram/webhook", s.setWebhook)
	a.POST("/settings/test-mail", s.testMail)
	a.GET("/security", s.securityPage)
	a.POST("/security/password", s.changePassword)
	a.POST("/security/totp/setup", s.totpSetup)
	a.POST("/security/totp/enable", s.totpEnable)
	a.POST("/security/totp/disable", s.totpDisable)
	a.POST("/security/oauth/unlink", s.oauthUnlink)
	a.GET("/audit", s.auditPage)
	s.extraRoutes(a)
	s.updateRoutes(a)
	s.adminUserRoutes(a)
	s.polarisRoutes(a)
	s.panelRoutes(a)
}

func page(c *gin.Context) int {
	p, _ := strconv.Atoi(c.Query("page"))
	return max(p, 1)
}

func atoiU(s string) uint {
	n, _ := strconv.Atoi(s)
	return uint(max(n, 0))
}

// ---- dashboard ----

func (s *Server) dashboard(c *gin.Context) {
	now := s.now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rToday, _ := s.shop.RevenueSince(today)
	r30, _ := s.shop.RevenueSince(today.AddDate(0, 0, -29))
	counts := map[string]int64{}
	for _, st := range []string{model.OrderPending, model.OrderPaid, model.OrderFailed} {
		var n int64
		s.st.DB.Model(&model.Order{}).Where("status = ?", st).Count(&n)
		counts[st] = n
	}
	var low []model.SKU
	s.st.DB.Where("active = ? AND deliverer_kind = ? AND stock_cache <= low_stock", true, "stock").Find(&low)
	var attention []model.Order
	s.st.DB.Where("status IN ?", []string{model.OrderFailed, model.OrderPaid}).Order("id DESC").Limit(20).Find(&attention)
	var recent []model.Order
	s.st.DB.Order("id DESC").Limit(10).Find(&recent)
	var openTickets int64
	s.st.DB.Model(&model.Ticket{}).Where("status = ?", model.TicketOpen).Count(&openTickets)
	s.render(c, 200, "admin_dashboard", gin.H{"Today": rToday, "Month": r30, "Counts": counts, "Low": low,
		"Attention": attention, "Recent": recent, "OpenTickets": openTickets, "NewRelease": s.availableUpdate()})
}

// ---- categories ----

func (s *Server) categories(c *gin.Context) {
	var cats []model.Category
	s.st.DB.Order("sort, id").Find(&cats)
	counts := map[uint]int64{}
	for _, cat := range cats {
		var n int64
		s.st.DB.Model(&model.Product{}).Where("category_id = ?", cat.ID).Count(&n)
		counts[cat.ID] = n
	}
	s.render(c, 200, "admin_categories", gin.H{"Categories": cats, "Counts": counts})
}

func (s *Server) saveCategory(c *gin.Context) {
	back := s.cfg.AdminPath + "/categories"
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" || len([]rune(name)) > 100 {
		redirectErr(c, back, errors.New("请填写分类名称（最多 100 字）"))
		return
	}
	sort, _ := strconv.Atoi(c.PostForm("sort"))
	cat := model.Category{ID: atoiU(c.PostForm("id")), Name: name, Sort: sort, Visible: c.PostForm("visible") == "on"}
	var err error
	if cat.ID == 0 {
		err = s.st.DB.Create(&cat).Error
	} else {
		err = s.st.DB.Model(&model.Category{ID: cat.ID}).Updates(map[string]any{"name": cat.Name, "sort": cat.Sort, "visible": cat.Visible}).Error
	}
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "category_save", fmt.Sprint(cat.ID), name)
	redirectMsg(c, back, "已保存")
}

func (s *Server) deleteCategory(c *gin.Context) {
	back := s.cfg.AdminPath + "/categories"
	var n int64
	s.st.DB.Model(&model.Product{}).Where("category_id = ?", c.Param("id")).Count(&n)
	if n > 0 {
		redirectErr(c, back, errors.New("分类下还有商品，不能删除"))
		return
	}
	s.st.DB.Delete(&model.Category{}, c.Param("id"))
	s.audit(c, "category_delete", c.Param("id"), "")
	redirectMsg(c, back, "已删除")
}

// ---- products ----

func (s *Server) products(c *gin.Context) {
	var prods []model.Product
	q := s.st.DB.Preload("SKUs", func(db *gorm.DB) *gorm.DB { return db.Order("sort, id") }).Order("sort, id")
	if cat := c.Query("category"); cat != "" {
		q = q.Where("category_id = ?", cat)
	}
	q.Find(&prods)
	var cats []model.Category
	s.st.DB.Order("sort, id").Find(&cats)
	names := map[uint]string{}
	for _, cat := range cats {
		names[cat.ID] = cat.Name
	}
	kinds := map[string]string{}
	for _, d := range s.shop.Reg.Kinds() {
		kinds[d.Kind()] = d.Label()
	}
	s.render(c, 200, "admin_products", gin.H{"Products": prods, "CategoryNames": names, "Categories": cats, "Kinds": kinds})
}

func (s *Server) productForm(c *gin.Context) {
	p := model.Product{Status: model.ProductOn}
	if id := c.Param("id"); id != "" {
		if err := s.st.DB.Preload("SKUs", func(db *gorm.DB) *gorm.DB { return db.Order("sort, id") }).Take(&p, id).Error; err != nil {
			redirectErr(c, s.cfg.AdminPath+"/products", errors.New("商品不存在"))
			return
		}
	}
	var cats []model.Category
	s.st.DB.Order("sort, id").Find(&cats)
	kinds := map[string]string{}
	for _, d := range s.shop.Reg.Kinds() {
		kinds[d.Kind()] = d.Label()
	}
	s.render(c, 200, "admin_product", gin.H{"Product": p, "Categories": cats, "Kinds": kinds})
}

func (s *Server) saveProduct(c *gin.Context) {
	id := atoiU(c.PostForm("id"))
	back := s.cfg.AdminPath + "/products/new"
	if id != 0 {
		back = fmt.Sprintf("%s/products/%d", s.cfg.AdminPath, id)
	}
	title := strings.TrimSpace(c.PostForm("title"))
	catID := atoiU(c.PostForm("category_id"))
	if title == "" || catID == 0 {
		redirectErr(c, back, errors.New("请填写商品名称并选择分类"))
		return
	}
	sort, _ := strconv.Atoi(c.PostForm("sort"))
	status := model.ProductOff
	if c.PostForm("status") == model.ProductOn {
		status = model.ProductOn
	}
	cover := strings.TrimSpace(c.PostForm("cover"))
	if up, err := s.saveUpload(c, "cover_file"); err != nil {
		redirectErr(c, back, err)
		return
	} else if up != "" {
		cover = up
	}
	p := model.Product{ID: id, CategoryID: catID, Title: title, DescriptionMD: c.PostForm("description"),
		Cover: cover, Status: status, Sort: sort}
	var err error
	if id == 0 {
		err = s.st.DB.Create(&p).Error
	} else {
		err = s.st.DB.Model(&model.Product{ID: id}).Select("category_id", "title", "description_md", "cover", "status", "sort").Updates(&p).Error
	}
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "product_save", fmt.Sprint(p.ID), title)
	redirectMsg(c, fmt.Sprintf("%s/products/%d", s.cfg.AdminPath, p.ID), "已保存")
}

func (s *Server) deleteProduct(c *gin.Context) {
	back := s.cfg.AdminPath + "/products"
	var n int64
	s.st.DB.Model(&model.Order{}).Where("sku_id IN (?)", s.st.DB.Model(&model.SKU{}).Select("id").Where("product_id = ?", c.Param("id"))).Count(&n)
	if n > 0 {
		redirectErr(c, back, errors.New("商品已有订单，请改为下架而不是删除"))
		return
	}
	s.st.DB.Transaction(func(tx *gorm.DB) error {
		tx.Where("sku_id IN (?)", tx.Model(&model.SKU{}).Select("id").Where("product_id = ?", c.Param("id"))).Delete(&model.Card{})
		tx.Where("product_id = ?", c.Param("id")).Delete(&model.SKU{})
		return tx.Delete(&model.Product{}, c.Param("id")).Error
	})
	s.audit(c, "product_delete", c.Param("id"), "")
	redirectMsg(c, back, "已删除")
}

// announceProduct posts a product to the Telegram channel.
func (s *Server) announceProduct(c *gin.Context) {
	back := s.cfg.AdminPath + "/products/" + c.Param("id")
	st := s.st.Settings()
	if st.TGToken == "" || st.TGChannel == "" {
		redirectErr(c, back, errors.New("请先在设置中填写 Bot Token 和频道"))
		return
	}
	var p model.Product
	if err := s.st.DB.Preload("SKUs", "active = ?", true).Take(&p, c.Param("id")).Error; err != nil {
		redirectErr(c, back, err)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🆕 <b>%s</b>\n", htmlEscape(p.Title))
	for _, k := range p.SKUs {
		var prices []string
		for _, cur := range []string{model.CNY, model.USDT, model.XTR} {
			if v := k.Price(cur); v > 0 {
				prices = append(prices, shop.FormatMoney(v, cur))
			}
		}
		fmt.Fprintf(&b, "• %s：%s\n", htmlEscape(k.Name), strings.Join(prices, " / "))
	}
	var kb *tgapi.Keyboard
	if base := strings.TrimRight(st.PublicURL, "/"); strings.HasPrefix(base, "https://") {
		kb = &tgapi.Keyboard{InlineKeyboard: [][]tgapi.InlineButton{{{Text: "立即购买", URL: fmt.Sprintf("%s/p/%d", base, p.ID)}}}}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	if err := (&tgapi.Client{Token: st.TGToken}).SendMessage(ctx, st.TGChannel, b.String(), true, kb); err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "product_announce", c.Param("id"), st.TGChannel)
	redirectMsg(c, back, "已推送到频道")
}

// ---- SKUs ----

func (s *Server) skuForm(c *gin.Context) {
	sku := model.SKU{ProductID: atoiU(c.Query("product")), Active: true, LimitPerOrder: 10, LowStock: 5, DelivererKind: "stock"}
	if id := c.Param("id"); id != "" {
		if err := s.st.DB.Take(&sku, id).Error; err != nil {
			redirectErr(c, s.cfg.AdminPath+"/products", errors.New("规格不存在"))
			return
		}
	}
	var p model.Product
	if err := s.st.DB.Take(&p, sku.ProductID).Error; err != nil {
		redirectErr(c, s.cfg.AdminPath+"/products", errors.New("商品不存在"))
		return
	}
	s.render(c, 200, "admin_sku", gin.H{"SKU": sku, "Product": p, "Deliverers": s.shop.Reg.Kinds(),
		"PF": s.polarisFormData(c.Request.Context(), &sku), "PK": s.panelFormData(c.Request.Context(), &sku)})
}

func (s *Server) saveSKU(c *gin.Context) {
	id := atoiU(c.PostForm("id"))
	pid := atoiU(c.PostForm("product_id"))
	back := fmt.Sprintf("%s/skus/new?product=%d", s.cfg.AdminPath, pid)
	if id != 0 {
		back = fmt.Sprintf("%s/skus/%d", s.cfg.AdminPath, id)
	}
	fail := func(err error) { redirectErr(c, back, err) }
	name := strings.TrimSpace(c.PostForm("name"))
	if name == "" {
		fail(errors.New("请填写规格名称"))
		return
	}
	parse := func(field string, places int) (int64, error) {
		v := strings.TrimSpace(c.PostForm(field))
		if v == "" {
			return 0, nil
		}
		return payment.ParseDecimal(v, places)
	}
	cny, err1 := parse("price_cny", 2)
	usdt, err2 := parse("price_usdt", 6)
	stars, err3 := parse("price_stars", 0)
	if err := errors.Join(err1, err2, err3); err != nil {
		fail(fmt.Errorf("价格格式不正确：%w", err))
		return
	}
	if cny == 0 && usdt == 0 && stars == 0 {
		fail(errors.New("至少填写一种币种的价格"))
		return
	}
	kind := c.PostForm("deliverer_kind")
	d, err := s.shop.Reg.Get(kind)
	if err != nil {
		fail(err)
		return
	}
	cfg := strings.TrimSpace(c.PostForm("deliverer_cfg"))
	if kind == "panelcode" && c.PostForm("pk_target") != "" {
		if cfg, err = s.panelCfgFromForm(c); err != nil {
			fail(err)
			return
		}
	}
	if kind == "polaris" && c.PostForm("pc_mode") != "" {
		if cfg, err = s.polarisCfgFromForm(c); err != nil {
			fail(err)
			return
		}
	}
	if err := d.Validate(json.RawMessage(cfg)); err != nil {
		fail(fmt.Errorf("发货配置：%w", err))
		return
	}
	if kind == "polaris" {
		if err := s.checkPolarisCfg(c.Request.Context(), cfg); err != nil {
			fail(fmt.Errorf("发货配置：%w", err))
			return
		}
	}
	wholesale := strings.TrimSpace(c.PostForm("wholesale"))
	if wholesale != "" {
		var tiers []model.WholesaleTier
		if err := json.Unmarshal([]byte(wholesale), &tiers); err != nil {
			fail(fmt.Errorf("批发价格式不正确：%w", err))
			return
		}
		for _, t := range tiers {
			if t.MinQty < 2 || t.Percent < 1 || t.Percent > 100 {
				fail(errors.New("批发价：min_qty ≥ 2，percent 在 1–100 之间"))
				return
			}
		}
	}
	limit, _ := strconv.Atoi(c.PostForm("limit_per_order"))
	low, _ := strconv.Atoi(c.PostForm("low_stock"))
	sort, _ := strconv.Atoi(c.PostForm("sort"))
	sku := model.SKU{ID: id, ProductID: pid, Name: name, PriceCNY: cny, PriceUSDT: usdt, PriceStars: stars,
		DelivererKind: kind, DelivererCfg: cfg, Wholesale: wholesale, LimitPerOrder: max(limit, 1), LowStock: max(low, 0),
		Active: c.PostForm("active") == "on", Sort: sort}
	if id == 0 {
		err = s.st.DB.Create(&sku).Error
	} else {
		err = s.st.DB.Model(&model.SKU{ID: id}).Select("name", "price_cny", "price_usdt", "price_stars", "deliverer_kind",
			"deliverer_cfg", "wholesale", "limit_per_order", "low_stock", "active", "sort").Updates(&sku).Error
	}
	if err != nil {
		fail(err)
		return
	}
	deliver.RefreshStock(s.st.DB, sku.ID)
	s.audit(c, "sku_save", fmt.Sprint(sku.ID), name+" · "+kind)
	redirectMsg(c, fmt.Sprintf("%s/products/%d", s.cfg.AdminPath, pid), "规格已保存")
}

func (s *Server) deleteSKU(c *gin.Context) {
	var sku model.SKU
	if err := s.st.DB.Take(&sku, c.Param("id")).Error; err != nil {
		redirectErr(c, s.cfg.AdminPath+"/products", err)
		return
	}
	back := fmt.Sprintf("%s/products/%d", s.cfg.AdminPath, sku.ProductID)
	var n int64
	s.st.DB.Model(&model.Order{}).Where("sku_id = ?", sku.ID).Count(&n)
	if n > 0 {
		redirectErr(c, back, errors.New("规格已有订单，请停用而不是删除"))
		return
	}
	s.st.DB.Where("sku_id = ?", sku.ID).Delete(&model.Card{})
	s.st.DB.Delete(&sku)
	s.audit(c, "sku_delete", c.Param("id"), sku.Name)
	redirectMsg(c, back, "已删除")
}

// ---- cards ----

type cardRow struct {
	model.Card
	Preview string
}

func (s *Server) stockSKUs() ([]model.SKU, map[uint]string) {
	var skus []model.SKU
	s.st.DB.Where("deliverer_kind = ?", "stock").Order("product_id, sort, id").Find(&skus)
	var prods []model.Product
	s.st.DB.Find(&prods)
	titles := map[uint]string{}
	for _, p := range prods {
		titles[p.ID] = p.Title
	}
	return skus, titles
}

func (s *Server) cards(c *gin.Context) {
	skus, titles := s.stockSKUs()
	data := gin.H{"SKUs": skus, "Titles": titles, "Status": c.Query("status")}
	skuID := atoiU(c.Query("sku"))
	if skuID == 0 && len(skus) > 0 {
		skuID = skus[0].ID
	}
	data["SKUID"] = skuID
	if skuID != 0 {
		q := s.st.DB.Model(&model.Card{}).Where("sku_id = ?", skuID)
		if st := c.Query("status"); st != "" {
			q = q.Where("status = ?", st)
		}
		var total int64
		q.Count(&total)
		var cards []model.Card
		p := page(c)
		q.Order("id DESC").Offset((p - 1) * 100).Limit(100).Find(&cards)
		rows := make([]cardRow, len(cards))
		for i, cd := range cards {
			secret := s.st.Box.MustOpen(cd.SecretEnc)
			r := []rune(secret)
			preview := string(r[:min(4, len(r))]) + "••••"
			rows[i] = cardRow{Card: cd, Preview: preview}
		}
		counts := map[string]int64{}
		var grouped []struct {
			Status string
			N      int64
		}
		s.st.DB.Model(&model.Card{}).Select("status, COUNT(*) AS n").Where("sku_id = ?", skuID).Group("status").Scan(&grouped)
		for _, g := range grouped {
			counts[g.Status] = g.N
		}
		data["Cards"], data["Total"], data["Page"], data["Counts"] = rows, total, p, counts
		data["HasNext"] = int64(p*100) < total
	}
	s.render(c, 200, "admin_cards", data)
}

func (s *Server) importCards(c *gin.Context) {
	skuID := atoiU(c.PostForm("sku_id"))
	back := fmt.Sprintf("%s/cards?sku=%d", s.cfg.AdminPath, skuID)
	var sku model.SKU
	if err := s.st.DB.Where("id = ? AND deliverer_kind = ?", skuID, "stock").Take(&sku).Error; err != nil {
		redirectErr(c, back, errors.New("请选择卡密库存型规格"))
		return
	}
	text := c.PostForm("cards")
	if f, err := c.FormFile("file"); err == nil {
		if f.Size > 5<<20 {
			redirectErr(c, back, errors.New("文件不能超过 5MB"))
			return
		}
		fh, err := f.Open()
		if err == nil {
			buf := make([]byte, f.Size)
			n, _ := fh.Read(buf)
			fh.Close()
			text += "\n" + csvToLines(string(buf[:n]))
		}
	}
	r, err := deliver.ImportCards(&deliver.Env{Store: s.st}, skuID, text)
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "cards_import", fmt.Sprint(skuID), fmt.Sprintf("新增 %d，重复 %d", r.Added, r.Duplicate))
	redirectMsg(c, back, fmt.Sprintf("导入完成：新增 %d 条，重复跳过 %d 条", r.Added, r.Duplicate))
}

// csvToLines takes the first column of a CSV (or a plain text file).
func csvToLines(s string) string {
	s = strings.TrimPrefix(s, "\uFEFF")
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.Contains(line, ",") && !strings.Contains(line, " ") {
			line, _, _ = strings.Cut(line, ",")
		}
		out = append(out, strings.Trim(strings.TrimSpace(line), `"`))
	}
	return strings.Join(out, "\n")
}

func (s *Server) voidCard(c *gin.Context) {
	var cd model.Card
	if err := s.st.DB.Take(&cd, c.Param("id")).Error; err != nil {
		redirectErr(c, s.cfg.AdminPath+"/cards", err)
		return
	}
	back := fmt.Sprintf("%s/cards?sku=%d", s.cfg.AdminPath, cd.SKUID)
	res := s.st.DB.Model(&model.Card{}).Where("id = ? AND status = ?", cd.ID, model.CardAvailable).Update("status", model.CardVoid)
	if res.RowsAffected == 0 {
		redirectErr(c, back, errors.New("只能作废可售的卡密"))
		return
	}
	deliver.RefreshStock(s.st.DB, cd.SKUID)
	s.audit(c, "card_void", fmt.Sprint(cd.ID), "")
	redirectMsg(c, back, "已作废")
}

// exportCards downloads a SKU's available cards after re-authentication.
func (s *Server) exportCards(c *gin.Context) {
	skuID := atoiU(c.PostForm("sku_id"))
	back := fmt.Sprintf("%s/cards?sku=%d", s.cfg.AdminPath, skuID)
	if err := s.reauth(c); err != nil {
		redirectErr(c, back, err)
		return
	}
	status := c.PostForm("status")
	if status == "" {
		status = model.CardAvailable
	}
	var cards []model.Card
	s.st.DB.Where("sku_id = ? AND status = ?", skuID, status).Order("id").Find(&cards)
	var b strings.Builder
	for _, cd := range cards {
		b.WriteString(s.st.Box.MustOpen(cd.SecretEnc))
		b.WriteString("\n")
	}
	s.audit(c, "cards_export", fmt.Sprint(skuID), fmt.Sprintf("%s · %d 条", status, len(cards)))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="cards-%d-%s.txt"`, skuID, status))
	c.Header("Cache-Control", "no-store")
	c.Data(200, "text/plain; charset=utf-8", []byte(b.String()))
}

// ---- orders ----

func (s *Server) orders(c *gin.Context) {
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
	var total int64
	q.Count(&total)
	p := page(c)
	var orders []model.Order
	q.Order("id DESC").Offset((p - 1) * 50).Limit(50).Find(&orders)
	s.render(c, 200, "admin_orders", gin.H{"Orders": orders, "Total": total, "Page": p, "HasNext": int64(p*50) < total,
		"Status": c.Query("status"), "Channel": c.Query("channel"), "Q": c.Query("q")})
}

func (s *Server) adminOrder(c *gin.Context) {
	o, err := s.shop.OrderByNo(c.Param("no"))
	if err != nil {
		redirectErr(c, s.cfg.AdminPath+"/orders", err)
		return
	}
	items, _ := s.shop.Items(o.ID)
	var pays []model.Payment
	s.st.DB.Where("order_id = ?", o.ID).Order("id").Find(&pays)
	var bindings []model.LicenseBinding
	s.st.DB.Where("order_id = ?", o.ID).Find(&bindings)
	var job model.Job
	s.st.DB.Where("key = ?", fmt.Sprintf("deliver:%d", o.ID)).Take(&job)
	var coupon model.Coupon
	if o.CouponID != nil {
		s.st.DB.Take(&coupon, *o.CouponID)
	}
	var sku model.SKU
	s.st.DB.Take(&sku, o.SKUID)
	s.render(c, 200, "admin_order", gin.H{"Order": o, "Items": items, "Payments": pays, "Bindings": bindings, "Job": job,
		"Coupon": coupon, "SKU": sku})
}

func (s *Server) orderAction(c *gin.Context) {
	o, err := s.shop.OrderByNo(c.Param("no"))
	back := s.cfg.AdminPath + "/orders/" + c.Param("no")
	if err != nil {
		redirectErr(c, s.cfg.AdminPath+"/orders", err)
		return
	}
	action := c.Param("action")
	var msg string
	switch action {
	case "paid":
		note := strings.TrimSpace(c.PostForm("note"))
		if note == "" {
			err = errors.New("请填写补单依据（支付流水号或备注）")
			break
		}
		if o.Status != model.OrderPending && o.Status != model.OrderExpired {
			err = errors.New("只有待支付或已超时的订单可以补单")
			break
		}
		_, err = s.shop.MarkPaid(c.Request.Context(), shop.PaidInput{OrderNo: o.OrderNo, Channel: model.PayManual,
			TradeNo: o.OrderNo + ":" + secure.RandomToken(6), Raw: "admin " + admin(c).Username + ": " + note, Trusted: true})
		msg = "已补单，正在发货"
	case "redeliver":
		err, msg = s.shop.Redeliver(o.ID), "已重新加入发货队列"
	case "manual":
		err, msg = s.shop.ManualDeliver(o.ID, c.PostForm("content"), c.PostForm("per_line") == "on"), "已发货"
	case "resend":
		err, msg = s.shop.ResendNotice(o.ID), "已重新发送通知"
	case "refund":
		var warnings []string
		warnings, err = s.shop.Refund(c.Request.Context(), o.ID, strings.TrimSpace(c.PostForm("reason")))
		msg = "已退款"
		if len(warnings) > 0 {
			msg += "。注意：" + strings.Join(warnings, "；")
		}
	default:
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		redirectErr(c, back, errors.New(userMessageAdmin(err)))
		return
	}
	s.audit(c, "order_"+action, o.OrderNo, strings.TrimSpace(c.PostForm("note")+" "+c.PostForm("reason")))
	redirectMsg(c, back, msg)
}

func userMessageAdmin(err error) string { return err.Error() }

// ---- licenses ----

func (s *Server) licenses(c *gin.Context) {
	q := s.st.DB.Model(&model.LicenseBinding{})
	if kw := strings.TrimSpace(c.Query("q")); kw != "" {
		q = q.Where("license_id = ? OR email LIKE ? OR instance = ?", kw, "%"+strings.ToLower(kw)+"%", kw)
	}
	if st := c.Query("status"); st != "" {
		q = q.Where("status = ?", st)
	}
	var total int64
	q.Count(&total)
	p := page(c)
	var bs []model.LicenseBinding
	q.Order("id DESC").Offset((p - 1) * 50).Limit(50).Find(&bs)
	orderNos := map[uint]string{}
	for _, b := range bs {
		var o model.Order
		if s.st.DB.Select("id, order_no").Take(&o, b.OrderID).Error == nil {
			orderNos[b.ID] = o.OrderNo
		}
	}
	s.render(c, 200, "admin_licenses", gin.H{"Bindings": bs, "OrderNos": orderNos, "Total": total, "Page": p,
		"HasNext": int64(p*50) < total, "Q": c.Query("q"), "Status": c.Query("status"), "PolarisReady": s.shop.Polaris().Ready()})
}

func (s *Server) licenseAction(c *gin.Context) {
	back := s.cfg.AdminPath + "/licenses?q=" + c.Param("id")
	b, err := s.shop.Binding(c.Param("id"))
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	pc := s.shop.Polaris()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	ref := "admin-" + strings.ToLower(secure.RandomToken(8))
	ref = strings.NewReplacer("_", "x").Replace(ref)
	switch c.Param("action") {
	case "extend":
		days, _ := strconv.Atoi(c.PostForm("days"))
		if days < 1 || days > 3650 {
			redirectErr(c, back, errors.New("天数需在 1–3650 之间"))
			return
		}
		term := polaris.Term{Days: days}
		if c.PostForm("updates") == "on" {
			term.UpdatesDays = days // the update period grows with it
		}
		l, err := pc.Extend(ctx, b.LicenseID, ref, term)
		if err != nil {
			redirectErr(c, back, err)
			return
		}
		s.st.DB.Model(b).Update("expires_at", l.ExpiresAt)
		s.audit(c, "license_extend", b.LicenseID, fmt.Sprintf("+%d 天 → %s", days, l.ExpiresAt))
		redirectMsg(c, back, "已续期至 "+l.ExpiresAt)
	case "revoke":
		if _, err := pc.Revoke(ctx, b.LicenseID, ref, "后台吊销"); err != nil {
			redirectErr(c, back, err)
			return
		}
		s.st.DB.Model(b).Update("status", model.LicenseRevoked)
		s.audit(c, "license_revoke", b.LicenseID, "")
		redirectMsg(c, back, "已吊销")
	case "refresh":
		l, err := pc.Get(ctx, b.LicenseID)
		if err != nil {
			redirectErr(c, back, err)
			return
		}
		status := model.LicenseIssued
		switch {
		case l.Revoked:
			status = model.LicenseRevoked
		case l.Instance != "":
			status = model.LicenseActivated
		}
		s.st.DB.Model(b).Updates(map[string]any{"status": status, "instance": l.Instance, "expires_at": l.ExpiresAt})
		redirectMsg(c, back, "已同步")
	default:
		c.Status(404)
	}
}

// ---- coupons ----

func (s *Server) coupons(c *gin.Context) {
	var cs []model.Coupon
	s.st.DB.Order("id DESC").Find(&cs)
	s.render(c, 200, "admin_coupons", gin.H{"Coupons": cs})
}

func (s *Server) saveCoupon(c *gin.Context) {
	back := s.cfg.AdminPath + "/coupons"
	code := strings.TrimSpace(c.PostForm("code"))
	if code == "" {
		code = strings.ToUpper(secure.RandomToken(6))
	}
	typ := c.PostForm("type")
	var value int64
	var err error
	switch typ {
	case model.CouponPercent:
		var v int
		v, err = strconv.Atoi(c.PostForm("value"))
		value = int64(v)
		if err == nil && (v < 1 || v > 100) {
			err = errors.New("折扣百分比需在 1–100 之间")
		}
	case model.CouponFixed:
		value, err = payment.ParseDecimal(c.PostForm("value"), 2)
	default:
		err = errors.New("请选择类型")
	}
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	maxUses, _ := strconv.Atoi(c.PostForm("max_uses"))
	cp := model.Coupon{Code: code, Type: typ, Value: value, SKUScope: strings.ReplaceAll(c.PostForm("sku_scope"), " ", ""),
		MaxUses: max(maxUses, 0), Active: true}
	if v := c.PostForm("expires_at"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, time.Local)
		if err != nil {
			redirectErr(c, back, errors.New("到期日格式应为 YYYY-MM-DD"))
			return
		}
		t = t.Add(24*time.Hour - time.Second)
		cp.ExpiresAt = &t
	}
	if err := s.st.DB.Create(&cp).Error; err != nil {
		redirectErr(c, back, errors.New("保存失败（优惠码是否重复？）"))
		return
	}
	s.audit(c, "coupon_create", code, fmt.Sprintf("%s %d", typ, value))
	redirectMsg(c, back, "已创建优惠码 "+code)
}

func (s *Server) deleteCoupon(c *gin.Context) {
	s.st.DB.Model(&model.Coupon{}).Where("id = ?", c.Param("id")).Update("active", false)
	s.audit(c, "coupon_disable", c.Param("id"), "")
	redirectMsg(c, s.cfg.AdminPath+"/coupons", "已停用")
}

// ---- tickets ----

func (s *Server) tickets(c *gin.Context) {
	q := s.st.DB.Model(&model.Ticket{})
	if st := c.DefaultQuery("status", model.TicketOpen); st != "all" {
		q = q.Where("status = ?", st)
	}
	var ts []model.Ticket
	q.Order("id DESC").Limit(100).Find(&ts)
	s.render(c, 200, "admin_tickets", gin.H{"Tickets": ts, "Status": c.DefaultQuery("status", model.TicketOpen)})
}

func (s *Server) replyTicket(c *gin.Context) {
	back := s.cfg.AdminPath + "/tickets"
	var t model.Ticket
	if err := s.st.DB.Take(&t, c.Param("id")).Error; err != nil {
		redirectErr(c, back, err)
		return
	}
	reply := strings.TrimSpace(c.PostForm("reply"))
	if reply == "" {
		redirectErr(c, back, errors.New("请填写回复内容"))
		return
	}
	s.st.DB.Model(&t).Updates(map[string]any{"reply": reply, "status": model.TicketReplied})
	msg := "已回复"
	if s.shop.Notify.MailReady() {
		body := fmt.Sprintf("您好，\n\n关于您的工单 #%d（订单 %s）：\n\n%s\n\n—— 您的问题 ——\n%s\n", t.ID, t.OrderNo, reply, t.Content)
		if err := s.shop.Notify.Mail(t.Email, fmt.Sprintf("【%s】工单 #%d 回复", s.st.Settings().SiteName, t.ID), body); err != nil {
			msg = "已保存回复，但邮件发送失败：" + err.Error()
		}
	} else {
		msg = "已保存回复（未配置 SMTP，未发送邮件）"
	}
	s.audit(c, "ticket_reply", fmt.Sprint(t.ID), "")
	redirectMsg(c, back, msg)
}

func (s *Server) closeTicket(c *gin.Context) {
	s.st.DB.Model(&model.Ticket{}).Where("id = ?", c.Param("id")).Update("status", model.TicketClosed)
	redirectMsg(c, s.cfg.AdminPath+"/tickets", "已关闭")
}

// ---- settings ----

func (s *Server) settingsPage(c *gin.Context) {
	st := s.st.Settings()
	secrets := map[string]bool{"EPayKey": st.EPayKey != "", "TronGridKey": st.TronGridKey != "", "SMTPPass": st.SMTPPass != "",
		"TGToken": st.TGToken != "", "TGWebhookSecret": st.TGWebhookSecret != "", "PolarisSecret": st.PolarisSecret != "",
		"TurnstileSecret":    st.TurnstileSecret != "",
		"GitHubClientSecret": st.GitHubClientSecret != "", "PanelToken": st.PanelToken != "", "GoogleClientSecret": st.GoogleClientSecret != ""}
	base := s.publicURL(c)
	s.render(c, 200, "admin_settings", gin.H{"S": st, "Secrets": secrets, "Base": base,
		"HookURL": base + "/hooks/polaris", "RenewURL": base + "/renew?lid={license_id}",
		"RenewBot": renewBotTemplate(st.TGBotUsername), "OAuthBase": base + s.cfg.AdminPath + "/oauth/"})
}

func renewBotTemplate(user string) string {
	if user == "" {
		return ""
	}
	return "https://t.me/" + user + "?start=renew_{license_id}"
}

func (s *Server) saveSettings(c *gin.Context) {
	back := s.cfg.AdminPath + "/settings"
	st := s.st.Settings()
	str := func(k string) string { return strings.TrimSpace(c.PostForm(k)) }
	secret := func(k string, cur *string) {
		if v := str(k); v != "" {
			*cur = v
		}
		if c.PostForm(k+"_clear") == "on" {
			*cur = ""
		}
	}
	on := func(k string) bool { return c.PostForm(k) == "on" }
	switch c.PostForm("section") {
	case "site":
		st.SiteName, st.PublicURL, st.Notice, st.SupportURL = str("site_name"), strings.TrimRight(str("public_url"), "/"), c.PostForm("notice"), str("support_url")
		st.OrderTTL, _ = strconv.Atoi(str("order_ttl"))
		st.MaxUnpaid, _ = strconv.Atoi(str("max_unpaid"))
		st.OrderTTL, st.MaxUnpaid = min(max(st.OrderTTL, 5), 120), min(max(st.MaxUnpaid, 1), 20)
		st.RemindEnabled = on("remind_enabled")
		st.TurnstileSite = str("turnstile_site")
		secret("turnstile_secret", &st.TurnstileSecret)
		if st.SiteName == "" {
			st.SiteName = "Kaku 发卡"
		}
	case "payment":
		st.EPayURL, st.EPayPID = str("epay_url"), str("epay_pid")
		secret("epay_key", &st.EPayKey)
		st.EPayAlipay, st.EPayWxpay = on("epay_alipay"), on("epay_wxpay")
		st.USDTAddress, st.USDTEnabled, st.StarsEnabled = str("usdt_address"), on("usdt_enabled"), on("stars_enabled")
		secret("trongrid_key", &st.TronGridKey)
		if st.USDTAddress != "" && !payment.ValidTRONAddress(st.USDTAddress) {
			redirectErr(c, back, errors.New("USDT 收款地址不是有效的 TRON 主网地址"))
			return
		}
		if st.EPayURL != "" && !(&payment.EPay{BaseURL: st.EPayURL, PID: "x", Key: "x"}).Ready() {
			redirectErr(c, back, errors.New("易支付地址需为 HTTPS，且不能带查询参数"))
			return
		}
	case "mail":
		st.SMTPHost, st.SMTPUser, st.SMTPFrom = str("smtp_host"), str("smtp_user"), str("smtp_from")
		st.SMTPPort, _ = strconv.Atoi(str("smtp_port"))
		secret("smtp_pass", &st.SMTPPass)
	case "telegram":
		secret("tg_token", &st.TGToken)
		st.TGChannel = str("tg_channel")
		st.TGAdminChat, _ = strconv.ParseInt(str("tg_admin_chat"), 10, 64)
		if st.TGWebhookSecret == "" {
			st.TGWebhookSecret = strings.NewReplacer("-", "a", "_", "b").Replace(secure.RandomToken(24))
		}
	case "polaris":
		st.PolarisURL = strings.TrimRight(str("polaris_url"), "/")
		secret("polaris_secret", &st.PolarisSecret)
		if st.PolarisSecret != "" && len(st.PolarisSecret) < 32 {
			redirectErr(c, back, errors.New("授权服务密钥至少 32 位（与 polaris-license 的 KAKU_API_SECRET 一致）"))
			return
		}
	case "appearance":
		if t, a, m := str("theme"), str("accent"), str("mode"); validTheme(t) && validAccent(a) && validMode(m) {
			st.Theme, st.Accent, st.Mode = t, a, m
		}
		st.LogoURL = str("logo_url")
		if up, err := s.saveUpload(c, "logo_file"); err != nil {
			redirectErr(c, back, err)
			return
		} else if up != "" {
			st.LogoURL = up
		}
		if st.LogoURL != "" && !strings.HasPrefix(st.LogoURL, "https://") && !strings.HasPrefix(st.LogoURL, "/") {
			redirectErr(c, back, errors.New("Logo 地址需以 https:// 或 / 开头"))
			return
		}
	case "panel":
		st.PanelURL = strings.TrimRight(str("panel_url"), "/")
		secret("panel_token", &st.PanelToken)
		st.PanelRedeemHint = strings.TrimSpace(c.PostForm("panel_redeem_hint"))
		if st.PanelToken != "" && !strings.HasPrefix(st.PanelToken, "plt_") {
			redirectErr(c, back, errors.New("API 令牌应以 plt_ 开头（在面板「扩展 → API 令牌」创建）"))
			return
		}
	case "login":
		st.GitHubClientID, st.GoogleClientID = str("github_client_id"), str("google_client_id")
		secret("github_client_secret", &st.GitHubClientSecret)
		secret("google_client_secret", &st.GoogleClientSecret)
		st.AdminEntry = on("admin_link_in_footer")
	default:
		redirectErr(c, back, errors.New("未知设置分组"))
		return
	}
	if err := s.st.SaveSettings(st); err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "settings_save", c.PostForm("section"), "")
	redirectMsg(c, back+"#"+c.PostForm("section"), "设置已保存")
}

// setWebhook registers the bot webhook with Telegram and reads the bot's
// username.
func (s *Server) setWebhook(c *gin.Context) {
	back := s.cfg.AdminPath + "/settings#telegram"
	st := s.st.Settings()
	base := strings.TrimRight(st.PublicURL, "/")
	if st.TGToken == "" || !strings.HasPrefix(base, "https://") {
		redirectErr(c, back, errors.New("需要 Bot Token，且站点地址必须是 HTTPS"))
		return
	}
	bot := &tgapi.Client{Token: st.TGToken}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	var me tgapi.User
	if err := bot.Call(ctx, "getMe", map[string]any{}, &me); err != nil {
		redirectErr(c, back, err)
		return
	}
	if err := bot.Call(ctx, "setWebhook", map[string]any{
		"url": base + "/tg/webhook/" + st.TGWebhookSecret, "secret_token": st.TGWebhookSecret,
		"allowed_updates": []string{"message", "callback_query", "pre_checkout_query"}, "drop_pending_updates": true,
	}, nil); err != nil {
		redirectErr(c, back, err)
		return
	}
	bot.Call(ctx, "setMyCommands", map[string]any{"commands": []map[string]string{
		{"command": "shop", "description": "选购商品"}, {"command": "orders", "description": "我的订单"},
		{"command": "help", "description": "帮助"},
	}}, nil)
	st.TGBotUsername = me.Username
	s.st.SaveSettings(st)
	s.audit(c, "telegram_webhook", me.Username, "")
	redirectMsg(c, back, "Webhook 已设置：@"+me.Username)
}

func (s *Server) testMail(c *gin.Context) {
	back := s.cfg.AdminPath + "/settings#mail"
	to := strings.TrimSpace(c.PostForm("to"))
	if err := s.shop.Notify.Mail(to, "Kaku 测试邮件", "如果您收到这封邮件，说明 SMTP 配置正确。"); err != nil {
		redirectErr(c, back, err)
		return
	}
	redirectMsg(c, back, "测试邮件已发送")
}

func (s *Server) auditPage(c *gin.Context) {
	var logs []model.AuditLog
	p := page(c)
	q := s.st.DB.Model(&model.AuditLog{})
	if a := c.Query("action"); a != "" {
		q = q.Where("action = ?", a)
	}
	var total int64
	q.Count(&total)
	q.Order("id DESC").Offset((p - 1) * 100).Limit(100).Find(&logs)
	s.render(c, 200, "admin_audit", gin.H{"Logs": logs, "Page": p, "HasNext": int64(p*100) < total})
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

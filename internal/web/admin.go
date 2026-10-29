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
	a.POST("/products/:id/toggle", s.toggleProduct)
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
	a.POST("/coupons/:id/enable", s.enableCoupon)
	a.GET("/tickets", s.tickets)
	a.POST("/tickets/:id/reply", s.replyTicket)
	a.POST("/tickets/:id/close", s.closeTicket)
	a.GET("/settings", s.settingsPage)
	a.GET("/settings/:tab", s.settingsPage)
	a.POST("/settings", s.saveSettings)
	a.POST("/settings/telegram/webhook", s.setWebhook)
	a.POST("/settings/test-mail", s.testMail)
	a.GET("/security", s.securityPage)
	a.POST("/security/password", s.changePassword)
	a.POST("/security/totp/setup", s.totpSetup)
	a.POST("/security/totp/enable", s.totpEnable)
	a.POST("/security/totp/disable", s.totpDisable)
	a.POST("/security/oauth/unlink", s.oauthUnlink)
	a.POST("/security/oauth/link", s.oauthLinkStart)
	a.GET("/audit", s.auditPage)
	s.extraRoutes(a)
	s.updateRoutes(a)
	s.adminUserRoutes(a)
	s.polarisRoutes(a)
	s.panelRoutes(a)
	s.quickRoutes(a)
	s.templateRoutes(a)
	a.GET("/settings/theme.css", s.themePreviewCSS)
	a.POST("/uploads/image", s.uploadImage)
	a.POST("/markdown/preview", s.previewMarkdown)
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

// setupCheck is one item of the dashboard's 开店检查.
type setupCheck struct {
	Done       bool
	Text, Hint string
	Link       string
}

// setupChecks lists what a shop needs before it can really sell; nil once
// everything is done.
func (s *Server) setupChecks() []setupCheck {
	st := s.st.Settings()
	ap := s.cfg.AdminPath
	var onSale int64
	s.st.DB.Model(&model.Product{}).Where("status = ?", model.ProductOn).Count(&onSale)
	checks := []setupCheck{
		{strings.HasPrefix(st.PublicURL, "https://"), "填写站点地址（HTTPS）", "支付回调、邮件和续费链接都要用它；不填买家付款后无法自动确认", ap + "/settings/site"},
		{len(s.shop.Methods(model.ChannelWeb)) > 0, "开通至少一种支付方式", "易支付（支付宝 / 微信）或 USDT", ap + "/settings/payment"},
		{s.shop.Notify.MailReady(), "配置发件邮箱（SMTP）", "买家靠邮件收到发货内容和工单回复", ap + "/settings/mail"},
		{onSale > 0, "上架第一个商品", "从模板创建最快", ap + "/products/quick"},
		{st.BackupEnabled, "开启自动备份", "每天备份数据库，出问题时能恢复", ap + "/backups"},
	}
	for _, c := range checks {
		if !c.Done {
			return checks
		}
	}
	return nil
}

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
	titles := map[uint]string{}
	for _, k := range low {
		if _, ok := titles[k.ProductID]; !ok {
			var p model.Product
			s.st.DB.Select("id, title").Take(&p, k.ProductID)
			titles[k.ProductID] = p.Title
		}
	}
	s.render(c, 200, "admin_dashboard", gin.H{"Today": rToday, "Month": r30, "Counts": counts, "Low": low, "LowTitles": titles,
		"Attention": attention, "Recent": recent, "OpenTickets": openTickets, "NewRelease": s.availableUpdate(), "Setup": s.setupChecks()})
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
	have := map[string]bool{}
	for _, cat := range cats {
		have[cat.Name] = true
	}
	s.render(c, 200, "admin_categories", gin.H{"Presets": categoryPresets, "Have": have, "Categories": cats, "Counts": counts})
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
	if kw := strings.TrimSpace(c.Query("q")); kw != "" {
		q = q.Where("title LIKE ?", "%"+kw+"%")
	}
	switch c.Query("status") {
	case model.ProductOn, model.ProductOff:
		q = q.Where("status = ?", c.Query("status"))
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
	// per product: price range, stock, sales in the last 30 days
	since := s.now().AddDate(0, 0, -30)
	rows := make([]productRow, 0, len(prods))
	for _, p := range prods {
		r := productRow{Product: p, Stock: -1}
		var ids []uint
		for _, k := range p.SKUs {
			ids = append(ids, k.ID)
			if k.Active && k.PriceCNY > 0 && (r.MinCNY == 0 || k.PriceCNY < r.MinCNY) {
				r.MinCNY = k.PriceCNY
			}
			if k.PriceCNY > r.MaxCNY {
				r.MaxCNY = k.PriceCNY
			}
			if k.DelivererKind == "stock" && k.Active {
				if r.Stock < 0 {
					r.Stock = 0
				}
				r.Stock += k.StockCache
				if k.StockCache == 0 {
					r.SoldOut++
				}
			}
		}
		if len(ids) > 0 {
			s.st.DB.Model(&model.Order{}).Where("sku_id IN ? AND status = ? AND created_at >= ?", ids, model.OrderDelivered, since).
				Select("COALESCE(SUM(qty), 0)").Scan(&r.Sold30)
		}
		if c.Query("status") == "soldout" && r.SoldOut == 0 {
			continue
		}
		rows = append(rows, r)
	}
	s.render(c, 200, "admin_products", gin.H{"Products": prods, "Rows": rows, "CategoryNames": names, "Categories": cats, "Kinds": kinds,
		"Q": c.Query("q"), "Cat": c.Query("category"), "Status": c.Query("status")})
}

// productRow is a product with the figures the product list shows.
type productRow struct {
	model.Product
	MinCNY, MaxCNY int64
	Stock          int // card stock of active stock specs, -1 = none
	SoldOut        int // stock specs with nothing left
	Sold30         int64
}

// toggleProduct puts a product on or off sale from the list.
func (s *Server) toggleProduct(c *gin.Context) {
	back := s.cfg.AdminPath + "/products"
	if r := c.PostForm("back"); strings.HasPrefix(r, s.cfg.AdminPath+"/products") {
		back = r
	}
	var p model.Product
	if err := s.st.DB.Take(&p, c.Param("id")).Error; err != nil {
		redirectErr(c, back, errors.New("商品不存在"))
		return
	}
	next := model.ProductOn
	if p.Status == model.ProductOn {
		next = model.ProductOff
	}
	if next == model.ProductOn {
		if blocking, _ := s.productTodo(&p); len(blocking) > 0 {
			redirectErr(c, back, fmt.Errorf("「%s」还不能上架：%s", p.Title, strings.Join(blocking, "；")))
			return
		}
	}
	s.st.DB.Model(&model.Product{ID: p.ID}).Update("status", next)
	s.audit(c, "product_save", fmt.Sprint(p.ID), p.Title+" · "+map[string]string{model.ProductOn: "上架", model.ProductOff: "下架"}[next])
	redirectMsg(c, back, fmt.Sprintf("「%s」已%s", p.Title, map[string]string{model.ProductOn: "上架", model.ProductOff: "下架"}[next]))
}

func (s *Server) productForm(c *gin.Context) {
	p := model.Product{Status: model.ProductOff} // a new product has nothing to sell yet
	if id := c.Param("id"); id != "" {
		if err := s.st.DB.Take(&p, id).Error; err != nil {
			redirectErr(c, s.cfg.AdminPath+"/products", errors.New("商品不存在"))
			return
		}
	}
	s.renderProductForm(c, &p, nil)
}

// renderProductForm draws the product page; after a failed save it keeps
// what the owner typed (a long description is painful to lose).
func (s *Server) renderProductForm(c *gin.Context, p *model.Product, saveErr error) {
	if p.ID != 0 {
		s.st.DB.Where("product_id = ?", p.ID).Order("sort, id").Find(&p.SKUs)
	}
	var cats []model.Category
	s.st.DB.Order("sort, id").Find(&cats)
	kinds := map[string]string{}
	for _, d := range s.shop.Reg.Kinds() {
		kinds[d.Kind()] = d.Label()
	}
	blocking, warnings := s.productTodo(p)
	data := gin.H{"Product": *p, "Categories": cats, "Kinds": kinds, "Todo": blocking, "Warn": warnings}
	code := http.StatusOK
	if saveErr != nil {
		data["Error"] = saveErr.Error()
		code = http.StatusUnprocessableEntity
	}
	s.render(c, code, "admin_product", data)
}

func (s *Server) saveProduct(c *gin.Context) {
	id := atoiU(c.PostForm("id"))
	title := strings.TrimSpace(c.PostForm("title"))
	catID := atoiU(c.PostForm("category_id"))
	sort, _ := strconv.Atoi(c.PostForm("sort"))
	status := model.ProductOff
	if c.PostForm("status") == model.ProductOn {
		status = model.ProductOn
	}
	p := model.Product{ID: id, CategoryID: catID, Title: title, DescriptionMD: c.PostForm("description"),
		Cover: strings.TrimSpace(c.PostForm("cover")), Status: status, Sort: sort}
	if title == "" || catID == 0 {
		s.renderProductForm(c, &p, errors.New("请填写商品名称并选择分类"))
		return
	}
	if up, err := s.saveUpload(c, "cover_file"); err != nil {
		s.renderProductForm(c, &p, err)
		return
	} else if up != "" {
		p.Cover = up
	}
	if p.Status == model.ProductOn {
		if blocking, _ := s.productTodo(&p); len(blocking) > 0 {
			s.renderProductForm(c, &p, errors.New("还不能上架："+strings.Join(blocking, "；")))
			return
		}
	}
	var err error
	if id == 0 {
		err = s.st.DB.Create(&p).Error
	} else {
		err = s.st.DB.Model(&model.Product{ID: id}).Select("category_id", "title", "description_md", "cover", "status", "sort").Updates(&p).Error
	}
	if err != nil {
		s.renderProductForm(c, &p, err)
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
	s.renderSKUForm(c, &sku, nil)
}

// renderSKUForm draws the SKU page; after a failed save it shows the
// error with what the owner typed, rather than a fresh form.
func (s *Server) renderSKUForm(c *gin.Context, sku *model.SKU, saveErr error) {
	var p model.Product
	if err := s.st.DB.Take(&p, sku.ProductID).Error; err != nil {
		redirectErr(c, s.cfg.AdminPath+"/products", errors.New("商品不存在"))
		return
	}
	data := gin.H{"SKU": *sku, "Product": p, "Deliverers": s.shop.Reg.Kinds(),
		"PF": s.polarisFormData(c.Request.Context(), sku), "PK": s.panelFormData(c.Request.Context(), sku), "GF": s.genericFormData(sku)}
	code := http.StatusOK
	if saveErr != nil {
		data["Error"] = saveErr.Error()
		code = http.StatusUnprocessableEntity
	}
	s.render(c, code, "admin_sku", data)
}

func (s *Server) saveSKU(c *gin.Context) {
	sku, err := s.skuFromForm(c)
	if err != nil {
		s.renderSKUForm(c, sku, err)
		return
	}
	if sku.ID == 0 {
		err = s.st.DB.Create(sku).Error
	} else {
		err = s.st.DB.Model(&model.SKU{ID: sku.ID}).Select("name", "price_cny", "price_usdt", "price_stars", "deliverer_kind",
			"deliverer_cfg", "wholesale", "limit_per_order", "low_stock", "active", "sort").Updates(sku).Error
	}
	if err != nil {
		s.renderSKUForm(c, sku, err)
		return
	}
	deliver.RefreshStock(s.st.DB, sku.ID)
	s.audit(c, "sku_save", fmt.Sprint(sku.ID), sku.Name+" · "+sku.DelivererKind)
	redirectMsg(c, fmt.Sprintf("%s/products/%d", s.cfg.AdminPath, sku.ProductID), "规格已保存")
}

// skuFromForm reads and checks the SKU form. On error the returned SKU
// still holds as much of the input as could be read, to show it again.
func (s *Server) skuFromForm(c *gin.Context) (*model.SKU, error) {
	limit, _ := strconv.Atoi(c.PostForm("limit_per_order"))
	low, _ := strconv.Atoi(c.PostForm("low_stock"))
	sort, _ := strconv.Atoi(c.PostForm("sort"))
	sku := &model.SKU{ID: atoiU(c.PostForm("id")), ProductID: atoiU(c.PostForm("product_id")), Name: strings.TrimSpace(c.PostForm("name")),
		DelivererKind: c.PostForm("deliverer_kind"), DelivererCfg: strings.TrimSpace(c.PostForm("deliverer_cfg")),
		Wholesale: strings.TrimSpace(c.PostForm("wholesale")), LimitPerOrder: max(limit, 1), LowStock: max(low, 0),
		Active: c.PostForm("active") == "on", Sort: sort}
	parse := func(field string, places int) (int64, error) {
		v := strings.TrimSpace(c.PostForm(field))
		if v == "" {
			return 0, nil
		}
		return payment.ParseDecimal(v, places)
	}
	var err1, err2, err3 error
	sku.PriceCNY, err1 = parse("price_cny", 2)
	sku.PriceUSDT, err2 = parse("price_usdt", 6)
	sku.PriceStars, err3 = parse("price_stars", 0)
	priceErr := errors.Join(err1, err2, err3)

	// the delivery configuration first, so a later error still shows it
	var cfgErr error
	if v, ok := genericCfgFromForm(c, sku.DelivererKind); ok {
		sku.DelivererCfg = v
	}
	switch {
	case sku.DelivererKind == "panelcode" && c.PostForm("pk_target") != "":
		var v string
		if v, cfgErr = s.panelCfgFromForm(c); cfgErr == nil {
			sku.DelivererCfg = v
		}
	case sku.DelivererKind == "polaris" && c.PostForm("pc_mode") != "":
		var v string
		v, cfgErr = s.polarisCfgFromForm(c)
		if v != "" {
			sku.DelivererCfg = v
		}
	}
	wholesale, wsForm, wsErr := wholesaleFromForm(c)
	if wsForm && wsErr == nil {
		sku.Wholesale = wholesale
	}

	switch {
	case sku.Name == "":
		return sku, errors.New("请填写规格名称")
	case priceErr != nil:
		return sku, fmt.Errorf("价格格式不正确：%w", priceErr)
	case sku.PriceCNY == 0 && sku.PriceUSDT == 0 && sku.PriceStars == 0:
		return sku, errors.New("至少填写一种币种的价格")
	case cfgErr != nil:
		return sku, cfgErr
	case wsErr != nil:
		return sku, wsErr
	}
	d, err := s.shop.Reg.Get(sku.DelivererKind)
	if err != nil {
		return sku, err
	}
	if err := d.Validate(json.RawMessage(sku.DelivererCfg)); err != nil {
		return sku, fmt.Errorf("发货配置：%w", err)
	}
	if sku.DelivererKind == "polaris" {
		if err := s.checkPolarisCfg(c.Request.Context(), sku.DelivererCfg); err != nil {
			return sku, fmt.Errorf("发货配置：%w", err)
		}
	}
	if sku.Wholesale != "" {
		var tiers []model.WholesaleTier
		if err := json.Unmarshal([]byte(sku.Wholesale), &tiers); err != nil {
			return sku, fmt.Errorf("批发价格式不正确：%w", err)
		}
		for _, t := range tiers {
			if t.MinQty < 2 || t.Percent < 1 || t.Percent > 100 {
				return sku, errors.New("批发价：min_qty ≥ 2，percent 在 1–100 之间")
			}
		}
	}
	return sku, nil
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
	OrderNo string
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

// cardPreview shows the start and end of a card, enough to tell cards
// apart without showing the whole secret.
func cardPreview(secret string) string {
	r := []rune(secret)
	if len(r) <= 8 {
		return string(r[:min(2, len(r))]) + "••••"
	}
	return string(r[:4]) + "…" + string(r[len(r)-4:])
}

func (s *Server) cards(c *gin.Context) {
	a := s.currentAdmin(c)
	skus, titles := s.stockSKUs()
	data := gin.H{"SKUs": skus, "Titles": titles, "Status": c.Query("status"), "TOTP": a != nil && a.TOTPEnabled}
	skuID := atoiU(c.Query("sku"))
	if skuID != 0 {
		found := false
		for _, k := range skus {
			found = found || k.ID == skuID
		}
		if !found {
			data["Error"] = "规格不存在或不是卡密库存，已显示第一个规格"
			skuID = 0
		}
	}
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
			rows[i] = cardRow{Card: cd, Preview: cardPreview(secret)}
			if cd.OrderID != nil {
				var o model.Order
				if s.st.DB.Select("order_no").Take(&o, *cd.OrderID).Error == nil {
					rows[i].OrderNo = o.OrderNo
				}
			}
		}
		var sku model.SKU
		s.st.DB.Take(&sku, skuID)
		data["SKU"] = sku
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
	kind := sku.DelivererKind
	if d, err := s.shop.Reg.Get(kind); err == nil {
		kind = d.Label()
	}
	// a manual SKU's question and the buyer's answer lead the remark
	askLabel, askValue, remark := "", "", o.Remark
	if sku.DelivererKind == "manual" {
		if cfg, err := deliver.ParseManualCfg(sku.DelivererCfg); err == nil && cfg.Ask != "" {
			if v, ok := strings.CutPrefix(o.Remark, cfg.Ask+"："); ok {
				askLabel = cfg.Ask
				askValue, remark, _ = strings.Cut(v, "；备注：")
			}
		}
	}
	s.render(c, 200, "admin_order", gin.H{"Order": o, "Items": items, "Payments": pays, "Bindings": bindings, "Job": job,
		"Coupon": coupon, "SKU": sku, "KindLabel": kind, "AskLabel": askLabel, "AskValue": askValue, "Remark": remark,
		"Manual": sku.DelivererKind == "manual", "RefundNote": refundNote(o, &sku)})
}

// refundNote says what a refund will actually do for this order.
func refundNote(o *model.Order, sku *model.SKU) string {
	var parts []string
	if o.PayMethod == model.PayStars {
		parts = append(parts, "Stars 会自动原路退回买家")
	} else {
		parts = append(parts, "Kaku 只把订单标记为已退款，钱需要你在支付平台（易支付后台 / 原转账方式）手动退给买家")
	}
	switch sku.DelivererKind {
	case "polaris":
		parts = append(parts, "已签发的 Polaris 授权会被吊销")
	case "panelcode":
		parts = append(parts, "未兑换的面板兑换码会作废，已兑换的不受影响")
	case "webhook":
		parts = append(parts, "如果配置了退款通知地址，会通知你的系统回收")
	default:
		if o.Status == model.OrderDelivered {
			parts = append(parts, "已发出的内容无法收回")
		}
	}
	return strings.Join(parts, "；") + "。"
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
	case "epay-check":
		var paid bool
		paid, err = s.shop.CheckEPay(c.Request.Context(), o, 0)
		switch {
		case err != nil:
			err = fmt.Errorf("向支付平台查询失败：%w", err)
		case paid:
			msg = "支付平台确认已付款，正在发货"
		default:
			msg = "支付平台显示这笔订单还没有付款"
		}
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

// couponSKU is a spec a coupon can be limited to.
type couponSKU struct {
	ID   uint
	Name string // 商品 · 规格
}

func (s *Server) coupons(c *gin.Context) {
	var cs []model.Coupon
	s.st.DB.Order("id DESC").Find(&cs)
	var prods []model.Product
	s.st.DB.Preload("SKUs", func(db *gorm.DB) *gorm.DB { return db.Order("sort, id") }).Order("sort, id").Find(&prods)
	var skus []couponSKU
	names := map[string]string{}
	for _, p := range prods {
		for _, k := range p.SKUs {
			n := p.Title + " · " + k.Name
			skus = append(skus, couponSKU{k.ID, n})
			names[strconv.Itoa(int(k.ID))] = n
		}
	}
	scope := map[uint]string{}
	for _, cp := range cs {
		if cp.SKUScope == "" {
			continue
		}
		var parts []string
		for _, id := range strings.Split(cp.SKUScope, ",") {
			if n, ok := names[id]; ok {
				parts = append(parts, n)
			} else {
				parts = append(parts, "已删除的规格")
			}
		}
		scope[cp.ID] = strings.Join(parts, "、")
	}
	s.render(c, 200, "admin_coupons", gin.H{"Coupons": cs, "SKUs": skus, "Scope": scope})
}

func (s *Server) enableCoupon(c *gin.Context) {
	s.st.DB.Model(&model.Coupon{}).Where("id = ?", c.Param("id")).Update("active", true)
	s.audit(c, "coupon_enable", c.Param("id"), "")
	redirectMsg(c, s.cfg.AdminPath+"/coupons", "已启用")
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
	cp := model.Coupon{Code: code, Type: typ, Value: value, SKUScope: strings.Join(c.PostFormArray("sku_scope"), ","),
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
	desc := fmt.Sprintf("减 %d%%", value)
	if typ != "percent" {
		desc = "立减 " + shop.FormatMoney(value, model.CNY)
	}
	s.audit(c, "coupon_create", code, desc)
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
	s.render(c, 200, "admin_tickets", gin.H{"MailReady": s.shop.Notify.MailReady(), "Tickets": ts, "Status": c.DefaultQuery("status", model.TicketOpen)})
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

// settingsTab is one page of the settings (one card each).
type settingsTab struct{ ID, Name string }

var settingsTabs = []settingsTab{
	{"site", "站点"}, {"appearance", "外观"}, {"payment", "支付"}, {"mail", "邮件"}, {"telegram", "Telegram"},
	{"login", "登录方式"}, {"panel", "面板对接"}, {"polaris", "授权服务"},
}

func validSettingsTab(id string) bool {
	for _, t := range settingsTabs {
		if t.ID == id {
			return true
		}
	}
	return false
}

// settingsURL is the page of one settings tab.
func (s *Server) settingsURL(tab string) string { return s.cfg.AdminPath + "/settings/" + tab }

func (s *Server) settingsPage(c *gin.Context) {
	tab := c.Param("tab")
	if tab == "" {
		tab = "site"
	}
	if !validSettingsTab(tab) {
		c.Redirect(http.StatusSeeOther, s.settingsURL("site"))
		return
	}
	st := s.st.Settings()
	secrets := map[string]bool{"EPayKey": st.EPayKey != "", "TronGridKey": st.TronGridKey != "", "SMTPPass": st.SMTPPass != "",
		"TGToken": st.TGToken != "", "TGWebhookSecret": st.TGWebhookSecret != "", "PolarisSecret": st.PolarisSecret != "",
		"TurnstileSecret":    st.TurnstileSecret != "",
		"GitHubClientSecret": st.GitHubClientSecret != "", "PanelToken": st.PanelToken != "", "GoogleClientSecret": st.GoogleClientSecret != ""}
	base := s.publicURL(c)
	s.render(c, 200, "admin_settings", gin.H{"Tab": tab, "Last": s.lastActions(), "EPayLast": s.shop.LastEPay(), "S": st, "Secrets": secrets, "Base": base,
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
	back := s.settingsURL("site")
	if validSettingsTab(c.PostForm("section")) {
		back = s.settingsURL(c.PostForm("section"))
	}
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
	redirectMsg(c, back, "设置已保存")
}

// setWebhook registers the bot webhook with Telegram and reads the bot's
// username.
func (s *Server) setWebhook(c *gin.Context) {
	back := s.settingsURL("telegram")
	done := func(err error, msg string) { s.actionDone(c, "telegram", back, err, msg) }
	st := s.st.Settings()
	base := strings.TrimRight(st.PublicURL, "/")
	if st.TGToken == "" || !strings.HasPrefix(base, "https://") {
		done(errors.New("需要先保存 Bot Token，并在「站点」里填写 HTTPS 站点地址"), "")
		return
	}
	bot := &tgapi.Client{Token: st.TGToken}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	var me tgapi.User
	if err := bot.Call(ctx, "getMe", map[string]any{}, &me); err != nil {
		done(fmt.Errorf("Bot Token 无效或连不上 Telegram：%w", err), "")
		return
	}
	if err := bot.Call(ctx, "setWebhook", map[string]any{
		"url": base + "/tg/webhook/" + st.TGWebhookSecret, "secret_token": st.TGWebhookSecret,
		"allowed_updates": []string{"message", "callback_query", "pre_checkout_query"}, "drop_pending_updates": true,
	}, nil); err != nil {
		done(fmt.Errorf("设置 Webhook 失败：%w", err), "")
		return
	}
	commands := []map[string]string{
		{"command": "start", "description": "🏠 主菜单"}, {"command": "shop", "description": "🛒 选购商品"},
		{"command": "orders", "description": "📦 我的订单"}, {"command": "help", "description": "❓ 帮助"},
		{"command": "cancel", "description": "❌ 取消当前操作"},
	}
	bot.Call(ctx, "setMyCommands", map[string]any{"commands": commands}, nil)
	if st.TGAdminChat != 0 {
		// the admin also sees today's sales
		bot.Call(ctx, "setMyCommands", map[string]any{"commands": append(commands, map[string]string{"command": "stats", "description": "📊 今日销售"}),
			"scope": map[string]any{"type": "chat", "chat_id": st.TGAdminChat}}, nil)
	}
	bot.Call(ctx, "setChatMenuButton", map[string]any{"menu_button": map[string]string{"type": "commands"}}, nil)
	st.TGBotUsername = me.Username
	s.st.SaveSettings(st)
	s.audit(c, "telegram_webhook", me.Username, "")
	done(nil, "Webhook 已设置：@"+me.Username+"，买家现在可以在 Telegram 里下单")
}

func (s *Server) testMail(c *gin.Context) {
	back := s.settingsURL("mail")
	done := func(err error, msg string) { s.actionDone(c, "mail", back, err, msg) }
	to := strings.TrimSpace(c.PostForm("to"))
	if err := s.shop.Notify.Mail(to, "Kaku 测试邮件", "如果您收到这封邮件，说明 SMTP 配置正确。"); err != nil {
		done(fmt.Errorf("发送失败：%w", err), "")
		return
	}
	done(nil, "测试邮件已发送到 "+to+"，请查收（也看看垃圾邮件箱）")
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
	s.render(c, 200, "admin_audit", gin.H{"Logs": logs, "Page": p, "HasNext": int64(p*100) < total, "Action": c.Query("action")})
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

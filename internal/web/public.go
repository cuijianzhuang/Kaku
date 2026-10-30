package web

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	qrcode "github.com/skip2/go-qrcode"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/payment"
	"github.com/cuijianzhuang/kaku/internal/polaris"
	"github.com/cuijianzhuang/kaku/internal/shop"
)

func (s *Server) publicRoutes(r *gin.Engine) {
	r.GET("/", s.home)
	r.GET("/robots.txt", s.robots)
	r.GET("/sitemap.xml", s.sitemap)
	r.GET("/p/:id", s.productPage)
	r.POST("/order", s.createOrder)
	r.GET("/pay/:no", s.payPage)
	r.GET("/order/:no", s.orderPage)
	r.POST("/order/:no", s.unlockOrder)
	r.GET("/order/:no/status", s.orderStatus)
	r.GET("/order/:no/item/:id", s.downloadItem)
	r.GET("/query", s.queryPage)
	r.POST("/query", s.query)
	r.GET("/renew", s.renewPage)
	r.POST("/renew", s.renew)
	r.GET("/ticket", s.ticketPage)
	r.POST("/ticket", s.createTicket)
	r.Any("/notify/epay", s.epayNotify)
	r.GET("/return/epay", s.epayReturn)
	r.POST("/hooks/polaris", s.polarisHook)
	r.POST("/tg/webhook/:secret", s.tgWebhook)
}

func (s *Server) home(c *gin.Context) {
	cats, err := s.shop.Catalog()
	if err != nil {
		s.fail(c, err)
		return
	}
	stock := map[uint]int{}
	for _, cat := range cats {
		for _, p := range cat.Products {
			for i := range p.SKUs {
				stock[p.SKUs[i].ID] = s.shop.Stock(&p.SKUs[i])
			}
		}
	}
	s.render(c, 200, "index", gin.H{"Categories": cats, "Stock": stock})
}

type skuView struct {
	model.SKU
	Stock         int
	Selected      bool
	QuantityLimit int
	Methods       []shop.Method
	Ask           string // manual SKUs: what the buyer must fill in
	Note          string // manual SKUs: when and how it is delivered
	Tiers         string // "3:80,10:50" for the reference total
	TierTip       string // "满 3 件 8 折 · 满 10 件 5 折"
}

// tierText describes wholesale tiers for buyers.
func tierText(tiers []model.WholesaleTier) (attr, tip string) {
	var a, t []string
	for _, x := range tiers {
		if x.MinQty < 2 || x.Percent <= 0 || x.Percent >= 100 {
			continue
		}
		a = append(a, fmt.Sprintf("%d:%d", x.MinQty, x.Percent))
		zhe := strconv.FormatFloat(float64(x.Percent)/10, 'f', -1, 64)
		t = append(t, fmt.Sprintf("满 %d 件 %s 折", x.MinQty, zhe))
	}
	return strings.Join(a, ","), strings.Join(t, " · ")
}

func (s *Server) productPage(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	s.renderProduct(c, uint(id), nil, nil)
}

// renderProduct draws a product page; after a failed order it shows the
// error with what the buyer typed (except the query password).
func (s *Server) renderProduct(c *gin.Context, id uint, form map[string]string, orderErr error) {
	p, err := s.shop.Product(id)
	if err != nil {
		s.fail(c, err)
		return
	}
	var skus []skuView
	if form == nil {
		form = map[string]string{}
	}
	for i := range p.SKUs {
		k := &p.SKUs[i]
		v := skuView{SKU: *k, Stock: s.shop.Stock(k), Methods: s.shop.MethodsFor(model.ChannelWeb, k)}
		v.QuantityLimit = max(k.LimitPerOrder, 0)
		if v.Stock > 0 && (v.QuantityLimit == 0 || v.Stock < v.QuantityLimit) {
			v.QuantityLimit = v.Stock
		}
		v.Tiers, v.TierTip = tierText(k.Tiers())
		if k.DelivererKind == "manual" {
			if cfg, err := deliver.ParseManualCfg(k.DelivererCfg); err == nil {
				v.Ask, v.Note = cfg.Ask, cfg.Note
			}
		}
		skus = append(skus, v)
	}
	inStock := false
	for _, k := range skus {
		if k.Stock != 0 {
			inStock = true
		}
	}
	selected := -1
	for i, k := range skus {
		if k.Stock != 0 && strconv.FormatUint(uint64(k.ID), 10) == form["sku_id"] {
			selected = i
			break
		}
	}
	if selected < 0 {
		for i, k := range skus {
			if k.Stock != 0 && len(k.Methods) > 0 {
				selected = i
				break
			}
		}
	}
	if selected < 0 {
		for i, k := range skus {
			if k.Stock != 0 {
				selected = i
				break
			}
		}
	}
	quantityLimit := 0
	canPurchase := false
	if selected >= 0 {
		skus[selected].Selected = true
		quantityLimit = skus[selected].QuantityLimit
		canPurchase = len(skus[selected].Methods) > 0
	}
	// sold count and remaining stock for the badges under the title
	var sold int64
	ids := make([]uint, 0, len(p.SKUs))
	stockLeft, unlimited := 0, false
	for _, k := range skus {
		ids = append(ids, k.ID)
		if k.Stock < 0 {
			unlimited = true
		} else {
			stockLeft += k.Stock
		}
	}
	if len(ids) > 0 {
		s.st.DB.Model(&model.Order{}).Where("sku_id IN ? AND status = ?", ids, model.OrderDelivered).Select("COALESCE(SUM(qty), 0)").Scan(&sold)
	}
	data := gin.H{"Product": p, "SKUs": skus, "Methods": s.shop.Methods(model.ChannelWeb), "InStock": inStock, "Form": form,
		"Sold": sold, "StockLeft": stockLeft, "Unlimited": unlimited, "QuantityLimit": quantityLimit, "CanPurchase": canPurchase}
	code := http.StatusOK
	if orderErr != nil {
		data["Error"] = orderErr.Error()
		code = http.StatusUnprocessableEntity
	}
	s.render(c, code, "product", data)
}

// fail shows a buyer-safe error page.
func (s *Server) fail(c *gin.Context, err error) {
	var ue *shop.UserError
	if errors.As(err, &ue) {
		s.render(c, http.StatusBadRequest, "error", gin.H{"Message": ue.Msg})
		return
	}
	s.log.Error("request failed", "path", c.Request.URL.Path, "err", err)
	s.render(c, http.StatusInternalServerError, "error", gin.H{"Message": "服务器出错了，请稍后再试"})
}

func userMessage(err error) string {
	var ue *shop.UserError
	if errors.As(err, &ue) {
		return ue.Msg
	}
	return "服务器出错了，请稍后再试"
}

func (s *Server) verifyTurnstile(c *gin.Context) error {
	st := s.st.Settings()
	if st.TurnstileSecret == "" || st.TurnstileSite == "" {
		return nil
	}
	token := c.PostForm("cf-turnstile-response")
	if token == "" {
		return &shop.UserError{Msg: "请完成人机验证"}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
	defer cancel()
	form := url.Values{"secret": {st.TurnstileSecret}, "response": {token}, "remoteip": {c.ClientIP()}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &shop.UserError{Msg: "人机验证服务暂不可用，请稍后再试"}
	}
	defer resp.Body.Close()
	var r struct {
		Success bool `json:"success"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if !r.Success {
		return &shop.UserError{Msg: "人机验证未通过"}
	}
	return nil
}

func (s *Server) setAccess(c *gin.Context, orderNo string) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie("ko_"+orderNo, s.shop.AccessToken(orderNo), 30*86400, "/order/"+orderNo, "", s.cfg.SecureCookie, true)
	c.SetCookie("ko_"+orderNo, s.shop.AccessToken(orderNo), 30*86400, "/pay/"+orderNo, "", s.cfg.SecureCookie, true)
}

func (s *Server) hasAccess(c *gin.Context, orderNo string) bool {
	v, _ := c.Cookie("ko_" + orderNo)
	return s.shop.CheckAccess(orderNo, v)
}

func (s *Server) createOrder(c *gin.Context) {
	ip, now := c.ClientIP(), s.now()
	back := "/p/" + c.PostForm("product_id")
	if s.orderLimit.blocked(ip, now) {
		redirectErr(c, back, errors.New("下单过于频繁，请稍后再试"))
		return
	}
	if err := s.verifyTurnstile(c); err != nil {
		redirectErr(c, back, err)
		return
	}
	skuID, _ := strconv.Atoi(c.PostForm("sku_id"))
	qty, _ := strconv.Atoi(c.DefaultPostForm("qty", "1"))
	s.orderLimit.hit(ip, now)
	o, err := s.shop.CreateOrder(c.Request.Context(), shop.CreateInput{
		SKUID: uint(skuID), Qty: qty, Email: c.PostForm("email"), QueryPassword: c.PostForm("password"),
		PayMethod: c.PostForm("method"), Coupon: c.PostForm("coupon"), Remark: c.PostForm("remark"),
		Ask: c.PostForm("ask"),
		IP:  ip, Channel: model.ChannelWeb,
	})
	if err != nil {
		pid, _ := strconv.Atoi(c.PostForm("product_id"))
		form := map[string]string{"sku_id": c.PostForm("sku_id"), "qty": c.PostForm("qty"), "email": c.PostForm("email"),
			"coupon": c.PostForm("coupon"), "remark": c.PostForm("remark"), "ask": c.PostForm("ask"), "method": c.PostForm("method")}
		s.renderProduct(c, uint(pid), form, errors.New(userMessage(err)))
		return
	}
	s.setAccess(c, o.OrderNo)
	if o.Status != model.OrderPending {
		c.Redirect(http.StatusSeeOther, "/order/"+o.OrderNo)
		return
	}
	c.Redirect(http.StatusSeeOther, "/pay/"+o.OrderNo)
}

func (s *Server) payPage(c *gin.Context) {
	o, err := s.shop.OrderByNo(c.Param("no"))
	if err != nil {
		s.fail(c, err)
		return
	}
	if o.Status == model.OrderPending && s.now().Sub(o.CreatedAt) > 20*time.Second {
		// paid in another tab or the notification was lost: don't offer to pay again
		if paid, _ := s.shop.CheckEPay(c.Request.Context(), o, 5*time.Second); paid {
			o.Status = model.OrderPaid
		}
	}
	if o.Status != model.OrderPending {
		c.Redirect(http.StatusSeeOther, "/order/"+o.OrderNo)
		return
	}
	data := gin.H{"Order": o, "Remaining": int(time.Until(o.ExpiresAt).Seconds())}
	switch o.PayMethod {
	case model.PayEPayAlipay, model.PayEPayWxpay:
		base := s.publicURL(c)
		u, err := s.shop.EPay().CheckoutURL(o.PayMethod, o.OrderNo, o.ProductTitle+" "+o.SKUName, o.PayAmount,
			base+"/notify/epay", base+"/return/epay?no="+o.OrderNo)
		if err != nil {
			s.fail(c, &shop.UserError{Msg: "支付通道暂不可用：" + err.Error()})
			return
		}
		data["CheckoutURL"] = u
	case model.PayUSDT:
		addr := s.st.Settings().USDTAddress
		data["Address"] = addr
		data["USDT"] = payment.Decimal(o.PayAmount, 6)
		if png, err := qrcode.Encode(addr, qrcode.Medium, 220); err == nil {
			data["QR"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		}
	case model.PayStars:
		if u := s.st.Settings().TGBotUsername; u != "" {
			data["BotURL"] = "https://t.me/" + u
		}
	}
	s.render(c, 200, "pay", data)
}

func (s *Server) orderPage(c *gin.Context) {
	o, err := s.shop.OrderByNo(c.Param("no"))
	if err != nil {
		s.fail(c, err)
		return
	}
	data := gin.H{"Order": o, "Access": s.hasAccess(c, o.OrderNo)}
	if data["Access"] == true && o.Status == model.OrderDelivered {
		items, err := s.shop.Items(o.ID)
		if err != nil {
			s.fail(c, err)
			return
		}
		data["Items"] = items
		for _, it := range items {
			if it.Source == model.SourcePolaris && it.FileName == "" && o.RenewOf == "" {
				data["PolarisHelp"] = true
			}
			if it.Source == model.SourcePanel {
				data["PanelHelp"] = s.shop.PanelRedeemHint()
			}
		}
	}
	if o.Status == model.OrderPaid {
		var sku model.SKU
		if s.st.DB.Take(&sku, o.SKUID).Error == nil && sku.DelivererKind == "manual" {
			data["Manual"] = true
			if cfg, err := deliver.ParseManualCfg(sku.DelivererCfg); err == nil {
				data["ManualNote"] = cfg.Note
			}
		}
	}
	s.render(c, 200, "order", data)
}

func (s *Server) unlockOrder(c *gin.Context) {
	no, ip, now := c.Param("no"), c.ClientIP(), s.now()
	if s.queryLimit.blocked(ip, now) {
		redirectErr(c, "/order/"+no, errors.New("尝试次数过多，请 15 分钟后再试"))
		return
	}
	found, err := s.shop.FindOrders(no, "", c.PostForm("password"))
	if err != nil || len(found) == 0 {
		s.queryLimit.hit(ip, now)
		redirectErr(c, "/order/"+no, errors.New("查询密码不正确"))
		return
	}
	s.setAccess(c, no)
	c.Redirect(http.StatusSeeOther, "/order/"+no)
}

func (s *Server) orderStatus(c *gin.Context) {
	o, err := s.shop.OrderByNo(c.Param("no"))
	if err != nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	if o.Status == model.OrderPending {
		if paid, _ := s.shop.CheckEPay(c.Request.Context(), o, 5*time.Second); paid {
			o, _ = s.shop.OrderByNo(o.OrderNo)
		}
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"status": o.Status, "label": statusLabel(o.Status)})
}

func (s *Server) downloadItem(c *gin.Context) {
	o, err := s.shop.OrderByNo(c.Param("no"))
	if err != nil || !s.hasAccess(c, o.OrderNo) || o.Status != model.OrderDelivered {
		c.String(http.StatusForbidden, "无权访问")
		return
	}
	id, _ := strconv.Atoi(c.Param("id"))
	items, _ := s.shop.Items(o.ID)
	for _, it := range items {
		if int(it.ID) == id {
			name := it.FileName
			if name == "" {
				name = fmt.Sprintf("%s-%d.txt", o.OrderNo, it.ID)
			}
			c.Header("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
			c.Header("Cache-Control", "no-store")
			c.Data(200, "application/octet-stream", []byte(it.Content))
			return
		}
	}
	c.String(404, "not found")
}

func (s *Server) queryPage(c *gin.Context) { s.render(c, 200, "query", nil) }

func (s *Server) query(c *gin.Context) {
	ip, now := c.ClientIP(), s.now()
	keep := func(h gin.H) gin.H { h["No"], h["Email"] = c.PostForm("order_no"), c.PostForm("email"); return h }
	if s.queryLimit.blocked(ip, now) {
		s.render(c, http.StatusTooManyRequests, "query", keep(gin.H{"Error": "尝试次数过多，请 15 分钟后再试"}))
		return
	}
	found, err := s.shop.FindOrders(c.PostForm("order_no"), c.PostForm("email"), c.PostForm("password"))
	if err != nil {
		s.render(c, 400, "query", keep(gin.H{"Error": userMessage(err)}))
		return
	}
	if len(found) == 0 {
		s.queryLimit.hit(ip, now)
		s.render(c, 200, "query", keep(gin.H{"Error": "没有找到订单，请检查订单号 / 邮箱和查询密码", "Forgot": true}))
		return
	}
	for _, o := range found {
		s.setAccess(c, o.OrderNo)
	}
	if len(found) == 1 {
		c.Redirect(http.StatusSeeOther, "/order/"+found[0].OrderNo)
		return
	}
	s.render(c, 200, "query", gin.H{"Orders": found})
}

func (s *Server) renewPage(c *gin.Context) {
	lid := strings.TrimSpace(c.Query("lid"))
	data := gin.H{"LicenseID": lid}
	if lid != "" {
		b, err := s.shop.Binding(lid)
		if err != nil {
			data["Error"] = userMessage(err)
		} else {
			skus, titles, err := s.shop.RenewSKUs(b)
			if err != nil {
				s.fail(c, err)
				return
			}
			var views []skuView
			for i := range skus {
				views = append(views, skuView{SKU: skus[i], Methods: s.shop.MethodsFor(model.ChannelWeb, &skus[i])})
			}
			data["Binding"], data["SKUs"], data["Titles"] = b, views, titles
			data["Methods"] = s.shop.Methods(model.ChannelWeb)
		}
	}
	s.render(c, 200, "renew", data)
}

func (s *Server) renew(c *gin.Context) {
	lid := strings.TrimSpace(c.PostForm("lid"))
	back := "/renew?lid=" + url.QueryEscape(lid)
	ip, now := c.ClientIP(), s.now()
	if s.queryLimit.blocked(ip, now) {
		redirectErr(c, back, errors.New("尝试次数过多，请 15 分钟后再试"))
		return
	}
	b, err := s.shop.Binding(lid)
	if err != nil {
		redirectErr(c, back, errors.New(userMessage(err)))
		return
	}
	email := c.PostForm("email")
	if !shop.OwnsLicense(b, email, 0) {
		s.queryLimit.hit(ip, now)
		redirectErr(c, back, errors.New("邮箱与购买时不一致"))
		return
	}
	skuID, _ := strconv.Atoi(c.PostForm("sku_id"))
	qty, _ := strconv.Atoi(c.DefaultPostForm("qty", "1"))
	o, err := s.shop.CreateOrder(c.Request.Context(), shop.CreateInput{
		SKUID: uint(skuID), Qty: qty, Email: email, QueryPassword: c.PostForm("password"),
		PayMethod: c.PostForm("method"), Coupon: c.PostForm("coupon"), IP: ip, Channel: model.ChannelWeb, RenewOf: b.LicenseID,
	})
	if err != nil {
		redirectErr(c, back, errors.New(userMessage(err)))
		return
	}
	s.setAccess(c, o.OrderNo)
	c.Redirect(http.StatusSeeOther, "/pay/"+o.OrderNo)
}

func (s *Server) ticketPage(c *gin.Context) {
	s.render(c, 200, "ticket", gin.H{"OrderNo": c.Query("no")})
}

// ticketFail shows the form again with the error and what was typed.
func (s *Server) ticketFail(c *gin.Context, code int, msg string) {
	s.render(c, code, "ticket", gin.H{"OrderNo": c.PostForm("order_no"), "Email": c.PostForm("email"),
		"Content": c.PostForm("content"), "Error": msg})
}

func (s *Server) createTicket(c *gin.Context) {
	ip, now := c.ClientIP(), s.now()
	if s.ticketLimit.blocked(ip, now) {
		s.ticketFail(c, http.StatusTooManyRequests, "提交过于频繁，请稍后再试")
		return
	}
	no, email := strings.TrimSpace(c.PostForm("order_no")), strings.TrimSpace(c.PostForm("email"))
	content := strings.TrimSpace(c.PostForm("content"))
	if content == "" || len([]rune(content)) > 2000 || email == "" || len(email) > 200 {
		s.ticketFail(c, http.StatusUnprocessableEntity, "请填写邮箱和问题描述（最多 2000 字）")
		return
	}
	// Only the buyer can open a ticket about an order.
	if no != "" {
		found, _ := s.shop.FindOrders(no, "", c.PostForm("password"))
		if len(found) == 0 {
			s.ticketLimit.hit(ip, now)
			s.ticketFail(c, http.StatusUnprocessableEntity, "订单号或查询密码不正确")
			return
		}
	}
	s.ticketLimit.hit(ip, now)
	t := model.Ticket{OrderNo: no, Email: strings.ToLower(email), Content: content, Status: model.TicketOpen, ClientIP: ip}
	if err := s.st.DB.Create(&t).Error; err != nil {
		s.fail(c, err)
		return
	}
	go s.shop.Notify.Admin(fmt.Sprintf("📮 新工单 #%d（订单 %s）：%s", t.ID, no, truncate(content, 200)))
	redirectMsg(c, "/ticket", "已提交，我们会通过邮件回复您")
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// epayNotify handles the asynchronous EPay notification: signature, then
// amount, then a second confirmation by querying the gateway.
func (s *Server) epayNotify(c *gin.Context) {
	if err := c.Request.ParseForm(); err != nil {
		c.String(400, "fail")
		return
	}
	values := c.Request.Form
	ep := s.shop.EPay()
	n, err := ep.ParseNotify(values)
	if err != nil {
		s.log.Warn("epay notify rejected", "err", err, "ip", c.ClientIP())
		s.shop.RecordEPay(false, fmt.Sprintf("收到一条支付回调（来自 %s），但%v，已拒绝。请检查易支付的商户号和密钥", c.ClientIP(), err))
		c.String(400, "fail")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	o, err := s.shop.OrderByNo(n.OrderNo)
	if err != nil {
		s.shop.RecordEPay(false, "收到回调，但订单 "+n.OrderNo+" 不存在")
		c.String(404, "fail")
		return
	}
	// Second confirmation against the gateway. The signature already proves
	// the notification came from someone holding the merchant key, so if the
	// gateway's query API cannot answer (many gateways restrict or change
	// it) the signed notification is accepted; only an explicit "not paid"
	// or a different amount rejects it.
	r, qerr := ep.Query(ctx, n.OrderNo)
	switch {
	case qerr != nil && errors.Is(qerr, payment.ErrQuery):
		s.log.Warn("epay query unavailable, trusting signed notify", "order", n.OrderNo, "err", qerr)
	case qerr != nil:
		s.log.Warn("epay query failed", "order", n.OrderNo, "err", qerr)
	case !r.Paid || r.Amount != n.Amount:
		s.log.Warn("epay query did not confirm", "order", n.OrderNo, "paid", r.Paid, "amount", r.Amount)
		s.shop.RecordEPay(false, fmt.Sprintf("收到订单 %s 的回调，但向支付平台查询显示未付款或金额不符，已拒绝", n.OrderNo))
		c.String(400, "fail")
		return
	}
	if _, err := s.shop.MarkPaid(ctx, shop.PaidInput{OrderNo: n.OrderNo, Channel: shop.EPayChannel(o, n.Method), TradeNo: n.TradeNo,
		Amount: n.Amount, Currency: model.CNY, Raw: values.Encode()}); err != nil {
		s.log.Warn("epay mark paid failed", "order", n.OrderNo, "err", err)
		s.shop.RecordEPay(false, fmt.Sprintf("收到订单 %s 的回调，但无法确认付款：%v", n.OrderNo, err))
		c.String(400, "fail")
		return
	}
	s.shop.RecordEPay(true, "收到订单 "+n.OrderNo+" 的支付回调，已确认付款")
	c.String(200, "success")
}

// epayReturn is where the cashier sends the buyer back. The return URL is
// not signed, so it only triggers a query to the gateway before showing
// the order.
func (s *Server) epayReturn(c *gin.Context) {
	no := c.Query("no")
	if no == "" {
		no = c.Query("out_trade_no")
	}
	if o, err := s.shop.OrderByNo(no); err == nil {
		s.shop.CheckEPay(c.Request.Context(), o, 0)
	}
	c.Redirect(http.StatusSeeOther, "/order/"+url.PathEscape(no))
}

func (s *Server) polarisHook(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 16<<10))
	if err != nil {
		c.JSON(400, gin.H{"error": "bad request"})
		return
	}
	if err := polaris.VerifyRequest(s.st.Settings().PolarisSecret, c.Request, body, s.now()); err != nil {
		c.JSON(401, gin.H{"error": err.Error()})
		return
	}
	var ev struct {
		Event, LicenseID, Instance string
	}
	var raw map[string]string
	if err := json.Unmarshal(body, &raw); err != nil {
		c.JSON(400, gin.H{"error": "bad json"})
		return
	}
	ev.Event, ev.LicenseID, ev.Instance = raw["event"], raw["license_id"], raw["instance"]
	if err := s.shop.PolarisEvent(ev.Event, ev.LicenseID, ev.Instance); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (s *Server) tgWebhook(c *gin.Context) {
	secret := s.st.Settings().TGWebhookSecret
	if secret == "" || s.bot == nil || subtle.ConstantTimeCompare([]byte(c.Param("secret")), []byte(secret)) != 1 ||
		subtle.ConstantTimeCompare([]byte(c.GetHeader("X-Telegram-Bot-Api-Secret-Token")), []byte(secret)) != 1 {
		c.Status(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		c.Status(400)
		return
	}
	s.bot.HandleUpdate(body)
	c.Status(200)
}

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/payment"
	"github.com/cuijianzhuang/kaku/internal/secure"
)

// Product templates for common kinds of goods: one click creates a product
// (not yet on sale) and a SKU with a working delivery setup, then the
// owner fills in the content.

type quickTemplate struct {
	ID, Name, Icon, Summary, Title, SKU, Desc string
	Kind                                      string
	Cfg                                       func() any
	Next                                      string // what to do after creating
}

var quickTemplates = []quickTemplate{
	{ID: "card", Name: "卡密 / 激活码", Icon: "i-key", Kind: "stock",
		Summary: "游戏点卡、软件激活码、会员兑换码：导入一批，每件发一张。",
		Title:   "会员兑换码", SKU: "月卡",
		Desc: "付款后自动发货，卡密显示在订单页并发送到邮箱。\n\n兑换方法：【请填写，例如在官网「兑换中心」输入卡密】",
		Next: "导入卡密"},
	{ID: "account", Name: "账号密码", Icon: "i-users", Kind: "stock",
		Summary: "成品账号：每行一个「账号----密码」，每件发一个账号。",
		Title:   "成品账号", SKU: "普通账号",
		Desc: "付款后自动发货，格式为「账号----密码」。\n\n- 登录后请立即修改密码\n- 售后时效：【请填写，例如 24 小时内包换】",
		Next: "导入账号"},
	{ID: "download", Name: "资源下载 / 教程", Icon: "i-download", Kind: "text",
		Summary: "网盘链接、软件下载、教程文档：所有买家收到同样的内容。",
		Title:   "资源合集", SKU: "永久版",
		Desc: "付款后自动发送下载地址和提取码。",
		Cfg: func() any {
			return deliver.TextCfg{Text: "下载地址：【请填写网盘链接】\n提取码：【请填写】\n\n订单号 {order_no}，如链接失效请提交工单。"}
		},
		Next: "填写下载地址"},
	{ID: "recharge", Name: "人工代充", Icon: "i-wallet", Kind: "manual",
		Summary: "话费、游戏、会员代充：下单时买家填写充值账号，你在后台处理后发货。",
		Title:   "会员代充", SKU: "1 个月",
		Desc: "下单时填写需要充值的账号，付款后人工处理，完成后发送通知。",
		Cfg: func() any {
			return deliver.ManualCfg{Ask: "充值账号", Note: "付款后 30 分钟内处理（9:00–23:00），完成后邮件通知"}
		},
		Next: "确认价格和说明"},
	{ID: "service", Name: "人工服务", Icon: "i-message", Kind: "manual",
		Summary: "代办、定制、咨询：付款后在后台填写交付内容。",
		Title:   "定制服务", SKU: "标准",
		Desc: "付款后我们会尽快联系你，交付内容在订单页查看。",
		Cfg:  func() any { return deliver.ManualCfg{Ask: "联系方式", Note: "付款后 24 小时内联系你"} },
		Next: "确认价格和说明"},
	{ID: "api", Name: "接口发货", Icon: "i-refresh", Kind: "webhook",
		Summary: "已有自己的系统：付款后 Kaku 调用你的接口取得发货内容。",
		Title:   "接口商品", SKU: "默认",
		Desc: "付款后自动发货。",
		Cfg:  func() any { return deliver.WebhookCfg{Secret: secure.RandomToken(24)} },
		Next: "填写接口地址"},
}

func quickTemplateByID(id string) *quickTemplate {
	for i := range quickTemplates {
		if quickTemplates[i].ID == id {
			return &quickTemplates[i]
		}
	}
	return nil
}

func (s *Server) quickRoutes(a *gin.RouterGroup) {
	a.GET("/products/quick", s.quickPage)
	a.POST("/products/quick", s.quickCreate)
}

func (s *Server) quickPage(c *gin.Context) {
	var cats []model.Category
	s.st.DB.Order("sort, id").Find(&cats)
	pick := c.DefaultQuery("t", "card")
	if quickTemplateByID(pick) == nil {
		pick = "card"
	}
	s.render(c, 200, "admin_quick", gin.H{"Templates": quickTemplates, "Categories": cats, "Pick": pick, "Tab": "general", "CatHint": "游戏点卡"})
}

func (s *Server) quickCreate(c *gin.Context) {
	t := quickTemplateByID(c.PostForm("template"))
	back := s.cfg.AdminPath + "/products/quick"
	if t != nil {
		back += "?t=" + t.ID
	}
	title := strings.TrimSpace(c.PostForm("title"))
	skuName := strings.TrimSpace(c.PostForm("sku_name"))
	price, err := payment.ParseDecimal(strings.TrimSpace(c.DefaultPostForm("price_cny", "0")), 2)
	switch {
	case t == nil:
		err = errors.New("请选择一个模板")
	case title == "":
		err = errors.New("请填写商品名称")
	case err != nil || price <= 0:
		err = errors.New("请填写价格（元），例如 9.9")
	}
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	catID, err := s.categoryFromForm(c)
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	if skuName == "" {
		skuName = t.SKU
	}
	cfg := ""
	if t.Cfg != nil {
		b, _ := json.Marshal(t.Cfg())
		cfg = string(b)
	}
	p := model.Product{CategoryID: catID, Title: title, Status: model.ProductOff, DescriptionMD: t.Desc}
	if err := s.st.DB.Create(&p).Error; err != nil {
		redirectErr(c, back, err)
		return
	}
	sku := model.SKU{ProductID: p.ID, Name: skuName, PriceCNY: price, DelivererKind: t.Kind, DelivererCfg: cfg,
		LimitPerOrder: 10, LowStock: 5, Active: true}
	if t.Kind != "stock" {
		sku.LowStock = 0
	}
	if err := s.st.DB.Create(&sku).Error; err != nil {
		redirectErr(c, back, err)
		return
	}
	deliver.RefreshStock(s.st.DB, sku.ID)
	s.audit(c, "product_quick", fmt.Sprint(p.ID), t.Name)
	next := fmt.Sprintf("%s/skus/%d", s.cfg.AdminPath, sku.ID)
	if t.Kind == "stock" {
		next = fmt.Sprintf("%s/cards?sku=%d", s.cfg.AdminPath, sku.ID)
	}
	redirectMsg(c, next, fmt.Sprintf("已创建商品「%s」（暂未上架）。下一步：%s，然后在商品页把状态改为上架", title, t.Next))
}

// productTodo lists what keeps a product from being sold properly: text a
// template left to fill in, specs whose delivery is not set up, no specs.
// blocking items stop it going on sale; the rest are warnings.
func (s *Server) productTodo(p *model.Product) (blocking, warnings []string) {
	if strings.Contains(p.DescriptionMD, deliver.Placeholder) {
		blocking = append(blocking, "商品详情里还有「"+deliver.Placeholder+"…】」，请换成实际说明")
	}
	if p.ID == 0 {
		return
	}
	var skus []model.SKU
	s.st.DB.Where("product_id = ? AND active = ?", p.ID, true).Order("sort, id").Find(&skus)
	if len(skus) == 0 {
		blocking = append(blocking, "还没有启用的规格，买家无法下单")
	}
	for i := range skus {
		k := &skus[i]
		d, err := s.shop.Reg.Get(k.DelivererKind)
		if err == nil {
			err = d.Validate(json.RawMessage(k.DelivererCfg))
		}
		if err != nil {
			blocking = append(blocking, fmt.Sprintf("规格「%s」的发货设置未完成：%s", k.Name, err.Error()))
			continue
		}
		if k.PriceCNY == 0 && k.PriceUSDT == 0 && k.PriceStars == 0 {
			blocking = append(blocking, fmt.Sprintf("规格「%s」还没有价格", k.Name))
		}
		if k.DelivererKind == "stock" && s.shop.Stock(k) == 0 {
			warnings = append(warnings, fmt.Sprintf("规格「%s」没有库存，买家会看到缺货", k.Name))
		}
	}
	return
}

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/panelapi"
	"github.com/cuijianzhuang/kaku/internal/payment"
)

// Product templates share one entry (通用商品 / Polaris 授权 / 面板套餐) and a
// category picker that can create the category on the spot; the category
// page offers common categories to add in one go.

func (s *Server) templateRoutes(a *gin.RouterGroup) {
	a.GET("/products/panel-import", s.panelImportPage)
	a.POST("/products/panel-import", s.panelImport)
	a.POST("/categories/presets", s.addCategoryPresets)
}

// categoryPresets are common shop categories.
var categoryPresets = []string{"游戏点卡", "会员充值", "软件激活码", "成品账号", "资源下载", "机场套餐", "软件授权", "人工服务"}

// categoryFromForm returns the chosen category, creating it when the
// owner picked 「新建分类」 and typed a name (an existing name is reused).
func (s *Server) categoryFromForm(c *gin.Context) (uint, error) {
	v := c.PostForm("category_id")
	if v != "new" && v != "" {
		if id := atoiU(v); id != 0 {
			return id, nil
		}
	}
	name := strings.TrimSpace(c.PostForm("new_category"))
	if name == "" {
		return 0, errors.New("请选择分类，或填写新分类的名称")
	}
	if len([]rune(name)) > 100 {
		return 0, errors.New("分类名称最多 100 字")
	}
	var cat model.Category
	if s.st.DB.Where("name = ?", name).Take(&cat).Error == nil {
		return cat.ID, nil
	}
	cat = model.Category{Name: name, Visible: true}
	if err := s.st.DB.Create(&cat).Error; err != nil {
		return 0, err
	}
	s.audit(c, "category_save", fmt.Sprint(cat.ID), name)
	return cat.ID, nil
}

func (s *Server) addCategoryPresets(c *gin.Context) {
	back := s.cfg.AdminPath + "/categories"
	added := 0
	for _, name := range c.PostFormArray("preset") {
		name = strings.TrimSpace(name)
		if name == "" || len([]rune(name)) > 100 {
			continue
		}
		var n int64
		s.st.DB.Model(&model.Category{}).Where("name = ?", name).Count(&n)
		if n > 0 {
			continue
		}
		cat := model.Category{Name: name, Visible: true}
		if s.st.DB.Create(&cat).Error == nil {
			added++
			s.audit(c, "category_save", fmt.Sprint(cat.ID), name)
		}
	}
	if added == 0 {
		redirectErr(c, back, errors.New("没有新增：请勾选还没有的分类"))
		return
	}
	redirectMsg(c, back, fmt.Sprintf("已新增 %d 个分类", added))
}

var priceRE = regexp.MustCompile(`\d+(\.\d{1,2})?`)

// panelPrice reads a plan's listed price ("¥15", "15.00 元") as fen.
func panelPrice(v string) int64 {
	m := priceRE.FindString(v)
	if m == "" {
		return 0
	}
	n, err := payment.ParseDecimal(m, 2)
	if err != nil {
		return 0
	}
	return n
}

func (s *Server) panelImportPage(c *gin.Context) {
	data := gin.H{"Categories": s.categoryList(), "Tab": "panel"}
	if !s.shop.Panel().Ready() {
		data["NotReady"] = true
	} else if offer, err := s.shop.PanelOffer(c.Request.Context(), true); err != nil {
		data["Err"] = panelapi.Explain(err)
	} else {
		data["Offer"] = offer
	}
	s.render(c, 200, "admin_panel_import", data)
}

func (s *Server) panelImport(c *gin.Context) {
	back := s.cfg.AdminPath + "/products/panel-import"
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	offer, err := s.shop.PanelOffer(ctx, true)
	if err != nil {
		redirectErr(c, back, errors.New(panelapi.Explain(err)))
		return
	}
	title := strings.TrimSpace(c.PostForm("title"))
	if title == "" {
		redirectErr(c, back, errors.New("请填写商品名称"))
		return
	}
	days := int(atoiU(c.PostForm("expire_days")))
	type pick struct {
		cfg   deliver.PanelCodeCfg
		price int64
	}
	var picks []pick
	seen := map[string]bool{}
	for _, v := range c.PostFormArray("target") {
		if seen[v] {
			continue
		}
		seen[v] = true
		kind, id, _ := strings.Cut(v, ":")
		switch kind {
		case "plan":
			for _, p := range offer.Plans {
				if p.ID == id {
					picks = append(picks, pick{deliver.PanelCodeCfg{PlanID: p.ID, Name: p.Name, Detail: planDetail(p), ExpireDays: days}, panelPrice(p.Price)})
				}
			}
		case "pack":
			for _, p := range offer.Packs {
				if p.ID == id {
					picks = append(picks, pick{deliver.PanelCodeCfg{PackID: p.ID, Name: p.Name, Detail: "流量包 · " + gb(p.Traffic), ExpireDays: days}, 0})
				}
			}
		}
	}
	if len(picks) == 0 {
		redirectErr(c, back, errors.New("至少勾选一个套餐或流量包"))
		return
	}
	catID, err := s.categoryFromForm(c)
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	p := model.Product{CategoryID: catID, Title: title, Status: model.ProductOff,
		DescriptionMD: "付款后自动发放面板兑换码。\n\n兑换方法：" + s.shop.PanelRedeemHint()}
	if err := s.st.DB.Create(&p).Error; err != nil {
		redirectErr(c, back, err)
		return
	}
	missing := 0
	for i, pk := range picks {
		cfg, _ := json.Marshal(pk.cfg)
		if pk.price == 0 {
			missing++
		}
		s.st.DB.Create(&model.SKU{ProductID: p.ID, Name: pk.cfg.Name, PriceCNY: pk.price, DelivererKind: "panelcode", DelivererCfg: string(cfg),
			LimitPerOrder: 10, Active: true, Sort: i})
	}
	s.audit(c, "product_import_panel", fmt.Sprint(p.ID), fmt.Sprintf("%d 个套餐", len(picks)))
	msg := fmt.Sprintf("已创建商品和 %d 个规格（暂未上架），价格已按面板标价填好；核对后把商品状态改为上架", len(picks))
	if missing > 0 {
		msg = fmt.Sprintf("已创建商品和 %d 个规格（暂未上架）。其中 %d 个没有面板标价：逐个打开规格填写价格，然后把商品状态改为上架", len(picks), missing)
	}
	redirectMsg(c, fmt.Sprintf("%s/products/%d", s.cfg.AdminPath, p.ID), msg)
}

func (s *Server) categoryList() []model.Category {
	var cats []model.Category
	s.st.DB.Order("sort, id").Find(&cats)
	return cats
}

package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/deliver"
	"github.com/cuijianzhuang/kaku/internal/model"
)

// Forms for the general delivery methods (fixed text, webhook, manual) and
// wholesale tiers, so no SKU needs hand-written JSON.

// genericForm is what the SKU page needs to draw those forms.
type genericForm struct {
	Text      deliver.TextCfg
	Webhook   deliver.WebhookCfg
	Manual    deliver.ManualCfg
	Wholesale []model.WholesaleTier // always 3 rows, empty ones for new tiers
	Cards     int64                 // stock SKUs: available cards
}

func (s *Server) genericFormData(sku *model.SKU) genericForm {
	var f genericForm
	switch sku.DelivererKind {
	case "text":
		if c, err := deliver.ParseTextCfg(sku.DelivererCfg); err == nil {
			f.Text = *c
		}
	case "webhook":
		if c, err := deliver.ParseWebhookCfg(sku.DelivererCfg); err == nil {
			f.Webhook = *c
		}
	case "manual":
		if c, err := deliver.ParseManualCfg(sku.DelivererCfg); err == nil {
			f.Manual = *c
		}
	case "stock":
		if sku.ID != 0 {
			s.st.DB.Model(&model.Card{}).Where("sku_id = ? AND status = ?", sku.ID, model.CardAvailable).Count(&f.Cards)
		}
	}
	if sku.Wholesale != "" {
		json.Unmarshal([]byte(sku.Wholesale), &f.Wholesale)
	}
	for len(f.Wholesale) < 3 {
		f.Wholesale = append(f.Wholesale, model.WholesaleTier{})
	}
	return f
}

// genericCfgFromForm builds the configuration of a text, webhook or manual
// SKU from its form. ok is false when the form has no such section (an
// older page or an API client posting raw JSON).
func genericCfgFromForm(c *gin.Context, kind string) (cfg string, ok bool) {
	var v any
	switch kind {
	case "text":
		if _, ok := c.GetPostForm("tx_text"); !ok {
			return "", false
		}
		v = deliver.TextCfg{Text: strings.ReplaceAll(c.PostForm("tx_text"), "\r\n", "\n"), PerUnit: c.PostForm("tx_per_unit") == "on"}
	case "webhook":
		if _, ok := c.GetPostForm("wh_url"); !ok {
			return "", false
		}
		v = deliver.WebhookCfg{URL: strings.TrimSpace(c.PostForm("wh_url")), Secret: strings.TrimSpace(c.PostForm("wh_secret")),
			RevokeURL: strings.TrimSpace(c.PostForm("wh_revoke_url"))}
	case "manual":
		if _, ok := c.GetPostForm("mn_ask"); !ok {
			return "", false
		}
		v = deliver.ManualCfg{Ask: strings.TrimSpace(c.PostForm("mn_ask")), Note: strings.TrimSpace(c.PostForm("mn_note"))}
	case "stock":
		return "", true
	default:
		return "", false
	}
	b, _ := json.Marshal(v)
	return string(b), true
}

// wholesaleFromForm reads the tier rows (满 N 件打 X 折). ok is false when
// the form posts the raw JSON field instead.
func wholesaleFromForm(c *gin.Context) (string, bool, error) {
	qtys, ok := c.GetPostFormArray("ws_qty")
	if !ok {
		return "", false, nil
	}
	pcts := c.PostFormArray("ws_pct")
	var tiers []model.WholesaleTier
	seen := map[int]bool{}
	for i, q := range qtys {
		q = strings.TrimSpace(q)
		p := ""
		if i < len(pcts) {
			p = strings.TrimSpace(pcts[i])
		}
		if q == "" && p == "" {
			continue
		}
		n, err1 := strconv.Atoi(q)
		pct, err2 := strconv.Atoi(p)
		switch {
		case err1 != nil || err2 != nil:
			return "", true, errors.New("批发价：件数和百分比都要填整数")
		case n < 2:
			return "", true, errors.New("批发价：起批件数至少 2 件")
		case pct < 1 || pct > 99:
			return "", true, errors.New("批发价：百分比填 1–99，例如 90 表示按原价 90%（9 折）")
		case seen[n]:
			return "", true, fmt.Errorf("批发价：%d 件填了两次", n)
		}
		seen[n] = true
		tiers = append(tiers, model.WholesaleTier{MinQty: n, Percent: pct})
	}
	if len(tiers) == 0 {
		return "", true, nil
	}
	b, _ := json.Marshal(tiers)
	return string(b), true, nil
}

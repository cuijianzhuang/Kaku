package web

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/update"
)

func (s *Server) updateRoutes(a *gin.RouterGroup) {
	a.GET("/update", s.updatePage)
	a.GET("/update/state", s.updateState)
	a.POST("/update/check", s.checkUpdate)
	a.POST("/update/run", s.runUpdate)
	a.POST("/update/settings", s.saveUpdateSettings)
}

// availableUpdate is the newer release already known (no network access).
func (s *Server) availableUpdate() *update.Release {
	u := s.shop.Updater
	if u == nil {
		return nil
	}
	if r, _ := u.Cached(); r != nil && update.Newer(u.Current, r.Tag) {
		return r
	}
	return nil
}

// updateToken changes when the running version or the pending request
// changes; the upgrade page polls it and reloads.
func (s *Server) updateToken() string {
	u := s.shop.Updater
	return s.cfg.Version + "|" + strconv.FormatBool(u.Pending())
}

func (s *Server) updatePage(c *gin.Context) {
	u := s.shop.Updater
	if u == nil {
		redirectErr(c, s.cfg.AdminPath, update.ErrUnsupported)
		return
	}
	data := gin.H{"S": s.st.Settings(), "Current": u.Current, "Mode": string(u.Mode()), "Pending": u.Pending(),
		"Last": u.LastStatus(), "Token": s.updateToken()}
	rel, checked := u.Cached()
	if checked.IsZero() {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
		var err error
		rel, err = u.Latest(ctx, false)
		cancel()
		if err != nil {
			data["CheckErr"] = err.Error()
		}
		_, checked = u.Cached()
	}
	data["Latest"], data["Checked"] = rel, checked
	data["HasNew"] = rel != nil && update.Newer(u.Current, rel.Tag)
	data["TZ"] = s.now().Format("MST，UTC-07:00")
	s.render(c, 200, "admin_update", data)
}

func (s *Server) updateState(c *gin.Context) {
	if s.shop.Updater == nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	c.JSON(200, gin.H{"status": s.updateToken()})
}

func (s *Server) checkUpdate(c *gin.Context) {
	back := s.cfg.AdminPath + "/update"
	u := s.shop.Updater
	if u == nil {
		redirectErr(c, back, update.ErrUnsupported)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Second)
	defer cancel()
	rel, err := u.Latest(ctx, true)
	switch {
	case err != nil:
		redirectErr(c, back, err)
	case update.Newer(u.Current, rel.Tag):
		redirectMsg(c, back, "发现新版本 "+rel.Tag)
	default:
		redirectMsg(c, back, "已是最新版本")
	}
}

func (s *Server) runUpdate(c *gin.Context) {
	back := s.cfg.AdminPath + "/update"
	u := s.shop.Updater
	if u == nil {
		redirectErr(c, back, update.ErrUnsupported)
		return
	}
	rel, _ := u.Cached()
	if rel == nil || rel.Tag != c.PostForm("tag") || !update.Newer(u.Current, rel.Tag) {
		redirectErr(c, back, errors.New("版本信息已过期，请先检查更新"))
		return
	}
	if err := s.shop.StartUpgrade(c.Request.Context(), rel.Tag); err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "upgrade", rel.Tag, "from "+u.Current)
	redirectMsg(c, back, "已开始升级到 "+rel.Tag+"，服务会自动重启，完成后本页自动刷新")
}

func (s *Server) saveUpdateSettings(c *gin.Context) {
	back := s.cfg.AdminPath + "/update"
	st := s.st.Settings()
	st.AutoUpdate = c.PostForm("auto_update") == "on"
	st.UpdateNotify = c.PostForm("update_notify") == "on"
	hour, _ := strconv.Atoi(c.PostForm("auto_update_hour"))
	st.AutoUpdateHour = min(max(hour, 0), 23)
	if err := s.st.SaveSettings(st); err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "settings_save", "update", "")
	redirectMsg(c, back, "升级设置已保存")
}

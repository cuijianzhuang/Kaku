package web

import (
	"errors"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/secure"
)

// Admin accounts. Every admin has full access; adding, removing and
// resetting another admin needs the current admin's password (and code).

func (s *Server) adminUserRoutes(a *gin.RouterGroup) {
	a.GET("/admins", s.adminsPage)
	a.POST("/admins", s.createAdmin)
	a.POST("/admins/:id/password", s.resetAdminPassword)
	a.POST("/admins/:id/delete", s.deleteAdmin)
}

// recordLogin notes a successful sign-in on the account and in the audit log.
func (s *Server) recordLogin(a *model.Admin, method, detail, ip string) {
	now := s.now()
	s.st.DB.Model(&model.Admin{}).Where("id = ?", a.ID).Updates(map[string]any{"last_login": now, "last_ip": ip})
	s.st.Audit(a.Username, "login", method, detail, ip)
}

type adminRow struct {
	model.Admin
	Identities []model.AdminIdentity
	Self       bool
}

func (s *Server) adminsPage(c *gin.Context) {
	me := admin(c)
	var admins []model.Admin
	s.st.DB.Order("id").Find(&admins)
	var idents []model.AdminIdentity
	s.st.DB.Order("id").Find(&idents)
	rows := make([]adminRow, len(admins))
	for i, a := range admins {
		rows[i] = adminRow{Admin: a, Self: a.ID == me.ID}
		for _, id := range idents {
			if id.AdminID == a.ID {
				rows[i].Identities = append(rows[i].Identities, id)
			}
		}
	}
	s.render(c, 200, "admin_admins", gin.H{"Admins": rows, "Me": me})
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{2,64}$`)

func (s *Server) createAdmin(c *gin.Context) {
	back := s.cfg.AdminPath + "/admins"
	if err := s.reauth(c); err != nil {
		redirectErr(c, back, err)
		return
	}
	name, pw := strings.TrimSpace(c.PostForm("username")), c.PostForm("password")
	switch {
	case !usernameRe.MatchString(name):
		redirectErr(c, back, errors.New("用户名为 2–64 位字母、数字或 _ . @ -"))
		return
	case len(pw) < 12:
		redirectErr(c, back, errors.New("初始密码至少 12 位"))
		return
	}
	var n int64
	s.st.DB.Model(&model.Admin{}).Where("username = ?", name).Count(&n)
	if n > 0 {
		redirectErr(c, back, errors.New("用户名已存在"))
		return
	}
	if err := s.st.DB.Create(&model.Admin{Username: name, PasswordHash: secure.HashPassword(pw), Epoch: 1}).Error; err != nil {
		redirectErr(c, back, err)
		return
	}
	s.audit(c, "admin_create", name, "")
	redirectMsg(c, back, "已添加管理员 "+name+"，请把初始密码安全地交给对方，登录后在「安全」页面修改")
}

func (s *Server) targetAdmin(c *gin.Context) (*model.Admin, error) {
	var a model.Admin
	if s.st.DB.Where("id = ?", c.Param("id")).Take(&a).Error != nil {
		return nil, errors.New("管理员不存在")
	}
	return &a, nil
}

func (s *Server) resetAdminPassword(c *gin.Context) {
	back := s.cfg.AdminPath + "/admins"
	if err := s.reauth(c); err != nil {
		redirectErr(c, back, err)
		return
	}
	t, err := s.targetAdmin(c)
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	pw := c.PostForm("password")
	if len(pw) < 12 {
		redirectErr(c, back, errors.New("新密码至少 12 位"))
		return
	}
	// a new password signs the account out everywhere; two-step verification stays as it was
	s.st.DB.Model(t).Updates(map[string]any{"password_hash": secure.HashPassword(pw), "epoch": t.Epoch + 1})
	if t.ID == admin(c).ID {
		t.Epoch++
		s.setSession(c, t)
	}
	s.audit(c, "admin_password_reset", t.Username, "")
	redirectMsg(c, back, "已重置 "+t.Username+" 的密码，其已登录的会话全部退出")
}

func (s *Server) deleteAdmin(c *gin.Context) {
	back := s.cfg.AdminPath + "/admins"
	if err := s.reauth(c); err != nil {
		redirectErr(c, back, err)
		return
	}
	t, err := s.targetAdmin(c)
	if err != nil {
		redirectErr(c, back, err)
		return
	}
	if t.ID == admin(c).ID {
		redirectErr(c, back, errors.New("不能删除自己"))
		return
	}
	var n int64
	s.st.DB.Model(&model.Admin{}).Count(&n)
	if n <= 1 {
		redirectErr(c, back, errors.New("至少保留一个管理员"))
		return
	}
	s.st.DB.Where("admin_id = ?", t.ID).Delete(&model.AdminIdentity{})
	s.st.DB.Delete(t)
	s.audit(c, "admin_delete", t.Username, "")
	redirectMsg(c, back, "已删除管理员 "+t.Username)
}

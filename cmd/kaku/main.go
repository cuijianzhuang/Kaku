// Command kaku is a self-hosted shop for virtual goods.
//
//	kaku serve          run the shop (default)
//	kaku keygen         print a new KAKU_MASTER_KEY
//	kaku reset-admin    reset the admin password to ADMIN_PASSWORD and turn off 2FA
//	kaku backup FILE    copy the SQLite database (safe while running)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cuijianzhuang/kaku/internal/model"
	"github.com/cuijianzhuang/kaku/internal/notify"
	"github.com/cuijianzhuang/kaku/internal/secure"
	"github.com/cuijianzhuang/kaku/internal/shop"
	"github.com/cuijianzhuang/kaku/internal/store"
	"github.com/cuijianzhuang/kaku/internal/tgbot"
	"github.com/cuijianzhuang/kaku/internal/update"
	"github.com/cuijianzhuang/kaku/internal/web"
)

// Version is set at build time.
var Version = "dev"

const usage = `用法：kaku <命令>

  serve        启动商店（默认）
  keygen       生成新的主密钥 KAKU_MASTER_KEY
  reset-admin  将管理员密码重置为 ADMIN_PASSWORD，并关闭两步验证
  backup FILE  在线备份 SQLite 数据库到 FILE（PostgreSQL 请用 pg_dump）
  version      显示版本

环境变量：
  KAKU_MASTER_KEY  必填，32 字节主密钥（kaku keygen 生成）。加密卡密、发货内容和支付密钥，丢失后无法解密
  ADMIN_USER       首次启动创建的管理员用户名，默认 admin
  ADMIN_PASSWORD   首次启动创建的管理员密码（至少 12 位）
  ADMIN_PATH       后台路径，默认 /admin；建议改成不易猜到的路径
  LISTEN           监听地址，默认 :8080
  DATA_DIR         数据目录，默认 data（SQLite 数据库在此）
  DB_DRIVER        sqlite（默认）或 postgres
  DB_DSN           数据库连接串；sqlite 默认 $DATA_DIR/kaku.db
  TRUST_PROXY=1    在反向代理之后时，从 X-Forwarded-For 取客户端地址
  SECURE_COOKIE=1  Cookie 只走 HTTPS（HTTPS 部署时开启）`

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "keygen":
		fmt.Println(secure.NewMasterKey())
	case "reset-admin":
		err = resetAdmin()
	case "backup":
		err = backup(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("kaku", Version)
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		err = fmt.Errorf("未知命令 %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误：", err)
		os.Exit(1)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func openStore() (*store.Store, error) {
	key, err := secure.ParseMasterKey(os.Getenv("KAKU_MASTER_KEY"))
	if err != nil {
		return nil, fmt.Errorf("%w\n运行 kaku keygen 生成一个，并妥善备份", err)
	}
	box, err := secure.NewBox(key)
	if err != nil {
		return nil, err
	}
	driver := env("DB_DRIVER", "sqlite")
	dsn := os.Getenv("DB_DSN")
	if dsn == "" && driver == "sqlite" {
		dsn = filepath.Join(env("DATA_DIR", "data"), "kaku.db")
	}
	return store.Open(driver, dsn, box)
}

// ensureAdmin creates the first admin from ADMIN_USER / ADMIN_PASSWORD.
func ensureAdmin(st *store.Store) error {
	var n int64
	if err := st.DB.Model(&model.Admin{}).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	pw := os.Getenv("ADMIN_PASSWORD")
	if len(pw) < 12 {
		return errors.New("首次启动请设置 ADMIN_PASSWORD（至少 12 位）")
	}
	return st.DB.Create(&model.Admin{Username: env("ADMIN_USER", "admin"), PasswordHash: secure.HashPassword(pw), Epoch: 1}).Error
}

func serve() error {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := ensureAdmin(st); err != nil {
		return err
	}
	n := &notify.Notifier{Store: st, Log: log}
	svc := shop.New(st, n, log)
	svc.BackupDir = filepath.Join(env("DATA_DIR", "data"), "backups")
	svc.Updater = update.New(Version, env("DATA_DIR", "data"))
	bot := tgbot.New(svc, log)
	srv := web.New(web.Config{
		AdminPath: env("ADMIN_PATH", "/admin"), TrustProxy: os.Getenv("TRUST_PROXY") == "1",
		SecureCookie: os.Getenv("SECURE_COOKIE") == "1", Version: Version,
	}, svc, bot, log)
	hs := &http.Server{Addr: env("LISTEN", ":8080"), Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.Run(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		hs.Shutdown(shutdown)
	}()
	log.Info("kaku listening", "addr", hs.Addr, "admin", "/"+strings.Trim(env("ADMIN_PATH", "/admin"), "/"), "version", Version)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func resetAdmin() error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	pw := os.Getenv("ADMIN_PASSWORD")
	if len(pw) < 12 {
		return errors.New("请设置 ADMIN_PASSWORD（至少 12 位）")
	}
	user := env("ADMIN_USER", "admin")
	var a model.Admin
	if err := st.DB.Where("username = ?", user).Take(&a).Error; err != nil {
		a = model.Admin{Username: user, Epoch: 1}
	}
	a.PasswordHash, a.TOTPEnabled, a.TOTPSecret, a.Epoch = secure.HashPassword(pw), false, "", a.Epoch+1
	if err := st.DB.Save(&a).Error; err != nil {
		return err
	}
	fmt.Println("已重置管理员", user, "的密码，并关闭两步验证")
	return nil
}

// backup writes a consistent copy of the SQLite database.
func backup(args []string) error {
	if len(args) != 1 {
		return errors.New("用法：kaku backup 目标文件")
	}
	if d := env("DB_DRIVER", "sqlite"); d != "sqlite" {
		return fmt.Errorf("backup 只支持 SQLite；%s 请使用数据库自带的备份工具（如 pg_dump）", d)
	}
	if _, err := os.Stat(args[0]); err == nil {
		return fmt.Errorf("%s 已存在", args[0])
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.DB.Exec("VACUUM INTO ?", args[0]).Error; err != nil {
		return err
	}
	fmt.Println("已备份到", args[0])
	return nil
}

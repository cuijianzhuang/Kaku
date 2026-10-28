package web

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/kaku/internal/secure"
)

// Uploaded images (product covers, the shop logo) live in Config.UploadDir
// and are served from /uploads/. Only PNG, JPEG, GIF and WebP are kept,
// judged by their content rather than the file name, and stored under a
// random name, so an upload can never be served as a page or a script.

const maxUpload = 5 << 20

var imageExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

var uploadName = regexp.MustCompile(`^[A-Za-z0-9_-]{8,40}\.(png|jpg|gif|webp)$`)

var errNotImage = errors.New("只能上传 PNG、JPG、GIF 或 WebP 图片（不超过 5 MB）")

// saveUpload stores the image in the form field and returns its URL path
// ("" when the field is empty).
func (s *Server) saveUpload(c *gin.Context, field string) (string, error) {
	fh, err := c.FormFile(field)
	if errors.Is(err, http.ErrMissingFile) || errors.Is(err, http.ErrNotMultipart) || (err == nil && fh.Size == 0) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if s.cfg.UploadDir == "" {
		return "", errors.New("未配置上传目录")
	}
	if fh.Size > maxUpload {
		return "", errNotImage
	}
	f, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxUpload+1))
	if err != nil {
		return "", err
	}
	ext, ok := imageExt[http.DetectContentType(data)]
	if !ok || len(data) > maxUpload {
		return "", errNotImage
	}
	if err := os.MkdirAll(s.cfg.UploadDir, 0o755); err != nil {
		return "", err
	}
	name := secure.RandomToken(12) + ext
	if err := os.WriteFile(filepath.Join(s.cfg.UploadDir, name), data, 0o644); err != nil {
		return "", err
	}
	return "/uploads/" + name, nil
}

func (s *Server) serveUpload(c *gin.Context) {
	name := c.Param("name")
	if s.cfg.UploadDir == "" || !uploadName.MatchString(name) {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.File(filepath.Join(s.cfg.UploadDir, name))
}

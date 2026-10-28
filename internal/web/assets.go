package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// Static files are read once from the embedded FS. Pages link them with a
// content hash (?v=…), so browsers may keep them for a year and still pick
// up a new release at once. Text files are kept gzipped as well.

type asset struct {
	data, gz []byte
	ctype    string
	etag     string
}

func loadAssets() map[string]*asset {
	out := map[string]*asset{}
	sub, _ := fs.Sub(assets, "static")
	fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		a := &asset{data: data, etag: hex.EncodeToString(sum[:])[:12], ctype: mime.TypeByExtension(path.Ext(p))}
		if a.ctype == "" {
			a.ctype = "application/octet-stream"
		}
		if compressible(a.ctype) {
			var b bytes.Buffer
			w, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
			w.Write(data)
			w.Close()
			if b.Len() < len(data) {
				a.gz = b.Bytes()
			}
		}
		out[p] = a
		return nil
	})
	return out
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript") || strings.Contains(ctype, "json") ||
		strings.Contains(ctype, "xml") || strings.Contains(ctype, "svg")
}

func acceptsGzip(c *gin.Context) bool {
	return strings.Contains(c.GetHeader("Accept-Encoding"), "gzip")
}

// assetURL is the versioned link to a static file.
func (s *Server) assetURL(name string) string {
	if a := s.assets[name]; a != nil {
		return "/static/" + name + "?v=" + a.etag
	}
	return "/static/" + name
}

func (s *Server) serveAsset(c *gin.Context) {
	a := s.assets[strings.TrimPrefix(c.Param("filepath"), "/")]
	if a == nil {
		c.Status(http.StatusNotFound)
		return
	}
	h := c.Writer.Header()
	etag := `"` + a.etag + `"`
	h.Set("ETag", etag)
	h.Set("Vary", "Accept-Encoding")
	if c.Query("v") == a.etag {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=3600")
	}
	if c.GetHeader("If-None-Match") == etag {
		c.Status(http.StatusNotModified)
		return
	}
	body := a.data
	if a.gz != nil && acceptsGzip(c) {
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	c.Data(http.StatusOK, a.ctype, body)
}

// writePage sends a rendered page, gzipped when the browser accepts it.
func writePage(c *gin.Context, code int, ctype string, body []byte) {
	c.Header("Vary", "Accept-Encoding")
	if len(body) > 1024 && acceptsGzip(c) {
		var b bytes.Buffer
		w, _ := gzip.NewWriterLevel(&b, gzip.DefaultCompression)
		w.Write(body)
		w.Close()
		c.Header("Content-Encoding", "gzip")
		body = b.Bytes()
	}
	c.Data(code, ctype, body)
}

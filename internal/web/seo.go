package web

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// robots.txt keeps search engines on the catalogue: order, payment and
// lookup pages are private, and the admin path is not listed (listing it
// would reveal it; admin pages carry noindex instead).
func (s *Server) robots(c *gin.Context) {
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	for _, p := range []string{"/order/", "/pay/", "/query", "/ticket", "/renew", "/return/", "/notify/", "/hooks/", "/tg/"} {
		b.WriteString("Disallow: " + p + "\n")
	}
	b.WriteString("\nSitemap: " + s.publicURL(c) + "/sitemap.xml\n")
	c.Header("Cache-Control", "public, max-age=3600")
	c.String(http.StatusOK, b.String())
}

type sitemapURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type sitemapSet struct {
	XMLName xml.Name     `xml:"urlset"`
	NS      string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

func (s *Server) sitemap(c *gin.Context) {
	base := s.publicURL(c)
	set := sitemapSet{NS: "http://www.sitemaps.org/schemas/sitemap/0.9", URLs: []sitemapURL{{Loc: base + "/"}}}
	cats, err := s.shop.Catalog()
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	for _, cat := range cats {
		for _, p := range cat.Products {
			set.URLs = append(set.URLs, sitemapURL{Loc: fmt.Sprintf("%s/p/%d", base, p.ID), LastMod: p.UpdatedAt.Format("2006-01-02")})
		}
	}
	out, _ := xml.MarshalIndent(set, "", "  ")
	c.Header("Cache-Control", "public, max-age=3600")
	c.Data(http.StatusOK, "application/xml; charset=utf-8", append([]byte(xml.Header), out...))
}

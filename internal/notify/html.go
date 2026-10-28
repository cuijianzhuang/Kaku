package notify

import (
	"encoding/base64"
	"html"
	"regexp"
	"strings"
)

// Emails go out as multipart/alternative: the plain text as written, and an
// HTML version laid out from it (paragraphs, links, numbered items such as
// card keys in a monospace box) inside a simple branded card. Mail clients
// that cannot show HTML fall back to the text.

var (
	urlRe  = regexp.MustCompile(`https?://[^\s<>"（）()，。、；]+`)
	itemRe = regexp.MustCompile(`^\d+\.\s`)
	kvRe   = regexp.MustCompile(`^([^：:\s]{1,8})[：:](.+)$`)
)

func mailHTML(site, subject, body string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"></head>`)
	b.WriteString(`<body style="margin:0;padding:24px 12px;background:#f4f4f5;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;color:#18181b">`)
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0"><tr><td align="center">`)
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#ffffff;border:1px solid #e4e4e7;border-radius:12px">`)
	b.WriteString(`<tr><td style="padding:20px 28px;border-bottom:1px solid #e4e4e7;font-size:16px;font-weight:700">` + html.EscapeString(site) + `</td></tr>`)
	b.WriteString(`<tr><td style="padding:24px 28px 8px"><h1 style="margin:0 0 16px;font-size:19px;line-height:1.4">` + html.EscapeString(stripSite(subject)) + `</h1>`)
	for _, para := range strings.Split(strings.TrimSpace(body), "\n\n") {
		lines := strings.Split(para, "\n")
		var items, kv, text []string
		for _, l := range lines {
			l = strings.TrimSpace(l)
			switch {
			case l == "":
			case itemRe.MatchString(l):
				items = append(items, `<div style="padding:4px 0">`+linkify(l)+`</div>`)
			case kvRe.MatchString(l) && !urlRe.MatchString(l):
				m := kvRe.FindStringSubmatch(l)
				kv = append(kv, `<tr><td style="padding:4px 16px 4px 0;color:#71717a;white-space:nowrap;vertical-align:top">`+html.EscapeString(m[1])+
					`</td><td style="padding:4px 0">`+html.EscapeString(strings.TrimSpace(m[2]))+`</td></tr>`)
			default:
				text = append(text, linkify(l))
			}
		}
		if len(text) > 0 {
			b.WriteString(`<p style="margin:0 0 16px;font-size:15px;line-height:1.7">` + strings.Join(text, "<br>") + `</p>`)
		}
		if len(kv) > 0 {
			b.WriteString(`<table role="presentation" cellpadding="0" cellspacing="0" style="margin:0 0 16px;font-size:14px;line-height:1.6">` + strings.Join(kv, "") + `</table>`)
		}
		if len(items) > 0 {
			b.WriteString(`<div style="margin:0 0 16px;padding:12px 16px;background:#f4f4f5;border-radius:8px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:14px;word-break:break-all">` + strings.Join(items, "") + `</div>`)
		}
	}
	b.WriteString(`</td></tr><tr><td style="padding:16px 28px;border-top:1px solid #e4e4e7;font-size:12px;color:#a1a1aa">此邮件由 ` + html.EscapeString(site) + ` 自动发送，请勿直接回复。</td></tr>`)
	b.WriteString(`</table></td></tr></table></body></html>`)
	return b.String()
}

// linkify escapes a line and turns URLs into links.
func linkify(l string) string {
	var b strings.Builder
	last := 0
	for _, m := range urlRe.FindAllStringIndex(l, -1) {
		b.WriteString(html.EscapeString(l[last:m[0]]))
		u := html.EscapeString(l[m[0]:m[1]])
		b.WriteString(`<a href="` + u + `" style="color:#2563eb">` + u + `</a>`)
		last = m[1]
	}
	b.WriteString(html.EscapeString(l[last:]))
	return b.String()
}

// stripSite drops a leading 【site】 from the subject for the heading.
func stripSite(subject string) string {
	if strings.HasPrefix(subject, "【") {
		if i := strings.Index(subject, "】"); i > 0 {
			return subject[i+len("】"):]
		}
	}
	return subject
}

// b64 wraps base64 at 76 columns, as MIME asks.
func b64(s string) string {
	enc := base64.StdEncoding.EncodeToString([]byte(s))
	var b strings.Builder
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc)
	return b.String()
}

func decodeB64(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.NewReplacer("\r", "", "\n", "").Replace(s))
	return string(b), err
}

package web

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// renderMarkdown renders the small Markdown subset product descriptions
// need: headings, bullet lists, paragraphs, **bold**, `code` and
// [links](https://…). Everything is HTML-escaped first, so admin-entered
// text cannot inject markup.
func renderMarkdown(src string) template.HTML {
	var b strings.Builder
	inList := false
	para := []string{}
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
			para = para[:0]
		}
	}
	closeList := func() {
		if inList {
			b.WriteString("</ul>")
			inList = false
		}
	}
	for _, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		line := strings.TrimRight(raw, " \t")
		trim := strings.TrimSpace(line)
		switch {
		case trim == "":
			flush()
			closeList()
		case strings.HasPrefix(trim, "### "), strings.HasPrefix(trim, "## "), strings.HasPrefix(trim, "# "):
			flush()
			closeList()
			level := strings.Index(trim, " ")
			tag := []string{"", "h3", "h4", "h5"}[level]
			b.WriteString("<" + tag + ">" + inline(trim[level+1:]) + "</" + tag + ">")
		case strings.HasPrefix(trim, "- "), strings.HasPrefix(trim, "* "):
			flush()
			if !inList {
				b.WriteString("<ul>")
				inList = true
			}
			b.WriteString("<li>" + inline(trim[2:]) + "</li>")
		default:
			closeList()
			para = append(para, inline(trim))
		}
	}
	flush()
	closeList()
	return template.HTML(b.String())
}

var (
	boldRE = regexp.MustCompile(`\*\*(.+?)\*\*`)
	codeRE = regexp.MustCompile("`([^`]+)`")
	linkRE = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^\s)]+)\)`)
)

func inline(s string) string {
	s = html.EscapeString(s)
	s = codeRE.ReplaceAllString(s, "<code>$1</code>")
	s = boldRE.ReplaceAllString(s, "<strong>$1</strong>")
	s = linkRE.ReplaceAllString(s, `<a href="$2" rel="noopener nofollow" target="_blank">$1</a>`)
	return s
}

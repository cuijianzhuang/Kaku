package web

import (
	"html"
	"html/template"
	"regexp"
	"strings"
)

// renderMarkdown renders the small Markdown subset product descriptions
// need: headings, bullet and numbered lists, > quotes, paragraphs,
// **bold**, `code`, [links](https://…) and images ![](/uploads/… or
// https://…). Everything is HTML-escaped first, so admin-entered text
// cannot inject markup.
func renderMarkdown(src string) template.HTML {
	var b strings.Builder
	list := "" // "ul" or "ol" while inside a list
	quote := []string{}
	para := []string{}
	flush := func() {
		if len(para) > 0 {
			b.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
			para = para[:0]
		}
		if len(quote) > 0 {
			b.WriteString("<blockquote>" + strings.Join(quote, "<br>") + "</blockquote>")
			quote = quote[:0]
		}
	}
	closeList := func() {
		if list != "" {
			b.WriteString("</" + list + ">")
			list = ""
		}
	}
	openList := func(tag string) {
		if list != tag {
			closeList()
			b.WriteString("<" + tag + ">")
			list = tag
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
			openList("ul")
			b.WriteString("<li>" + inline(trim[2:]) + "</li>")
		case olRE.MatchString(trim):
			flush()
			openList("ol")
			b.WriteString("<li>" + inline(olRE.ReplaceAllString(trim, "")) + "</li>")
		case strings.HasPrefix(trim, ">"):
			if len(para) > 0 {
				flush()
			}
			closeList()
			quote = append(quote, inline(strings.TrimSpace(trim[1:])))
		default:
			closeList()
			if len(quote) > 0 {
				flush()
			}
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
	// images: only uploads on this site or https addresses
	imageRE = regexp.MustCompile(`!\[([^\]]*)\]\(((?:https://|/uploads/)[^\s)]+)\)`)
	olRE    = regexp.MustCompile(`^\d{1,3}[.、)] `)
)

func inline(s string) string {
	s = html.EscapeString(s)
	s = imageRE.ReplaceAllString(s, `<img src="$2" alt="$1" loading="lazy">`)
	s = codeRE.ReplaceAllString(s, "<code>$1</code>")
	s = boldRE.ReplaceAllString(s, "<strong>$1</strong>")
	s = linkRE.ReplaceAllString(s, `<a href="$2" rel="noopener nofollow" target="_blank">$1</a>`)
	return s
}

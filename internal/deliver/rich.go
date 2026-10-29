package deliver

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// Fixed content comes in a few shapes, so the buyer gets something better
// than a block of text: a net-disk share (link, extraction code, unzip
// password), a link with a button, or an article read on the order page.

// Text formats.
const (
	FormatText    = ""        // plain text, as typed
	FormatNetdisk = "netdisk" // 网盘：链接 + 提取码 + 解压密码
	FormatLink    = "link"    // 链接：按钮打开
	FormatArticle = "article" // 文章：Markdown，在订单页阅读
)

// Rich is delivered structured content. It is stored as JSON in the order
// item; Plain is what email, Telegram and copy buttons use.
type Rich struct {
	Kind   string `json:"kind"`
	URL    string `json:"url,omitempty"`
	Code   string `json:"code,omitempty"`   // 提取码
	Unzip  string `json:"unzip,omitempty"`  // 解压密码
	Button string `json:"button,omitempty"` // link button label
	Body   string `json:"body,omitempty"`   // note, or the article (Markdown)
}

// Provider names the net-disk service of a share link.
func (r *Rich) Provider() string { return NetdiskName(r.URL) }

// Plain is the content as text.
func (r *Rich) Plain() string {
	var b strings.Builder
	switch r.Kind {
	case FormatNetdisk:
		name := r.Provider()
		if name == "" {
			name = "网盘"
		}
		b.WriteString(name + "链接：" + r.URL)
		if r.Code != "" {
			b.WriteString("\n提取码：" + r.Code)
		}
		if r.Unzip != "" {
			b.WriteString("\n解压密码：" + r.Unzip)
		}
	case FormatLink:
		label := r.Button
		if label == "" {
			label = "链接"
		}
		b.WriteString(label + "：" + r.URL)
	case FormatArticle:
		return r.Body
	}
	if r.Body != "" {
		b.WriteString("\n\n" + r.Body)
	}
	return b.String()
}

// ShareText is a net-disk share the way the services write it, for pasting
// into the app: "链接：… 提取码：…".
func (r *Rich) ShareText() string {
	s := "链接：" + r.URL
	if r.Code != "" {
		s += " 提取码：" + r.Code
	}
	return s
}

// ParseRich reads stored structured content; ok is false for plain text.
func ParseRich(source, content string) (*Rich, bool) {
	switch source {
	case FormatNetdisk, FormatLink, FormatArticle:
	default:
		return nil, false
	}
	var r Rich
	if err := json.Unmarshal([]byte(content), &r); err != nil || r.Kind != source {
		return nil, false
	}
	return &r, true
}

var netdisks = []struct{ host, name string }{
	{"pan.baidu.com", "百度网盘"}, {"yun.baidu.com", "百度网盘"}, {"alipan.com", "阿里云盘"}, {"aliyundrive.com", "阿里云盘"},
	{"pan.quark.cn", "夸克网盘"}, {"123pan.com", "123 云盘"}, {"123684.com", "123 云盘"}, {"123912.com", "123 云盘"},
	{"lanzou", "蓝奏云"}, {"ilanzou.com", "蓝奏云"}, {"cloud.189.cn", "天翼云盘"}, {"caiyun.139.com", "移动云盘"},
	{"yun.139.com", "移动云盘"}, {"pan.xunlei.com", "迅雷云盘"}, {"115.com", "115 网盘"}, {"115cdn.com", "115 网盘"},
	{"drive.uc.cn", "UC 网盘"}, {"drive.google.com", "Google Drive"}, {"onedrive.live.com", "OneDrive"}, {"1drv.ms", "OneDrive"},
	{"sharepoint.com", "OneDrive"}, {"mega.nz", "MEGA"}, {"dropbox.com", "Dropbox"}, {"pan.wo.cn", "联通云盘"},
}

// NetdiskName recognises common net-disk share links.
func NetdiskName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	for _, n := range netdisks {
		if host == n.host || strings.HasSuffix(host, "."+n.host) || (!strings.Contains(n.host, ".") && strings.Contains(host, n.host)) {
			return n.name
		}
	}
	return ""
}

var (
	firstURL = regexp.MustCompile(`https?://[^\s"'<>，。；]+`)
	codeRe   = regexp.MustCompile(`(?:提取码|提取碼|访问码|訪問碼|密码|密碼|pwd|code)\s*[:：=]?\s*([A-Za-z0-9]{3,8})`)
)

// SplitShare pulls the link and extraction code out of a pasted share text
// such as "链接: https://pan.baidu.com/s/1abc 提取码: x7k2 复制这段内容…".
// A code already given wins.
func SplitShare(text, code string) (link, outCode string) {
	text = strings.TrimSpace(text)
	link = text
	if m := firstURL.FindString(text); m != "" {
		link = m
	}
	outCode = strings.TrimSpace(code)
	if outCode == "" {
		if m := codeRe.FindStringSubmatch(text); m != nil {
			outCode = m[1]
		} else if u, err := url.Parse(link); err == nil {
			outCode = u.Query().Get("pwd")
		}
	}
	return link, outCode
}

func validLink(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && !strings.ContainsAny(raw, " \n\"<>")
}

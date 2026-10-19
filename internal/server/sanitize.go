package server

import (
	"bytes"
	"strings"

	"golang.org/x/net/html"
)

// README HTML comes from GitHub's renderer, which already sanitises it.
// It's cleaned again here with an allow-list (defence in depth: it is
// shown inside this app's pages), and relative links are pointed at
// GitHub so images and links work.

var allowedTags = map[string]bool{
	"a": true, "abbr": true, "b": true, "blockquote": true, "br": true, "code": true, "dd": true, "del": true,
	"details": true, "div": true, "dl": true, "dt": true, "em": true, "h1": true, "h2": true, "h3": true, "h4": true,
	"h5": true, "h6": true, "hr": true, "i": true, "img": true, "ins": true, "kbd": true, "li": true, "ol": true,
	"p": true, "picture": true, "pre": true, "q": true, "s": true, "samp": true, "source": true, "span": true,
	"strike": true, "strong": true, "sub": true, "summary": true, "sup": true, "table": true, "tbody": true,
	"td": true, "tfoot": true, "th": true, "thead": true, "tr": true, "tt": true, "u": true, "ul": true, "article": true,
	"section": true, "g-emoji": true, "figure": true, "figcaption": true, "mark": true, "small": true,
}

var allowedAttrs = map[string]bool{
	"href": true, "src": true, "srcset": true, "alt": true, "title": true, "width": true, "height": true,
	"align": true, "colspan": true, "rowspan": true, "open": true, "id": true, "start": true, "media": true, "type": true,
}

// dropWithContent are removed along with everything inside them.
var dropWithContent = map[string]bool{"script": true, "style": true, "iframe": true, "object": true, "embed": true,
	"form": true, "input": true, "button": true, "textarea": true, "select": true, "svg": true, "math": true, "template": true, "noscript": true}

func sanitizeReadme(raw, full, branch string, images bool) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return ""
	}
	var buf bytes.Buffer
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			buf.WriteString(html.EscapeString(n.Data))
			return
		case html.ElementNode:
			tag := n.Data
			if dropWithContent[tag] {
				return
			}
			if tag == "img" && !images {
				return
			}
			if !allowedTags[tag] {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				return
			}
			buf.WriteString("<" + tag)
			for _, a := range n.Attr {
				key := strings.ToLower(a.Key)
				if !allowedAttrs[key] {
					continue
				}
				val := a.Val
				switch key {
				case "href":
					if !safeURL(val) {
						continue
					}
					val = absoluteRepoURL(val, full, branch, false)
				case "src":
					if !safeURL(val) {
						continue
					}
					val = absoluteRepoURL(val, full, branch, true)
				case "srcset":
					val = fixSrcset(val, full, branch)
				case "id":
					val = "readme-" + val
				}
				buf.WriteString(" " + key + `="` + html.EscapeString(val) + `"`)
			}
			if tag == "a" {
				buf.WriteString(` target="_blank" rel="noopener noreferrer nofollow"`)
			}
			if tag == "img" {
				buf.WriteString(` loading="lazy" referrerpolicy="no-referrer"`)
			}
			buf.WriteString(">")
			if isVoid(tag) {
				return
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			buf.WriteString("</" + tag + ">")
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return buf.String()
}

func isVoid(tag string) bool {
	switch tag {
	case "br", "hr", "img", "source":
		return true
	}
	return false
}

func safeURL(v string) bool {
	l := strings.ToLower(strings.TrimSpace(v))
	if i := strings.Index(l, ":"); i >= 0 && !strings.ContainsAny(l[:i], "/?#") {
		scheme := l[:i]
		return scheme == "http" || scheme == "https" || scheme == "mailto"
	}
	return true // relative
}

func fixSrcset(v, full, branch string) string {
	parts := strings.Split(v, ",")
	var out []string
	for _, p := range parts {
		f := strings.Fields(strings.TrimSpace(p))
		if len(f) == 0 || !safeURL(f[0]) {
			continue
		}
		f[0] = absoluteRepoURL(f[0], full, branch, true)
		out = append(out, strings.Join(f, " "))
	}
	return strings.Join(out, ", ")
}

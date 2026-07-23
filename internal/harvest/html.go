package harvest

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// ExtractHTML parses an HTML document and returns every candidate URL it
// references (spec §3) — resolved to absolute against baseURL — plus the
// subset that are script[src] bundles, which additionally feed the JS
// harvester (spec §4). Extracted from a[href], link[href], script[src],
// img[src], form[action], and data-* attributes that look URL-ish.
// Malformed HTML is tolerated (x/net/html is lenient, matching browser
// behavior); a body that fails to parse at all yields nil, nil rather than
// erroring the caller's harvest goroutine.
func ExtractHTML(body []byte, baseURL string) (links []string, scripts []string) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, nil
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, nil
	}

	seen := make(map[string]bool)
	resolve := func(raw string) string {
		raw = strings.TrimSpace(raw)
		if raw == "" || !looksNavigable(raw) {
			return ""
		}
		u, err := base.Parse(raw)
		if err != nil {
			return ""
		}
		u.Fragment = ""
		abs := u.String()
		if seen[abs] {
			return ""
		}
		seen[abs] = true
		return abs
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "a", "link":
				if v, ok := attrVal(n, "href"); ok {
					if abs := resolve(v); abs != "" {
						links = append(links, abs)
					}
				}
			case "script":
				if v, ok := attrVal(n, "src"); ok {
					if abs := resolve(v); abs != "" {
						links = append(links, abs)
						scripts = append(scripts, abs)
					}
				}
			case "img":
				if v, ok := attrVal(n, "src"); ok {
					if abs := resolve(v); abs != "" {
						links = append(links, abs)
					}
				}
			case "form":
				if v, ok := attrVal(n, "action"); ok {
					if abs := resolve(v); abs != "" {
						links = append(links, abs)
					}
				}
			}
			for _, a := range n.Attr {
				if strings.HasPrefix(a.Key, "data-") && looksURLish(a.Val) {
					if abs := resolve(a.Val); abs != "" {
						links = append(links, abs)
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return links, scripts
}

func attrVal(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// looksNavigable excludes href/src/action values that are never a same-app
// path worth seeding: anchors, script pseudo-URLs, inline data URIs, and
// contact-protocol links.
func looksNavigable(v string) bool {
	switch {
	case strings.HasPrefix(v, "#"),
		strings.HasPrefix(v, "javascript:"),
		strings.HasPrefix(v, "data:"),
		strings.HasPrefix(v, "mailto:"),
		strings.HasPrefix(v, "tel:"):
		return false
	default:
		return true
	}
}

func looksURLish(v string) bool {
	return strings.HasPrefix(v, "/") || strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://")
}

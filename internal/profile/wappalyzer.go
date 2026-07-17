package profile

import (
	"net/http"
	"strings"

	wappalyzer "github.com/projectdiscovery/wappalyzergo"
)

// WappalyzerConfidence is the fixed confidence applied to every
// wappalyzergo match (spec §4.5): it provides breadth, not the
// pentest-specific depth of §4.1-4.4.
const WappalyzerConfidence = 0.8

// Wappalyzer wraps wappalyzergo's bundled-fingerprint client.
type Wappalyzer struct {
	client *wappalyzer.Wappalyze
}

// NewWappalyzer loads wappalyzergo's embedded fingerprint database.
func NewWappalyzer() (*Wappalyzer, error) {
	c, err := wappalyzer.New()
	if err != nil {
		return nil, err
	}
	return &Wappalyzer{client: c}, nil
}

// applyWappalyzerSignal implements spec §4.5. w may be nil (e.g. if
// wappalyzergo's fingerprint DB failed to load); a nil w casts no votes
// rather than failing the whole profile.
func applyWappalyzerSignal(p *TargetProfile, w *Wappalyzer, headers http.Header, body []byte) {
	if w == nil {
		return
	}
	for key, info := range w.client.FingerprintWithInfo(headers, body) {
		// wappalyzergo keys a versioned match as "App:version" (see its
		// FormatAppVersion); split it back apart so "Nginx:1.18.0" and a
		// later "Nginx:1.19.0" both fuse into one "Nginx" Tech instead of
		// each becoming its own distinct, unmatchable entry.
		name, version := key, ""
		if i := strings.LastIndex(key, ":"); i >= 0 {
			name, version = key[:i], key[i+1:]
		}
		p.vote(name, mapWappalyzerCategory(info.Categories), LayerUnknown, SrcWappalyzer, WappalyzerConfidence, version, "wappalyzergo")
	}
}

// wappalyzerCategoryMap maps substrings of wappalyzergo's category names to
// smartbuster's category vocabulary (spec §4.5: "map its categories to
// ours"). Checked in order; first match wins.
var wappalyzerCategoryMap = []struct {
	keyword string
	our     string
}{
	{"web server", "server"},
	{"reverse proxy", "proxy"},
	{"cdn", "proxy"},
	{"programming language", "language"},
	{"cms", "cms"},
	{"framework", "framework"},
	{"security", "waf"},
	{"firewall", "waf"},
}

func mapWappalyzerCategory(categories []string) string {
	for _, cat := range categories {
		lower := strings.ToLower(cat)
		for _, m := range wappalyzerCategoryMap {
			if strings.Contains(lower, m.keyword) {
				return m.our
			}
		}
	}
	if len(categories) > 0 {
		return strings.ToLower(categories[0])
	}
	return ""
}

package profile

import (
	"net/http"
	"regexp"
	"strings"
)

func layerFromString(s string) Layer {
	switch strings.ToLower(s) {
	case "backend":
		return LayerBackend
	case "edge":
		return LayerEdge
	default:
		return LayerUnknown
	}
}

// applyHeaderSignals implements spec §4.1.
func applyHeaderSignals(p *TargetProfile, rs *Ruleset, headers http.Header) {
	for _, r := range rs.Headers {
		v := headers.Get(r.Header)
		if v == "" {
			continue
		}
		if r.Pattern != "" {
			re, err := regexp.Compile(r.Pattern)
			if err != nil || !re.MatchString(v) {
				continue
			}
		}
		p.vote(r.Tech, r.Category, layerFromString(r.Layer), SrcHeader, r.Confidence, "", r.ID)
	}
}

// applyCookieSignals implements spec §4.2. A rule's Cookie matches a
// response cookie whose name equals it or is prefixed by it (e.g. rule
// "wordpress_" matches observed cookie "wordpress_logged_in_abc123").
func applyCookieSignals(p *TargetProfile, rs *Ruleset, cookies []*http.Cookie) {
	for _, r := range rs.Cookies {
		for _, c := range cookies {
			if c.Name == r.Cookie || strings.HasPrefix(c.Name, r.Cookie) {
				p.vote(r.Tech, r.Category, layerFromString(r.Layer), SrcCookie, r.Confidence, "", r.ID)
				break
			}
		}
	}
}

// applyHTMLSignals implements spec §4.3 over the lowercased profile-fetch
// body: meta generator tags, script/link path patterns, comment markers.
func applyHTMLSignals(p *TargetProfile, rs *Ruleset, body []byte) {
	lower := strings.ToLower(string(body))
	for _, r := range rs.HTML {
		re, err := regexp.Compile(r.Pattern)
		if err != nil || !re.MatchString(lower) {
			continue
		}
		p.vote(r.Tech, r.Category, layerFromString(r.Layer), SrcHTML, r.Confidence, "", r.ID)
	}
}

// ApplyErrorPageSignal implements spec §4.6: match calibration's normalized
// representative not-found body against known framework error templates.
// Consumes calibration's own output — no extra request (see the
// ResponseSignature.NormBody / Baseline.RepBody deviation noted in the
// Phase 2a handoff report).
func ApplyErrorPageSignal(p *TargetProfile, rs *Ruleset, repBody string, repStatus int) {
	if repBody == "" {
		return
	}
	lower := strings.ToLower(repBody)
	for _, r := range rs.ErrorPages {
		if r.Status != 0 && r.Status != repStatus {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil || !re.MatchString(lower) {
			continue
		}
		p.vote(r.Tech, r.Category, layerFromString(r.Layer), SrcErrorPage, r.Confidence, "", r.ID)
	}
}

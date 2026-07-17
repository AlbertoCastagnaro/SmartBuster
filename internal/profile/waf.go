package profile

import (
	"net/http"
	"regexp"
	"strings"
)

// detectWAF implements spec §5: a lightweight wafw00f-style signature match
// over the profile fetch's headers, cookies, and body. The first matching
// rule sets p.WAF; matching also casts a "waf" category Tech vote so WAF
// vendors show up alongside other detected tech.
func detectWAF(p *TargetProfile, rs *Ruleset, headers http.Header, cookies []*http.Cookie, body []byte) {
	for _, r := range rs.WAF {
		if !wafRuleMatches(r, headers, cookies, body) {
			continue
		}
		if p.WAF == "" {
			p.WAF = r.Vendor
		}
		p.vote(r.Vendor, "waf", LayerEdge, SrcHeader, 0.8, "", r.ID)
	}
}

func wafRuleMatches(r WAFRule, headers http.Header, cookies []*http.Cookie, body []byte) bool {
	switch {
	case r.Header != "":
		v := headers.Get(r.Header)
		if v == "" {
			return false
		}
		if r.Pattern == "" {
			return true
		}
		re, err := regexp.Compile(r.Pattern)
		return err == nil && re.MatchString(v)
	case r.Cookie != "":
		for _, c := range cookies {
			if c.Name == r.Cookie || strings.HasPrefix(c.Name, r.Cookie) {
				return true
			}
		}
		return false
	case r.BodyOnly:
		re, err := regexp.Compile("(?i)" + r.Pattern)
		return err == nil && re.Match(body)
	default:
		return false
	}
}

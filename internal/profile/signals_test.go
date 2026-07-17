package profile

import (
	"net/http"
	"testing"
)

func testRuleset() *Ruleset {
	return &Ruleset{
		Headers: []HeaderRule{
			{ID: "h1", Header: "X-Powered-By", Pattern: "(?i)php", Tech: "PHP", Category: "language", Layer: "backend", Confidence: 0.7},
		},
		Cookies: []CookieRule{
			{ID: "c1", Cookie: "wordpress_", Tech: "WordPress", Category: "cms", Layer: "backend", Confidence: 0.85},
		},
		HTML: []HTMLRule{
			{ID: "m1", Pattern: "/wp-content/", Tech: "WordPress", Category: "cms", Layer: "backend", Confidence: 0.75},
		},
		ErrorPages: []ErrorPageRule{
			{ID: "e1", Pattern: "werkzeug debugger", Tech: "Flask", Category: "framework", Layer: "backend", Confidence: 0.85},
		},
	}
}

func TestApplyHeaderSignals(t *testing.T) {
	p := newTargetProfile("t")
	h := http.Header{}
	h.Set("X-Powered-By", "PHP/8.1.2")
	applyHeaderSignals(p, testRuleset(), h)

	tech, ok := p.Tech["PHP"]
	if !ok || tech.Confidence != 0.7 || tech.Layer != LayerBackend {
		t.Fatalf("PHP not detected as expected: %+v", p.Tech)
	}
}

func TestApplyHeaderSignals_NoMatch(t *testing.T) {
	p := newTargetProfile("t")
	h := http.Header{}
	h.Set("X-Powered-By", "ASP.NET")
	applyHeaderSignals(p, testRuleset(), h)
	if _, ok := p.Tech["PHP"]; ok {
		t.Fatalf("PHP should not be detected from ASP.NET header")
	}
}

func TestApplyCookieSignals_PrefixMatch(t *testing.T) {
	p := newTargetProfile("t")
	cookies := []*http.Cookie{{Name: "wordpress_logged_in_abc123", Value: "x"}}
	applyCookieSignals(p, testRuleset(), cookies)

	if _, ok := p.Tech["WordPress"]; !ok {
		t.Fatalf("WordPress not detected from prefix-matched cookie")
	}
}

func TestApplyHTMLSignals(t *testing.T) {
	p := newTargetProfile("t")
	body := []byte(`<html><script src="/wp-content/themes/x.js"></script></html>`)
	applyHTMLSignals(p, testRuleset(), body)

	if _, ok := p.Tech["WordPress"]; !ok {
		t.Fatalf("WordPress not detected from HTML pattern")
	}
}

func TestApplyErrorPageSignal(t *testing.T) {
	p := newTargetProfile("t")
	ApplyErrorPageSignal(p, testRuleset(), "Werkzeug Debugger", 500)

	if _, ok := p.Tech["Flask"]; !ok {
		t.Fatalf("Flask not detected from error-page fingerprint")
	}
}

func TestApplyErrorPageSignal_Empty(t *testing.T) {
	p := newTargetProfile("t")
	ApplyErrorPageSignal(p, testRuleset(), "", 404)
	if len(p.Tech) != 0 {
		t.Fatalf("expected no votes from empty repBody, got %+v", p.Tech)
	}
}

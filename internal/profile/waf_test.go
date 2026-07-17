package profile

import (
	"net/http"
	"testing"
)

func TestDetectWAF_HeaderMatch(t *testing.T) {
	rs, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := newTargetProfile("t")
	h := http.Header{}
	h.Set("Server", "cloudflare")

	detectWAF(p, rs, h, nil, nil)
	if p.WAF != "Cloudflare" {
		t.Fatalf("p.WAF = %q, want Cloudflare", p.WAF)
	}
}

func TestDetectWAF_CookieMatch(t *testing.T) {
	rs, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := newTargetProfile("t")
	cookies := []*http.Cookie{{Name: "incap_ses_123_456", Value: "x"}}

	detectWAF(p, rs, http.Header{}, cookies, nil)
	if p.WAF != "Imperva Incapsula" {
		t.Fatalf("p.WAF = %q, want Imperva Incapsula", p.WAF)
	}
}

func TestDetectWAF_NoMatch(t *testing.T) {
	rs, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := newTargetProfile("t")
	detectWAF(p, rs, http.Header{}, nil, []byte("plain html"))
	if p.WAF != "" {
		t.Fatalf("p.WAF = %q, want empty", p.WAF)
	}
}

// DoD §9 assertion 2: behind_cdn separates edge and backend layers, and
// derives extensions from the backend only.
func TestBehindCDN_LayerSeparation(t *testing.T) {
	rs, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := newTargetProfile("t")
	h := http.Header{}
	h.Set("Server", "cloudflare")
	h.Set("Cf-Ray", "abc-EWR")
	cookies := []*http.Cookie{{Name: "PHPSESSID", Value: "x"}}

	applyHeaderSignals(p, rs, h)
	applyCookieSignals(p, rs, cookies)
	detectWAF(p, rs, h, cookies, nil)

	if p.WAF != "Cloudflare" {
		t.Fatalf("p.WAF = %q, want Cloudflare", p.WAF)
	}
	cf, ok := p.Tech["Cloudflare"]
	if !ok || cf.Layer != LayerEdge {
		t.Fatalf("Cloudflare tech = %+v, want LayerEdge", cf)
	}
	php, ok := p.Tech["PHP"]
	if !ok || php.Layer != LayerBackend {
		t.Fatalf("PHP tech = %+v, want LayerBackend", php)
	}

	exts := p.ExtensionsForStack()
	if !containsStr(exts, ".php") {
		t.Fatalf("ExtensionsForStack() = %v, want .php from backend PHP", exts)
	}
}

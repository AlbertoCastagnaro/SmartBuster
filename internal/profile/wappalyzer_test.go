package profile

import (
	"net/http"
	"testing"
)

func TestWappalyzer_DetectsFromBundledFingerprints(t *testing.T) {
	w, err := NewWappalyzer()
	if err != nil {
		t.Fatalf("NewWappalyzer: %v", err)
	}
	p := newTargetProfile("t")
	h := http.Header{}
	h.Set("Server", "nginx/1.18.0")
	body := []byte(`<html><head></head><body>hello</body></html>`)

	applyWappalyzerSignal(p, w, h, body)

	tech, ok := p.Tech["Nginx"]
	if !ok {
		t.Fatalf("expected wappalyzergo to detect nginx from the Server header, got %+v", p.Tech)
	}
	if tech.Confidence != WappalyzerConfidence {
		t.Fatalf("confidence = %v, want %v", tech.Confidence, WappalyzerConfidence)
	}
}

func TestApplyWappalyzerSignal_NilAdapterCastsNoVote(t *testing.T) {
	p := newTargetProfile("t")
	applyWappalyzerSignal(p, nil, http.Header{}, nil)
	if len(p.Tech) != 0 {
		t.Fatalf("expected no votes with a nil adapter, got %+v", p.Tech)
	}
}

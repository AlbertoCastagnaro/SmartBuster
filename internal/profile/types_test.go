package profile

import "testing"

// DoD §9 assertion 3: confidence fusion is noisy-OR (two 0.7 sources ~0.91),
// and provenance (rule ids) lists every voting source.
func TestVote_NoisyORFusion(t *testing.T) {
	p := newTargetProfile("example.test")
	p.vote("nginx", "server", LayerEdge, SrcHeader, 0.7, "", "rule-a")
	p.vote("nginx", "server", LayerEdge, SrcWappalyzer, 0.7, "", "rule-b")

	got := p.Tech["nginx"].Confidence
	want := 1 - (1-0.7)*(1-0.7) // 0.91
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("fused confidence = %v, want %v", got, want)
	}
	if ids := p.Tech["nginx"].RuleIDs(); len(ids) != 2 || ids[0] != "rule-a" || ids[1] != "rule-b" {
		t.Fatalf("RuleIDs() = %v, want [rule-a rule-b]", ids)
	}
}

func TestVote_ConfidenceCapsAt099(t *testing.T) {
	p := newTargetProfile("example.test")
	for i := 0; i < 5; i++ {
		p.vote("PHP", "language", LayerBackend, SrcCookie, 0.9, "", "r")
	}
	if got := p.Tech["PHP"].Confidence; got > 0.99 {
		t.Fatalf("confidence = %v, want <= 0.99", got)
	}
}

func TestVote_MajorityLayer(t *testing.T) {
	p := newTargetProfile("example.test")
	p.vote("Ambiguous", "server", LayerBackend, SrcHeader, 0.5, "", "a")
	p.vote("Ambiguous", "server", LayerEdge, SrcHeader, 0.5, "", "b")
	if got := p.Tech["Ambiguous"].Layer; got != LayerUnknown {
		t.Fatalf("tie layer = %v, want LayerUnknown", got)
	}
	p.vote("Ambiguous", "server", LayerBackend, SrcHeader, 0.5, "", "c")
	if got := p.Tech["Ambiguous"].Layer; got != LayerBackend {
		t.Fatalf("layer after tiebreak = %v, want LayerBackend", got)
	}
}

func TestExtensionsForStack_PHPBackend(t *testing.T) {
	p := newTargetProfile("example.test")
	p.vote("PHP", "language", LayerBackend, SrcCookie, 0.85, "", "ck-phpsessid")

	exts := p.ExtensionsForStack()
	for _, want := range []string{".php", ".phtml", ".php5", "", ".html", ".txt", ".json", ".bak", ".old", ".zip", ".tar.gz", ".swp", ".~"} {
		if !containsStr(exts, want) {
			t.Errorf("ExtensionsForStack() missing %q; got %v", want, exts)
		}
	}
	if containsStr(exts, ".aspx") {
		t.Errorf("ExtensionsForStack() should not include ASP.NET extensions, got %v", exts)
	}
}

// spec §4.9: edge-layer techs never contribute stack-specific extensions.
func TestExtensionsForStack_EdgeLayerExcluded(t *testing.T) {
	p := newTargetProfile("example.test")
	p.vote("PHP", "language", LayerEdge, SrcHeader, 0.7, "", "r") // hypothetically edge-voted

	exts := p.ExtensionsForStack()
	if containsStr(exts, ".php") {
		t.Errorf("edge-layer PHP should not contribute .php, got %v", exts)
	}
}

func TestMatchScore(t *testing.T) {
	p := newTargetProfile("example.test")
	p.vote("WordPress", "cms", LayerBackend, SrcCookie, 0.85, "", "r")

	if got := p.MatchScore([]string{"wordpress"}); got != 0.85 {
		t.Fatalf("MatchScore(wordpress) = %v, want 0.85", got)
	}
	if got := p.MatchScore([]string{"cms"}); got != 0.85 {
		t.Fatalf("MatchScore(cms) = %v, want 0.85", got)
	}
	if got := p.MatchScore([]string{"nonexistent"}); got != 0 {
		t.Fatalf("MatchScore(nonexistent) = %v, want 0", got)
	}
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

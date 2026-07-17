package profile

import "testing"

// DoD §9 assertion: favicon vote fires at 0.9 on an exact hash match.
func TestApplyFaviconSignal_KnownHash(t *testing.T) {
	rs, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := newTargetProfile("t")
	applyFaviconSignal(p, rs, []byte("smartbuster-fixture-favicon-known-v1"))

	tech, ok := p.Tech["SmartBusterFixtureApp"]
	if !ok {
		t.Fatalf("expected a vote for the known favicon hash, got %+v", p.Tech)
	}
	if tech.Confidence != 0.9 {
		t.Fatalf("confidence = %v, want 0.9", tech.Confidence)
	}
}

func TestApplyFaviconSignal_UnknownHashCastsNoVote(t *testing.T) {
	rs, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p := newTargetProfile("t")
	applyFaviconSignal(p, rs, []byte("some completely different favicon body"))
	if len(p.Tech) != 0 {
		t.Fatalf("expected no votes for an unrecognized favicon, got %+v", p.Tech)
	}
}

func TestFaviconHash_Deterministic(t *testing.T) {
	body := []byte("same bytes every time")
	if FaviconHash(body) != FaviconHash(body) {
		t.Fatal("FaviconHash is not deterministic")
	}
	if FaviconHash(body) == FaviconHash([]byte("different bytes")) {
		t.Fatal("FaviconHash collided on different input (suspicious, not necessarily wrong, but check)")
	}
}

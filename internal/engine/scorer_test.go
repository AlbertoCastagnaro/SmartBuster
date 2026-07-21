package engine

import (
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
)

func TestIsSensitiveCandidate(t *testing.T) {
	cases := []struct {
		name string
		cand Candidate
		want bool
	}{
		{"sensitive tag", Candidate{Path: "whatever", Tags: []string{"config"}}, true},
		{"substring in path", Candidate{Path: "administrator", Tags: []string{"generic"}}, true},
		{"dotfile substring", Candidate{Path: ".env.local", Tags: []string{"generic"}}, true},
		{"neither", Candidate{Path: "index.html", Tags: []string{"generic"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSensitiveCandidate(&tc.cand); got != tc.want {
				t.Errorf("isSensitiveCandidate(%+v) = %v, want %v", tc.cand, got, tc.want)
			}
		})
	}
}

func newTestScorer() *DynamicScorer {
	return NewDynamicScorer(&profile.TargetProfile{}, ScoreWeights{WSem: 1.5, WAssoc: 1.0, WConv: 0.8}, DefaultMarkovOrder, DefaultMarkovMinSamples)
}

func TestDynamicScorer_SemSignal_ProtectedBoostsOnlySensitiveSiblings(t *testing.T) {
	s := newTestScorer()
	s.ObserveProtected("/admin-parent")

	sensitive := Candidate{Path: "administrator", ParentDir: "/admin-parent", Tags: []string{"generic"}}
	if got := s.semSignal(&sensitive); got != 1.0 {
		t.Errorf("expected sensitive sibling to be boosted, got %v", got)
	}

	plain := Candidate{Path: "index.html", ParentDir: "/admin-parent", Tags: []string{"generic"}}
	if got := s.semSignal(&plain); got != 0 {
		t.Errorf("expected non-sensitive sibling to be unboosted, got %v", got)
	}
}

func TestDynamicScorer_SemSignal_IndexOfBoostsWholeDir(t *testing.T) {
	s := newTestScorer()
	s.ObserveIndexOf("/uploads")

	plain := Candidate{Path: "notes.txt", ParentDir: "/uploads", Tags: []string{"generic"}}
	if got := s.semSignal(&plain); got != 1.0 {
		t.Errorf("expected any candidate under an open-listing dir to be boosted, got %v", got)
	}
}

// TestDynamicScorer_SemSignal_ScopedByParentDir is spec §9 DoD assertion 6
// (subtree-awareness): a discovery in one directory must not leak into an
// unrelated one.
func TestDynamicScorer_SemSignal_ScopedByParentDir(t *testing.T) {
	s := newTestScorer()
	s.ObserveProtected("/api")

	inScope := Candidate{Path: "secret", ParentDir: "/api", Tags: []string{"generic"}}
	if got := s.semSignal(&inScope); got != 1.0 {
		t.Errorf("expected /api candidate to be boosted, got %v", got)
	}

	outOfScope := Candidate{Path: "secret", ParentDir: "/other", Tags: []string{"generic"}}
	if got := s.semSignal(&outOfScope); got != 0 {
		t.Errorf("expected /other candidate to be unaffected by /api's discovery, got %v", got)
	}
}

func TestDynamicScorer_Boost_IsNeutralWithoutSignals(t *testing.T) {
	s := newTestScorer()
	c := Candidate{Path: "whatever", ParentDir: "/", Tags: []string{"generic"}}
	if got := s.Boost(&c); got != 1.0 {
		t.Errorf("expected neutral boost (1.0) with no signals present, got %v", got)
	}
}

func TestDynamicScorer_Boost_MultipliesActiveSignals(t *testing.T) {
	s := newTestScorer()
	s.ObserveIndexOf("/uploads") // forces semSignal=1.0
	c := Candidate{Path: "notes.txt", ParentDir: "/uploads", Tags: []string{"generic"}}

	got := s.Boost(&c)
	want := 1 + s.cfg.WSem*1.0 // assoc and markov are both 0 here (cold-start / no companions)
	if got != want {
		t.Errorf("Boost() = %v, want %v", got, want)
	}
}

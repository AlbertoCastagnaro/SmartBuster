package engine

import (
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

func openTestScope(t *testing.T) *scope.Scope {
	t.Helper()
	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// TestLoadCorpusTemplate_WBypassesCorpus is spec §0 contract G / DoD
// assertion 7: -w still works and bypasses the corpus entirely.
func TestLoadCorpusTemplate_WBypassesCorpus(t *testing.T) {
	entries := []wordlist.Entry{{Word: "admin", Type: wordlist.EntryDir, BasePrio: 1.0}}
	co, err := NewCoordinator("http://example.test", entries, Config{Wordlist: "words.txt"}, openTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	co.profileState = &profile.TargetProfile{Tech: map[string]*profile.Tech{}}

	co.loadCorpusTemplate()
	if co.corpusTemplate != nil {
		t.Errorf("expected corpusTemplate to stay nil when -w is given, got %v", co.corpusTemplate)
	}
}

// TestLoadCorpusTemplate_DefaultsToEmbeddedCorpus is the corpus-mode
// counterpart: no -w means the coordinator seeds from the embedded corpus
// (spec §0 contract E).
func TestLoadCorpusTemplate_DefaultsToEmbeddedCorpus(t *testing.T) {
	co, err := NewCoordinator("http://example.test", nil, Config{}, openTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	co.profileState = &profile.TargetProfile{Tech: map[string]*profile.Tech{}}

	co.loadCorpusTemplate()
	if len(co.corpusTemplate) == 0 {
		t.Fatal("expected a non-empty corpus template from the embedded default corpus")
	}
}

// TestReprioritizeIfChanged_RescoresOnProfileChange is spec §7's
// Reprioritize wiring: applyMatchScore recomputes Score with the shared
// corpus.Score formula once the profile changes.
func TestReprioritizeIfChanged_RescoresOnProfileChange(t *testing.T) {
	co, err := NewCoordinator("http://example.test", nil, Config{}, openTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	co.techBoostW = 2.0
	co.profileState = &profile.TargetProfile{Tech: map[string]*profile.Tech{}}

	co.frontier.Push(Candidate{Path: "wp-login.php", BasePrio: 0.5, Score: 0.5, Tags: []string{"wordpress"}})

	co.reprioritizeIfChanged()
	top := co.frontier.Pop()
	if top.Score != 0.5 {
		t.Fatalf("expected unboosted Score 0.5 with no tech detected, got %v", top.Score)
	}
	co.frontier.Push(top)

	co.profileState.Tech["WordPress"] = &profile.Tech{Name: "WordPress", Category: "cms", Confidence: 0.8, Layer: profile.LayerBackend}
	co.reprioritizeIfChanged()
	top = co.frontier.Pop()
	want := 0.5 * (1 + 2.0*0.8)
	if diff := top.Score - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("expected rescored Score %v after WordPress detected, got %v", want, top.Score)
	}
}

// TestReprioritizeIfChanged_NoThrashOnUnchangedProfile is spec §7's guard:
// "no thrash when profile is unchanged."
func TestReprioritizeIfChanged_NoThrashOnUnchangedProfile(t *testing.T) {
	co, err := NewCoordinator("http://example.test", nil, Config{}, openTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	co.techBoostW = 2.0
	co.profileState = &profile.TargetProfile{Tech: map[string]*profile.Tech{
		"WordPress": {Name: "WordPress", Category: "cms", Confidence: 0.8, Layer: profile.LayerBackend},
	}}
	co.reprioritizeIfChanged() // first call establishes lastReprioSig

	co.frontier.Push(Candidate{Path: "canary", BasePrio: 0.5, Score: 999, Tags: nil})
	co.reprioritizeIfChanged() // same profile: must be a no-op

	top := co.frontier.Pop()
	if top.Score != 999 {
		t.Errorf("expected reprioritizeIfChanged to skip rescoring on an unchanged profile, but Score changed to %v", top.Score)
	}
}

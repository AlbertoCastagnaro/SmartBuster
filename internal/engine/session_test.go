package engine_test

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

func findingPathsOf(t *testing.T, co *engine.Coordinator) []string {
	t.Helper()
	out := make([]string, len(co.Findings()))
	for i, f := range co.Findings() {
		u, err := url.Parse(f.URL)
		if err != nil {
			t.Fatalf("bad finding URL %q: %v", f.URL, err)
		}
		out[i] = u.Path
	}
	sort.Strings(out)
	return out
}

// TestSession_SaveResumeContinuesAndReproducesSeed is spec §8 DoD #5's
// end-to-end assertion: "save -> resume -> scan continues; same seed
// reproduces the continuation." It compares an uninterrupted reference run
// against a run that's stopped partway, saved, and resumed from that saved
// state — both must land on the same finding set.
func TestSession_SaveResumeContinuesAndReproducesSeed(t *testing.T) {
	words := make([]string, 25)
	for i := range words {
		words[i] = fmt.Sprintf("sessword%02d", i)
	}
	// fixtures.NewHonest's real paths: mixed into the noise so both the
	// reference and the interrupted/resumed run have something to find.
	words = append(words, "admin", "backup", "config.php", "login")
	entries := loadWordlist(t, words)
	cfg := engine.Config{Seed: 77, Rate: 60, Concurrency: 3, RequestTO: 2 * time.Second, MaxDepth: 4}

	// Reference: an uninterrupted run against its own fixture instance.
	refFx := fixtures.NewHonest()
	defer refFx.Close()
	refCo, err := engine.NewCoordinator(refFx.URL, entries, cfg, openScope(t))
	if err != nil {
		t.Fatal(err)
	}
	refCtx, refCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer refCancel()
	refCo.Run(refCtx)
	if refCtx.Err() != nil {
		t.Fatal("reference run did not complete before the test deadline")
	}

	// Live: stopped partway, saved, then resumed from that saved state —
	// against its own separate fixture instance (same server behavior,
	// different port, so any URL comparison must be by path, not by
	// full URL — see findingPathsOf).
	liveFx := fixtures.NewHonest()
	defer liveFx.Close()
	liveCo, err := engine.NewCoordinator(liveFx.URL, entries, cfg, openScope(t))
	if err != nil {
		t.Fatal(err)
	}
	liveCtx, liveCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer liveCancel()
	done := make(chan struct{})
	go func() { defer close(done); liveCo.Run(liveCtx) }()

	time.Sleep(150 * time.Millisecond) // let the live scan get partway in

	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	state, err := liveCo.Save(saveCtx)
	saveCancel()
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := liveCo.SubmitControl(context.Background(), engine.ControlCmd{Kind: engine.CtrlStop}); err != nil {
		t.Fatalf("SubmitControl(stop): %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("live scan did not stop after CtrlStop")
	}

	resumedCo, err := engine.NewCoordinatorFromSnapshot(state, entries, openScope(t))
	if err != nil {
		t.Fatalf("NewCoordinatorFromSnapshot: %v", err)
	}
	resumeCtx, resumeCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer resumeCancel()
	resumedCo.Run(resumeCtx)
	if resumeCtx.Err() != nil {
		t.Fatal("resumed run did not complete before the test deadline")
	}

	refPaths, resumedPaths := findingPathsOf(t, refCo), findingPathsOf(t, resumedCo)
	if len(refPaths) == 0 {
		t.Fatal("expected the reference run to have found something (fixtures.NewHonest always has real paths)")
	}
	if len(refPaths) != len(resumedPaths) {
		t.Fatalf("resumed run found %d paths, reference (uninterrupted) run found %d — save/resume must reproduce the same continuation\nref:     %v\nresumed: %v",
			len(resumedPaths), len(refPaths), refPaths, resumedPaths)
	}
	for i := range refPaths {
		if refPaths[i] != resumedPaths[i] {
			t.Errorf("finding set diverged at index %d: resumed=%q, reference=%q", i, resumedPaths[i], refPaths[i])
		}
	}
}

// White-box Phase 4a tests (package engine, not engine_test) for the two
// pieces that need direct access to Coordinator/dirState internals: the
// archive.org pacer's independence from the target's own rate, and the
// pendingSeeds merge arithmetic pushWordlistCandidates performs (spec §3:
// "a seed also present in the corpus becomes one candidate with unioned
// provenance and max prior" — and, just as important, doesn't double-count
// candidatesTotal for a merged path). Full end-to-end behavior (ancestor
// chain, stale-seed pruning, dedup as seen through Findings) is covered by
// internal/engine/seed_test.go instead, against real fixtures.
package engine

import (
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

func seedIntTestScope(t *testing.T) *scope.Scope {
	t.Helper()
	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// TestArchivePacer_IndependentOfTargetRate is spec §5.3/§7 DoD #3's "the
// separate archive.org limiter is used (not the target limiter)": even with
// the target's own rate configured far slower than archive.org's, a single
// archivePace() call should still complete in around ArchiveRateDefault's
// own ~1s period, not the target's.
func TestArchivePacer_IndependentOfTargetRate(t *testing.T) {
	cfg := Config{Rate: 0.01, Concurrency: 1, RequestTO: time.Second, MaxDepth: 1} // target: one request per 100s
	co, err := NewCoordinator("http://example.test", nil, cfg, seedIntTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	defer co.archivePacer.Stop()

	start := time.Now()
	co.archivePace()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("archivePace took %v; expected ~1s (ArchiveRateDefault), not to wait on the target's 0.01req/s rate", elapsed)
	}
}

func TestPushWordlistCandidates_MergesPendingSeedIntoTemplateEntry(t *testing.T) {
	entries := []wordlist.Entry{{Word: "shared", Type: wordlist.EntryDir, BasePrio: 0.3}}
	cfg := Config{Concurrency: 1, RequestTO: time.Second, MaxDepth: 1}
	co, err := NewCoordinator("http://example.test", entries, cfg, seedIntTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	defer co.archivePacer.Stop()
	co.profileState = &profile.TargetProfile{} // scoreCandidate needs a non-nil profile; this test doesn't exercise profiling itself

	ds := &dirState{path: "", state: dirScanning, pendingSeeds: map[string]Candidate{
		"shared": {Path: "shared", BasePrio: 0.95, Provenance: "robots:disallow"},
	}}
	co.dirs[""] = ds
	co.pushWordlistCandidates("", ds)

	if co.frontier.Len() != 1 {
		t.Fatalf("frontier.Len() = %d, want 1 (the seed should merge into the template entry, not add a second candidate)", co.frontier.Len())
	}
	cand := co.frontier.Pop()
	if cand.BasePrio != 0.95 {
		t.Errorf("BasePrio = %v, want 0.95 (the seed's higher prior should win)", cand.BasePrio)
	}
	if cand.Provenance != "wordlist+robots:disallow" {
		t.Errorf("Provenance = %q, want %q", cand.Provenance, "wordlist+robots:disallow")
	}
	if ds.candidatesTotal != 1 {
		t.Errorf("candidatesTotal = %d, want 1 (a merge must not double-count)", ds.candidatesTotal)
	}
	if ds.budget != 1 {
		t.Errorf("budget = %d, want 1", ds.budget)
	}
}

func TestPushWordlistCandidates_LeftoverSeedBecomesNewCandidate(t *testing.T) {
	cfg := Config{Concurrency: 1, RequestTO: time.Second, MaxDepth: 4}
	co, err := NewCoordinator("http://example.test", []wordlist.Entry{{Word: "decoy", Type: wordlist.EntryDir, BasePrio: 0.3}}, cfg, seedIntTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	defer co.archivePacer.Stop()
	co.profileState = &profile.TargetProfile{}

	ds := &dirState{path: "/old/admin", depth: 2, state: dirScanning, pendingSeeds: map[string]Candidate{
		"secret.php": {Path: "secret.php", Type: TypeFile, BasePrio: 0.95, Tags: []string{"generic"}, Depth: 3, ParentDir: "/old/admin", Provenance: "robots:disallow"},
	}}
	co.dirs["/old/admin"] = ds
	co.pushWordlistCandidates("/old/admin", ds)

	if ds.candidatesTotal != 2 { // the one wordlist entry ("decoy") plus the leftover seed
		t.Fatalf("candidatesTotal = %d, want 2", ds.candidatesTotal)
	}
	found := false
	for co.frontier.Len() > 0 {
		if c := co.frontier.Pop(); c.Path == "secret.php" {
			found = true
			if c.ParentDir != "/old/admin" || c.Provenance != "robots:disallow" {
				t.Errorf("leftover candidate = %+v, want ParentDir=/old/admin Provenance=robots:disallow", c)
			}
		}
	}
	if !found {
		t.Error("expected the leftover seed to be pushed as its own candidate")
	}
}

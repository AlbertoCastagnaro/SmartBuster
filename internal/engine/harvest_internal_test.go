// White-box Phase 4b tests (package engine, not engine_test) for the
// load-bearing seedInjectCh + applySeedBatch primitive: the SCANNING-branch
// merge fix (spec §0 contract B), the MAX_NEW_DIRS_PER_BATCH cap (spec §5,
// contract I), and — the part the build order calls out explicitly —
// concurrency safety under -race with both producer shapes (a trickle of
// small batches and one large batch) hammering the channel at once, proving
// the frontier stays single-writer with no mutex of its own. Full
// end-to-end coverage (fixture-driven crawl/JS/SPA behavior) lives in
// internal/engine/harvest_test.go instead.
package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/harvest"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/seed"
)

func harvestTestCoordinator(t *testing.T, maxDepth int) *Coordinator {
	t.Helper()
	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Concurrency: 1, RequestTO: time.Second, MaxDepth: maxDepth}
	co, err := NewCoordinator("http://example.test", nil, cfg, sc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.archivePacer.Stop() })
	co.profileState = &profile.TargetProfile{} // scoreCandidate needs a non-nil profile
	co.runCtx = context.Background()
	return co
}

// TestApplySeedBatch_MergesIntoScanningDirCandidate covers the DoD
// crawl_dedup case (spec §8): a seed landing on a path already queued as a
// candidate in a SCANNING dir must merge (union provenance, max BasePrio)
// into that one candidate, not first-writer-wins silently drop — the exact
// gap 4a flagged and contract B fixes via stageDirCandidate's SCANNING
// branch (mergeOrEnqueueGenerated).
func TestApplySeedBatch_MergesIntoScanningDirCandidate(t *testing.T) {
	co := harvestTestCoordinator(t, 4)
	ds := &dirState{path: "", state: dirScanning, knownPaths: map[string]bool{"shared": true}}
	co.dirs[""] = ds
	co.frontier.Push(Candidate{Path: "shared", ParentDir: "", BasePrio: 0.3, Provenance: "wordlist", Score: 0.3})

	co.applySeedBatch(SeedBatch{Seeds: []seed.Seed{
		{Path: "shared", BasePrio: 0.95, Provenance: "crawl:html"},
	}})

	if co.frontier.Len() != 1 {
		t.Fatalf("frontier.Len() = %d, want 1 (the seed should merge into the queued candidate, not add a second one)", co.frontier.Len())
	}
	cand := co.frontier.Pop()
	if cand.BasePrio != 0.95 {
		t.Errorf("BasePrio = %v, want 0.95 (the seed's higher prior should win)", cand.BasePrio)
	}
	if cand.Provenance != "wordlist+crawl:html" {
		t.Errorf("Provenance = %q, want %q", cand.Provenance, "wordlist+crawl:html")
	}
}

// TestApplySeedBatch_MergeIsMootIfAlreadyDispatched covers the case where
// the known path is no longer queued (already dispatched): the merge must
// be a no-op, not a panic or a phantom re-add.
func TestApplySeedBatch_MergeIsMootIfAlreadyDispatched(t *testing.T) {
	co := harvestTestCoordinator(t, 4)
	ds := &dirState{path: "", state: dirScanning, knownPaths: map[string]bool{"gone": true}}
	co.dirs[""] = ds // "gone" is known but nothing is queued for it (already dispatched)

	co.applySeedBatch(SeedBatch{Seeds: []seed.Seed{
		{Path: "gone", BasePrio: 0.95, Provenance: "crawl:js"},
	}})

	if co.frontier.Len() != 0 {
		t.Fatalf("frontier.Len() = %d, want 0 (nothing should be re-added for an already-dispatched path)", co.frontier.Len())
	}
}

// TestApplySeedBatch_CapsBlastRadius covers the dir_storm DoD case (spec
// §5, §8, contract I): a batch naming more distinct new directories than
// MAX_NEW_DIRS_PER_BATCH is truncated to the highest-prior seeds, and a
// seed.capped warning fires — nothing beyond the ceiling is materialized.
func TestApplySeedBatch_CapsBlastRadius(t *testing.T) {
	co := harvestTestCoordinator(t, 6)
	ds := &dirState{path: "", state: dirScanning, knownPaths: map[string]bool{}}
	co.dirs[""] = ds

	var events []Event
	co.emitter = EventFunc(func(e Event) { events = append(events, e) })

	total := harvest.MaxNewDirsPerBatch + 50
	seeds := make([]seed.Seed, total)
	for i := 0; i < total; i++ {
		seeds[i] = seed.Seed{Path: fmt.Sprintf("/wayback/%d/leaf", i), BasePrio: 0.7, Provenance: "wayback:20200101"}
	}
	co.applySeedBatch(SeedBatch{Seeds: seeds})

	newDirs := len(co.dirs) - 1 // -1 for root, which already existed
	if newDirs > harvest.MaxNewDirsPerBatch {
		t.Errorf("materialized %d new dirs, want <= MAX_NEW_DIRS_PER_BATCH (%d)", newDirs, harvest.MaxNewDirsPerBatch)
	}
	if newDirs == 0 {
		t.Error("expected at least some seeds to survive the cap (truncated, not zeroed)")
	}

	sawCapped := false
	for _, ev := range events {
		if ev.Type == EventWarning && len(ev.Message) > 0 && ev.Message[:11] == "seed.capped" {
			sawCapped = true
		}
	}
	if !sawCapped {
		t.Error("expected a seed.capped warning when the batch exceeds MAX_NEW_DIRS_PER_BATCH")
	}
}

// TestSeedInjectCh_ConcurrentProducers_Race is the load-bearing primitive's
// own test, built and run first per the build order (spec §9): both
// producer shapes named in spec §5 — a trickle of many small batches (the
// crawler, as pages come back) and one large batch (async Wayback) — fire
// concurrently from separate goroutines while a single drain loop (standing
// in for dispatchLoop's own `case batch := <-c.seedInjectCh`) is the only
// goroutine that ever calls applySeedBatch. Run under `go test -race`: the
// frontier has no mutex of its own (see Frontier in frontier.go) — this is
// what proves that's safe, for both shapes at once, not just one at a time.
func TestSeedInjectCh_ConcurrentProducers_Race(t *testing.T) {
	co := harvestTestCoordinator(t, 6)
	ds := &dirState{path: "", state: dirScanning, knownPaths: map[string]bool{}}
	co.dirs[""] = ds

	var applied int
	var mu sync.Mutex
	done := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case batch := <-co.seedInjectCh:
				co.applySeedBatch(batch)
				mu.Lock()
				applied += len(batch.Seeds)
				mu.Unlock()
			case <-done:
				// Drain whatever's left without blocking further.
				for {
					select {
					case batch := <-co.seedInjectCh:
						co.applySeedBatch(batch)
						mu.Lock()
						applied += len(batch.Seeds)
						mu.Unlock()
					default:
						return
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup

	// Shape 1: trickle — many goroutines each sending one small batch, as
	// the HTML crawler would as pages come back.
	const trickleN = 25
	for i := 0; i < trickleN; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			co.sendSeedBatch([]seed.Seed{
				{Path: fmt.Sprintf("trickle%d", i), BasePrio: 0.5, Provenance: "crawl:html"},
			})
		}(i)
	}

	// Shape 2: one large batch sent concurrently with the trickle, as async
	// Wayback would dump its CDX results.
	const bigN = 60
	wg.Add(1)
	go func() {
		defer wg.Done()
		big := make([]seed.Seed, bigN)
		for i := 0; i < bigN; i++ {
			big[i] = seed.Seed{Path: fmt.Sprintf("/big/%d/leaf", i), BasePrio: 0.7, Provenance: "wayback:20240101"}
		}
		co.sendSeedBatch(big)
	}()

	// A concurrent warning producer too (async Wayback's graceful-failure
	// path), exercising SeedBatch's other field under the same race.
	wg.Add(1)
	go func() {
		defer wg.Done()
		co.sendWarning("wayback: simulated failure")
	}()

	wg.Wait()
	close(done)
	<-drainDone

	mu.Lock()
	defer mu.Unlock()
	if want := trickleN + bigN; applied != want {
		t.Errorf("applied = %d, want %d (every seed from both producer shapes should have been applied exactly once)", applied, want)
	}
}

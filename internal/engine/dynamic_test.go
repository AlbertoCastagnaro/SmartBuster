// Phase 3 coordinator-level integration tests (spec §9 DoD). This file is
// package engine (white-box), not engine_test, specifically so tests that
// need to inspect scorer/dirState internals (the poisoning gate, the
// reprioritization throttle) can do so directly rather than through
// exported surface that doesn't exist for this purpose.
package engine

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// --- local test helpers (this file's own package, can't reuse engine_test's) ---

func dynTestScope(t *testing.T) *scope.Scope {
	t.Helper()
	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func dynTestWordlist(t *testing.T, words []string) []wordlist.Entry {
	t.Helper()
	p := filepath.Join(t.TempDir(), "words.txt")
	if err := os.WriteFile(p, []byte(strings.Join(words, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := wordlist.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

type dynCollectingAudit struct {
	mu      sync.Mutex
	records []AuditRecord
}

func (a *dynCollectingAudit) WriteRequest(r AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, r)
}

func (a *dynCollectingAudit) snapshot() []AuditRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AuditRecord(nil), a.records...)
}

func dynURLPath(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad URL %q: %v", raw, err)
	}
	return u.Path
}

// requestsToDiscover returns the 1-based count of non-probe requests needed
// until want has been requested at least once; -1 if never.
func requestsToDiscover(t *testing.T, records []AuditRecord, want string) int {
	t.Helper()
	n := 0
	for _, r := range records {
		if r.IsProbe {
			continue
		}
		n++
		if dynURLPath(t, r.URL) == want {
			return n
		}
	}
	return -1
}

func runDynScan(t *testing.T, target string, words []wordlist.Entry, cfg Config) (*Coordinator, []AuditRecord) {
	t.Helper()
	audit := &dynCollectingAudit{}
	cfg.Concurrency = 1
	if cfg.RequestTO == 0 {
		cfg.RequestTO = 2 * time.Second
	}
	cfg.Wordlist = "test"
	co, err := NewCoordinator(target, words, cfg, dynTestScope(t), WithAuditSink(audit))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	co.Run(ctx)
	if ctx.Err() != nil {
		t.Fatal("scan did not complete before the test's context deadline")
	}
	return co, audit.snapshot()
}

// --- §3.1 response-semantics: protected_admin fixture ---

// runDynScan rate-limits every ordering-sensitive test below: with an
// unbounded rate and a local httptest server, the dispatch loop can pop
// several more candidates from the frontier before a hit's result comes
// back and reprioritizes it (dispatch doesn't wait on results, only on the
// pacer and channel buffering) — a real characteristic of the coordinator's
// async design, not a Phase 3 bug, but it means an ordering assertion needs
// dispatch to trail results closely enough for reprioritization to matter.
const dynOrderingRate = 100

func TestDynamic_ProtectedAdmin_SensitiveSiblingsFoundFaster(t *testing.T) {
	fx := fixtures.NewProtectedAdmin()
	defer fx.Close()

	words := []string{"admin"}
	for i := 0; i < 10; i++ {
		words = append(words, "decoy"+string(rune('a'+i)))
	}
	words = append(words, "administrator", "admin-panel")
	entries := dynTestWordlist(t, words)

	measure := func(sem float64) int {
		_, records := runDynScan(t, fx.URL, entries, Config{
			Seed: 1, Weights: ScoreWeights{WSem: sem}, ReprioHits: 1, Rate: dynOrderingRate,
		})
		a := requestsToDiscover(t, records, "/administrator")
		b := requestsToDiscover(t, records, "/admin-panel")
		if a < 0 || b < 0 {
			t.Fatalf("planted siblings never found (WSem=%v)", sem)
		}
		return a + b
	}

	boosted := measure(10.0)
	baseline := measure(0)
	t.Logf("sum of positions: boosted=%d baseline=%d", boosted, baseline)
	if boosted >= baseline {
		t.Errorf("expected sensitive siblings to be found sooner with WSem>0: boosted=%d, baseline=%d", boosted, baseline)
	}
}

// --- §3.2 association: companions fixture ---

func TestDynamic_Companions_LinkedTermFoundFaster(t *testing.T) {
	fx := fixtures.NewCompanions()
	defer fx.Close()

	// Decoys use a different extension than login.php/config.php so they
	// don't also pick up the (separate, legitimate) extension-pivot boost —
	// this test isolates the companion-table reweight specifically.
	words := []string{"login.php"}
	for i := 0; i < 5; i++ {
		words = append(words, "decoy"+string(rune('a'+i))+".html")
	}
	words = append(words, "config.php")
	entries := dynTestWordlist(t, words)

	measure := func(assoc float64) int {
		_, records := runDynScan(t, fx.URL, entries, Config{
			Seed: 2, Weights: ScoreWeights{WAssoc: assoc}, ReprioHits: 1, Rate: dynOrderingRate,
		})
		n := requestsToDiscover(t, records, "/config.php")
		if n < 0 {
			t.Fatalf("planted companion never found (WAssoc=%v)", assoc)
		}
		return n
	}

	boosted := measure(50.0)
	baseline := measure(0)
	t.Logf("requests to find /config.php: boosted=%d baseline=%d", boosted, baseline)
	if boosted >= baseline {
		t.Errorf("expected the companion term to be found sooner with WAssoc>0: boosted=%d, baseline=%d", boosted, baseline)
	}
}

// --- §3.3 naming convention: naming_convention fixture ---

func TestDynamic_NamingConvention_PlantedSegmentFoundFaster(t *testing.T) {
	fx := fixtures.NewNamingConvention()
	defer fx.Close()

	// Extensionless, matching the fixture (spec §9 note: this keeps the
	// fixture from also triggering file-backup generation, which would
	// otherwise flood the frontier with candidates unrelated to what this
	// test isolates). MaxDepth:1 blocks recursion into these 200-status
	// "directory-shaped" hits, keeping the candidate pool exactly these
	// root-level words.
	words := []string{
		"get_user", "get_role", "get_order", "get_item",
		"get_status", "get_price", "get_stock", "get_review",
		"decoya", "decoyb",
	}
	words = append(words, "get_secret")
	entries := dynTestWordlist(t, words)

	measure := func(conv float64) int {
		_, records := runDynScan(t, fx.URL, entries, Config{
			Seed: 3, Weights: ScoreWeights{WConv: conv}, ReprioHits: 1, MaxDepth: 1, Rate: dynOrderingRate,
		})
		n := requestsToDiscover(t, records, "/get_secret")
		if n < 0 {
			t.Fatalf("planted segment never found (WConv=%v)", conv)
		}
		return n
	}

	boosted := measure(5000.0)
	baseline := measure(0)
	t.Logf("requests to find /get_secret: boosted=%d baseline=%d", boosted, baseline)
	if boosted >= baseline {
		t.Errorf("expected get_secret to be found sooner with WConv>0: boosted=%d, baseline=%d", boosted, baseline)
	}
}

// --- §3.2 generation: sequence + backup_trigger fixtures ---

func TestDynamic_Sequence_GeneratesAndFindsPlantedVersion(t *testing.T) {
	fx := fixtures.NewSequence()
	defer fx.Close()

	entries := dynTestWordlist(t, []string{"api", "v1"}) // "v2" is NOT in the wordlist
	co, _ := runDynScan(t, fx.URL, entries, Config{Seed: 4, ReprioHits: 1})

	found := false
	for _, f := range co.Findings() {
		if strings.HasSuffix(f.URL, "/api/v2") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected /api/v2 to be discovered via sibling/sequence generation; findings=%v", co.Findings())
	}
}

func TestDynamic_BackupTrigger_GeneratesAndFindsPlantedBackup(t *testing.T) {
	fx := fixtures.NewBackupTrigger()
	defer fx.Close()

	entries := dynTestWordlist(t, []string{"config.php"}) // "config.php.bak" is NOT in the wordlist
	co, _ := runDynScan(t, fx.URL, entries, Config{Seed: 5, ReprioHits: 1})

	found := false
	for _, f := range co.Findings() {
		if strings.HasSuffix(f.URL, "/config.php.bak") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected config.php.bak to be discovered via backup generation; findings=%v", co.Findings())
	}
}

// --- §9 open_listing fixture: coordinator-level smoke test ---

func TestDynamic_OpenListing_FlagsChildDirAndIsFound(t *testing.T) {
	fx := fixtures.NewOpenListing()
	defer fx.Close()

	entries := dynTestWordlist(t, []string{"uploads"})
	co, _ := runDynScan(t, fx.URL, entries, Config{Seed: 6})

	found := false
	for _, f := range co.Findings() {
		if strings.HasSuffix(f.URL, "/uploads") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected /uploads to be found; findings=%v", co.Findings())
	}
	if co.scorer == nil || !co.scorer.dirContext("/uploads").SawIndexOf {
		t.Errorf("expected DirContext(/uploads).SawIndexOf to be set after the open-listing hit")
	}
}

// --- §5 poisoning gate: direct unit test on the gated learning entry point ---

func newReadyCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	fx := fixtures.NewHonest()
	t.Cleanup(fx.Close)

	entries := dynTestWordlist(t, []string{"admin", "backup"})
	co, err := NewCoordinator(fx.URL, entries, Config{Seed: 7, Wordlist: "test", Concurrency: 1, RequestTO: 2 * time.Second}, dynTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	co.runCtx = context.Background()
	co.profileTarget(context.Background()) // builds c.scorer/profileState without running the full scan
	co.dirs[""] = &dirState{path: "", state: dirScanning}
	return co
}

func TestPoisoningGate_LowConfidenceLeavesLearnersUnchanged(t *testing.T) {
	co := newReadyCoordinator(t)

	res := WorkResult{
		Item:      WorkItem{Candidate: Candidate{Path: "secret.php", Type: TypeFile, ParentDir: ""}},
		Signature: ResponseSignature{Status: 200},
	}
	cls := Classification{IsHit: true, Confidence: 0.5} // below LEARN_MIN_CONF (default 0.8)
	co.learnFromHit(res, cls, "")

	if co.scorer.markov.trained != 0 {
		t.Errorf("expected markov.trained to stay 0 for a low-confidence hit, got %d", co.scorer.markov.trained)
	}
	if len(co.scorer.assoc.hitTerms) != 0 {
		t.Errorf("expected assoc.hitTerms to stay empty for a low-confidence hit, got %v", co.scorer.assoc.hitTerms)
	}
	if len(co.scorer.dirCtx) != 0 {
		t.Errorf("expected dirCtx to stay empty for a low-confidence hit, got %v", co.scorer.dirCtx)
	}
}

func TestPoisoningGate_WildcardSuspectDirLeavesLearnersUnchanged(t *testing.T) {
	co := newReadyCoordinator(t)
	co.dirs[""].wildcardSuspect = true

	res := WorkResult{
		Item:      WorkItem{Candidate: Candidate{Path: "secret.php", Type: TypeFile, ParentDir: ""}},
		Signature: ResponseSignature{Status: 200},
	}
	cls := Classification{IsHit: true, Confidence: 0.99} // high confidence, but the dir is wildcard-suspect
	co.learnFromHit(res, cls, "")

	if co.scorer.markov.trained != 0 {
		t.Errorf("expected markov.trained to stay 0 in a wildcard-suspect dir, got %d", co.scorer.markov.trained)
	}
	if len(co.scorer.assoc.hitTerms) != 0 {
		t.Errorf("expected assoc.hitTerms to stay empty in a wildcard-suspect dir, got %v", co.scorer.assoc.hitTerms)
	}
}

func TestPoisoningGate_HighConfidenceDoesLearn(t *testing.T) {
	co := newReadyCoordinator(t)

	res := WorkResult{
		Item:      WorkItem{Candidate: Candidate{Path: "secret.php", Type: TypeFile, ParentDir: ""}},
		Signature: ResponseSignature{Status: 200},
	}
	cls := Classification{IsHit: true, Confidence: 0.9}
	co.learnFromHit(res, cls, "")

	if co.scorer.markov.trained != 1 {
		t.Errorf("expected markov.trained == 1 for a qualifying hit, got %d", co.scorer.markov.trained)
	}
}

// --- §9 soft404_poison fixture: smoke test that the scan completes without
// runaway generation under a wildcard-like branch ---

func TestDynamic_Soft404Poison_ScanCompletes(t *testing.T) {
	fx := fixtures.NewSoft404Poison()
	defer fx.Close()

	entries := dynTestWordlist(t, []string{"vault", "readme.txt", "notes.txt", "item1", "item2"})
	co, records := runDynScan(t, fx.URL, entries, Config{Seed: 8, ReprioHits: 1})
	_ = co

	nonProbe := 0
	for _, r := range records {
		if !r.IsProbe {
			nonProbe++
		}
	}
	// A handful of directories x a handful of words; if generation cascaded
	// unchecked under the wildcard branch this would run into the hundreds.
	if nonProbe > 200 {
		t.Errorf("expected a bounded number of real requests, got %d (possible unchecked generation cascade)", nonProbe)
	}
}

// --- §6 throttled reprioritization ---

func TestReprioritization_ThrottledByHitCount(t *testing.T) {
	c := &Coordinator{
		frontier:       NewFrontier(),
		reprioHits:     25,
		reprioInterval: time.Hour, // so only the hit-count path can fire
	}
	c.scorer = newTestScorer()

	for i := 0; i < 100; i++ {
		c.markScorerDirty()
	}
	if want := 4; c.dynReprioCount != want { // 100 hits / 25 per sweep = 4
		t.Errorf("expected %d reprio sweeps for 100 hits at ReprioHits=25, got %d", want, c.dynReprioCount)
	}
}

func TestReprioritization_ThrottledByInterval(t *testing.T) {
	c := &Coordinator{
		frontier:       NewFrontier(),
		reprioHits:     1_000_000, // so only the interval path can fire
		reprioInterval: 10 * time.Millisecond,
	}
	c.scorer = newTestScorer()

	c.markScorerDirty()
	first := c.dynReprioCount
	if first != 1 {
		t.Fatalf("expected the first dirty mark to always sweep once, got %d", first)
	}
	c.markScorerDirty() // immediately after: interval not elapsed, hit-count nowhere near threshold
	if c.dynReprioCount != first {
		t.Errorf("expected no sweep before the interval elapses, got count %d", c.dynReprioCount)
	}
	time.Sleep(15 * time.Millisecond)
	c.markScorerDirty()
	if c.dynReprioCount != first+1 {
		t.Errorf("expected exactly one more sweep once the interval elapsed, got count %d", c.dynReprioCount)
	}
}

// --- §4/§9 reproducibility: same seed -> identical run, including ε-greedy ---

func TestDynamic_Reproducibility_SameSeedIdenticalFindings(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := []string{"admin", "backup", "config.php", "login", "decoy1", "decoy2", "decoy3"}
	entries := dynTestWordlist(t, words)
	cfg := Config{Seed: 42, Epsilon: 0.5, SubtreeBurst: 2, ReprioHits: 1}

	run := func() []string {
		co, _ := runDynScan(t, fx.URL, entries, cfg)
		var urls []string
		for _, f := range co.Findings() {
			urls = append(urls, f.URL)
		}
		return urls
	}

	a := run()
	b := run()
	if len(a) != len(b) {
		t.Fatalf("finding counts differ across identical-seed runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("finding order differs at index %d: %q vs %q", i, a[i], b[i])
		}
	}
}

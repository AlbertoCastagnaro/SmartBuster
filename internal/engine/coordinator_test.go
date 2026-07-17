package engine_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// --- test helpers ---

func loadWordlist(t *testing.T, words []string) []wordlist.Entry {
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

func openScope(t *testing.T) *scope.Scope {
	t.Helper()
	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func urlPath(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("bad URL %q: %v", rawURL, err)
	}
	return u.Path
}

type collectingEmitter struct {
	mu     sync.Mutex
	events []engine.Event
}

func (e *collectingEmitter) Emit(ev engine.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *collectingEmitter) has(t engine.EventType) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.Type == t {
			return true
		}
	}
	return false
}

type collectingAudit struct {
	mu      sync.Mutex
	records []engine.AuditRecord
}

func (a *collectingAudit) WriteRequest(r engine.AuditRecord) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, r)
}

func (a *collectingAudit) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.records)
}

func runScan(t *testing.T, target string, words []string, cfg engine.Config) (*engine.Coordinator, *collectingEmitter, *collectingAudit) {
	t.Helper()
	entries := loadWordlist(t, words)
	emitter := &collectingEmitter{}
	audit := &collectingAudit{}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 5
	}
	if cfg.RequestTO == 0 {
		cfg.RequestTO = 2 * time.Second
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 4
	}
	co, err := engine.NewCoordinator(target, entries, cfg, openScope(t),
		engine.WithEventEmitter(emitter), engine.WithAuditSink(audit))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	co.Run(ctx)
	if ctx.Err() != nil {
		t.Fatal("scan did not complete before the test's context deadline")
	}
	return co, emitter, audit
}

func findingPaths(t *testing.T, co *engine.Coordinator) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range co.Findings() {
		out[urlPath(t, f.URL)] = true
	}
	return out
}

// --- fixture-driven end-to-end tests ---

func TestCoordinator_Honest_FullRecall(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	co, _, _ := runScan(t, fx.URL, []string{"admin", "backup", "config.php", "login", "noise1", "noise2", "noise3"},
		engine.Config{Seed: 1})

	found := findingPaths(t, co)
	for _, real := range fx.RealPaths {
		if !found[real] {
			t.Errorf("expected %s found; findings=%v", real, co.Findings())
		}
	}
	if len(co.Findings()) != len(fx.RealPaths) {
		t.Errorf("expected exactly %d findings (full recall, no false positives), got %d: %v",
			len(fx.RealPaths), len(co.Findings()), co.Findings())
	}
}

func TestCoordinator_WildcardDir_SuppressesChildren(t *testing.T) {
	fx := fixtures.NewWildcardDir()
	defer fx.Close()

	co, emitter, _ := runScan(t, fx.URL, []string{"files", "secret", "backup", "config"},
		engine.Config{Seed: 2, MaxDepth: 3})

	found := findingPaths(t, co)
	if !found["/files"] {
		t.Errorf("expected /files itself to be discovered; findings=%v", co.Findings())
	}
	for p := range found {
		if strings.HasPrefix(p, "/files/") {
			t.Errorf("expected no children found under wildcard dir /files, got %s", p)
		}
	}
	if !emitter.has(engine.EventCalibrationDone) {
		t.Error("expected at least one calibration.done event")
	}
}

func TestCoordinator_SPA_NoHitFloodAndWarns(t *testing.T) {
	fx := fixtures.NewSPA()
	defer fx.Close()

	co, emitter, _ := runScan(t, fx.URL,
		[]string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"},
		engine.Config{Seed: 3})

	if len(co.Findings()) != 0 {
		t.Errorf("expected no findings for an SPA catch-all, got %d: %v", len(co.Findings()), co.Findings())
	}
	if !emitter.has(engine.EventWarning) {
		t.Error("expected an SPA warning event")
	}
}

func TestCoordinator_RateLimited_TriggersThrottle(t *testing.T) {
	// Once the throttle trips, backoff paces remaining dispatch down to
	// backoffFallbackRate for BackoffWindow (30s per spec §13 default) —
	// legitimately slow, not a hang. So this test only needs to observe the
	// throttle event fire, not wait for the full scan to complete; it uses
	// its own short deadline instead of the shared runScan helper (which
	// treats a context timeout as a test failure).
	fx := fixtures.NewRateLimited(20)
	defer fx.Close()

	words := make([]string, 40)
	for i := range words {
		words[i] = "word" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	entries := loadWordlist(t, words)
	emitter := &collectingEmitter{}

	co, err := engine.NewCoordinator(fx.URL, entries, engine.Config{
		Seed: 4, Concurrency: 3, RequestTO: 2 * time.Second,
	}, openScope(t), engine.WithEventEmitter(emitter))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	co.Run(ctx)

	if !emitter.has(engine.EventThrottle) {
		t.Error("expected a throttle event once 429s spike past the WAF-onset threshold")
	}
}

func TestCoordinator_Tarpit_CapsSlowBranch(t *testing.T) {
	// delay must clear 0.9*RequestTO (the tarpit trip threshold) with enough
	// margin over RequestTO that a normal local round trip doesn't tip a
	// request into a timeout error, even under -race scheduling jitter.
	fx := fixtures.NewTarpit(460 * time.Millisecond)
	defer fx.Close()

	words := make([]string, 40)
	for i := range words {
		words[i] = "word" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}

	_, emitter, _ := runScan(t, fx.URL, words, engine.Config{
		Seed: 5, Concurrency: 3, RequestTO: 500 * time.Millisecond,
	})

	if !emitter.has(engine.EventTrapDetected) {
		t.Error("expected a trap.detected event when a branch's median latency approaches the timeout")
	}
}

func TestCoordinator_RecursionMultiLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/admin":
			io.WriteString(w, "<html><body>Admin area top level, unique text alpha bravo.</body></html>")
		case "/admin/config":
			io.WriteString(w, "<html><body>Admin config nested page, unique text charlie delta.</body></html>")
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<html><body>404, nothing here at all.</body></html>")
		}
	}))
	defer srv.Close()

	co, _, _ := runScan(t, srv.URL, []string{"admin", "config", "secret"}, engine.Config{Seed: 6, MaxDepth: 4})

	found := findingPaths(t, co)
	if !found["/admin"] {
		t.Errorf("expected /admin found; findings=%v", co.Findings())
	}
	if !found["/admin/config"] {
		t.Errorf("expected nested /admin/config found via recursion; findings=%v", co.Findings())
	}
}

func TestCoordinator_ContentNoveltyPreventsDuplicateRecursion(t *testing.T) {
	shared := "<html><body>Identical payload served from two different paths.</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/widget", "/gadget":
			io.WriteString(w, shared)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<html><body>404, plain and boring.</body></html>")
		}
	}))
	defer srv.Close()

	co, _, audit := runScan(t, srv.URL, []string{"widget", "gadget"}, engine.Config{Seed: 7, MaxDepth: 3})

	if len(co.Findings()) != 1 {
		t.Fatalf("expected duplicate content to collapse into one finding with an alias, got %d: %v",
			len(co.Findings()), co.Findings())
	}
	f := co.Findings()[0]
	if len(f.Aliases) != 1 {
		t.Fatalf("expected exactly one alias recorded, got %v", f.Aliases)
	}

	// The aliased path must never have been recursed into: no audit record
	// should show a request whose parent directory is the alias path.
	aliasDir := urlPath(t, f.Aliases[0])
	audit.mu.Lock()
	defer audit.mu.Unlock()
	for _, rec := range audit.records {
		if rec.ParentDir == aliasDir {
			t.Errorf("expected no recursion into aliased duplicate-content path %s, but found request %s", aliasDir, rec.URL)
		}
	}
}

func TestCoordinator_AuditRecordsEveryRequest(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	_, _, audit := runScan(t, fx.URL, []string{"admin", "backup", "config.php", "login", "noise"}, engine.Config{Seed: 8})

	// N_PROBES(5) * len(ExtSet)(3) = 15 calibration probes at minimum, plus
	// one audit record per wordlist candidate.
	if audit.count() < 15+5 {
		t.Errorf("expected at least 20 audit records (probes + candidates), got %d", audit.count())
	}
}

func TestCoordinator_OutOfScopeTargetRefused(t *testing.T) {
	sc, err := scope.New(scope.Config{AllowHosts: []string{"only-this-host.example"}})
	if err != nil {
		t.Fatal(err)
	}
	entries := loadWordlist(t, []string{"admin"})
	_, err = engine.NewCoordinator("http://not-allowed.example", entries, engine.Config{}, sc)
	if err == nil {
		t.Fatal("expected an out-of-scope target to be refused")
	}
}

func TestCoordinator_InfiniteDir_PrunedWithoutHanging(t *testing.T) {
	fx := fixtures.NewInfiniteDir()
	defer fx.Close()

	co, _, _ := runScan(t, fx.URL, []string{"loop", "x1", "x2", "x3", "x4"}, engine.Config{Seed: 10, MaxDepth: 4})
	// runScan itself asserts the scan completes inside its context deadline,
	// which is the direct evidence of "no hang." A self-similar infinite
	// descent must also not flood findings.
	if len(co.Findings()) > 5 {
		t.Errorf("expected the infinite-descent branch pruned (few findings), got %d: %v", len(co.Findings()), co.Findings())
	}
	if found := findingPaths(t, co); !found["/loop"] {
		t.Errorf("expected /loop itself discovered; findings=%v", co.Findings())
	}
}

// TestCoordinator_ObservesConfiguredRate is the end-to-end counterpart to
// httpclient's TestPacerObservedAggregateRate: it exercises the coordinator
// with concurrency deliberately different from rate, over real (if local)
// network calls, and checks aggregate throughput tracks --rate rather than
// --concurrency (spec §5: "concurrency and request-rate are independent").
//
// The upper bound is the same provable ceiling as the pure-Pacer test —
// rate/(1-jitter), since NextInterval never returns less than
// base*(1-jitter) — with a bit more epsilon than the pure-Pacer test
// because real dispatch/network/goroutine-scheduling overhead adds
// measurement noise on top of the pacer's own timing (that overhead can
// only slow things down, never speed them up, so the ceiling direction is
// still sound). The lower bound is deliberately loose: it only guards
// against the pacer being wildly too slow, not a tight statistical claim,
// since real per-request overhead (unlike the isolated Pacer test) can
// legitimately eat into throughput.
func TestCoordinator_ObservesConfiguredRate(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := make([]string, 60)
	for i := range words {
		words[i] = fmt.Sprintf("candidateword%02d", i)
	}
	entries := loadWordlist(t, words)
	audit := &collectingAudit{}

	const rate = 30.0
	const jitter = 0.3
	co, err := engine.NewCoordinator(fx.URL, entries, engine.Config{
		Seed: 9, Concurrency: 10, Rate: rate, Jitter: jitter, RequestTO: 2 * time.Second,
	}, openScope(t), engine.WithAuditSink(audit))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	co.Run(ctx)
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		t.Fatal("scan did not complete before the test's context deadline")
	}

	observedRate := float64(audit.count()) / elapsed.Seconds()
	ceiling := rate / (1 - jitter) * 1.15
	if observedRate > ceiling {
		t.Errorf("observed aggregate rate %.1f req/s exceeds the hard jitter ceiling %.1f for configured rate %.1f (jitter=%.2f)",
			observedRate, ceiling, rate, jitter)
	}
	if observedRate < rate*0.3 {
		t.Errorf("observed aggregate rate %.1f req/s implausibly far below configured rate %.1f", observedRate, rate)
	}
}

// TestCoordinator_SameSeedIsReproducible demonstrates that the audit log's
// recorded seed makes a run replayable (spec §11/§15 DoD #4): the same
// seed against the same target produces the same set of requests.
func TestCoordinator_SameSeedIsReproducible(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()
	words := []string{"admin", "backup", "config.php", "login", "noise1", "noise2"}

	co1, _, audit1 := runScan(t, fx.URL, words, engine.Config{Seed: 123, Concurrency: 3})
	co2, _, audit2 := runScan(t, fx.URL, words, engine.Config{Seed: 123, Concurrency: 3})

	urls1, urls2 := sortedAuditURLs(audit1), sortedAuditURLs(audit2)
	if len(urls1) != len(urls2) {
		t.Fatalf("expected the same request count for the same seed, got %d vs %d", len(urls1), len(urls2))
	}
	for i := range urls1 {
		if urls1[i] != urls2[i] {
			t.Fatalf("expected identical request sets for the same seed; first divergence at index %d: %q vs %q", i, urls1[i], urls2[i])
		}
	}
	if len(co1.Findings()) != len(co2.Findings()) {
		t.Errorf("expected identical finding count across replayed runs, got %d vs %d", len(co1.Findings()), len(co2.Findings()))
	}
}

func sortedAuditURLs(a *collectingAudit) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.records))
	for i, r := range a.records {
		out[i] = r.URL
	}
	sort.Strings(out)
	return out
}

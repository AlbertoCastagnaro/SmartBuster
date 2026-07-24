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

// times returns every recorded request's timestamp, sorted — the raw
// material for the §7 rate-rigor property test's window/interval analysis.
func (a *collectingAudit) times() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]time.Time, len(a.records))
	for i, r := range a.records {
		out[i] = r.Time
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
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
// Phase 6a §7 upgrades this from "average looks right" to the rate cap's
// actual stealth-guarantee shape: a sliding-window mean close to target,
// a 95th-percentile window rate bound (a real upper bound under
// concurrency, not just "average is fine"), and the empirical
// inter-request interval mean tracking the configured JitterSpec. The
// interval *distribution's* precise mean/variance-per-kind claim is
// covered far more reliably at the pure-Limiter level (ratelimit_test.go's
// TestJitterSpec* — no OS/network scheduling noise there); this test's job
// is proving the wiring holds end-to-end, under real concurrency, not
// re-deriving that statistics.
func TestCoordinator_ObservesConfiguredRate(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := make([]string, 300)
	for i := range words {
		words[i] = fmt.Sprintf("candidateword%03d", i)
	}
	entries := loadWordlist(t, words)
	audit := &collectingAudit{}

	const rate = 40.0
	const jitter = 0.3
	co, err := engine.NewCoordinator(fx.URL, entries, engine.Config{
		Seed: 9, Concurrency: 10, Rate: rate, Jitter: jitter, RequestTO: 2 * time.Second,
	}, openScope(t), engine.WithAuditSink(audit))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	co.Run(ctx)
	if ctx.Err() != nil {
		t.Fatal("scan did not complete before the test's context deadline")
	}

	times := audit.times()
	if len(times) < 100 {
		t.Fatalf("need enough samples for the statistical checks to be meaningful, got %d", len(times))
	}

	// Sliding-window aggregate rate (spec §7): window-mean within
	// tolerance of target, and a real 95th-percentile upper bound rather
	// than just "average is fine". Windows are deliberately coarse (500ms)
	// so a handful of samples per window doesn't itself dominate the noise.
	const window = 500 * time.Millisecond
	rates := windowRates(times, window)
	if len(rates) < 4 {
		t.Fatalf("need multiple windows for a percentile to mean anything, got %d", len(rates))
	}
	mean := meanFloat(rates)
	if rel := (mean - rate) / rate; rel > 0.5 || rel < -0.5 {
		t.Errorf("window-mean rate %.1f too far from target %.1f (rel err %.2f)", mean, rate, rel)
	}
	p95 := percentileFloat(rates, 0.95)
	// The hard per-tick ceiling is rate/(1-jitter); across many windows
	// under real concurrency/scheduling noise, allow generous slack on top
	// of it rather than the single-tick bound httpclient's pure test uses.
	ceiling := rate / (1 - jitter) * 1.5
	if p95 > ceiling {
		t.Errorf("95th-percentile window rate %.1f exceeds bound %.1f for target %.1f (jitter=%.2f)", p95, ceiling, rate, jitter)
	}

	// Inter-request interval mean should track base=1/rate (spec §7's
	// jitter-distribution-match, end-to-end).
	intervals := interArrivalSeconds(times)
	base := 1 / rate
	gotMean := meanFloat(intervals)
	if rel := (gotMean - base) / base; rel > 0.6 || rel < -0.6 {
		t.Errorf("empirical inter-request interval mean %.4fs too far from base %.4fs (rel err %.2f)", gotMean, base, rel)
	}
}

// windowRates buckets times into fixed-size windows and returns each
// window's observed rate (count/window duration) — the raw material for a
// sliding-window mean/percentile check.
func windowRates(times []time.Time, window time.Duration) []float64 {
	if len(times) == 0 {
		return nil
	}
	start := times[0]
	buckets := map[int64]int{}
	for _, tm := range times {
		idx := int64(tm.Sub(start) / window)
		buckets[idx]++
	}
	rates := make([]float64, 0, len(buckets))
	for _, n := range buckets {
		rates = append(rates, float64(n)/window.Seconds())
	}
	return rates
}

func meanFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func percentileFloat(vals []float64, p float64) float64 {
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	idx := int(p * float64(len(sorted)-1))
	return sorted[idx]
}

// interArrivalSeconds returns the gaps between consecutive (already sorted)
// timestamps, in seconds.
func interArrivalSeconds(times []time.Time) []float64 {
	if len(times) < 2 {
		return nil
	}
	out := make([]float64, 0, len(times)-1)
	for i := 1; i < len(times); i++ {
		out = append(out, times[i].Sub(times[i-1]).Seconds())
	}
	return out
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

// TestCoordinator_HeaderProfileStableAndRefererChains is spec §5 end-to-end:
// a scan in a mode with Referer enabled sends a stable header-profile
// identity (never rotated per request) and a referer chain — root
// candidates referer the site root, recursion children referer their
// parent directory.
func TestCoordinator_HeaderProfileStableAndRefererChains(t *testing.T) {
	var mu sync.Mutex
	seenHeaders := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenHeaders[r.URL.Path] = r.Header.Clone()
		mu.Unlock()

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

	entries := loadWordlist(t, []string{"admin", "config", "secret"})
	emitter := &collectingEmitter{}
	// Mode "quiet" for its header profile/Referer/OrderJitter shape, but
	// with its slow RateCap (5 req/s) overridden to something fast — this
	// test's fixture needs many calibration probes (extSet grows from tech
	// profiling), and rate isn't what's under test here.
	co, err := engine.NewCoordinator(srv.URL, entries, engine.Config{Seed: 6, MaxDepth: 4, Mode: "quiet", Rate: 500}, openScope(t),
		engine.WithEventEmitter(emitter))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	co.Run(ctx)
	if ctx.Err() != nil {
		t.Fatal("scan did not complete before the test's context deadline")
	}

	found := findingPaths(t, co)
	if !found["/admin"] || !found["/admin/config"] {
		t.Fatalf("expected both /admin and nested /admin/config found; findings=%v", co.Findings())
	}

	mu.Lock()
	defer mu.Unlock()

	// Stable per-session identity: every real request (a probe or a
	// candidate) carries the same chrome-profile User-Agent — never a
	// per-request rotation.
	const wantUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	for path, h := range seenHeaders {
		if path == "/" {
			continue // target profiling (favicon/root GET) is outside the worker's HTTPDoer/header-profile boundary (spec §6 scope note)
		}
		if ua := h.Get("User-Agent"); ua != wantUA {
			t.Errorf("expected every worker-dispatched request (including %s) to carry the stable chrome profile's User-Agent, got %q", path, ua)
		}
		if h.Get("Sec-Fetch-Mode") == "" {
			t.Errorf("expected %s to carry the chrome profile's Sec-Fetch-Mode header", path)
		}
	}

	// Referer chains: /admin is a root candidate (referer = site root);
	// /admin/config is a recursion child of /admin (referer = /admin/).
	if got := seenHeaders["/admin"].Get("Referer"); got != srv.URL+"/" {
		t.Errorf("expected /admin's Referer to be the site root, got %q", got)
	}
	if got := seenHeaders["/admin/config"].Get("Referer"); got != srv.URL+"/admin/" {
		t.Errorf("expected /admin/config's Referer to be its parent dir, got %q", got)
	}
}

// TestCoordinator_ModeLiveSwitch_RaceClean is DoD #1: PATCH mode
// reconfigures pacer/headers/backoff/epsilon via controlCh while a scan is
// running, and must be -race clean — run with `go test -race` to actually
// exercise the guarantee; without -race this just checks it doesn't
// deadlock or panic.
func TestCoordinator_ModeLiveSwitch_RaceClean(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := make([]string, 200)
	for i := range words {
		words[i] = fmt.Sprintf("liveword%03d", i)
	}
	entries := loadWordlist(t, words)

	co, err := engine.NewCoordinator(fx.URL, entries, engine.Config{
		Seed: 21, Concurrency: 6, RequestTO: 2 * time.Second,
	}, openScope(t))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		co.Run(ctx)
		close(done)
	}()

	modes := []string{"quiet", "stealth", "fast", "normal"}
	for i := 0; i < 20; i++ {
		mode := modes[i%len(modes)]
		rate := float64(5 + i)
		concurrency := 3 + i%4
		_ = co.SubmitControl(ctx, engine.ControlCmd{
			Kind: engine.CtrlAdjust, SetMode: &mode, SetRate: &rate, SetConcurrency: &concurrency,
		})
		time.Sleep(2 * time.Millisecond)
	}

	<-done
	if ctx.Err() != nil {
		t.Fatal("scan did not complete before the test's context deadline")
	}
}

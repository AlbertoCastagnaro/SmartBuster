package daemon_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/daemon"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

// blockingTransport (test-local copy: package daemon_test can't reach the
// unexported one in package daemon) simulates a WS client whose TCP write
// buffer never drains.
type blockingTransport struct{ unblock chan struct{} }

func newBlockingTransport() *blockingTransport {
	return &blockingTransport{unblock: make(chan struct{})}
}
func (t *blockingTransport) WriteMessage(b []byte) error { <-t.unblock; return nil }
func (t *blockingTransport) Close() error                { return nil }

// countingAudit implements engine.AuditSink, counting every request the
// coordinator ever writes a record for — the lossless sink spec §2 says
// must stay complete regardless of how any WS client behaves.
type countingAudit struct {
	mu    sync.Mutex
	count int
}

func (a *countingAudit) WriteRequest(engine.AuditRecord) {
	a.mu.Lock()
	a.count++
	a.mu.Unlock()
}
func (a *countingAudit) get() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.count
}

func loadWordlistLocal(t *testing.T, words []string) []wordlist.Entry {
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

// manyHitsServer serves `hits` distinct real paths (word0000i, unique body
// each so they never collapse via the novelty gate) among a much larger
// word* namespace, so a scan against it produces a real burst of discrete
// hit events — the traffic pattern the hub's per-client hit-coalescing lane
// exists for.
func manyHitsServer(hits int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var idx int
		if _, err := fmt.Sscanf(r.URL.Path, "/word%05d", &idx); err == nil && idx < hits {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, fmt.Sprintf("<html><body>Unique hit body number %d, marker %d.</body></html>", idx, idx))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404, nothing here.</body></html>")
	}))
}

// runScanWithHub runs one scan wired through a real Hub (spec §2's
// daemonEmitter path), optionally with a permanently-stalled client
// attached, and returns how long the scan took plus how many audit records
// and findings it produced.
func runScanWithHub(t *testing.T, target string, words []string, cfg engine.Config, attachStalled bool) (elapsed time.Duration, findings []engine.Finding, auditCount int) {
	t.Helper()
	entries := loadWordlistLocal(t, words)
	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}

	hub := daemon.NewHub()
	hubCtx, hubCancel := context.WithCancel(context.Background())
	defer hubCancel()
	go hub.Run(hubCtx)

	if attachStalled {
		tr := newBlockingTransport()
		defer close(tr.unblock)
		hub.Register(daemon.NewClient(hub, tr))
	}

	audit := &countingAudit{}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 10
	}
	if cfg.RequestTO == 0 {
		cfg.RequestTO = 2 * time.Second
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 4
	}
	co, err := engine.NewCoordinator(target, entries, cfg, sc,
		engine.WithEventEmitter(hub.NewEmitter()), engine.WithAuditSink(audit))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	co.Run(ctx)
	elapsed = time.Since(start)
	if ctx.Err() != nil {
		t.Fatal("scan did not complete before the test's context deadline")
	}
	return elapsed, co.Findings(), audit.get()
}

// TestHub_StalledClientDoesNotSlowScanAndAuditStaysComplete is spec §8 DoD
// #1's exact integration assertion: "a stalled WS client does not slow the
// scan (assert scan completion time ≈ unimpaired) and the audit log is
// complete." Runs the same deterministic (seeded) scan with and without a
// permanently-stalled hub client attached and compares.
func TestHub_StalledClientDoesNotSlowScanAndAuditStaysComplete(t *testing.T) {
	const hits = 30
	words := make([]string, 200)
	for i := range words {
		words[i] = fmt.Sprintf("word%05d", i)
	}
	cfg := engine.Config{Seed: 42, Concurrency: 10}

	srvA := manyHitsServer(hits)
	defer srvA.Close()
	baselineElapsed, baselineFindings, baselineAudit := runScanWithHub(t, srvA.URL, words, cfg, false)

	srvB := manyHitsServer(hits)
	defer srvB.Close()
	stalledElapsed, stalledFindings, stalledAudit := runScanWithHub(t, srvB.URL, words, cfg, true)

	t.Logf("baseline: %v, %d findings, %d audit records", baselineElapsed, len(baselineFindings), baselineAudit)
	t.Logf("stalled:  %v, %d findings, %d audit records", stalledElapsed, len(stalledFindings), stalledAudit)

	// Generous margin (not a tight perf bound — this is a correctness test,
	// not a benchmark): a stalled client must not measurably slow the scan,
	// so completion time should stay in the same ballpark, not blow up.
	maxAllowed := baselineElapsed*3 + 500*time.Millisecond
	if stalledElapsed > maxAllowed {
		t.Errorf("scan with a stalled WS client took %v, want <= %v (baseline %v) — the hub must never block the coordinator",
			stalledElapsed, maxAllowed, baselineElapsed)
	}

	if len(stalledFindings) != len(baselineFindings) {
		t.Errorf("stalled-client run found %d findings, baseline found %d — same seed must reproduce the same result regardless of WS client behavior",
			len(stalledFindings), len(baselineFindings))
	}
	if len(stalledFindings) != hits {
		t.Errorf("expected exactly %d findings, got %d", hits, len(stalledFindings))
	}

	if stalledAudit != baselineAudit {
		t.Errorf("stalled-client run wrote %d audit records, baseline wrote %d — the audit log (lossless sink) must be complete regardless of any WS client",
			stalledAudit, baselineAudit)
	}
	if stalledAudit == 0 {
		t.Fatal("expected a non-zero number of audit records")
	}
}

package engine_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// startScan is runScan's non-blocking counterpart: it launches co.Run in its
// own goroutine and returns immediately, so a test can exercise controlCh
// against a scan that's actually in flight — runScan (used everywhere else)
// blocks until the scan finishes, which is no good for pause/resume/stop.
func startScan(t *testing.T, target string, words []string, cfg engine.Config) (co *engine.Coordinator, emitter *collectingEmitter, done <-chan struct{}) {
	t.Helper()
	entries := loadWordlist(t, words)
	emitter = &collectingEmitter{}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 5
	}
	if cfg.RequestTO == 0 {
		cfg.RequestTO = 2 * time.Second
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 4
	}
	co, err := engine.NewCoordinator(target, entries, cfg, openScope(t), engine.WithEventEmitter(emitter))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	t.Cleanup(cancel)
	d := make(chan struct{})
	go func() {
		defer close(d)
		co.Run(ctx)
	}()
	return co, emitter, d
}

// TestControlCh_ConcurrentCommandsRaceClean is this phase's second
// load-bearing concurrency assertion (spec §9): many goroutines hammering
// SubmitControl — the same entry point the daemon's REST handlers use —
// while a scan is actively dispatching, run under `go test -race`. The
// coordinator goroutine is controlCh's sole consumer (applyControl), so
// none of this should ever race with the frontier/dirs/overrides state
// those commands mutate.
func TestControlCh_ConcurrentCommandsRaceClean(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := make([]string, 60)
	for i := range words {
		words[i] = fmt.Sprintf("raceword%02d", i)
	}
	co, _, done := startScan(t, fx.URL, words, engine.Config{Seed: 200, Rate: 40, Concurrency: 5})

	var wg sync.WaitGroup
	submit := func(cmd engine.ControlCmd) {
		defer wg.Done()
		_ = co.SubmitControl(context.Background(), cmd)
	}
	for i := 0; i < 10; i++ {
		wg.Add(6)
		go submit(engine.ControlCmd{Kind: engine.CtrlPause})
		go submit(engine.ControlCmd{Kind: engine.CtrlResume})
		go submit(engine.ControlCmd{Kind: engine.CtrlBoost, Pattern: "raceword01", Factor: 2})
		go submit(engine.ControlCmd{Kind: engine.CtrlDemote, Pattern: "raceword02", Factor: 2})
		go submit(engine.ControlCmd{Kind: engine.CtrlExclude, Pattern: "raceword03"})
		go submit(engine.ControlCmd{Kind: engine.CtrlInject, Terms: []string{fmt.Sprintf("injected%d", i)}})
	}
	wg.Wait()

	// The pause/resume race above can leave the scan paused; force it back
	// on so the scan can actually reach completion. Sent after wg.Wait(), so
	// controlCh's FIFO ordering guarantees it lands after every command above.
	if err := co.SubmitControl(context.Background(), engine.ControlCmd{Kind: engine.CtrlResume}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("scan did not finish after a burst of concurrent control commands")
	}
}

// TestControlCh_PauseHaltsDispatchThenResumeCompletes exercises spec §4's
// "pause halts dispatch, in-flight drains; resume continues" via the exact
// path a REST /pause and /resume handler would use.
func TestControlCh_PauseHaltsDispatchThenResumeCompletes(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := make([]string, 40)
	for i := range words {
		words[i] = fmt.Sprintf("pauseword%02d", i)
	}
	entries := loadWordlist(t, words)
	audit := &collectingAudit{}
	co, err := engine.NewCoordinator(fx.URL, entries, engine.Config{
		Seed: 201, Rate: 30, Concurrency: 5, RequestTO: 2 * time.Second, MaxDepth: 4,
	}, openScope(t), engine.WithAuditSink(audit))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); co.Run(ctx) }()

	time.Sleep(150 * time.Millisecond) // let the scan get going
	if err := co.SubmitControl(context.Background(), engine.ControlCmd{Kind: engine.CtrlPause}); err != nil {
		t.Fatal(err)
	}

	countRecords := func() int { audit.mu.Lock(); defer audit.mu.Unlock(); return len(audit.records) }
	n1 := countRecords()
	time.Sleep(400 * time.Millisecond) // several pacer intervals' worth at rate 30 (~33ms each)
	n2 := countRecords()
	// Small slack for requests already in flight the instant pause landed.
	if n2 > n1+3 {
		t.Errorf("expected dispatch to halt while paused: %d audit records before wait, %d after 400ms paused", n1, n2)
	}

	if err := co.SubmitControl(context.Background(), engine.ControlCmd{Kind: engine.CtrlResume}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("scan did not finish after resume")
	}
	if countRecords() <= n2 {
		t.Error("expected more requests to have been dispatched after resume")
	}
}

// TestControlCh_StopCancelsPromptly exercises spec §4's "stop = cancel": a
// scan sized to run for tens of seconds at its configured rate must
// terminate within a couple of seconds of a CtrlStop.
func TestControlCh_StopCancelsPromptly(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := make([]string, 500)
	for i := range words {
		words[i] = fmt.Sprintf("stopword%03d", i)
	}
	co, _, done := startScan(t, fx.URL, words, engine.Config{Seed: 202, Rate: 25, Concurrency: 5})

	time.Sleep(100 * time.Millisecond)
	if err := co.SubmitControl(context.Background(), engine.ControlCmd{Kind: engine.CtrlStop}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("expected CtrlStop to cancel the scan within a few seconds, not let it run to natural completion")
	}
}

// TestControlCh_PinForcesCandidateNotInWordlist exercises spec §4.1's pin:
// "force-try + top priority (even if not in corpus)".
func TestControlCh_PinForcesCandidateNotInWordlist(t *testing.T) {
	shared := "<html><body>Secret pinned resource, unique marker quebec.</body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/secretconfig" {
			io.WriteString(w, shared)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404, nothing here.</body></html>")
	}))
	defer srv.Close()

	co, _, done := startScan(t, srv.URL, []string{"noise1", "noise2"}, engine.Config{Seed: 203, Rate: 20})

	if err := co.SubmitControl(context.Background(), engine.ControlCmd{Kind: engine.CtrlPin, Pattern: "secretconfig"}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("scan did not finish")
	}

	var found bool
	for _, f := range co.Findings() {
		if strings.HasSuffix(f.URL, "/secretconfig") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected pinned candidate /secretconfig to be found despite never being in the wordlist; findings=%v", co.Findings())
	}
}

// TestControlCh_ExcludeRemovesAndPreventsDispatch exercises spec §4.1's
// exclude: a real hit the wordlist would otherwise find must never appear
// once excluded, while an unrelated real hit still does.
func TestControlCh_ExcludeRemovesAndPreventsDispatch(t *testing.T) {
	fx := fixtures.NewHonest() // real paths include /admin and /backup
	defer fx.Close()

	co, _, done := startScan(t, fx.URL, []string{"admin", "backup", "config.php", "login"},
		engine.Config{Seed: 204, Rate: 15})

	if err := co.SubmitControl(context.Background(), engine.ControlCmd{Kind: engine.CtrlExclude, Pattern: "admin"}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("scan did not finish")
	}

	var sawAdmin, sawBackup bool
	for _, f := range co.Findings() {
		if strings.HasSuffix(f.URL, "/admin") {
			sawAdmin = true
		}
		if strings.HasSuffix(f.URL, "/backup") {
			sawBackup = true
		}
	}
	if sawAdmin {
		t.Errorf("expected excluded /admin to never be dispatched; findings=%v", co.Findings())
	}
	if !sawBackup {
		t.Errorf("expected unrelated /backup to still be found; findings=%v", co.Findings())
	}
}

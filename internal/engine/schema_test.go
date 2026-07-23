package engine_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// knownCategories mirrors events.go's eventCategories map (spec §3): every
// event this package can emit must carry exactly one of these.
var knownCategories = map[engine.Category]bool{
	engine.CategoryScan: true, engine.CategoryCalibration: true, engine.CategoryDiscovery: true,
	engine.CategoryTech: true, engine.CategoryTrap: true, engine.CategoryTelemetry: true,
	engine.CategoryWarning: true, engine.CategoryError: true, engine.CategoryControl: true,
}

func TestEventSchema_EveryEventHasACategory(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	_, emitter, _ := runScan(t, fx.URL, []string{"admin", "backup", "config.php", "login", "noise"},
		engine.Config{Seed: 100})

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	if len(emitter.events) == 0 {
		t.Fatal("expected at least one event")
	}
	for _, ev := range emitter.events {
		if ev.Category == "" {
			t.Errorf("event %q has no Category", ev.Type)
		}
		if !knownCategories[ev.Category] {
			t.Errorf("event %q has unknown Category %q", ev.Type, ev.Category)
		}
	}
}

func TestEventSchema_WarningCarriesStructuredSource(t *testing.T) {
	fx := fixtures.NewSPA()
	defer fx.Close()

	_, emitter, _ := runScan(t, fx.URL,
		[]string{"alpha", "bravo", "charlie", "delta", "echo"}, engine.Config{Seed: 101})

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	var found bool
	for _, ev := range emitter.events {
		if ev.Type != engine.EventWarning {
			continue
		}
		if ev.Category != engine.CategoryWarning {
			t.Errorf("warning event has Category %q, want %q", ev.Category, engine.CategoryWarning)
		}
		var wp engine.WarnPayload
		if err := json.Unmarshal(ev.Payload, &wp); err != nil {
			t.Fatalf("warning event Payload didn't decode as WarnPayload: %v (payload=%s)", err, ev.Payload)
		}
		if wp.Source == "" {
			t.Errorf("warning event Payload has empty Source (message=%q) — spec forbids inferring it from Message prefixes", ev.Message)
		}
		if wp.Source == "spa" {
			found = true
		}
	}
	if !found {
		t.Error("expected the SPA catch-all warning to carry WarnPayload{Source:\"spa\"}")
	}
}

func TestEventSchema_BranchPrunedOnDuplicateContent(t *testing.T) {
	shared := "<html><body>Identical payload served from two different paths, unique marker zulu.</body></html>"
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

	_, emitter, _ := runScan(t, srv.URL, []string{"widget", "gadget"}, engine.Config{Seed: 102, MaxDepth: 3})

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	for _, ev := range emitter.events {
		if ev.Type == engine.EventBranchPruned && ev.Category == engine.CategoryTrap &&
			strings.Contains(ev.Message, "duplicate content") {
			return
		}
	}
	t.Error("expected a branch.pruned(trap) event at the novelty-gate duplicate-content moment")
}

func TestEventSchema_BranchPrunedOnTarpit(t *testing.T) {
	fx := fixtures.NewTarpit(460 * time.Millisecond)
	defer fx.Close()

	words := make([]string, 40)
	for i := range words {
		words[i] = "word" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}

	_, emitter, _ := runScan(t, fx.URL, words, engine.Config{
		Seed: 103, Concurrency: 3, RequestTO: 500 * time.Millisecond,
	})

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	var sawTrapDetected, sawBranchPruned bool
	for _, ev := range emitter.events {
		if ev.Type == engine.EventTrapDetected {
			sawTrapDetected = true
		}
		if ev.Type == engine.EventBranchPruned && ev.Category == engine.CategoryTrap {
			sawBranchPruned = true
		}
	}
	if !sawTrapDetected || !sawBranchPruned {
		t.Errorf("expected both trap.detected and branch.pruned as two distinct moments (detected=%v, pruned=%v)",
			sawTrapDetected, sawBranchPruned)
	}
}

func TestEventSchema_ErrorEventOnConnectionFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := "http://" + ln.Addr().String()
	ln.Close() // nothing listens here anymore: every request fails fast

	_, emitter, _ := runScan(t, target, []string{"admin"}, engine.Config{
		Seed: 104, RequestTO: 500 * time.Millisecond,
	})

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	var found bool
	for _, ev := range emitter.events {
		if ev.Type != engine.EventError {
			continue
		}
		if ev.Category != engine.CategoryError {
			t.Errorf("error event has Category %q, want %q", ev.Category, engine.CategoryError)
		}
		var ep engine.ErrorPayload
		if err := json.Unmarshal(ev.Payload, &ep); err != nil {
			t.Fatalf("error event Payload didn't decode as ErrorPayload: %v (payload=%s)", err, ev.Payload)
		}
		if ep.Kind == "" || ep.URL == "" {
			t.Errorf("error event Payload missing Kind/URL: %+v", ep)
		}
		found = true
	}
	if !found {
		t.Error("expected at least one error event against a target with nothing listening")
	}
}

func TestEventSchema_StatsAndSnapshotOnTicker(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := make([]string, 30)
	for i := range words {
		words[i] = "word" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
	}
	// Slow enough (Rate req/s gates the coordinator's sole dispatch point)
	// that the scan outlives both StatsInterval (400ms) and SnapshotInterval
	// (1s) comfortably, without approaching runScan's 15s ctx deadline.
	_, emitter, _ := runScan(t, fx.URL, words, engine.Config{Seed: 105, Rate: 15})

	emitter.mu.Lock()
	defer emitter.mu.Unlock()
	var sawStats, sawSnapshot bool
	for _, ev := range emitter.events {
		if ev.Type == engine.EventStats {
			var sp engine.StatsPayload
			if err := json.Unmarshal(ev.Payload, &sp); err != nil {
				t.Fatalf("stats event Payload didn't decode: %v", err)
			}
			if ev.Category != engine.CategoryTelemetry {
				t.Errorf("stats event has Category %q, want %q", ev.Category, engine.CategoryTelemetry)
			}
			sawStats = true
		}
		if ev.Type == engine.EventSnapshot {
			var snap engine.SnapshotPayload
			if err := json.Unmarshal(ev.Payload, &snap); err != nil {
				t.Fatalf("snapshot event Payload didn't decode: %v", err)
			}
			sawSnapshot = true
		}
	}
	if !sawStats {
		t.Error("expected at least one stats event on STATS_INTERVAL")
	}
	if !sawSnapshot {
		t.Error("expected at least one frontier.snapshot event on SNAPSHOT_INTERVAL")
	}
}

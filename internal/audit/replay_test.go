package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// TestReplay_FromAuditLogReproducesRun is the literal DoD #4 check: not
// just "same Config.Seed produces the same requests" (a determinism claim
// about the coordinator), but "the audit log on disk is enough to replay
// the run" — read a real audit.jsonl's header back, reconstruct the
// scan's config purely from what's in that file, rerun, and confirm the
// second run's own audit log matches the first.
func TestReplay_FromAuditLogReproducesRun(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	wlPath := filepath.Join(t.TempDir(), "words.txt")
	words := []string{"admin", "backup", "config.php", "login", "noise1", "noise2"}
	if err := os.WriteFile(wlPath, []byte(strings.Join(words, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := wordlist.Load(wlPath)
	if err != nil {
		t.Fatal(err)
	}
	wlHash, err := wordlist.Hash(wlPath)
	if err != nil {
		t.Fatal(err)
	}

	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// --- first run: write a real audit.jsonl to disk ---
	auditPath1 := filepath.Join(t.TempDir(), "audit.jsonl")
	findings1 := runAndAudit(t, fx.URL, entries, wlPath, wlHash, 555, sc, auditPath1)

	// --- replay: read the header BACK from the file, not from memory ---
	h, err := ReadHeader(auditPath1)
	if err != nil {
		t.Fatal(err)
	}
	if h.Seed != 555 || h.Wordlist != wlPath || len(h.Targets) != 1 || h.Targets[0] != fx.URL {
		t.Fatalf("expected header to carry everything needed to replay, got %+v", h)
	}

	replayEntries, err := wordlist.Load(h.Wordlist)
	if err != nil {
		t.Fatal(err)
	}
	auditPath2 := filepath.Join(t.TempDir(), "audit-replay.jsonl")
	findings2 := runAndAudit(t, h.Targets[0], replayEntries, h.Wordlist, wlHash, h.Seed, sc, auditPath2)

	// --- compare: the replayed run made the same requests and found the same things ---
	urls1, urls2 := sortedRequestURLs(t, auditPath1), sortedRequestURLs(t, auditPath2)
	if len(urls1) != len(urls2) {
		t.Fatalf("expected the same request count when replaying from the log, got %d vs %d", len(urls1), len(urls2))
	}
	for i := range urls1 {
		if urls1[i] != urls2[i] {
			t.Fatalf("replayed run diverged at request %d: %q vs %q", i, urls1[i], urls2[i])
		}
	}
	if len(findings1) != len(findings2) {
		t.Fatalf("expected the same findings when replaying from the log, got %d vs %d", len(findings1), len(findings2))
	}
}

func runAndAudit(t *testing.T, target string, entries []wordlist.Entry, wlPath, wlHash string, seed int64, sc *scope.Scope, auditPath string) []engine.Finding {
	t.Helper()
	w, err := New(auditPath)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := w.WriteHeader(Header{
		Targets: []string{target}, Wordlist: wlPath, WordlistHash: wlHash,
		Seed: seed, Concurrency: 3,
	}); err != nil {
		t.Fatal(err)
	}

	co, err := engine.NewCoordinator(target, entries, engine.Config{
		Seed: seed, Concurrency: 3, RequestTO: 2 * time.Second,
	}, sc, engine.WithAuditSink(w))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	co.Run(ctx)
	if ctx.Err() != nil {
		t.Fatal("scan did not complete before the test's context deadline")
	}
	return co.Findings()
}

// sortedRequestURLs reads the URLs of every non-header line from a real
// audit.jsonl file on disk.
func sortedRequestURLs(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var urls []string
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // skip the header line
		}
		var e struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("invalid JSON line: %v", err)
		}
		urls = append(urls, e.URL)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(urls)
	return urls
}

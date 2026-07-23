package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

// TestSession_BuildAndRestoreRoundTrip is spec §8 DoD #5's fidelity
// assertion: serialize -> load -> deep-equal on the reconstructable fields.
// It exercises buildSnapshot/restoreSnapshot directly (a white-box test, so
// it can reach into both coordinators' private state) rather than through
// controlCh/Save, since the interesting thing to verify here is the
// serialization itself — controlCh's delivery is already covered by
// control_test.go's race tests.
func TestSession_BuildAndRestoreRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/admin":
			io.WriteString(w, "<html><body>Admin area, unique marker alpha.</body></html>")
		case "/admin/config":
			io.WriteString(w, "<html><body>Admin config nested, unique marker bravo.</body></html>")
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<html><body>404, nothing here.</body></html>")
		}
	}))
	defer srv.Close()

	entries := loadWordlistEntries(t, []string{"admin", "config", "secret", "one", "two", "three"})
	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewCoordinator(srv.URL, entries, Config{Seed: 55, Rate: 15, Concurrency: 2, MaxDepth: 4}, sc)
	if err != nil {
		t.Fatal(err)
	}

	// Cancel mid-scan (short deadline against a rate-limited target) so the
	// coordinator still holds live, non-trivial state — a resident
	// frontier, in-progress dirs — for buildSnapshot to capture, rather
	// than the fully-drained state a completed scan would leave.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c.Run(ctx)

	before := c.buildSnapshot()

	// Round-trip through JSON, exactly as the session file on disk would.
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var reloaded SessionState
	if err := json.Unmarshal(raw, &reloaded); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	c2, err := NewCoordinatorFromSnapshot(reloaded, entries, sc)
	if err != nil {
		t.Fatalf("NewCoordinatorFromSnapshot: %v", err)
	}
	if !c2.resumed {
		t.Error("expected resumed=true on a coordinator built from a snapshot")
	}

	assertCandidateSetsEqual(t, "frontier", c.frontier.All(), c2.frontier.All())

	if len(c.baselines) != len(c2.baselines) {
		t.Errorf("baselines: got %d dirs, want %d", len(c2.baselines), len(c.baselines))
	}
	for dir, b := range c.baselines {
		b2, ok := c2.baselines[dir]
		if !ok {
			t.Errorf("baseline for dir %q missing after restore", dir)
			continue
		}
		if b.RepStatus != b2.RepStatus || b.NoiseFloor != b2.NoiseFloor || b.IsSPA != b2.IsSPA || b.IsWildcard != b2.IsWildcard {
			t.Errorf("baseline for dir %q not preserved: got %+v, want %+v", dir, *b2, *b)
		}
	}

	if len(c.findings) != len(c2.findings) {
		t.Errorf("findings: got %d, want %d", len(c2.findings), len(c.findings))
	}

	if c.scorer != nil && c2.scorer != nil {
		m1, m2 := c.scorer.markov.MarshalState(), c2.scorer.markov.MarshalState()
		if m1.Trained != m2.Trained {
			t.Errorf("markov.Trained: got %d, want %d", m2.Trained, m1.Trained)
		}
		if len(m1.Counts) != len(m2.Counts) {
			t.Errorf("markov.Counts: got %d contexts, want %d", len(m2.Counts), len(m1.Counts))
		}

		a1, a2 := c.scorer.assoc.MarshalState(), c2.scorer.assoc.MarshalState()
		if len(a1.HitTerms) != len(a2.HitTerms) {
			t.Errorf("assoc.HitTerms: got %d dirs, want %d", len(a2.HitTerms), len(a1.HitTerms))
		}

		if len(c.scorer.dirCtx) != len(c2.scorer.dirCtx) {
			t.Errorf("dirCtx: got %d dirs, want %d", len(c2.scorer.dirCtx), len(c.scorer.dirCtx))
		}
	}

	visited1, visited2 := c.crawlVisited.snapshot(), c2.crawlVisited.snapshot()
	if len(visited1) != len(visited2) {
		t.Errorf("visitedSet: got %d entries, want %d", len(visited2), len(visited1))
	}

	if len(c.dirs) != len(c2.dirs) {
		t.Errorf("dirs: got %d, want %d (dirs mid-CALIBRATING when saved restart instead of round-tripping, so a mismatch here may indicate that, not a bug)", len(c2.dirs), len(c.dirs))
	}
}

func assertCandidateSetsEqual(t *testing.T, label string, a, b []Candidate) {
	t.Helper()
	if len(a) != len(b) {
		t.Errorf("%s: got %d candidates, want %d", label, len(b), len(a))
		return
	}
	key := func(c Candidate) string { return c.ParentDir + "/" + c.Path }
	sort.Slice(a, func(i, j int) bool { return key(a[i]) < key(a[j]) })
	sort.Slice(b, func(i, j int) bool { return key(b[i]) < key(b[j]) })
	for i := range a {
		if key(a[i]) != key(b[i]) {
			t.Errorf("%s[%d]: got %q, want %q", label, i, key(b[i]), key(a[i]))
			continue
		}
		if a[i].Score != b[i].Score {
			t.Errorf("%s %q: Score got %v, want %v", label, key(a[i]), b[i].Score, a[i].Score)
		}
	}
}

func loadWordlistEntries(t *testing.T, words []string) []wordlist.Entry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "words.txt")
	content := ""
	for _, w := range words {
		content += w + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := wordlist.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

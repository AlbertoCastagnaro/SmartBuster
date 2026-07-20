package engine_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/corpus"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// TestCoordinator_CorpusMode_FullRecall is the corpus-default-path
// counterpart of TestCoordinator_Honest_FullRecall: with no -w, the
// coordinator seeds from the corpus (spec §0 contract E) and must still
// find every real path — the fixture's real paths ("admin", "backup",
// "config.php", "login") are all present in the embedded minimal corpus.
func TestCoordinator_CorpusMode_FullRecall(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	sc := openScope(t)
	co, err := engine.NewCoordinator(fx.URL, nil, engine.Config{Seed: 100, Concurrency: 5, RequestTO: 2 * time.Second}, sc)
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
	for _, real := range fx.RealPaths {
		if !found[real] {
			t.Errorf("expected %s found via the corpus default path; findings=%v", real, co.Findings())
		}
	}
}

// wpPlantedPaths are distinctive, non-root paths a WordPress site would
// serve, all present in the embedded corpus's wordpress-tagged term list.
var wpPlantedPaths = []string{"/wp-login.php", "/xmlrpc.php", "/wp-cron.php"}

func newWordPressPlantedServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			w.Header().Set("X-Generator", "WordPress 6.4")
			http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "0123456789abcdef0123456789abcdef"})
			io.WriteString(w, "<html><body>Welcome</body></html>")
			return
		}
		for _, p := range wpPlantedPaths {
			if r.URL.Path == p {
				io.WriteString(w, "<html><body>planted: "+p+"</body></html>")
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
}

// requestsToDiscoverAll returns the 1-based count of non-probe requests
// (in completion order) needed until every path in want has been
// requested at least once.
func requestsToDiscoverAll(records []engine.AuditRecord, want []string) int {
	remaining := map[string]bool{}
	for _, p := range want {
		remaining[p] = true
	}
	n := 0
	for _, r := range records {
		if r.IsProbe {
			continue
		}
		n++
		delete(remaining, urlPathOf(r.URL))
		if len(remaining) == 0 {
			return n
		}
	}
	return -1 // not all discovered
}

func urlPathOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Path
}

// wpStackProfile approximates the profile newWordPressPlantedServer's
// signals actually produce (WordPress via header, PHP via cookie —
// mirrored from the real detection this package's other tests observe),
// used to give the flat baseline the *same* expansion breadth as the real
// corpus-mode run: only the resulting order should differ.
func wpStackProfile() *profile.TargetProfile {
	return &profile.TargetProfile{Tech: map[string]*profile.Tech{
		"WordPress": {Name: "WordPress", Category: "cms", Confidence: 0.7, Layer: profile.LayerBackend},
		"PHP":       {Name: "PHP", Category: "language", Confidence: 0.97, Layer: profile.LayerBackend},
	}}
}

// buildFlatCommonalityWordlist writes a flat wordlist file (Phase 1 style)
// from the embedded corpus's own term universe, expanded exactly as the
// real corpus-mode run would expand it (same stems/backups, via
// wpStackProfile) but ordered by pure BasePrio — no MatchScore tech boost
// — the "Phase 1 flat-order baseline" spec §9 DoD assertion 8 asks for.
// Holding the candidate universe identical and varying only the ordering
// isolates exactly what profile-driven reordering buys.
//
// Queries every (term,tags) pair directly rather than going through
// corpus.Select, since Select's tag gate (spec §5.1) is itself part of
// what corpus mode does differently from a flat wordlist — a flat file
// has no notion of tags at all, so nothing should be gated out here.
func buildFlatCommonalityWordlist(t *testing.T) string {
	t.Helper()
	db, err := corpus.Default()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT t.term, t.type, t.weight, group_concat(DISTINCT tt.tag)
		FROM terms t JOIN term_tags tt ON tt.term_id = t.id
		GROUP BY t.id`)
	if err != nil {
		t.Fatal(err)
	}
	var all []corpus.Candidate
	for rows.Next() {
		var term, tagCSV string
		var typ int
		var weight float64
		if err := rows.Scan(&term, &typ, &weight, &tagCSV); err != nil {
			t.Fatal(err)
		}
		all = append(all, corpus.Candidate{
			Path: term, Type: corpus.TermType(typ), BasePrio: weight, Tags: strings.Split(tagCSV, ","),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	expanded := corpus.Expand(all, wpStackProfile(), corpus.DefaultTechBoostW)
	sort.SliceStable(expanded, func(i, j int) bool { return expanded[i].BasePrio > expanded[j].BasePrio })

	path := filepath.Join(t.TempDir(), "flat.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, c := range expanded {
		if _, err := f.WriteString(c.Path + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// TestCoordinator_CorpusMode_WordPressPathsFoundFasterThanFlatBaseline is
// spec §9 DoD assertion 8: a scan against a WordPress-signaling target
// reaches its planted wp-paths in materially fewer requests than the
// Phase 1 flat-order baseline over the same term universe.
func TestCoordinator_CorpusMode_WordPressPathsFoundFasterThanFlatBaseline(t *testing.T) {
	flatWordlistPath := buildFlatCommonalityWordlist(t)

	runOnce := func(t *testing.T, wordlistPath string) int {
		t.Helper()
		srv := newWordPressPlantedServer()
		defer srv.Close()

		var entries []wordlist.Entry
		if wordlistPath != "" {
			var err error
			entries, err = wordlist.Load(wordlistPath)
			if err != nil {
				t.Fatal(err)
			}
		}

		audit := &collectingAudit{}
		co, err := engine.NewCoordinator(srv.URL, entries, engine.Config{
			Seed: 200, Concurrency: 1, RequestTO: 2 * time.Second, Wordlist: wordlistPath,
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

		audit.mu.Lock()
		records := append([]engine.AuditRecord(nil), audit.records...)
		audit.mu.Unlock()

		n := requestsToDiscoverAll(records, wpPlantedPaths)
		if n < 0 {
			t.Fatalf("planted WP paths never all discovered; got %d audit records", len(records))
		}
		return n
	}

	corpusRequests := runOnce(t, "")
	baselineRequests := runOnce(t, flatWordlistPath)

	t.Logf("requests to discover all planted WP paths: corpus(tech-boosted)=%d flat-baseline=%d", corpusRequests, baselineRequests)
	if corpusRequests >= baselineRequests {
		t.Errorf("expected the corpus's tech-boosted ordering to reach the planted WP paths in fewer requests than the flat baseline: corpus=%d, baseline=%d",
			corpusRequests, baselineRequests)
	}
}

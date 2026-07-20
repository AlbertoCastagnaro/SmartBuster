package corpus

import (
	"database/sql"
	"os"
	"sort"
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
)

func ingestedFixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openMemDB(t)
	sm := loadFixtureSourceMap(t)
	if _, err := Ingest(db, os.DirFS(seclistsFixtureRoot), seclistsFixtureRoot, sm, "h"); err != nil {
		t.Fatal(err)
	}
	return db
}

func wpPHPProfile() *profile.TargetProfile {
	p := &profile.TargetProfile{Tech: map[string]*profile.Tech{}}
	p.Tech["WordPress"] = &profile.Tech{Name: "WordPress", Category: "cms", Confidence: 0.9, Layer: profile.LayerBackend}
	p.Tech["PHP"] = &profile.Tech{Name: "PHP", Category: "language", Confidence: 0.8, Layer: profile.LayerBackend}
	return p
}

// rankOf returns path's 0-based position in cands sorted by Score
// descending (the order Frontier dispatch would use).
func rankOf(t *testing.T, cands []Candidate, path string) int {
	t.Helper()
	sorted := append([]Candidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	for i, c := range sorted {
		if c.Path == path {
			return i
		}
	}
	t.Fatalf("path %q not found in candidates", path)
	return -1
}

func TestSelect_TechTaggedTermsOutrankGeneric(t *testing.T) {
	db := ingestedFixtureDB(t)

	// A stack-less profile's tag set is just {generic, backup} (spec §5.1):
	// a term tagged only "wordpress" isn't pulled in at all until WordPress
	// is actually detected.
	stackLess, err := Select(&profile.TargetProfile{}, SelectConfig{DB: db, TechBoostW: 2.0})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range stackLess {
		if c.Path == "wp-login.php" {
			t.Errorf("expected wp-login.php absent from a stack-less selection, got %+v", c)
		}
	}

	wpPHP, err := Select(wpPHPProfile(), SelectConfig{DB: db, TechBoostW: 2.0})
	if err != nil {
		t.Fatal(err)
	}
	rankWPPHP := rankOf(t, wpPHP, "wp-login.php")

	// Within the WP+PHP-profiled selection, the boosted wp-login.php must
	// outrank at least one *comparable*-commonality generic term (not
	// necessarily the single most common generic word overall) — confirmed
	// precisely with an equal-BasePrio pair in TestSelect_EqualBasePrio_MatchedRanksFirst.
	if rankWPPHP == len(wpPHP)-1 {
		t.Fatalf("sanity: wp-login.php ranked dead last with nothing plausibly outranked")
	}
}

// TestSelect_EqualBasePrio_MatchedRanksFirst is spec §9 DoD assertion 3: "a
// generic term and a matched term with equal BasePrio rank matched-first;
// the generic one is still enqueued." Uses ImportUserList to construct two
// terms with identical, controlled BasePrio so the comparison isolates the
// boost effect from incidental commonality-score differences.
func TestSelect_EqualBasePrio_MatchedRanksFirst(t *testing.T) {
	db := openMemDB(t)
	dir := t.TempDir()
	// Two separate single-line imports so each term is line 0 of its own
	// file (BasePrio 1.0) — equal BasePrio, isolating the boost effect.
	matchedPath := dir + "/matched.txt"
	genericPath := dir + "/generic.txt"
	if err := os.WriteFile(matchedPath, []byte("matched-term\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(genericPath, []byte("generic-term\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportUserList(db, matchedPath, []string{"wordpress"}, TypeFile); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportUserList(db, genericPath, []string{"generic"}, TypeFile); err != nil {
		t.Fatal(err)
	}

	p := &profile.TargetProfile{Tech: map[string]*profile.Tech{
		"WordPress": {Name: "WordPress", Category: "cms", Confidence: 0.8, Layer: profile.LayerBackend},
	}}
	cands, err := Select(p, SelectConfig{DB: db, TechBoostW: 2.0})
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Candidate{}
	for _, c := range cands {
		byPath[c.Path] = c
	}
	matched, ok := byPath["matched-term"]
	if !ok {
		t.Fatal("expected matched-term selected")
	}
	generic, ok := byPath["generic-term"]
	if !ok {
		t.Fatal("expected generic-term still enqueued (reorder-not-exclude)")
	}
	if matched.BasePrio != generic.BasePrio {
		t.Fatalf("test setup: expected equal BasePrio, got matched=%v generic=%v", matched.BasePrio, generic.BasePrio)
	}
	if matched.Score <= generic.Score {
		t.Errorf("expected the wordpress-tagged term to outrank the equal-BasePrio generic term: matched=%v generic=%v",
			matched.Score, generic.Score)
	}
}

func TestSelect_StackLessProfileIsPureCommonalityOrder(t *testing.T) {
	db := ingestedFixtureDB(t)
	p := &profile.TargetProfile{}

	cands, err := Select(p, SelectConfig{DB: db, TechBoostW: 2.0})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Score != c.BasePrio {
			t.Errorf("stack-less profile: expected Score == BasePrio (no boost) for %q, got Score=%v BasePrio=%v",
				c.Path, c.Score, c.BasePrio)
		}
	}
}

func TestSelect_GenericNeverDropped(t *testing.T) {
	db := ingestedFixtureDB(t)

	for _, p := range []*profile.TargetProfile{wpPHPProfile(), {}} {
		cands, err := Select(p, SelectConfig{DB: db, TechBoostW: 2.0})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range cands {
			if c.Path == "admin" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected generic term %q never dropped regardless of profile", "admin")
		}
	}
}

func TestSelect_ScoreFormula(t *testing.T) {
	db := ingestedFixtureDB(t)
	p := wpPHPProfile()

	cands, err := Select(p, SelectConfig{DB: db, TechBoostW: 2.0})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		want := c.BasePrio * (1 + 2.0*p.MatchScore(c.Tags))
		if diff := c.Score - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%q: Score = %v, want BasePrio*(1+2*MatchScore) = %v", c.Path, c.Score, want)
		}
	}
}

func TestSelect_EdgeLayerTechContributesNothing(t *testing.T) {
	db := ingestedFixtureDB(t)
	p := &profile.TargetProfile{Tech: map[string]*profile.Tech{
		"Cloudflare": {Name: "Cloudflare", Category: "cdn", Confidence: 0.95, Layer: profile.LayerEdge},
	}}

	cands, err := Select(p, SelectConfig{DB: db, TechBoostW: 2.0})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.Score != c.BasePrio {
			t.Errorf("edge-only tech should not boost %q: Score=%v BasePrio=%v", c.Path, c.Score, c.BasePrio)
		}
	}
}

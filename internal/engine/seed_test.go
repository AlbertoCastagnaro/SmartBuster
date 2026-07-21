package engine_test

import (
	"strings"
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

func findingByPath(findings []engine.Finding, path string) (engine.Finding, bool) {
	for _, f := range findings {
		if strings.HasSuffix(f.URL, path) {
			return f, true
		}
	}
	return engine.Finding{}, false
}

// TestSeed_RobotsAndSitemap_DeepAncestorChainAndDedup covers spec §7 DoD #1
// (robots Disallow/Allow + Sitemap: line), #2 (sitemap urlset), #4 (a deep
// Disallow seed materializes its ancestor chain and gets confirmed), and #5
// (a seed landing on a path the wordlist already has merges into one
// candidate with unioned provenance) all against one fixture.
func TestSeed_RobotsAndSitemap_DeepAncestorChainAndDedup(t *testing.T) {
	fx := fixtures.NewSeedTarget()
	defer fx.Close()

	// "shared" is also declared via robots.txt's Allow: line — the dedup
	// case. A handful of decoys keep the wordlist non-trivial.
	words := []string{"shared", "decoy1", "decoy2", "decoy3", "decoy4"}

	co, _, _ := runScan(t, fx.URL, words, engine.Config{
		Seed: 10, Robots: true, Sitemap: true,
	})
	findings := co.Findings()

	deep, ok := findingByPath(findings, "/old/admin/secret.php")
	if !ok {
		t.Fatalf("deep robots.txt Disallow seed never confirmed; findings=%+v", findings)
	}
	if deep.Provenance != "robots:disallow" {
		t.Errorf("deep seed Provenance = %q, want %q", deep.Provenance, "robots:disallow")
	}

	fromSitemap, ok := findingByPath(findings, "/from-sitemap")
	if !ok {
		t.Fatalf("sitemap-declared path never confirmed; findings=%+v", findings)
	}
	if fromSitemap.Provenance != "sitemap" {
		t.Errorf("sitemap finding Provenance = %q, want %q", fromSitemap.Provenance, "sitemap")
	}

	shared, ok := findingByPath(findings, "/shared")
	if !ok {
		t.Fatalf("/shared (in both the wordlist and robots.txt Allow) never confirmed; findings=%+v", findings)
	}
	if !strings.Contains(shared.Provenance, "wordlist") || !strings.Contains(shared.Provenance, "robots:allow") {
		t.Errorf("/shared Provenance = %q, want it to mention both wordlist and robots:allow (unioned, spec §3)", shared.Provenance)
	}
}

// TestSeed_StaleAncestor_NotConfirmed covers spec §7 DoD #4's second half:
// a seed whose ancestor doesn't really exist is never confirmed, even
// though its ancestor chain gets materialized and calibrated regardless —
// stale-seed pruning falls out of ordinary baseline classification, with no
// special-casing.
func TestSeed_StaleAncestor_NotConfirmed(t *testing.T) {
	fx := fixtures.NewHonest() // only /admin, /backup, /config.php, /login are real; everything else 404s
	defer fx.Close()
	stub := fixtures.NewStubCDX(fx.Server, [][2]string{
		{"/fake/deep/secret.php", "20230101000000"},
	})
	defer stub.Close()

	co, emitter, _ := runScan(t, fx.URL, []string{"admin", "backup"}, engine.Config{
		Seed: 11, Wayback: true, WaybackURL: stub.URL,
	})

	for _, f := range co.Findings() {
		if strings.Contains(f.URL, "/fake") {
			t.Errorf("a wholly fake ancestor chain must never be confirmed, got finding %+v", f)
		}
	}

	var sawFake, sawFakeDeep bool
	for _, ev := range emitter.events {
		if ev.Type == engine.EventCalibrationDone {
			switch ev.Dir {
			case "/fake":
				sawFake = true
			case "/fake/deep":
				sawFakeDeep = true
			}
		}
	}
	if !sawFake || !sawFakeDeep {
		t.Errorf("expected /fake and /fake/deep to still be materialized and calibrated (just never confirmed); sawFake=%v sawFakeDeep=%v", sawFake, sawFakeDeep)
	}
}

// TestSeed_Wayback_FindsDeepPathAndFiltersAssets covers spec §7 DoD #3: CDX
// rows are parsed, scope/asset filtering is applied (a planted .png must
// never surface as a finding), and a genuinely deep, real historical path is
// found end-to-end via the Wayback source.
func TestSeed_Wayback_FindsDeepPathAndFiltersAssets(t *testing.T) {
	fx := fixtures.NewSeedTarget()
	defer fx.Close()
	stub := fixtures.NewStubCDX(fx.Server, [][2]string{
		{"/old/admin/secret.php", "20260101000000"},
		{"/decoy-asset.png", "20260101000000"},
	})
	defer stub.Close()

	co, _, audit := runScan(t, fx.URL, []string{"decoy"}, engine.Config{
		Seed: 12, Wayback: true, WaybackURL: stub.URL,
	})

	if _, ok := findingByPath(co.Findings(), "/old/admin/secret.php"); !ok {
		t.Fatalf("deep Wayback seed never confirmed; findings=%+v", co.Findings())
	}
	for _, r := range audit.records {
		if strings.Contains(r.URL, "decoy-asset.png") {
			t.Errorf("static-asset noise from Wayback should be filtered before ever being requested, got %s", r.URL)
		}
	}
}

// TestSeed_GracefulDegradation covers spec §7 DoD #7: a missing robots.txt/
// sitemap.xml is not an error, and a Wayback endpoint that can't be reached
// produces a warning event, zero seeds, and no crash — the scan still
// completes normally on the ordinary wordlist.
func TestSeed_GracefulDegradation(t *testing.T) {
	fx := fixtures.NewHonest() // no robots.txt / sitemap.xml at all (404s)
	defer fx.Close()

	co, emitter, _ := runScan(t, fx.URL, []string{"admin", "backup"}, engine.Config{
		Seed: 13, Robots: true, Sitemap: true, Wayback: true, WaybackURL: "http://127.0.0.1:1",
	})

	if _, ok := findingByPath(co.Findings(), "/admin"); !ok {
		t.Error("ordinary wordlist findings should be unaffected by seeding failures")
	}

	sawWaybackWarning := false
	for _, ev := range emitter.events {
		if ev.Type == engine.EventWarning && strings.Contains(ev.Message, "wayback") {
			sawWaybackWarning = true
		}
	}
	if !sawWaybackWarning {
		t.Error("expected a warning event when the Wayback endpoint is unreachable")
	}
}

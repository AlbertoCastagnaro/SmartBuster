package engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// TestHarvest_CrawlFindsLinkedPathWithNoPageRefetch covers the DoD
// linked_paths case (spec §8 #1): a path only reachable via an a[href] on
// an ordinary wordlist-fetched page (/admin) is crawled, confirmed, and
// attributed to crawl:html — and /admin itself is fetched exactly once
// (harvesting never re-fetches the page it piggybacks on).
func TestHarvest_CrawlFindsLinkedPathWithNoPageRefetch(t *testing.T) {
	fx := fixtures.NewCrawlTarget()
	defer fx.Close()

	co, _, audit := runScan(t, fx.URL, []string{"admin", "shared", "decoy1", "decoy2"}, engine.Config{
		Seed: 20, Crawl: true, JSHarvest: true,
	})

	f, ok := findingByPath(co.Findings(), "/secret-page")
	if !ok {
		t.Fatalf("linked path never confirmed; findings=%+v", co.Findings())
	}
	if f.Provenance != "crawl:html" {
		t.Errorf("Provenance = %q, want %q", f.Provenance, "crawl:html")
	}

	adminReqs := 0
	for _, r := range audit.records {
		if r.URL == fx.URL+"/admin" {
			adminReqs++
		}
	}
	if adminReqs != 1 {
		t.Errorf("/admin fetched %d times, want exactly 1 (crawling must not re-fetch the source page)", adminReqs)
	}
}

// TestHarvest_JSEndpointsExtractedAndConfirmed covers the DoD js_endpoints
// case (spec §8 #2): endpoints referenced from a fetched bundle are
// extracted, enqueued, calibration-validated, and found.
func TestHarvest_JSEndpointsExtractedAndConfirmed(t *testing.T) {
	fx := fixtures.NewCrawlTarget()
	defer fx.Close()

	co, _, _ := runScan(t, fx.URL, []string{"admin", "decoy1"}, engine.Config{
		Seed: 21, Crawl: true, JSHarvest: true,
	})

	for _, path := range []string{"/api/v1/users", "/internal/status"} {
		f, ok := findingByPath(co.Findings(), path)
		if !ok {
			t.Errorf("JS-harvested endpoint %s never confirmed; findings=%+v", path, co.Findings())
			continue
		}
		if f.Provenance != "crawl:js" {
			t.Errorf("%s Provenance = %q, want %q", path, f.Provenance, "crawl:js")
		}
	}
}

// TestHarvest_JSNoiseFilteredOut covers the DoD js_noise case (spec §8 #3):
// a bare mime-type string, a template-literal interpolation, and a quoted
// fake-regex string must never be requested — only the bundle's real paths
// survive extraction and get confirmed. Ancestor-chain materialization
// (spec §4) legitimately requests intermediate dirs like /api and /api/v1
// too under crawl:js provenance — that's not noise, so this checks
// confirmed *findings*, not every raw request.
func TestHarvest_JSNoiseFilteredOut(t *testing.T) {
	fx := fixtures.NewCrawlTarget()
	defer fx.Close()

	co, _, audit := runScan(t, fx.URL, []string{"admin", "decoy1"}, engine.Config{
		Seed: 22, Crawl: true, JSHarvest: true,
	})

	var jsFindings []string
	for _, f := range co.Findings() {
		if f.Provenance == "crawl:js" {
			jsFindings = append(jsFindings, f.URL)
		}
	}
	if len(jsFindings) != 2 {
		t.Errorf("crawl:js findings = %v, want exactly the bundle's 2 real endpoints (noise must never survive extraction)", jsFindings)
	}
	for _, u := range jsFindings {
		if !strings.HasSuffix(u, "/api/v1/users") && !strings.HasSuffix(u, "/internal/status") {
			t.Errorf("unexpected crawl:js finding %s — noise from bundle.js should never be requested", u)
		}
	}
	for _, r := range audit.records {
		if strings.Contains(r.URL, "userId") {
			t.Errorf("template-literal noise should never be requested, got %s", r.URL)
		}
	}
}

// TestHarvest_MergesIntoScanningWordlistCandidate covers the DoD
// crawl_dedup case (spec §8 #4, §0 contract B): a linked path that's also
// an ordinary wordlist candidate in a SCANNING dir merges into one
// candidate with unioned provenance, rather than the pre-4b
// first-writer-wins gap silently dropping the crawl attribution. "admin"
// is listed first (wordlist.Load ranks BasePrio by line order, so it
// dispatches — and triggers the crawl — first) and "shared" last, and a
// modest Rate paces dispatch (otherwise, at the default unbounded rate,
// local-httptest round-trips are fast enough that even the lowest-priority
// candidate can dispatch within microseconds of the highest, racing ahead
// of the crawl goroutine): together they guarantee /shared is still
// queued, not yet dispatched, when the crawl's batch for it arrives — the
// deterministic version of the internal white-box merge test
// (TestApplySeedBatch_MergesIntoScanningDirCandidate), now exercised
// through the real crawl pipeline end to end.
func TestHarvest_MergesIntoScanningWordlistCandidate(t *testing.T) {
	fx := fixtures.NewCrawlTarget()
	defer fx.Close()

	words := []string{"admin", "decoy1", "decoy2", "decoy3", "decoy4", "decoy5", "decoy6", "decoy7", "decoy8", "shared"}
	co, _, _ := runScan(t, fx.URL, words, engine.Config{
		Seed: 23, Crawl: true, JSHarvest: true, Rate: 50,
	})

	f, ok := findingByPath(co.Findings(), "/shared")
	if !ok {
		t.Fatalf("/shared (in both the wordlist and a crawled link) never confirmed")
	}
	if !strings.Contains(f.Provenance, "wordlist") || !strings.Contains(f.Provenance, "crawl:html") {
		t.Errorf("/shared Provenance = %q, want it to mention both wordlist and crawl:html (merged)", f.Provenance)
	}
}

// TestHarvest_OffScopeLinksDropped covers the DoD offscope_links case
// (spec §8 #6): a link to another host must never be requested.
func TestHarvest_OffScopeLinksDropped(t *testing.T) {
	fx := fixtures.NewCrawlTarget()
	defer fx.Close()

	_, _, audit := runScan(t, fx.URL, []string{"admin", "decoy1"}, engine.Config{
		Seed: 24, Crawl: true, JSHarvest: true,
	})

	for _, r := range audit.records {
		if strings.Contains(r.URL, "off-host") {
			t.Errorf("off-host link should never be requested, got %s", r.URL)
		}
	}
}

// TestHarvest_SPAPivot_RecoversAPIEndpoints covers the DoD spa_with_api
// case (spec §8 #3 assertion): spa.pivot fires, and a target that yields
// zero findings from brute-force alone (an identical shell everywhere)
// still yields its real API surface via JS harvesting.
func TestHarvest_SPAPivot_RecoversAPIEndpoints(t *testing.T) {
	fx := fixtures.NewSPATarget()
	defer fx.Close()

	co, emitter, _ := runScan(t, fx.URL, []string{"decoy1", "decoy2"}, engine.Config{
		Seed: 25, Crawl: true, JSHarvest: true,
	})

	if !emitter.has(engine.EventSPAPivot) {
		t.Fatal("expected a spa.pivot event")
	}
	for _, path := range []string{"/api/v1/orders", "/api/v1/profile"} {
		if _, ok := findingByPath(co.Findings(), path); !ok {
			t.Errorf("SPA-pivot JS endpoint %s never confirmed; findings=%+v", path, co.Findings())
		}
	}
}

// TestHarvest_HeadlessOff_NoHeadlessActivity covers the DoD headless_spa
// case's "without it, gracefully skipped" half (spec §8 #7): with
// --headless unset, an ordinary scan proceeds with no headless-related
// warning at all.
func TestHarvest_HeadlessOff_NoHeadlessActivity(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	_, emitter, _ := runScan(t, fx.URL, []string{"admin", "backup"}, engine.Config{Seed: 26})

	for _, ev := range emitter.events {
		if ev.Type == engine.EventWarning && strings.Contains(ev.Message, "headless") {
			t.Errorf("unexpected headless warning with --headless unset: %q", ev.Message)
		}
	}
}

// TestHarvest_HeadlessOn_DegradesGracefullyWithoutDriver covers the DoD
// headless_spa case's driver-absent half (spec §8 #7): with --headless set
// but no playwright driver available, the scan warns and proceeds
// normally rather than failing.
func TestHarvest_HeadlessOn_DegradesGracefullyWithoutDriver(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	co, emitter, _ := runScan(t, fx.URL, []string{"admin", "backup"}, engine.Config{
		Seed: 27, Headless: true,
	})

	if _, ok := findingByPath(co.Findings(), "/admin"); !ok {
		t.Error("ordinary scan should be unaffected by headless being unavailable")
	}
	sawHeadlessWarning := false
	for _, ev := range emitter.events {
		if ev.Type == engine.EventWarning && strings.Contains(ev.Message, "headless") {
			sawHeadlessWarning = true
		}
	}
	if !sawHeadlessWarning {
		t.Error("expected a headless warning (no driver installed) with --headless set")
	}
}

// TestHarvest_AsyncWayback_DispatchesBeforeCDXReturns covers DoD #9 (spec
// §8, contract H): with a slow stub CDX, the scan must dispatch a real
// (non-seed) candidate before the CDX call returns — Wayback moved off the
// critical path, not stalling scan start.
func TestHarvest_AsyncWayback_DispatchesBeforeCDXReturns(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	served := make(chan time.Time, 1)
	stub := fixtures.NewStubCDXDelayed(fx.Server, [][2]string{{"/late/path", "20240101000000"}}, 800*time.Millisecond, served)
	defer stub.Close()

	_, _, audit := runScan(t, fx.URL, []string{"admin", "backup"}, engine.Config{
		Seed: 28, Wayback: true, WaybackURL: stub.URL,
	})

	var cdxServedAt time.Time
	select {
	case cdxServedAt = <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("CDX stub was never hit")
	}

	var firstRealDispatch time.Time
	for _, r := range audit.records {
		if r.Provenance == "wordlist" {
			if firstRealDispatch.IsZero() || r.Time.Before(firstRealDispatch) {
				firstRealDispatch = r.Time
			}
		}
	}
	if firstRealDispatch.IsZero() {
		t.Fatal("no ordinary wordlist candidate was ever dispatched")
	}
	if !firstRealDispatch.Before(cdxServedAt) {
		t.Errorf("first real dispatch (%v) did not precede the CDX response (%v); Wayback should never stall scan start", firstRealDispatch, cdxServedAt)
	}
}

// TestHarvest_AsyncWayback_DirStormCapped covers the DoD dir_storm case
// (spec §8 #10, contract I): a Wayback batch naming far more than
// MAX_NEW_DIRS_PER_BATCH distinct new directories is truncated and logs
// seed.capped — the safety belt paired with moving Wayback off the
// critical path, never separated from it.
func TestHarvest_AsyncWayback_DirStormCapped(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()

	rows := make([][2]string, 0, 520)
	for i := 0; i < 520; i++ {
		rows = append(rows, [2]string{fmt.Sprintf("/storm/%d/leaf.php", i), "20200101000000"})
	}
	stub := fixtures.NewStubCDX(fx.Server, rows)
	defer stub.Close()

	_, emitter, _ := runScan(t, fx.URL, []string{"admin"}, engine.Config{
		Seed: 29, Wayback: true, WaybackURL: stub.URL,
	})

	sawCapped := false
	for _, ev := range emitter.events {
		if ev.Type == engine.EventWarning && strings.HasPrefix(ev.Message, "seed.capped") {
			sawCapped = true
		}
	}
	if !sawCapped {
		t.Error("expected a seed.capped warning for a batch exceeding MAX_NEW_DIRS_PER_BATCH")
	}
}

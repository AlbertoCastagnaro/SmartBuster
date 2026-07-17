package profile

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

func testOpts(t *testing.T) Options {
	t.Helper()
	rs, err := Load(LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return Options{Ruleset: rs, FaviconProbe: true}
}

// The root fetch and the favicon fetch must both be paced (not just active
// probes) — otherwise scan-start bursts ahead of --rate.
func TestProfileTarget_PacesRootAndFaviconFetches(t *testing.T) {
	fx := fixtures.NewFaviconKnown() // serves both "/" and "/favicon.ico"
	defer fx.Close()

	var paceCalls int64
	opts := testOpts(t)
	opts.Pace = func() { atomic.AddInt64(&paceCalls, 1) }

	client := httpclient.New(httpclient.Config{})
	ProfileTarget(context.Background(), client, fx.URL, opts)

	if got := atomic.LoadInt64(&paceCalls); got != 2 {
		t.Fatalf("Pace() called %d times, want 2 (root + favicon)", got)
	}
}

// spec §0 contract H: the scope enforcer gates the root fetch and the
// favicon fetch just like any other request.
func TestProfileTarget_OutOfScopeSendsNoRequests(t *testing.T) {
	fx := fixtures.NewPHPApache()
	defer fx.Close()

	var paceCalls int64
	opts := testOpts(t)
	opts.InScope = func(string) bool { return false }
	opts.Pace = func() { atomic.AddInt64(&paceCalls, 1) }

	client := httpclient.New(httpclient.Config{})
	p := ProfileTarget(context.Background(), client, fx.URL, opts)

	if len(p.Tech) != 0 {
		t.Fatalf("expected no tech detected when out of scope, got %+v", p.Tech)
	}
	if got := atomic.LoadInt64(&paceCalls); got != 0 {
		t.Fatalf("Pace() called %d times, want 0 (nothing should have been paced/sent)", got)
	}
}

// DoD §9 assertion 1: framework fixtures detect the correct backend tech at
// high confidence, and ExtensionsForStack() returns the right set.
func TestProfileTarget_PHPApache(t *testing.T) {
	fx := fixtures.NewPHPApache()
	defer fx.Close()
	client := httpclient.New(httpclient.Config{})

	p := ProfileTarget(context.Background(), client, fx.URL, testOpts(t))

	php, ok := p.Tech["PHP"]
	if !ok || php.Confidence < 0.8 {
		t.Fatalf("PHP not detected at high confidence: %+v", p.Tech)
	}
	exts := p.ExtensionsForStack()
	if !containsStr(exts, ".php") {
		t.Fatalf("ExtensionsForStack() = %v, want .php", exts)
	}
}

func TestProfileTarget_DotNetIIS(t *testing.T) {
	fx := fixtures.NewDotNetIIS()
	defer fx.Close()
	client := httpclient.New(httpclient.Config{})

	p := ProfileTarget(context.Background(), client, fx.URL, testOpts(t))

	asp, ok := p.Tech["ASP.NET"]
	if !ok || asp.Confidence < 0.8 {
		t.Fatalf("ASP.NET not detected at high confidence: %+v", p.Tech)
	}
	exts := p.ExtensionsForStack()
	if !containsStr(exts, ".aspx") {
		t.Fatalf("ExtensionsForStack() = %v, want .aspx", exts)
	}
}

// DoD §9 assertion 2: behind_cdn separates edge/backend and records WAF.
func TestProfileTarget_BehindCDN(t *testing.T) {
	fx := fixtures.NewBehindCDN()
	defer fx.Close()
	client := httpclient.New(httpclient.Config{})

	p := ProfileTarget(context.Background(), client, fx.URL, testOpts(t))

	if p.WAF != "Cloudflare" {
		t.Fatalf("p.WAF = %q, want Cloudflare", p.WAF)
	}
	php, ok := p.Tech["PHP"]
	if !ok || php.Layer != LayerBackend {
		t.Fatalf("PHP = %+v, want LayerBackend", php)
	}
	if cf := p.Tech["Cloudflare"]; cf == nil || cf.Layer != LayerEdge {
		t.Fatalf("Cloudflare = %+v, want LayerEdge", cf)
	}
}

// DoD §9 assertion: waf_challenge fixture triggers waf.detected, not a
// tech-only false read.
func TestProfileTarget_WAFChallenge(t *testing.T) {
	fx := fixtures.NewWAFChallenge()
	defer fx.Close()
	client := httpclient.New(httpclient.Config{})

	p := ProfileTarget(context.Background(), client, fx.URL, testOpts(t))
	if p.WAF != "Cloudflare" {
		t.Fatalf("p.WAF = %q, want Cloudflare", p.WAF)
	}
}

// DoD §9 assertion: favicon_known fixture fires the favicon vote at 0.9.
func TestProfileTarget_FaviconKnown(t *testing.T) {
	fx := fixtures.NewFaviconKnown()
	defer fx.Close()
	client := httpclient.New(httpclient.Config{})

	p := ProfileTarget(context.Background(), client, fx.URL, testOpts(t))
	tech, ok := p.Tech["SmartBusterFixtureApp"]
	if !ok || tech.Confidence != 0.9 {
		t.Fatalf("favicon tech = %+v, want confidence 0.9", tech)
	}
}

// spec §3: FaviconProbe=false must not fetch /favicon.ico at all — verified
// indirectly (the known-favicon vote never fires) since the fixture would
// otherwise cast it on any fetch of that path.
func TestProfileTarget_FaviconProbeDisabled(t *testing.T) {
	fx := fixtures.NewFaviconKnown()
	defer fx.Close()

	opts := testOpts(t)
	opts.FaviconProbe = false
	client := httpclient.New(httpclient.Config{})
	p := ProfileTarget(context.Background(), client, fx.URL, opts)

	if _, ok := p.Tech["SmartBusterFixtureApp"]; ok {
		t.Fatalf("favicon tech should not be detected when FaviconProbe=false: %+v", p.Tech)
	}
}

// DoD §9 assertion 6: --active-probes=false sends zero confirmer requests;
// =true fires only for techs in the confidence band, and confirmation
// raises the tech's confidence.
func TestRefineAfterCalibration_ActiveProbeGating(t *testing.T) {
	fx := fixtures.NewWordPress()
	defer fx.Close()
	client := httpclient.New(httpclient.Config{})

	// Root fetch alone should land WordPress in the confirmation band via
	// the (deliberately mid-confidence) X-Generator header signal.
	opts := testOpts(t)
	p := ProfileTarget(context.Background(), client, fx.URL, opts)
	wp, ok := p.Tech["WordPress"]
	if !ok || wp.Confidence < ActiveProbeConfLo || wp.Confidence >= ActiveProbeConfHi {
		t.Fatalf("WordPress confidence = %v, want it in [%v,%v) before active probing", wp.Confidence, ActiveProbeConfLo, ActiveProbeConfHi)
	}
	preRefineConf := wp.Confidence

	t.Run("disabled sends no confirmer request", func(t *testing.T) {
		p := ProfileTarget(context.Background(), client, fx.URL, opts)
		disabledOpts := opts
		disabledOpts.ActiveProbes = false
		RefineAfterCalibration(context.Background(), client, p, disabledOpts, nil, "", 404)
		if p.Tech["WordPress"].Confidence != preRefineConf {
			t.Fatalf("confidence changed with ActiveProbes=false: got %v, want unchanged %v", p.Tech["WordPress"].Confidence, preRefineConf)
		}
	})

	t.Run("enabled confirms and raises confidence", func(t *testing.T) {
		p := ProfileTarget(context.Background(), client, fx.URL, opts)
		enabledOpts := opts
		enabledOpts.ActiveProbes = true
		RefineAfterCalibration(context.Background(), client, p, enabledOpts, nil, "", 404)
		got := p.Tech["WordPress"].Confidence
		if got <= preRefineConf {
			t.Fatalf("confidence after confirmation = %v, want > %v", got, preRefineConf)
		}
	})
}

// spec §4.7: a tech already at/above ActiveProbeConfHi is not re-probed.
func TestRunActiveProbes_SkipsOutOfBandTech(t *testing.T) {
	var hit int64
	p := newTargetProfile("t")
	p.Services = append(p.Services, ServiceTarget{BaseURL: "http://unreachable.invalid.test"})
	p.vote("WordPress", "cms", LayerBackend, SrcCookie, 0.9, "", "r") // already >= ActiveProbeConfHi

	rs := testOpts(t).Ruleset
	opts := Options{Ruleset: rs, InScope: func(string) bool { atomic.AddInt64(&hit, 1); return true }}
	runActiveProbes(context.Background(), httpclient.New(httpclient.Config{}), p, opts, nil)

	if atomic.LoadInt64(&hit) != 0 {
		t.Fatalf("expected no confirmer probe for a tech already at 0.9 confidence, InScope was checked %d times", hit)
	}
}

func TestConfirms_FallsBackWithoutBaseline(t *testing.T) {
	resp := &ProfileResponse{StatusCode: 200}
	if !confirms(resp, "/wp-login.php", nil) {
		t.Fatal("expected a 200 to confirm when no baseline is available")
	}
	resp404 := &ProfileResponse{StatusCode: 404}
	if confirms(resp404, "/wp-login.php", nil) {
		t.Fatal("expected a 404 not to confirm when no baseline is available")
	}
}

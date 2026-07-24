// preset_internal_test.go is package engine (white-box), like
// dynamic_test.go: Phase 6a's mode/AIMD/header-profile wiring is easiest to
// verify by inspecting Coordinator's own unexported fields directly (the
// active Limiter, headerProfile, backoffSpec) rather than only through
// externally-observable behavior, and by driving the AIMD controller with
// a controlled clock instead of a real network's timing.
package engine

import (
	"encoding/json"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

func presetTestScope(t *testing.T) *scope.Scope {
	t.Helper()
	sc, err := scope.New(scope.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// collectingEmitterInternal mirrors engine_test's collectingEmitter — can't
// reuse that one directly since this file is package engine, not engine_test.
type collectingEmitterInternal struct {
	mu     sync.Mutex
	events []Event
}

func (e *collectingEmitterInternal) Emit(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, ev)
}

func (e *collectingEmitterInternal) has(t EventType) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.Type == t {
			return true
		}
	}
	return false
}

func (e *collectingEmitterInternal) hasWarningSource(source string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.Type != EventWarning {
			continue
		}
		var wp WarnPayload
		if json.Unmarshal(ev.Payload, &wp) == nil && wp.Source == source {
			return true
		}
	}
	return false
}

// TestResolvePreset_ModeDefaultsAndOverrides is spec §2's "explicit flags
// override individual fields on top": an empty Mode resolves to "normal",
// an unknown Mode also falls back to "normal," and any explicit Config
// field wins over whichever preset (named or default) was selected.
func TestResolvePreset_ModeDefaultsAndOverrides(t *testing.T) {
	normal := ResolvePreset("", Config{})
	if normal.HeaderProfile != "chrome" || normal.RateCap != 0 {
		t.Fatalf("expected empty Mode to resolve to normal's preset, got %+v", normal)
	}

	unknown := ResolvePreset("not-a-real-mode", Config{})
	if unknown.HeaderProfile != normal.HeaderProfile || unknown.RateCap != normal.RateCap || unknown.Jitter != normal.Jitter {
		t.Fatalf("expected an unknown mode to fall back to normal, got %+v want %+v", unknown, normal)
	}

	stealth := ResolvePreset("stealth", Config{})
	if stealth.Epsilon != 0 || !stealth.OrderJitter || !stealth.Referer {
		t.Fatalf("expected stealth's preset shape, got %+v", stealth)
	}

	overridden := ResolvePreset("stealth", Config{Rate: 99, Concurrency: 7, HeaderProfile: "safari"})
	if overridden.RateCap != 99 || overridden.Concurrency != 7 || overridden.HeaderProfile != "safari" {
		t.Fatalf("expected explicit Config fields to override stealth's own values, got %+v", overridden)
	}
	// Fields left untouched by cfg still come from the preset.
	if !overridden.OrderJitter {
		t.Fatalf("expected non-overridden preset fields (OrderJitter) to survive, got %+v", overridden)
	}
}

// TestNewCoordinator_ModeAppliesConcurrencyAndRate confirms mode selection
// actually reaches the constructed Coordinator's effective Concurrency
// (spec §2's table: "quiet" is a low-concurrency preset), not just the
// Preset value in isolation.
func TestNewCoordinator_ModeAppliesConcurrencyAndRate(t *testing.T) {
	entries := []wordlist.Entry{{Word: "admin"}}
	co, err := NewCoordinator("http://example.invalid", entries, Config{Mode: "quiet", Seed: 1}, presetTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if co.config.Concurrency != 5 {
		t.Fatalf("expected quiet mode's concurrency (5) to become the effective concurrency, got %d", co.config.Concurrency)
	}
	if co.limiter.Unbounded() {
		t.Fatal("expected quiet mode's RateCap (5) to make the limiter bounded, not unbounded")
	}
	if co.headerProfile.Load() != "chrome" {
		t.Fatalf("expected quiet mode's header profile, got %q", co.headerProfile.Load())
	}
	if !co.orderJitter || !co.refererEnabled {
		t.Fatal("expected quiet mode to enable OrderJitter and Referer")
	}
}

// TestApplyPreset_LiveModeSwitchReconfiguresEverything is spec §2's "PATCH
// mode -> controlCh -> coordinator applies the preset live": every
// preset-governed knob changes, in one call, without touching concurrency
// (which PATCH's own SetConcurrency field owns independently).
func TestApplyPreset_LiveModeSwitchReconfiguresEverything(t *testing.T) {
	entries := []wordlist.Entry{{Word: "admin"}}
	co, err := NewCoordinator("http://example.invalid", entries, Config{Mode: "fast", Seed: 1}, presetTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if co.headerProfile.Load() != "minimal" || co.orderJitter || co.backoffSpec.Enabled {
		t.Fatalf("expected fast mode's initial shape, got profile=%q orderJitter=%v backoff=%v",
			co.headerProfile.Load(), co.orderJitter, co.backoffSpec.Enabled)
	}

	co.applyPreset(PresetFor("stealth"))

	if co.headerProfile.Load() != "chrome" {
		t.Fatalf("expected header profile to swap to stealth's on a live mode switch, got %q", co.headerProfile.Load())
	}
	if !co.orderJitter || !co.refererEnabled {
		t.Fatal("expected OrderJitter/Referer to turn on after switching to stealth")
	}
	if !co.backoffSpec.Enabled || co.epsilon != 0 {
		t.Fatalf("expected stealth's backoff enabled and epsilon 0, got backoff=%v epsilon=%v", co.backoffSpec.Enabled, co.epsilon)
	}
	if co.limiter.Unbounded() {
		t.Fatal("expected stealth's RateCap to make the limiter bounded")
	}
}

// TestAIMDBackoff_EngagesAndRecovers drives the coordinator's own
// trigger/recovery methods directly with a controlled clock (no real
// network involved — spec §9 DoD #4's "AIMD engages... emits throttle...
// recovers to cap on a clean window" observed as a curve, deterministically).
func TestAIMDBackoff_EngagesAndRecovers(t *testing.T) {
	entries := []wordlist.Entry{{Word: "admin"}}
	emitter := &collectingEmitterInternal{}
	co, err := NewCoordinator("http://example.invalid", entries, Config{Mode: "normal", Seed: 1}, presetTestScope(t),
		WithEventEmitter(emitter))
	if err != nil {
		t.Fatal(err)
	}
	// Shrink the recovery window so the test doesn't need to wait 10s —
	// white-box access to the coordinator's own field, not part of any
	// public surface.
	co.backoffSpec.RecoveryWindow = 20 * time.Millisecond
	co.backoffSpec.Step = 5
	co.limiter.SetRateCap(10)

	if rate, _, triggered := co.limiter.AIMDState(); triggered || rate != 10 {
		t.Fatalf("expected an untriggered limiter at the configured cap before any onset, got rate=%v triggered=%v", rate, triggered)
	}

	co.triggerBackoff()
	if !emitter.has(EventThrottle) {
		t.Fatal("expected a throttle event once triggerBackoff runs")
	}
	rateAfterTrigger, _, triggered := co.limiter.AIMDState()
	if !triggered || rateAfterTrigger >= 10 {
		t.Fatalf("expected a decreased, triggered rate right after the onset, got rate=%v triggered=%v", rateAfterTrigger, triggered)
	}

	// Too soon: no recovery step yet.
	co.maybeRecoverBackoff()
	if rate, _, _ := co.limiter.AIMDState(); rate != rateAfterTrigger {
		t.Fatalf("expected no recovery before the clean window elapses, rate changed to %v", rate)
	}

	// Repeated recovery calls until the AIMD controller reports "back at
	// cap" (the recovered=true edge maybeRecoverBackoff turns into the
	// throttle-recovered warning) — an additive-increase curve, not a
	// one-shot revert.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
		co.maybeRecoverBackoff()
		if rate, _, triggered := co.limiter.AIMDState(); rate >= 10 && !triggered {
			break
		}
	}
	if rate, _, triggered := co.limiter.AIMDState(); rate != 10 || triggered {
		t.Fatalf("expected the AIMD controller to fully recover to the rate cap, got rate=%v triggered=%v", rate, triggered)
	}
	if !emitter.hasWarningSource("throttle-recovered") {
		t.Fatal("expected a throttle-recovered warning once the rate cap is reached")
	}
}

// TestAIMDBackoff_DisabledPresetNeverTriggers: fast mode's Backoff is
// disabled — triggerBackoff must be a true no-op, not just a suppressed
// event.
func TestAIMDBackoff_DisabledPresetNeverTriggers(t *testing.T) {
	entries := []wordlist.Entry{{Word: "admin"}}
	emitter := &collectingEmitterInternal{}
	co, err := NewCoordinator("http://example.invalid", entries, Config{Mode: "fast", Seed: 1}, presetTestScope(t),
		WithEventEmitter(emitter))
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := co.limiter.AIMDState()
	co.triggerBackoff()
	after, _, triggered := co.limiter.AIMDState()
	if after != before || triggered {
		t.Fatalf("expected fast mode's disabled backoff to leave the limiter untouched, before=%v after=%v triggered=%v", before, after, triggered)
	}
	if emitter.has(EventThrottle) {
		t.Fatal("expected no throttle event when the preset's backoff is disabled")
	}
}

// TestRefererFor implements spec §5's referer-chain rule directly.
func TestRefererFor(t *testing.T) {
	entries := []wordlist.Entry{{Word: "admin"}}
	co, err := NewCoordinator("http://example.invalid", entries, Config{Mode: "quiet", Seed: 1}, presetTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if got := co.refererFor(""); got != "http://example.invalid/" {
		t.Errorf("expected root candidates to referer the site root, got %q", got)
	}
	if got := co.refererFor("/admin"); got != "http://example.invalid/admin/" {
		t.Errorf("expected a recursion child to referer its parent dir, got %q", got)
	}

	co.refererEnabled = false
	if got := co.refererFor("/admin"); got != "" {
		t.Errorf("expected no referer at all when the preset has Referer off, got %q", got)
	}
}

// TestPopBand_StaysWithinTopTierButShuffles is spec §5's OrderJitter: the
// seeded shuffle only ever picks among near-equal top scores, never
// promotes a clearly-lower-scored candidate ahead of the pack.
func TestPopBand_StaysWithinTopTierButShuffles(t *testing.T) {
	f := NewFrontier()
	// Five near-tied top candidates, one clear outlier far below.
	for i := 0; i < 5; i++ {
		f.Push(Candidate{Path: string(rune('a' + i)), Score: 10.0 - float64(i)*0.01})
	}
	f.Push(Candidate{Path: "outlier", Score: 1.0})

	rng := rand.New(rand.NewSource(7))
	seenPaths := map[string]bool{}
	for i := 0; i < 5; i++ {
		c := f.PopBand(rng, 0.05)
		if c.Path == "outlier" {
			t.Fatal("expected PopBand to never surface the far-lower-scored outlier while the top tier still has candidates")
		}
		seenPaths[c.Path] = true
	}
	if len(seenPaths) < 2 {
		t.Fatalf("expected the seeded shuffle to vary which near-tied candidate comes out first across pops, got only %v", seenPaths)
	}
	// The outlier is all that's left now.
	if last := f.Pop(); last.Path != "outlier" {
		t.Fatalf("expected the outlier to be the only thing left, got %q", last.Path)
	}
}

// TestResolvePreset_FingerprintAxis is spec §6: --fingerprint is its own
// axis, independent of --mode — stealth defaults TLSProfile to "chrome",
// every other preset defaults it off, and an explicit override wins either
// way (mirroring how --header-profile already overrides HeaderProfile).
func TestResolvePreset_FingerprintAxis(t *testing.T) {
	if got := ResolvePreset("normal", Config{}).TLSProfile; got != "" {
		t.Fatalf("expected normal mode's TLSProfile to default off, got %q", got)
	}
	if got := ResolvePreset("fast", Config{}).TLSProfile; got != "" {
		t.Fatalf("expected fast mode's TLSProfile to default off, got %q", got)
	}
	if got := ResolvePreset("stealth", Config{}).TLSProfile; got != httpclient.ProfileChrome {
		t.Fatalf("expected stealth mode's TLSProfile to default to chrome, got %q", got)
	}
	if got := ResolvePreset("normal", Config{Fingerprint: "firefox"}).TLSProfile; got != "firefox" {
		t.Fatalf("expected --fingerprint to override normal mode's off-by-default TLSProfile, got %q", got)
	}
	if got := ResolvePreset("stealth", Config{Fingerprint: "safari"}).TLSProfile; got != "safari" {
		t.Fatalf("expected --fingerprint to override stealth's own chrome default, got %q", got)
	}
}

// TestNewCoordinator_FingerprintSelectsHTTPDoer is the structural half of
// contract C's consistency fix: NewCoordinator builds exactly one HTTPDoer
// (spec §4) — a plain httpclient.Client when no fingerprint is active, a
// tls-client-backed httpclient.TLSDoer when it is — and that single value
// is what profile/seed/worker all now share (see profiling.go, seed.go,
// worker.go), so there is no longer a second concrete client for a
// fingerprinting WAF to tell apart from the worker's own requests.
func TestNewCoordinator_FingerprintSelectsHTTPDoer(t *testing.T) {
	entries := []wordlist.Entry{{Word: "admin"}}

	plain, err := NewCoordinator("http://example.invalid", entries, Config{Mode: "normal", Seed: 1}, presetTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.client.(*httpclient.Client); !ok {
		t.Fatalf("expected normal mode (no fingerprint) to build a plain httpclient.Client, got %T", plain.client)
	}

	stealth, err := NewCoordinator("http://example.invalid", entries, Config{Mode: "stealth", Seed: 1}, presetTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stealth.client.(*httpclient.TLSDoer); !ok {
		t.Fatalf("expected stealth mode to build a tls-client-backed TLSDoer, got %T", stealth.client)
	}

	explicit, err := NewCoordinator("http://example.invalid", entries, Config{Mode: "normal", Fingerprint: "firefox", Seed: 1}, presetTestScope(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := explicit.client.(*httpclient.TLSDoer); !ok {
		t.Fatalf("expected --fingerprint to select a TLSDoer even in normal mode, got %T", explicit.client)
	}
}

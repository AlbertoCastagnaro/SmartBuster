// preset.go implements Phase 6a's mode system (spec §2): a mode is a
// coherent bundle of pacing/header/backoff/exploration settings, not a pile
// of independent flags. NewCoordinator resolves one Preset at construction
// (spec §8: Config.Mode defaults to "normal") and applyAdjust re-resolves
// and re-applies it live on a PATCH .../{id} mode switch (spec §2, control.go).
package engine

import (
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
)

// BackoffSpec configures the AIMD adaptive controller (spec §4) a preset
// turns on/off and tunes. Enabled=false (fast mode) means detectWAFOnset's
// triggers are simply never applied to the pacer.
type BackoffSpec struct {
	Enabled        bool
	Decrease       float64       // multiplicative decrease factor on trigger (BACKOFF_DECREASE)
	Step           float64       // additive increase per clean recovery window (BACKOFF_STEP)
	RecoveryWindow time.Duration // clean window required before a recovery step (RECOVERY_WINDOW)
}

// Preset is a coherent stealth-tier bundle (spec §2): users pick a mode:
// "fast"|"normal"|"quiet"|"stealth"; explicit Config fields override
// individual preset fields on top (see ResolvePreset).
type Preset struct {
	RateCap       float64 // req/s; 0 = unbounded
	Jitter        httpclient.JitterSpec
	Concurrency   int
	HeaderProfile string // httpclient.Profile* — "chrome"|"firefox"|"safari"|"minimal"
	OrderJitter   bool   // §5 intra-priority-band shuffle
	Referer       bool   // §5 referer chains
	Backoff       BackoffSpec
	Epsilon       float64 // ε-greedy exploration; 0 in stealth

	// TLSProfile is Phase 6b's fingerprint axis (spec §2, §6): ""
	// (every preset but stealth) keeps 6a's plain net/http-backed Client;
	// a browser name ("chrome"|"firefox"|"safari") selects a coherent
	// BrowserProfile (TLS ClientHello + HTTP/2 settings + header values +
	// header order, spec §2's httpclient.BrowserProfile) with the
	// tls-client HTTPDoer behind it instead — subsuming 6a's separate
	// HeaderProfile for as long as fingerprinting is active (spec §2: "a
	// Chrome JA3 wearing Firefox headers is itself a tell", so the two
	// must never be selected independently once TLSProfile != ""). No
	// separate proxy or HTTP2-fingerprint field belongs here: proxy is a
	// single opt-in upstream for the whole session, not a per-mode
	// property (Config.Proxy, spec §5), and the HTTP/2 fingerprint is
	// already baked into TLSClient's bundled ClientProfile, not a value a
	// preset picks on its own (spec §1: no proxy pool/rotation either).
	TLSProfile string
}

// DefaultBackoffDecrease/Step/RecoveryWindow are spec §8's table defaults —
// "gentle" AIMD, used by normal/quiet; stealth tunes more aggressively (see
// presetTable).
const (
	DefaultBackoffDecrease       = 0.5
	DefaultBackoffStep           = 0.5
	DefaultBackoffRecoveryWindow = 10 * time.Second
)

// presetTable is spec §2's mode table. Numeric picks not literally spelled
// out by the spec (e.g. "high"/"low" concurrency, quiet's exact rate) are
// this build's concrete defaults for the qualitative shape the table
// describes; --rate/-c/etc. (or PATCH) always override them per-field.
var presetTable = map[string]Preset{
	"fast": {
		RateCap:       0,
		Jitter:        httpclient.JitterSpec{Kind: "none"},
		Concurrency:   40,
		HeaderProfile: httpclient.ProfileMinimal,
		Backoff:       BackoffSpec{Enabled: false},
		Epsilon:       0,
	},
	"normal": {
		RateCap:       0,
		Jitter:        httpclient.JitterSpec{Kind: "uniform", Param1: 0.15},
		Concurrency:   DefaultConcurrency,
		HeaderProfile: httpclient.ProfileChrome,
		Backoff: BackoffSpec{
			Enabled: true, Decrease: DefaultBackoffDecrease, Step: DefaultBackoffStep,
			RecoveryWindow: DefaultBackoffRecoveryWindow,
		},
		// Epsilon stays 0 (pure greedy) by default, same as every other
		// mode: ε-greedy exploration ties its randomness to real dispatch
		// timing under concurrency (epsilonRNG is drawn each time popNext
		// runs, and how many times that's been called so far depends on
		// live completion order, not just the seed), so turning it on by
		// default would quietly break the "same seed -> same requests"
		// guarantee for every caller that never asked for exploration.
		// DefaultEpsilon remains available as an explicit opt-in (--epsilon).
		Epsilon: 0,
	},
	"quiet": {
		RateCap:       5,
		Jitter:        httpclient.JitterSpec{Kind: "gaussian", Param1: 0.4},
		Concurrency:   5,
		HeaderProfile: httpclient.ProfileChrome,
		OrderJitter:   true,
		Referer:       true,
		Backoff: BackoffSpec{
			Enabled: true, Decrease: DefaultBackoffDecrease, Step: DefaultBackoffStep,
			RecoveryWindow: DefaultBackoffRecoveryWindow,
		},
		Epsilon: 0,
	},
	"stealth": {
		RateCap:       1,
		Jitter:        httpclient.JitterSpec{Kind: "bursty", Param1: 3, Param2: 5},
		Concurrency:   2,
		HeaderProfile: httpclient.ProfileChrome,
		OrderJitter:   true,
		Referer:       true,
		Backoff: BackoffSpec{
			Enabled: true, Decrease: 0.3, Step: 0.25, RecoveryWindow: 15 * time.Second,
		},
		Epsilon: 0,
		// spec §7: stealth's TLSProfile defaults to "chrome" (latest
		// bundled) — the fingerprint axis is on by default here, off
		// everywhere else (spec §6), independently of --fingerprint.
		TLSProfile: httpclient.ProfileChrome,
	},
}

// PresetFor returns mode's preset, falling back to "normal" for an unknown
// or empty name.
func PresetFor(mode string) Preset {
	if p, ok := presetTable[mode]; ok {
		return p
	}
	return presetTable["normal"]
}

// ResolvePreset builds mode's preset and applies cfg's explicit overrides
// on top (spec §2: "explicit flags override individual fields on top").
// Following this Config's existing "<=0/empty means unset, apply the
// default" convention (Rate/Jitter/Concurrency/Epsilon already worked this
// way pre-6a): a caller that never touches these fields gets exactly the
// selected preset, and any of them can still be dialed independently.
func ResolvePreset(mode string, cfg Config) Preset {
	p := PresetFor(mode)
	if cfg.Rate != 0 {
		p.RateCap = cfg.Rate
	}
	if cfg.Jitter != 0 {
		p.Jitter = httpclient.JitterSpec{Kind: "uniform", Param1: cfg.Jitter}
	}
	if cfg.JitterKind != "" {
		p.Jitter.Kind = cfg.JitterKind
	}
	if cfg.Concurrency > 0 {
		p.Concurrency = cfg.Concurrency
	}
	if cfg.HeaderProfile != "" {
		p.HeaderProfile = cfg.HeaderProfile
	}
	if cfg.Epsilon != 0 {
		p.Epsilon = cfg.Epsilon
	}
	// spec §6: --fingerprint is its own axis, independent of --mode —
	// stealth defaults TLSProfile to "chrome" above, every other preset
	// defaults it off ("", plain net/http), and an explicit --fingerprint
	// overrides either default the same way --header-profile already does.
	if cfg.Fingerprint != "" {
		p.TLSProfile = cfg.Fingerprint
	}
	return p
}

// newHTTPDoer builds the HTTPDoer every on-target request shares for the
// scan's lifetime (spec §2, §4 contract C): preset.TLSProfile == "" keeps
// 6a's plain net/http-backed Client; otherwise it's 6b's tls-client-backed
// TLSDoer, routed through cfg.Proxy if set (spec §5). Built once, here,
// rather than per-request or re-built on a live mode switch — spec §5's
// "one browser identity per session" (a tls-client instance's connection
// pool is already committed to its ClientProfile's JA3 at construction).
func newHTTPDoer(preset Preset, cfg Config) (httpclient.HTTPDoer, error) {
	clientCfg := httpclient.Config{
		Concurrency:    cfg.Concurrency,
		RequestTimeout: cfg.RequestTO,
	}
	if preset.TLSProfile == "" {
		return httpclient.New(clientCfg), nil
	}
	return httpclient.NewTLSDoer(clientCfg, preset.TLSProfile, cfg.Proxy)
}

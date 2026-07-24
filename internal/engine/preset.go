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

	// 6b fields: present, unused in 6a. 6b's tls-client implementation
	// selects on these once the HTTPDoer boundary (spec §6) grows a second
	// implementation; stealth's TLSProfile/Proxies stay zero-valued until then.
	TLSProfile string
	Proxies    []string
	HTTP2FP    string
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
	return p
}

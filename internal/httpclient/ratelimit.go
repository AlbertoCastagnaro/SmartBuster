package httpclient

import (
	"math"
	"math/rand"
	"time"
)

// DefaultJitter is the default fractional jitter applied to the pacing
// interval under JitterSpec's "uniform" kind: actual interval is drawn
// uniformly from [1-JITTER, 1+JITTER].
const DefaultJitter = 0.30

// backoffFallbackRate is the pacing rate a backoff falls back to when the
// configured rate is unbounded (0). Multiplying an unbounded "0 interval"
// by a decrease factor is still 0 — hammering is exactly what triggers a
// backoff, so an active backoff must impose real pacing even then.
const backoffFallbackRate = 2.0 // req/s

// JitterSpec configures the pacer's inter-request interval distribution
// (spec §3, Phase 6a tier 1): "none" is a fixed interval, "uniform" is the
// pre-6a behavior (interval drawn from base*[1-J,1+J]), "gaussian" is less
// metronomic than uniform (interval ~ N(base, (base*sigma)^2), clamped
// >=0), and "bursty" alternates a few near-back-to-back requests with a
// longer pause, modeling human/browser traffic rather than an even spread.
type JitterSpec struct {
	Kind   string // "none"|"uniform"|"gaussian"|"bursty"
	Param1 float64
	Param2 float64
}

// Limiter computes jittered pacing intervals for the coordinator's single
// global dispatch gate (spec §5: "this is the only pacing point — worker
// count does not affect aggregate rate"). It is not goroutine-safe; it is
// designed to be owned and driven by exactly one goroutine (the coordinator),
// consistent with the single-owner architecture and the requirement to use
// one explicit seeded *rand.Rand rather than shared/global state.
//
// Phase 6a folds the AIMD adaptive-backoff controller (spec §4) into Limiter
// too, rather than a separate type: the pacer already reads "the current
// rate" from here every tick, so a coordinator-driven AIMD decrease/recover
// is just another way rate gets mutated between ticks, exactly like SetRate
// already was.
type Limiter struct {
	rate    float64 // requests/sec; <= 0 means unbounded
	rateCap float64 // ceiling AIMD recovery must never exceed (spec §4); mirrors the active preset's RateCap
	jitter  JitterSpec

	lastTrigger time.Time // last AIMD trigger (or recovery step); gates both re-trigger and the next recovery step
	triggered   bool      // a backoff is active: rate < rateCap because of a trigger, not yet fully recovered

	burstRemaining int // "bursty" jitter's own state: requests left in the current burst
}

// NewLimiter is the legacy constructor: a plain fractional uniform jitter,
// unchanged from pre-6a behavior. Still used where a full JitterSpec isn't
// warranted (e.g. the archive.org politeness limiter).
func NewLimiter(rate, jitter float64) *Limiter {
	if jitter <= 0 {
		jitter = DefaultJitter
	}
	return NewLimiterSpec(rate, JitterSpec{Kind: "uniform", Param1: jitter})
}

// NewLimiterSpec builds a Limiter driven by a full JitterSpec distribution
// (spec §3), starting with rate as both the current rate and the AIMD
// recovery ceiling.
func NewLimiterSpec(rate float64, spec JitterSpec) *Limiter {
	return &Limiter{rate: rate, rateCap: rate, jitter: spec}
}

// Unbounded reports whether pacing is disabled (rate <= 0): workers pull as
// fast as they complete.
func (l *Limiter) Unbounded() bool {
	return l.rate <= 0
}

// SetRate live-adjusts the pacing rate (Phase 5a's PATCH .../{id}: spec §4).
// Not goroutine-safe, same as the rest of Limiter — callers must be the
// coordinator goroutine that already owns it exclusively.
func (l *Limiter) SetRate(rate float64) {
	l.rate = rate
}

// SetRateCap sets both the live rate and the AIMD recovery ceiling — used
// when a mode switch or an explicit PATCH rate should immediately take
// effect and become the new target recovery converges toward.
func (l *Limiter) SetRateCap(cap float64) {
	l.rateCap = cap
	l.rate = cap
}

// SetJitterSpec live-swaps the jitter distribution (PATCH mode, spec §2):
// resets bursty's in-progress burst state so a mode switch starts the new
// distribution cleanly rather than mid-burst.
func (l *Limiter) SetJitterSpec(spec JitterSpec) {
	l.jitter = spec
	l.burstRemaining = 0
}

// NextInterval returns the jittered wait duration until the next dispatch
// slot, drawing from rng according to the active JitterSpec.
func (l *Limiter) NextInterval(rng *rand.Rand, now time.Time) time.Duration {
	if l.Unbounded() {
		return 0
	}
	base := float64(time.Second) / l.rate
	return l.jitteredInterval(rng, base)
}

func (l *Limiter) jitteredInterval(rng *rand.Rand, base float64) time.Duration {
	switch l.jitter.Kind {
	case "none":
		return time.Duration(base)
	case "gaussian":
		sigma := l.jitter.Param1
		if sigma <= 0 {
			sigma = 0.3
		}
		v := base + rng.NormFloat64()*sigma*base
		if v < 0 {
			v = 0
		}
		return time.Duration(v)
	case "bursty":
		return l.burstyInterval(rng, base)
	default: // "uniform", "" (legacy default/zero value)
		j := l.jitter.Param1
		if j <= 0 {
			j = DefaultJitter
		}
		factor := (1 - j) + rng.Float64()*(2*j)
		return time.Duration(base * factor)
	}
}

// burstyInterval alternates a run of short near-back-to-back intervals
// (a "burst") with a single longer pause, modeling human/browser traffic
// rather than an even spread (spec §3): Param1 is the mean burst size,
// Param2 the pause's multiplier over base.
func (l *Limiter) burstyInterval(rng *rand.Rand, base float64) time.Duration {
	burstMean := l.jitter.Param1
	if burstMean <= 0 {
		burstMean = 3
	}
	pauseMult := l.jitter.Param2
	if pauseMult <= 0 {
		pauseMult = 5
	}

	if l.burstRemaining > 0 {
		l.burstRemaining--
		return time.Duration(base * 0.15 * (0.5 + rng.Float64()))
	}
	size := int(math.Round(burstMean + (rng.Float64()-0.5)*burstMean))
	if size < 1 {
		size = 1
	}
	l.burstRemaining = size - 1
	return time.Duration(base * pauseMult * (0.75 + 0.5*rng.Float64()))
}

// TriggerBackoff applies AIMD's multiplicative decrease (spec §4): the
// current rate is cut by decrease (falling back to backoffFallbackRate as
// the base when currently unbounded, so an active backoff always imposes
// real pacing). Callers (the coordinator) are expected to gate repeat
// triggers themselves via InBackoff, so a spike doesn't compound the
// decrease on every single sample within one onset.
func (l *Limiter) TriggerBackoff(decrease float64, now time.Time) {
	base := l.rate
	if base <= 0 {
		base = backoffFallbackRate
	}
	l.rate = base * decrease
	l.lastTrigger = now
	l.triggered = true
}

// InBackoff reports whether a trigger is still within its settle window —
// the coordinator's guard against re-triggering on every sample in a single
// onset rather than once per spike.
func (l *Limiter) InBackoff(now time.Time, settle time.Duration) bool {
	return l.triggered && now.Sub(l.lastTrigger) < settle
}

// MaybeRecover implements AIMD's additive-increase recovery (spec §4): once
// a full clean window has passed since the last trigger or recovery step,
// the rate steps up by step, never exceeding rateCap. Returns true exactly
// on the step that reaches rateCap (bounded case) or the step that lifts an
// unbounded target back to fully unbounded (rateCap <= 0) — the caller's
// cue to emit the one-time "throttle-recovered" warning.
func (l *Limiter) MaybeRecover(step float64, window time.Duration, now time.Time) bool {
	if !l.triggered || now.Sub(l.lastTrigger) < window {
		return false
	}
	l.lastTrigger = now
	if l.rateCap <= 0 {
		// Unbounded target: there's no meaningful intermediate step toward
		// "infinite," so one clean window fully lifts the backoff.
		l.rate = 0
		l.triggered = false
		return true
	}
	l.rate += step
	if l.rate >= l.rateCap {
		l.rate = l.rateCap
		l.triggered = false
		return true
	}
	return false
}

// AIMDState/SetAIMDState round-trip a Limiter's adaptive-rate state (Phase
// 5a session save/resume, spec §6's "wafState", upgraded for Phase 6a's
// AIMD controller).
func (l *Limiter) AIMDState() (rate float64, lastTrigger time.Time, triggered bool) {
	return l.rate, l.lastTrigger, l.triggered
}

func (l *Limiter) SetAIMDState(rate float64, lastTrigger time.Time, triggered bool) {
	l.rate = rate
	l.lastTrigger = lastTrigger
	l.triggered = triggered
}

// Pacer drives a re-armable timer channel according to a Limiter. It has no
// internal goroutine: the owning goroutine receives from C(), does its work,
// then calls Advance() to arm the next tick. rate<=0 makes every tick fire
// immediately, so dispatch is gated only by frontier/channel backpressure.
type Pacer struct {
	limiter *Limiter
	rng     *rand.Rand
	timer   *time.Timer
}

func NewPacer(limiter *Limiter, rng *rand.Rand) *Pacer {
	p := &Pacer{limiter: limiter, rng: rng}
	p.arm()
	return p
}

func (p *Pacer) arm() {
	d := p.limiter.NextInterval(p.rng, time.Now())
	if p.timer == nil {
		p.timer = time.NewTimer(d)
		return
	}
	p.timer.Reset(d)
}

// C returns the tick channel. After a receive from C, call Advance to arm
// the next tick.
func (p *Pacer) C() <-chan time.Time {
	return p.timer.C
}

// Advance arms the next tick.
func (p *Pacer) Advance() {
	p.arm()
}

// Stop releases the underlying timer.
func (p *Pacer) Stop() {
	if p.timer != nil {
		p.timer.Stop()
	}
}

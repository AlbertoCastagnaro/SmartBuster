package httpclient

import (
	"math/rand"
	"time"
)

// DefaultJitter is the default fractional jitter applied to the pacing
// interval: actual interval is drawn uniformly from [1-JITTER, 1+JITTER].
const DefaultJitter = 0.30

// backoffFallbackRate is the pacing rate a backoff falls back to when the
// configured rate is unbounded (0). Multiplying an unbounded "0 interval"
// by the backoff factor is still 0 — hammering is exactly what triggers a
// backoff, so an active backoff must impose real pacing even then.
const backoffFallbackRate = 2.0 // req/s

// Limiter computes jittered pacing intervals for the coordinator's single
// global dispatch gate (spec §5: "this is the only pacing point — worker
// count does not affect aggregate rate"). It is not goroutine-safe; it is
// designed to be owned and driven by exactly one goroutine (the coordinator),
// consistent with the single-owner architecture and the requirement to use
// one explicit seeded *rand.Rand rather than shared/global state.
type Limiter struct {
	rate   float64 // requests/sec; <= 0 means unbounded
	jitter float64

	backoffMultiplier float64
	backoffUntil      time.Time
}

func NewLimiter(rate, jitter float64) *Limiter {
	if jitter <= 0 {
		jitter = DefaultJitter
	}
	return &Limiter{rate: rate, jitter: jitter, backoffMultiplier: 1}
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

// BackoffState/SetBackoffState round-trip a Limiter's WAF-onset backoff
// window (Phase 5a session save/resume, spec §6's "wafState").
func (l *Limiter) BackoffState() (multiplier float64, until time.Time) {
	return l.backoffMultiplier, l.backoffUntil
}

func (l *Limiter) SetBackoffState(multiplier float64, until time.Time) {
	l.backoffMultiplier = multiplier
	l.backoffUntil = until
}

// NextInterval returns the jittered wait duration until the next dispatch
// slot, drawing the jitter factor from rng. If a backoff window is active
// (see TriggerBackoff), the interval is scaled by the backoff factor.
func (l *Limiter) NextInterval(rng *rand.Rand, now time.Time) time.Duration {
	inBackoff := now.Before(l.backoffUntil)

	if l.Unbounded() {
		if !inBackoff {
			return 0
		}
		base := float64(time.Second) / backoffFallbackRate
		return time.Duration(base * l.backoffMultiplier)
	}

	base := float64(time.Second) / l.rate
	jitterFactor := (1 - l.jitter) + rng.Float64()*(2*l.jitter)
	interval := base * jitterFactor

	if inBackoff {
		interval *= l.backoffMultiplier
	}
	return time.Duration(interval)
}

// TriggerBackoff multiplies the pacing interval by factor until window
// elapses, per spec §6.4 (WAF/rate-limit onset).
func (l *Limiter) TriggerBackoff(factor float64, window time.Duration, now time.Time) {
	l.backoffMultiplier = factor
	l.backoffUntil = now.Add(window)
}

// InBackoff reports whether a backoff window is currently active.
func (l *Limiter) InBackoff(now time.Time) bool {
	return now.Before(l.backoffUntil)
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

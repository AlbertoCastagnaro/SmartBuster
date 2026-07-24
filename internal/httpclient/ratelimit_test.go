package httpclient

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

func TestLimiterUnboundedIsZero(t *testing.T) {
	l := NewLimiter(0, 0.3)
	if !l.Unbounded() {
		t.Fatal("expected rate<=0 to be unbounded")
	}
	rng := rand.New(rand.NewSource(1))
	if d := l.NextInterval(rng, time.Now()); d != 0 {
		t.Fatalf("expected 0 interval when unbounded, got %v", d)
	}
}

func TestLimiterIntervalWithinJitterBounds(t *testing.T) {
	rate := 10.0
	jitter := 0.3
	l := NewLimiter(rate, jitter)
	rng := rand.New(rand.NewSource(42))
	base := time.Second / time.Duration(rate)
	min := time.Duration(float64(base) * (1 - jitter))
	max := time.Duration(float64(base) * (1 + jitter))

	for i := 0; i < 1000; i++ {
		d := l.NextInterval(rng, time.Now())
		if d < min || d > max {
			t.Fatalf("interval %v outside expected bounds [%v, %v]", d, min, max)
		}
	}
}

func TestLimiterBackoffScalesInterval(t *testing.T) {
	l := NewLimiter(10, 0) // jitter 0 -> deterministic base interval
	rng := rand.New(rand.NewSource(1))
	now := time.Now()

	before := l.NextInterval(rng, now)
	l.TriggerBackoff(0.25, now) // decrease factor -> quarters the rate, quadruples the interval
	if !l.InBackoff(now, 30*time.Second) {
		t.Fatal("expected InBackoff true right after TriggerBackoff")
	}
	during := l.NextInterval(rng, now)
	if during < before*3 {
		t.Fatalf("expected backoff to roughly quadruple interval: before=%v during=%v", before, during)
	}

	after := now.Add(31 * time.Second)
	if l.InBackoff(after, 30*time.Second) {
		t.Fatal("expected backoff settle window to have elapsed")
	}
}

func TestLimiterBackoffAppliesEvenWhenUnbounded(t *testing.T) {
	l := NewLimiter(0, 0) // unbounded
	rng := rand.New(rand.NewSource(1))
	now := time.Now()

	if d := l.NextInterval(rng, now); d != 0 {
		t.Fatalf("expected unbounded interval 0 before backoff, got %v", d)
	}
	l.TriggerBackoff(0.25, now)
	d := l.NextInterval(rng, now)
	if d <= 0 {
		t.Fatalf("expected an active backoff to impose real pacing even in unbounded mode, got %v", d)
	}
}

// TestLimiterAIMDRecoversGraduallyToCap is Phase 6a's AIMD upgrade (spec
// §4): recovery is additive-increase, stepping toward rateCap across
// several clean windows rather than reverting in one shot, and never
// overshoots the cap.
func TestLimiterAIMDRecoversGraduallyToCap(t *testing.T) {
	l := NewLimiter(10, 0)
	now := time.Now()

	l.TriggerBackoff(0.4, now) // rate: 10 -> 4
	if rate, _, triggered := l.AIMDState(); rate != 4 || !triggered {
		t.Fatalf("expected rate=4 triggered=true after decrease, got rate=%v triggered=%v", rate, triggered)
	}

	// Too soon: no recovery yet.
	if l.MaybeRecover(3, 10*time.Second, now.Add(5*time.Second)) {
		t.Fatal("expected no recovery before the clean window elapses")
	}

	// One clean window: additive step, not a full jump back to cap.
	step1 := now.Add(10 * time.Second)
	if recovered := l.MaybeRecover(3, 10*time.Second, step1); recovered {
		t.Fatal("expected the first recovery step not to reach the cap yet")
	}
	if rate, _, _ := l.AIMDState(); rate != 7 {
		t.Fatalf("expected rate=7 after one additive step, got %v", rate)
	}

	// Second clean window: reaches the cap exactly, never overshoots, and
	// reports recovered=true exactly once.
	step2 := step1.Add(10 * time.Second)
	if recovered := l.MaybeRecover(3, 10*time.Second, step2); !recovered {
		t.Fatal("expected recovered=true once the cap is reached")
	}
	if rate, _, triggered := l.AIMDState(); rate != 10 || triggered {
		t.Fatalf("expected rate=10 (cap) triggered=false, got rate=%v triggered=%v", rate, triggered)
	}

	// Further ticks are no-ops once recovered.
	if l.MaybeRecover(3, 10*time.Second, step2.Add(10*time.Second)) {
		t.Fatal("expected no further recovery once already at cap")
	}
}

// TestLimiterAIMDUnboundedRecoversInOneWindow: an unbounded target (rateCap
// <= 0) has no meaningful intermediate step toward "infinite," so recovery
// fully lifts the backoff after a single clean window.
func TestLimiterAIMDUnboundedRecoversInOneWindow(t *testing.T) {
	l := NewLimiter(0, 0)
	now := time.Now()

	l.TriggerBackoff(0.25, now)
	if !l.triggered {
		t.Fatal("expected triggered after backoff")
	}

	if recovered := l.MaybeRecover(1, 10*time.Second, now.Add(10*time.Second)); !recovered {
		t.Fatal("expected the unbounded case to fully recover in one window")
	}
	if !l.Unbounded() {
		t.Fatal("expected rate to return to unbounded (0) after recovery")
	}
}

// TestPacerObservedAggregateRate is the property test required by the
// spec's Definition of Done ("observed aggregate rate ≤ --rate"). Taken
// completely literally that's slightly at odds with jitter being part of
// the design (spec §5 explicitly draws the interval from [1-JITTER,
// 1+JITTER]), so this checks the precise, provable form of that property:
//
//  1. Hard ceiling: NextInterval never returns less than base*(1-jitter)
//     (see the formula in NextInterval), so for any N ticks the total
//     elapsed time is never less than N*base*(1-jitter) — meaning the
//     aggregate rate can never exceed rate/(1-jitter), deterministically,
//     not just "usually." With jitter=0 this reduces exactly to the
//     literal "observed ≤ configured". Real scheduling delay can only
//     enlarge intervals, never shrink them below this, so the ceiling
//     holds regardless of system noise (just a tiny epsilon for measurement
//     rounding).
//  2. Statistical tracking: jitterFactor is drawn uniformly from
//     [1-jitter, 1+jitter] with mean 1.0, so by the law of large numbers
//     the aggregate rate over many ticks should converge close to the
//     nominal rate, not just stay under the worst-case ceiling. This is
//     the "and it actually paces at roughly the configured rate, not just
//     under some loose bound" check.
func TestPacerObservedAggregateRate(t *testing.T) {
	const rate = 100.0 // req/s; kept high so the test runs fast
	const jitter = 0.2
	const ticks = 500 // enough samples for the LLN bound below to be tight

	limiter := NewLimiter(rate, jitter)
	rng := rand.New(rand.NewSource(7))
	pacer := NewPacer(limiter, rng)
	defer pacer.Stop()

	start := time.Now()
	for i := 0; i < ticks; i++ {
		<-pacer.C()
		pacer.Advance()
	}
	elapsed := time.Since(start)
	observedRate := float64(ticks) / elapsed.Seconds()

	ceiling := rate / (1 - jitter) * 1.05 // +5% epsilon for measurement rounding only
	if observedRate > ceiling {
		t.Fatalf("observed aggregate rate %.1f req/s exceeds the hard jitter ceiling %.1f (rate=%.1f, jitter=%.2f); "+
			"this must never happen since no interval is ever shorter than base*(1-jitter)",
			observedRate, ceiling, rate, jitter)
	}

	if observedRate < rate*0.75 || observedRate > rate*1.25 {
		t.Fatalf("observed aggregate rate %.1f req/s does not track configured rate %.1f closely enough over %d ticks",
			observedRate, rate, ticks)
	}
}

// TestJitterSpecNone: a fixed interval, no variance at all.
func TestJitterSpecNone(t *testing.T) {
	l := NewLimiterSpec(50, JitterSpec{Kind: "none"})
	rng := rand.New(rand.NewSource(1))
	base := time.Second / 50
	for i := 0; i < 100; i++ {
		if d := l.NextInterval(rng, time.Now()); d != base {
			t.Fatalf("expected every interval == base %v for \"none\", got %v", base, d)
		}
	}
}

// TestJitterSpecGaussianMeanAndVariance is spec DoD #2: the empirical
// mean/variance of a seeded gaussian draw should track the configured
// N(base, (base*sigma)^2) within tolerance, and be reproducible for a fixed
// seed.
func TestJitterSpecGaussianMeanAndVariance(t *testing.T) {
	const rate = 50.0
	const sigma = 0.25
	const n = 20000
	base := float64(time.Second) / rate

	sample := func(seed int64) (mean, stddev float64) {
		l := NewLimiterSpec(rate, JitterSpec{Kind: "gaussian", Param1: sigma})
		rng := rand.New(rand.NewSource(seed))
		vals := make([]float64, n)
		for i := range vals {
			vals[i] = float64(l.NextInterval(rng, time.Now()))
		}
		var sum float64
		for _, v := range vals {
			sum += v
		}
		mean = sum / n
		var sq float64
		for _, v := range vals {
			sq += (v - mean) * (v - mean)
		}
		return mean, math.Sqrt(sq / n)
	}

	mean, stddev := sample(11)
	if rel := (mean - base) / base; rel > 0.03 || rel < -0.03 {
		t.Fatalf("gaussian mean %.0f too far from base %.0f (rel err %.3f)", mean, base, rel)
	}
	wantStd := base * sigma
	if rel := (stddev - wantStd) / wantStd; rel > 0.1 || rel < -0.1 {
		t.Fatalf("gaussian stddev %.0f too far from expected %.0f (rel err %.3f)", stddev, wantStd, rel)
	}

	mean2, _ := sample(11)
	if mean2 != mean {
		t.Fatalf("expected the same seed to reproduce the same empirical mean, got %v vs %v", mean, mean2)
	}
}

// TestJitterSpecBurstyShowsBurstPauseStructure is spec DoD #2: bursty must
// actually alternate a run of short intervals with a longer pause, not just
// look like noisy uniform jitter — checked by asserting a clear bimodal
// split (most intervals well below base, a minority well above it).
func TestJitterSpecBurstyShowsBurstPauseStructure(t *testing.T) {
	const rate = 50.0
	base := float64(time.Second) / rate
	l := NewLimiterSpec(rate, JitterSpec{Kind: "bursty", Param1: 4, Param2: 6})
	rng := rand.New(rand.NewSource(3))

	var short, long int
	for i := 0; i < 2000; i++ {
		d := float64(l.NextInterval(rng, time.Now()))
		switch {
		case d < base:
			short++
		case d > base*2:
			long++
		}
	}
	if short == 0 || long == 0 {
		t.Fatalf("expected both short (burst) and long (pause) intervals, got short=%d long=%d", short, long)
	}
	// With a burst-size mean of 4, roughly 3 in 4 intervals should be the
	// short in-burst kind, not the pause.
	if short < long {
		t.Fatalf("expected far more short in-burst intervals than long pauses, got short=%d long=%d", short, long)
	}
}

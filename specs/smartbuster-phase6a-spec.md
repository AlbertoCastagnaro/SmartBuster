# smartbuster — Phase 6a Build Specification (Modes, Timing & Request Shape)

*Build-ready spec for the first two stealth tiers on the existing HTTP client: mode presets, jitter distributions, adaptive backoff, realistic header profiles, and making `mode` live. Plus the deferred rate-guarantee rigor audit. Tier 3 (TLS/HTTP-2 fingerprint mimicry + proxies) is Phase 6b. Read alongside the implementation plan (Phase 6) and the stealth design discussion (three-tier model).*


## 1. Integration contract — verify against committed code

| # | Attachment point | Confirmed | 6a action |
|---|---|---|---|
| A | Global token-bucket `Pacer`/`Limiter`, **single-goroutine-owner** on the coordinator | confirmed | Enrich with jitter *distributions* + time-budget pacing; still coordinator-only |
| B | Phase 1 §6.4 minimal WAF-onset backoff (`BACKOFF_FACTOR`/`BACKOFF_WINDOW`) + `throttle` event | confirmed | Upgrade to a proper **AIMD adaptive controller** (§4) |
| C | Worker sets request headers (a `User-Agent` today) | confirm current UA handling | Add **ordered realistic header profiles** + stable per-session identity (§5) |
| D | `PATCH /scans/{id}` `mode` accepted but **inert** (5a) | confirmed | Wire it to **presets**, applied live via `controlCh` (§2) |
| E | HTTP client is stock `net/http` (`httpclient.Client`) | confirmed | 6a stays on it; define the **client boundary** 6b will swap behind (§6) |
| F | `dirRand`/seeded RNG; `TestCoordinator_ObservesConfiguredRate` exists (rigor deferred from Phase 3) | confirmed | Make jitter draws seeded; rewrite that test as a statistical property test (§7) |

## 2. Modes as presets

A **preset** is a coherent bundle, not a pile of flags. Users pick a mode; explicit flags override individual fields on top.

```go
type Mode string // "fast" | "normal" | "quiet" | "stealth"
type Preset struct {
	RateCap        float64        // req/s; 0 = unbounded
	Jitter         JitterSpec     // §3
	Concurrency    int
	HeaderProfile  string         // §5: "chrome"|"firefox"|"safari"|"minimal"
	OrderJitter    bool           // §5 intra-band shuffle
	Referer        bool           // §5 referer chains
	Backoff        BackoffSpec    // §4
	Epsilon        float64        // exploration; 0 in stealth
	// 6b fields (present, unused in 6a): TLSProfile, Proxies, HTTP2FP
}
```
| Mode | RateCap | Jitter | Concurrency | Headers | Backoff | Notes |
|---|---|---|---|---|---|---|
| `fast` | 0 | none | high | `minimal` | off | CTF/authorized-loud |
| `normal` | 0 | light uniform ±0.15 | default | `chrome` | gentle | default |
| `quiet` | low (e.g. 5) | gaussian or bursty | low | `chrome`, referer, order-jitter | AIMD on | log/threshold evasion |
| `stealth` | very low | bursty/human | low | full profile, ε=0 | AIMD aggressive | **+ 6b fingerprint/proxies** |

`PATCH mode` → `controlCh` → coordinator applies the preset **live** (reconfigure pacer distribution + rate, swap header profile, set backoff/epsilon), single-writer. This makes the 5b UI's `mode` field real (its handoff already renders it reserved — no UI rework, just this wiring).

## 3. Jitter distributions (tier 1)

The pacer draws each inter-request interval from a seeded distribution (reproducibility preserved):
```go
type JitterSpec struct { Kind string; Param1, Param2 float64 } // "none"|"uniform"|"gaussian"|"bursty"
```
- **uniform**: interval ∈ `base·[1-J, 1+J]` (current behavior; `J=Param1`).
- **gaussian**: interval ∼ `N(base, (base·σ)²)`, clamped ≥ 0 (`σ=Param1`). Less metronomic than uniform.
- **bursty/human**: alternate short bursts (a few near-back-to-back requests) with longer pauses — models human/browser traffic, which is bursty, not evenly spaced. Param1 = burst size mean, Param2 = pause multiplier.
- **time-budget pacing** (orthogonal, composable): given `--budget T`, target rate = `remainingFrontier / remainingTime`, recomputed as the frontier grows, so the scan spreads over `T` regardless of size.

All draws use the seeded RNG on the coordinator goroutine.

## 4. Adaptive backoff controller (tier 1, upgrades Phase 1 §6.4)

Replace the one-shot multiply with **AIMD** (additive-increase/multiplicative-decrease), the TCP-congestion pattern:
- **Triggers** (from calibration's signals — reuse, don't rebuild): a `429` spike, a WAF-challenge cluster, a `403` spike, or a latency spike vs. baseline. Any → **multiplicative decrease**: `rate *= BACKOFF_DECREASE` (default 0.5), emit `throttle`.
- **Recovery**: on a clean window (no triggers for `RECOVERY_WINDOW`), **additive increase** `rate += BACKOFF_STEP` toward the preset's `RateCap` — never overshoot the cap.
- Coordinator-owned; the pacer reads the current adaptive rate each tick. Emit `throttle` on decrease and a `warning`(`Source:"throttle-recovered"`) when back at cap, so the 5b log shows the backoff curve.

## 5. Request shape (tier 2)

- **Header profiles**: realistic browser header *sets* with correct *values* — `User-Agent`, `Accept`, `Accept-Language`, `Accept-Encoding`, `Sec-Fetch-*`, `sec-ch-ua`, `Upgrade-Insecure-Requests` — one profile (`chrome`/`firefox`/`safari`) per session. **Stable per-session identity**: pick one profile at scan start, never rotate UA per request (per-request rotation is a *bigger* tell than one consistent fake). `minimal` profile = today's behavior.
  - **Scope note**: precise header *ordering* and `Host`-header casing are a fingerprint layer `net/http` won't fully honor (its `Header` is an unordered map). Faithful header order is **6b** (the tls-client controls it). 6a nails values + set + identity; ordering fidelity is called out as 6b's job so it isn't half-done here.
- **Referer chains**: when enabled, set a plausible `Referer` — for a recursion child, its parent dir; for root candidates, the site root or empty — so traffic reads as navigation, not enumeration.
- **Order jitter**: priority dispatch is already non-lexical (so it avoids the classic alphabetical-scan tell for free); `OrderJitter` adds a *seeded* shuffle **within a priority band** (near-equal scores) so ordering isn't perfectly deterministic, without discarding the smart prioritization. Off by default; on in quiet/stealth.

## 6. The client boundary (sets up 6b)

Define the seam 6b swaps behind, without changing behavior in 6a:
```go
type HTTPDoer interface { Do(ctx context.Context, req Request) (Response, error) }
```
`net/http` impl = today's client (fast/normal/quiet). 6b adds a `tls-client` impl selected by the preset's (currently unused) `TLSProfile`/`Proxies` fields. 6a just introduces the interface and routes the worker through it, so 6b is a drop-in rather than surgery.

## 7. The rate-guarantee rigor audit (deferred from Phase 3 — now due)

`TestCoordinator_ObservesConfiguredRate` becomes a **statistical property test**, because the rate cap is now a *stealth guarantee*, not just politeness:
- Run K seeded scans at a target `RateCap`; measure observed aggregate req/s over sliding windows.
- Assert: window-mean within tolerance of target; **95th-percentile window rate ≤ target·1.1** (a real upper-bound, not just "average is fine"); and the empirical **inter-request interval distribution matches the configured `JitterSpec`** (mean/variance within tolerance per kind).
- Add: adaptive backoff **engages** on a simulated `429`/WAF cluster and **recovers** to cap on a clean window (AIMD curve observed).
- This is the test that certifies stealth-mode pacing actually holds under concurrency.

## 8. Config & defaults
```go
Mode        string        // --mode; default "normal"
Budget      time.Duration // --budget; 0 = off (time-budget pacing)
JitterKind  string        // override preset
HeaderProfile string      // override preset
// backoff
```
| Constant | Default | Meaning |
|---|---|---|
| `BACKOFF_DECREASE` | 0.5 | multiplicative decrease on throttle |
| `BACKOFF_STEP` | 0.5 req/s | additive recovery increase |
| `RECOVERY_WINDOW` | 10 s | clean window before recovery |

## 9. Tests & DoD

1. **Modes live**: `PATCH mode` reconfigures pacer/headers/backoff/epsilon via `controlCh`, `-race` clean with a running scan; each preset yields its bundle.
2. **Jitter distributions**: each kind produces the correct empirical mean/variance (seeded, reproducible); bursty shows burst/pause structure.
3. **Time-budget pacing**: a scan with `--budget T` completes in ≈ T regardless of frontier size.
4. **Adaptive backoff**: AIMD engages on simulated `429`/WAF cluster, emits `throttle`, recovers to cap on a clean window (curve asserted).
5. **Header shape**: one stable profile per session (no per-request UA change — asserted); realistic values; referer chains correct; order-jitter shuffles only within a band.
6. **Rate rigor (§7)**: the statistical property test passes — mean + 95th-percentile bound + jitter-distribution match.
7. **Client boundary**: worker routes through `HTTPDoer`; net/http impl unchanged in behavior.
8. `go build`, `go vet`, `go test -race ./...` clean.

## 10. Build order & handoff
Pre-6 patch (§0) → `Preset`/`Mode` system + wire `PATCH mode` via `controlCh` → `JitterSpec` distributions in the pacer → AIMD adaptive controller (reusing calibration triggers) → header profiles + stable identity + referer + order-jitter → `HTTPDoer` boundary → the §7 rigor audit. Build the preset system + `controlCh` mode-wiring first (it's the control surface everything else attaches to).

**Before Phase 6b, report back:** the `Preset` struct + `HTTPDoer` boundary as built, how `mode` switching reconfigures the running pacer, and any deviation. **6b** plugs `bogdanfinn/tls-client` into the `HTTPDoer` seam behind the `stealth` preset's `TLSProfile`/`Proxies` fields, adds browser TLS/HTTP-2 fingerprint profiles + header-order fidelity + proxy/Tor rotation — the tier-3 fingerprint layer this phase deliberately left as a clean seam.

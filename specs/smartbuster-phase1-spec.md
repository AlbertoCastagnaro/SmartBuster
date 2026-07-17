# smartbuster — Phase 1 Build Specification (MVP Core)

*Build-ready spec for the coordinator/worker skeleton + calibration engine. Precise enough to implement without re-deriving design intent. Read alongside `smartbuster-implementation-plan.md` (§4, §5, Phase 0–1) and `smartbuster-concept.md`.*

---

## 0. Scope

**In scope for Phase 1**
- Coordinator/worker actor skeleton with global-paced dispatch (Phase 0 foundation, included here since Phase 1 builds directly on it).
- Tuned HTTP client, global token-bucket rate limiter with jitter, scope enforcer, `--dry-run`.
- Similarity-hash service (SimHash) + body normalization.
- **Calibration subsystem** (the load-bearing piece): per-directory negative baseline, variance-derived threshold, multi-signal classification, SPA/wildcard/WAF detection.
- Frequency-ordered frontier (flat wordlist → priority heap).
- Recursion with depth/scope/budget/time limits + content-novelty trap pruning.
- Confidence model + gate.
- JSONL audit log; results as tree + flat list + plaintext.
- Adversarial fixture test server + acceptance tests.

**Explicitly deferred**
- SQLite tagged corpus, tech detection, nmap, extension permutation → Phase 2.
- Co-occurrence, Markov, response-semantics boosting, exploration/exploitation → Phase 3.
- Wayback/robots/sitemap/JS harvesting → Phase 4.
- Web UI → Phase 5. Stealth fingerprinting → Phase 6.

Phase 1's frontier scoring is **commonality-prior only**, but the scoring function and the event emission are built with the hooks later phases plug into.

## 1. Package layout

```
smartbuster/
  go.mod                         # module github.com/<you>/smartbuster
  cmd/smartbuster/main.go        # CLI entry, flag parsing, wires engine
  internal/engine/
    coordinator.go               # owns all mutable state; the dispatch loop
    worker.go                    # stateless HTTP executor
    frontier.go                  # priority heap
    types.go                     # shared structs (below)
    config.go                    # Config + defaults
    events.go                    # Event types + emitter
  internal/calibration/
    calibration.go               # Calibrate(), Classify()
    normalize.go                 # body normalization pipeline
  internal/simhash/simhash.go    # SimHash + Hamming
  internal/httpclient/
    client.go                    # tuned Transport
    ratelimit.go                 # global token bucket + jitter
  internal/scope/scope.go        # in/out-of-scope decisions
  internal/wordlist/wordlist.go  # load flat frequency-ordered list
  internal/audit/audit.go        # JSONL writer
  internal/output/results.go     # tree + list + plaintext, dedup
  test/fixtures/server.go        # adversarial httptest server
```

Rule: workers hold no shared mutable state; only the coordinator mutates the frontier, baselines, tree, and learners. Cross-goroutine communication is via channels only.

## 2. Core types (`internal/engine/types.go`)

```go
package engine

import "time"

type CandidateType int

const (
	TypeDir CandidateType = iota
	TypeFile
	TypeStem     // extension appended at runtime (Phase 2)
	TypeFullPath
)

// Candidate: one path to test.
type Candidate struct {
	Path       string        // relative to base, e.g. "admin" or "config.php"
	Type       CandidateType
	BasePrio   float64       // commonality prior (Phase 1: normalized wordlist rank)
	Score      float64       // effective frontier priority
	Depth      int
	ParentDir  string        // directory this lives under; "" = root
	Provenance string        // "wordlist" | "recursion:/admin" | ...
}

// ResponseSignature: compact per-response fingerprint. Computed IN THE WORKER
// (normalization needs the requested path; keeps large bodies off the channel).
type ResponseSignature struct {
	Status      int
	BodyLen     int
	WordCount   int
	SimHash     uint64        // over the normalized body
	RawBodyHash uint64        // xxhash of normalized body, for exact dedup
	RedirectTo  string        // normalized redirect target ("" if none)
	ContentType string
	SetCookie   bool
	Reflected   bool          // requested token echoed in body (diagnostic)
	Elapsed     time.Duration
}

// Baseline: learned "not found" profile for one directory.
type Baseline struct {
	Dir         string
	Samples     []ResponseSignature
	NoiseFloor  int           // max intra-baseline Hamming + margin
	LenMean     float64
	LenStdDev   float64
	RepStatus   int           // representative not-found status
	RepSimHash  uint64
	RepRedirect string
	IsWildcard  bool
	IsSPA       bool
}

type Classification struct {
	IsHit      bool
	Confidence float64        // [0,1]
	Reason     string
}

type Finding struct {
	URL         string
	Status      int
	Size        int
	Confidence  float64
	Provenance  string
	ContentHash uint64
	Aliases     []string
}

// Channel messages.
type WorkItem struct {
	Candidate Candidate
	URL       string
	IsProbe   bool          // calibration probe vs. real candidate
	ProbeTag  string        // groups probes belonging to one directory
}

type WorkResult struct {
	Item      WorkItem
	Signature ResponseSignature
	Err       error
}
```

## 3. Coordinator (`coordinator.go`)

Single goroutine owning: `frontier`, `map[string]*Baseline` (per-dir), the discovered tree, `seenContent map[uint64][]string` (hash → URLs, for dedup/aliases), counters, and the RNG (seeded).

Per-directory lifecycle is a small state machine: a directory is `NEW → CALIBRATING → SCANNING → DONE`. Real candidates for a directory are not dispatched until its baseline exists.

```
run(ctx):
  seedRootDirectory()                       // enqueue root as a NEW directory
  for ctx not cancelled:
    select:
      case res := <-results:
        handleResult(res)                    // classify / build baseline / recurse
      case <-pacer.C:                        // global rate gate (see §5)
        item, ok := nextDispatchable()       // calibration probes first, then top-of-frontier
        if ok: workCh <- item
      case <-ctx.Done(): drainAndExit()
    if frontier.Empty() && noInFlight() && allDirsDone(): finish()

handleResult(res):
  if res.IsProbe:
    collectProbe(res)                        // when a dir's probes complete -> Calibrate()
    if dirProbesComplete(dir):
      baselines[dir] = Calibrate(dir, collectedProbes[dir])
      emit(Event{Type:"calibration.done", ...})
      dir.state = SCANNING
    return
  b := baselines[res.Item.Candidate.ParentDir]
  c := Classify(res.Signature, b)
  auditWrite(res, c)                         // ALWAYS, lossless
  if c.IsHit:
    f := makeFinding(res, c)
    if dup := seenContent[res.Signature.RawBodyHash]; dup != nil:
      addAlias(dup, f.URL); return           // content already seen -> alias, no recurse
    seenContent[hash] = append(...)
    recordFinding(f); emit(Event{Type:"hit", ...})
    if isDirectory(res) && c.Confidence >= RECURSE_MIN_CONF && withinLimits(res):
      enqueueChildDirectory(f.URL)           // NEW dir -> triggers its own calibration
  detectWAFOnset(res)                        // §6.4
```

`nextDispatchable()` priority: (1) pending calibration probes for any `CALIBRATING` dir, (2) highest-score candidate from a `SCANNING` dir. This guarantees calibration gates real requests.

## 4. Worker (`worker.go`)

```
worker(ctx, workCh, resultsCh, client):
  for item := range workCh:
    resp, elapsed, err := client.Do(ctx, item.URL)   // no auto-redirect follow
    if err: resultsCh <- WorkResult{item, {}, err}; continue
    body := readCapped(resp.Body, MAX_BODY)           // 256 KiB cap
    norm := Normalize(body, requestedToken(item))     // §7
    sig := ResponseSignature{
      Status: resp.StatusCode, BodyLen: len(body),
      WordCount: countWords(norm), SimHash: SimHash(norm),
      RawBodyHash: xxhash(norm),
      RedirectTo: normalizeRedirect(resp.Header.Get("Location")),
      ContentType: mediaType(resp), SetCookie: resp.Header.Get("Set-Cookie") != "",
      Reflected: item.IsProbe && bytes.Contains(body, requestedToken(item)),
      Elapsed: elapsed,
    }
    resultsCh <- WorkResult{item, sig, nil}
```

Workers are pure functions of `(item, network)`; they never touch coordinator state.

## 5. HTTP client & rate limiter

**Transport (`httpclient/client.go`).** `MaxIdleConns` high, `MaxIdleConnsPerHost` = concurrency, `MaxConnsPerHost` = `PER_HOST_CONN_CAP` (default 20), `IdleConnTimeout` 90s, keep-alive on, `DisableCompression=false`. **Redirects NOT followed** (`CheckRedirect` returns `ErrUseLastResponse`) — we classify the 30x itself. Per-request timeout `REQUEST_TIMEOUT` (default 10s).

**Rate limiter (`ratelimit.go`).** Single global token bucket feeding the coordinator's `pacer`. The bucket emits a tick every `interval`, where `interval = (1/rate) * jitterFactor` and `jitterFactor` is drawn per-tick from the configured distribution. **This is the only pacing point** — worker count does not affect aggregate rate. Default distribution: uniform in `[1-JITTER, 1+JITTER]` (JITTER default 0.3). `rate=0` means "unbounded" (bucket disabled; workers pull as fast as they complete).

## 6. Calibration (`calibration/calibration.go`) — the core

### 6.1 Baseline construction

```
Calibrate(dir, probes []ResponseSignature) Baseline:
  # probes = N_PROBES * len(EXT_SET) signatures already gathered by the coordinator
  # EXT_SET (Phase 1 fixed): ["", ".php", ".html"]; N_PROBES default 5
  noiseFloor = 0
  for group in probesByExt(probes):
    for (a,b) in pairs(group):
      noiseFloor = max(noiseFloor, hamming(a.SimHash, b.SimHash))
  noiseFloor += HAMMING_MARGIN                      # default 3

  lenMean, lenStd = meanStdDev(lens(probes))
  rep = medoid(probes)                              # sample with min total distance to others

  isSPA = allStatus(probes, 200) and allHTML(probes) and
          maxPairwiseHamming(probes) <= SPA_SELFSIM # default 2
  isWildcard = (not isSPA) and
               not looksLikeStandard404(rep) and
               maxPairwiseHamming(probes) <= WILDCARD_SELFSIM  # default 4

  return Baseline{dir, probes, noiseFloor, lenMean, lenStd,
                  rep.Status, rep.SimHash, rep.RedirectTo, isWildcard, isSPA}
```

`looksLikeStandard404`: status in {404,410} OR (status 200 AND body clearly an error template). For Phase 1, `status in {404,410}` is sufficient; the wildcard test mainly catches "200-for-everything under this dir."

The coordinator generates probe paths as `dir + "/" + randToken(12) + ext`. `randToken` uses the seeded RNG so runs are reproducible.

### 6.2 Classification

```
Classify(sig, b) Classification:
  # redirect-to-baseline = not found
  if sig.RedirectTo != "" and sig.RedirectTo == b.RepRedirect:
    return {false, 0.95, "matches baseline redirect"}

  d = minHamming(sig.SimHash, simhashes(b.Samples))
  lenZ = abs(sig.BodyLen - b.LenMean) / max(b.LenStdDev, 1)

  if b.IsWildcard and d <= b.NoiseFloor:
    return {false, 0.9, "within wildcard-dir baseline"}
  if d <= b.NoiseFloor:
    return {false, 0.9, "within baseline noise floor"}

  # diverges -> candidate hit; corroborate
  conf = 0.5
  if statusClass(sig.Status) != statusClass(b.RepStatus): conf += 0.2
  if d > 2*b.NoiseFloor:                              conf += 0.2
  if lenZ > LEN_Z_THRESHOLD:                          conf += 0.1   # default 3.0
  conf = min(conf, 0.99)

  # 401/403 = exists-and-protected: strong signal even if body is a login page
  if sig.Status == 401 or sig.Status == 403:
    conf = max(conf, 0.85)

  return {true, conf, "diverges from baseline"}
```

`statusClass(n)` = `n/100` (2xx,3xx,4xx,5xx). `minHamming` compares against all baseline samples and takes the minimum (nearest not-found).

### 6.3 SPA handling (Phase 1 scope)
If `baseline.IsSPA`, emit a `warning` event ("brute-force likely futile: SPA catch-all") and continue at reduced expectation (do not error out). The actual pivot to JS harvesting is Phase 4; Phase 1 must at least detect it and avoid reporting the shell as thousands of hits (the noise-floor check already suppresses them).

### 6.4 WAF / rate-limit onset (minimal, Phase 1)
Maintain a small ring buffer of recent `(status, SimHash)`. If a *new* cluster appears — a run of responses that match neither any baseline nor prior findings, OR a spike of 429/403 — emit a `throttle` event and multiply the rate interval by `BACKOFF_FACTOR` (default 4) for `BACKOFF_WINDOW` (default 30s). Full adaptive backoff is Phase 6; Phase 1 just prevents poisoning and hammering.

## 7. Normalization (`calibration/normalize.go`)

Run in the worker, given the raw body and the requested token. Order matters:

1. Cap to `MAX_BODY` (256 KiB).
2. Lowercase.
3. Remove all occurrences of the requested path token (defeats reflected-path soft-404s). Harmless no-op when not reflected.
4. Regex-strip volatile substrings (replace with a constant):
   - CSRF/nonce/token attributes: `(?i)(csrf|nonce|token|_token|authenticity_token)["'=:\s]+[a-z0-9\-_]+`
   - Long hex/base64 blobs (session ids, hashes): `[a-f0-9]{16,}`, `[A-Za-z0-9+/]{24,}={0,2}`
   - UUIDs: `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`
   - ISO timestamps and epoch-like numbers: `\d{4}-\d{2}-\d{2}[t\s][\d:.\-+z]+`, `\b\d{10,13}\b`
5. Collapse all whitespace runs to a single space; trim.
6. Tokenize into word 2-shingles for SimHash input.

Both Go's `regexp` and the engine are RE2-based (linear time), so this is ReDoS-safe over hostile bodies.

## 8. SimHash (`simhash/simhash.go`)

Charikar 64-bit. For each 2-shingle feature: hash to `uint64` (xxhash); for each of 64 bits, `+1` if set else `-1`, weighted by feature count; final fingerprint bit = sign. `Hamming(a,b)=popcount(a^b)`. Similarity is expressed as raw Hamming distance throughout (lower = more similar); thresholds are Hamming integers, not ratios. TLSH is deferred (SimHash is sufficient for HTML-sized bodies at Phase 1).

## 9. Frontier (`frontier.go`)

A max-heap keyed by `Candidate.Score`. Phase 1 `Score = BasePrio`, where `BasePrio` is the normalized inverse rank from the wordlist (rank 0 → 1.0, decreasing). API: `Push(Candidate)`, `Pop() Candidate`, `Len()`, and `Reprioritize(fn)` (unused in Phase 1, present for later signals). When a directory enters `SCANNING`, its wordlist candidates are pushed with `ParentDir` set and `Depth = parent.Depth+1`.

## 10. Recursion & trap control

Enqueue a child directory (`enqueueChildDirectory`) only if ALL hold:
- `depth+1 <= MAX_DEPTH` (default 4)
- child URL in scope
- `dirRequestBudget[parent]` not exhausted (`PER_DIR_BUDGET`, default = wordlist size, i.e. one full pass)
- time spent in the parent branch `< TIME_PER_BRANCH` (default 0)=disabled; set in stealth)
- content not already in `seenContent` (novelty) — else record alias, don't recurse
- confidence `>= RECURSE_MIN_CONF` (default 0.7)

During a branch, if hit-rate stays `>= WILDCARD_HITRATE` (default 0.9) over a window, mark the branch wildcard-suspect and stop recursing it. Tarpit guard: if a branch's median `Elapsed >= 0.9*REQUEST_TIMEOUT`, deprioritize and cap it.

## 11. Audit log (`audit/audit.go`)

Append-only JSONL, one object per request (probes included), written for **every** result before any UI/console output. This file is the system of record and the eval instrumentation.

```json
{"ts":"2026-07-16T10:00:00.123Z","method":"GET","url":"https://t/admin",
 "req_headers":{"User-Agent":"..."},"status":301,"size":178,"elapsed_ms":42,
 "is_probe":false,"parent_dir":"/","provenance":"wordlist",
 "classified":{"is_hit":true,"confidence":0.9,"reason":"diverges from baseline",
   "baseline_dir":"/","hamming":37,"noise_floor":6},
 "sim_hash":"0x...","raw_hash":"0x..."}
```

Also record a header line at scan start: config, target(s), RNG seed, wordlist path + hash, ruleset/version. This makes the run replayable.

## 12. Output (`output/results.go`)

- **Tree**: nested by path; each node = confirmed Finding with status/size/confidence/provenance.
- **Flat list**: for exports; dedup by `ContentHash` with aliases collapsed.
- **Plaintext/stdout**: gobuster-style `PATH (Status: N) [Size: B] [conf: 0.xx]` for pipeline use.
- JSON export of Findings. (Burp/Markdown exports = Phase 5; the Finding model already carries what they need.)

## 13. Configuration & defaults (`config.go`)

```go
type Config struct {
	Targets      []string
	Wordlist     string
	Concurrency  int           // default 20
	Rate         float64       // req/s; 0 = unbounded; default 0
	Jitter       float64       // default 0.30
	MaxDepth     int           // default 4
	RequestTO    time.Duration // default 10s
	Seed         int64         // default: time-based, recorded to audit
	Scope        ScopeConfig
	DryRun       bool
	OutDir       string
}
```

| Constant | Default | Meaning |
|---|---|---|
| `N_PROBES` | 5 | negative probes per extension per directory |
| `EXT_SET` | `["", ".php", ".html"]` | calibration probe extensions (Phase 1 fixed) |
| `HAMMING_MARGIN` | 3 | added to intra-baseline max Hamming → noise floor |
| `SPA_SELFSIM` | 2 | max pairwise Hamming for SPA detection |
| `WILDCARD_SELFSIM` | 4 | max pairwise Hamming for wildcard detection |
| `LEN_Z_THRESHOLD` | 3.0 | length z-score to corroborate a hit |
| `RECURSE_MIN_CONF` | 0.7 | min confidence to recurse into a dir |
| `WILDCARD_HITRATE` | 0.9 | branch hit-rate that flags a wildcard trap |
| `PER_HOST_CONN_CAP` | 20 | max simultaneous connections per host |
| `MAX_BODY` | 256 KiB | body read cap before normalization |
| `BACKOFF_FACTOR` / `BACKOFF_WINDOW` | 4 / 30s | throttle response on WAF onset |
| `BACKOFF_FALLBACK_RATE` | 2 req/s | pacing rate an active backoff falls back to when the configured rate is unbounded (0) — multiplying an unbounded "0 interval" by `BACKOFF_FACTOR` is still 0, so backoff needs a real rate to fall back to (`internal/httpclient/ratelimit.go`) |
| `MIN_STRIPPABLE_TOKEN_LEN` | 3 | shortest requested-path token normalization will strip from a body; below this, a word-boundary-delimited strip of an extremely common short word (e.g. "a") would still over-match, so shorter tokens are left alone (`internal/calibration/normalize.go`) |
| `WILDCARD_WINDOW` | 20 | sliding window (sample count) over which a directory's hit-rate is evaluated for the wildcard/hit-rate trap guard (`internal/engine/coordinator.go`) |
| `TARPIT_WINDOW` | 10 | sample count of recent response times a directory's median-elapsed tarpit check is computed over (`internal/engine/coordinator.go`) |
| `WAF_RING_SIZE` | 20 | ring-buffer size (recent request outcomes) the WAF/rate-limit onset detector evaluates over (`internal/engine/coordinator.go`) |
| `WAF_SPIKE_THRESHOLD` | 5 | 429/403 count within the ring that trips backoff (`internal/engine/coordinator.go`) |
| `WAF_NOVEL_RUN_LENGTH` | 8 | consecutive responses matching neither any baseline nor a prior finding that trips backoff (`internal/engine/coordinator.go`) |

All tunable; these are starting values to be refined by the eval harness (Phase 7).

## 14. Adversarial fixture server (`test/fixtures/server.go`)

An `httptest.Server` with routes producing each pathology. Each is a table-driven test asserting the expected engine behavior:

| Fixture | Behavior | Expected |
|---|---|---|
| `hard404` | custom page, **status 200**, for unknown paths; real paths distinct | 0 false positives; real paths found |
| `reflected404` | 200 page that **echoes the requested path** | 0 false positives (normalization strips path) |
| `volatile404` | 200 page with a fresh timestamp/token each time | 0 false positives (normalization + noise floor) |
| `wildcardDir` | everything under `/files/` returns the same page | dir flagged wildcard; children suppressed |
| `spa` | every path → identical `index.html`, 200 | `IsSPA` true; warning emitted; no hit flood |
| `redirect404` | unknown → 302 `/login`; real → 200 | redirect-to-baseline suppressed; real found |
| `ratelimited` | 200 until N req, then 429 | throttle event; backoff engages |
| `infiniteDir` | `/a/a/a/...` always 200, self-similar | branch pruned by novelty/self-similarity |
| `tarpit` | responds after ~timeout | branch deprioritized/capped; no hang |
| `honest` | normal 404s; a known set of real paths | full recall of the known set |

## 15. Definition of Done

1. `smartbuster scan <host> -w list.txt` runs concurrently; observed **aggregate** rate ≤ `--rate` (property test); out-of-scope refused; `--dry-run` prints without sending.
2. Full adversarial suite passes: **zero false positives** on `hard404`/`reflected404`/`volatile404`/`redirect404`; SPA + wildcard detected; rate-limit → backoff; traps pruned without hanging; full recall on `honest`.
3. `go test -race ./...` clean.
4. Audit log is complete and replays the run (seed recorded); results emit as tree + list + plaintext.
5. On a soft-404 target, false-positive count is **zero vs. gobuster's many** (the headline Phase 1 win).

---

**Handoff note for Claude Code.** Build in package order: `simhash` → `normalize` → `httpclient`/`ratelimit` → `scope` → `types`/`frontier` → `worker` → `calibration` → `coordinator` → `audit`/`output` → `cmd`. Write the fixture server and the calibration table-tests *first* (before the coordinator) so calibration is validated in isolation. The two companion docs carry the rationale; this spec carries the contracts.

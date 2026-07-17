# smartbuster — Phased Implementation Plan

*Detailed technical plan covering the full roadmap, from MVP through v2 stealth.*

---

## 1. Scope & structure of this document

This single document covers the **entire roadmap**, MVP through the v2 stealth engine, organised into sequential phases. v2 work (the stealth/request-engine hardening) is isolated in **Phase 6** and clearly marked. The rationale for one document rather than a separate v2 plan: the phases build on shared primitives and each other, so a single roadmap stays coherent and avoids drift between documents. The high-level vision and contributions live in the companion **`smartbuster-concept.md`**.

- **MVP** = Phases 0–1 (a working, calibrated, frequency-ordered enumerator that already beats naive tools on soft-404 handling and ordering).
- **Beta** = Phases 2–4 (profiling, dynamic intelligence, discovery expansion).
- **1.0** = Phase 5 (web UI) + Phase 7 (evaluation & hardening).
- **v2** = Phase 6 (stealth & request-engine fidelity).

## 2. Goals and non-goals

**Goals**
- Discover more paths with fewer requests than gobuster/ffuf/feroxbuster on the same corpus (efficiency = speed + stealth).
- Near-zero false positives on soft-404 / catch-all / wildcard targets.
- Profile-driven wordlist selection and runtime extension generation.
- Adaptive, explainable prioritisation with full provenance.
- Live observability (web UI) and a lossless, reportable audit trail.
- Realistic, tiered stealth (v2).

**Non-goals**
- No exploitation — reconnaissance only.
- No ML/LM ranker and no external advisor/plugin API (heuristic + lightweight statistical models only).
- No attempt at exhaustive commercial-grade technology coverage — open-fork fingerprints are sufficient.
- Not a general web crawler or a vulnerability scanner (though it integrates with both).

## 3. Language & technology stack

**Core language: Go** (decision rationale recorded separately; summary: the actor-model concurrency backbone is Go-native, `wappalyzergo` gives a maintained tech-detection engine, the ProjectDiscovery ecosystem provides reference/libraries, and `bogdanfinn/tls-client` covers stealth fingerprinting — the tool is I/O-bound so Rust's raw-perf/fingerprint-fidelity edge is marginal).

| Concern | Library / approach |
|---|---|
| HTTP client (core) | `net/http` with a tuned `Transport` (keep-alive, per-host caps) |
| Retries/backoff | `projectdiscovery/retryablehttp-go` (or equivalent) |
| Tech fingerprinting | `projectdiscovery/wappalyzergo` + open-fork signatures + custom rules |
| Favicon hashing | `mmh3` |
| Similarity hashing | SimHash (64-bit) for fast body compare; TLSH for larger bodies |
| HTML parsing | `golang.org/x/net/html` |
| Corpus store | SQLite via pure-Go `modernc.org/sqlite` (no cgo) |
| nmap parsing | `encoding/xml` (+ a thin nmap-XML struct set) |
| CLI/config | `projectdiscovery/goflags` or `cobra`+`viper` |
| Web server | `net/http` + `coder/websocket` (formerly nhooyr) |
| UI assets | embedded via `embed.FS` (single self-contained binary) |
| Web UI frontend | Svelte or React, built to static assets, embedded |
| Stealth (v2) | `bogdanfinn/tls-client` (uTLS + patched HTTP/2 profiles) |
| Headless (optional) | `playwright-go` — out-of-process, optional heavy mode |

## 4. Architecture overview

```
                +-------------------------------------------------+
                |                   Engine (Go pkg)               |
                |                                                 |
  CLI  ───────► |   Coordinator (single owner of mutable state)   |
 (in-process)   |     • priority frontier (scored heap)           |
                |     • discovered tree                            |
                |     • per-directory baselines (calibration)     |
                |     • online learners (Markov, co-occurrence)   |
                |     • target profile                            |
                |          ▲ results        │ dispatch (paced)    |
                |          │                ▼                     |
                |     ┌──────────── worker pool (N, stateless)────┐|
                |     │  HTTP I/O only; global rate limiter gate  │|
                |     └───────────────────────────────────────────┘|
                |          │ event stream (one source of truth)   |
                +──────────┼──────────────────────────────────────+
                           │
        ┌──────────────────┼───────────────────┐
        ▼                  ▼                    ▼
   Audit log          Web UI daemon         Results/exports
   (JSONL, all)       (REST + WS, sampled)  (JSON/Burp/MD/txt)
```

**Key architectural decisions (from design discussion):**

- **Actor / coordinator model.** A single coordinator owns *all* mutable state; a pool of stateless workers performs only HTTP I/O. Communication is via channels. This eliminates data races on the frontier and learners by construction (single owner, no locks on shared state), makes backpressure automatic (bounded channels), and gives one place to tap for observability.
- **Global pacing.** The rate limiter is a single global token bucket; the coordinator paces dispatch so the *aggregate* rate is controlled. **Concurrency and request-rate are independent knobs** (e.g. 20 workers at 2 req/s).
- **Two front-ends, one binary.** The CLI links the engine in-process. The GUI runs the engine as a localhost daemon serving an embedded web UI. Same engine package, two entry points.
- **One event stream, multiple consumers.** The coordinator emits typed events; the audit log persists all of them losslessly, the UI receives a sampled/aggregated subset, and results are distilled from them.

## 5. Cross-cutting primitives

These few components reappear across many subsystems — building them well once pays off repeatedly.

- **Similarity-hash service.** SimHash/TLSH over normalised response bodies. Used by **calibration** (is this the not-found page?), **dedup** (have we seen this content?), and **trap detection** (is this branch self-similar?).
- **Confidence model & gate.** Every classification produces a confidence score; only high-confidence findings spawn recursion or train learners. This single gate defends against soft-404 poisoning *and* traps.
- **Provenance tracker.** Every candidate records which signal/layer/seed produced it; carried through to hits, the UI, and the audit log.
- **Scope enforcer.** Hosts/CIDRs/URL patterns with explicit exclusions; the engine hard-refuses anything out of scope. Consulted on every candidate and every nmap/seed import.
- **Global rate limiter + jitter.** Token bucket with configurable jitter distribution; the single point controlling aggregate request cadence.

## 6. Core data model

- **Candidate** — `path`, `type` (`dir` | `file` | `stem` | `full-path` | `param`), `tags` (tech affinity), `basePriority`, `score`, `provenance`, `sourceSignal`, `depth`.
- **TargetProfile** — `edgeTech[]`, `backendTech[]` (each with confidence), `openPorts[]`, `wafVendor?`, `isSPA`, per-service base URLs.
- **Baseline** (per directory) — status class, length distribution (min/max/mean/stddev), word/line counts, normalised-body similarity hash, redirect target, content-type, `reflectsPath` flag, response-time band.
- **Finding** — `url`, `status`, `size`, `confidence`, `provenance`, `contentHash`, `aliases[]`.
- **Event** — typed with a discriminator (`scan.started`, `calibration.done`, `hit`, `tech.detected`, `frontier.snapshot`, `stats`, `trap.detected`, `branch.pruned`, `throttle`, `scan.finished`, `warning`, `error`).
- **AuditEntry** — one JSONL line per request: timestamp, method, full URL, headers sent, status, size, timing, classification decision + reason (which baseline, similarity score), provenance, confidence.
- **Session** — full serialisable state: config, discovered tree, frontier, learner state, baselines, RNG seed.
- **Corpus** (SQLite) — `terms(term, type, weight)`, `term_tags(term_id, tag)`, `extensions(tech, ext, weight)`, `cooccurrence(path_a, path_b, weight)`, and a `source_map` of glob→tags/type ingestion rules.

## 7. Requirements

**Functional**
- FR1 Enumerate directories/files over HTTP/HTTPS against one or more targets.
- FR2 Auto-calibrate per target and per directory; classify responses by learned baseline, not status alone.
- FR3 Detect target technologies from passive + minimal-active signals; produce a confidence-scored profile.
- FR4 Compose and order candidates from a tagged corpus using the profile; generate extensions at runtime.
- FR5 Ingest an existing nmap XML scan as a signal/seed source.
- FR6 Seed from Wayback/`gau`, `robots.txt`, `sitemap.xml`; harvest endpoints from HTML/JS.
- FR7 Adaptively re-prioritise from discoveries (recursion, response semantics, co-occurrence, naming conventions).
- FR8 Control recursion with depth/scope/budget limits and detect/prune traps.
- FR9 Provide CLI and a live local web UI (view + control) from one binary.
- FR10 Produce a lossless audit log, distilled results, and exports: JSON; Burp/ZAP in two modes (a discovered-URL list for site-map import, and an upstream `--proxy` passthrough that populates Burp history live during the scan); Markdown/HTML report; plaintext/stdout.
- FR11 Save/resume sessions; reproduce a scan from a seed.
- FR12 (v2) Provide tiered stealth: timing, header profiles, TLS/HTTP-2 fingerprint mimicry, proxy rotation.

**Non-functional**
- NFR1 Efficiency: measurably fewer requests to reach coverage targets than baselines on the same corpus.
- NFR2 Correctness: near-zero false positives on the adversarial fixture suite; race-free under `-race`.
- NFR3 Reproducibility: seeded runs are replayable; audit log is complete.
- NFR4 Security: localhost-only control server, token auth, Origin validation, CSRF protection; scope hard-enforced.
- NFR5 Portability: single static binary, no cgo, cross-compiled for Linux/macOS/Windows.
- NFR6 Usability: `smartbuster scan <target>` works on defaults; depth available but not required.

---

## 8. Phases

Each phase lists **objective**, **deliverables**, **technical detail**, **dependencies**, and **exit criteria (Definition of Done)**. Test fixtures are built *within* the phase that needs them, especially for calibration.

### Phase 0 — Foundations

**Objective.** Project skeleton, engine boundary, HTTP client, config, and a CLI that can fire a static wordlist. Establishes the coordinator/worker skeleton everything else plugs into.

**Deliverables.**
- Repo layout: `/engine` (library), `/cmd/smartbuster` (CLI), `/web` (UI, later), `/eval` (harness, later).
- Coordinator + worker-pool skeleton with channels; a naive static frontier (plain list).
- Tuned `net/http` transport (keep-alive, per-host connection cap, timeouts).
- Config layering (defaults < file < flags) via `goflags`/`cobra`+`viper`.
- Scope enforcer (hosts/CIDRs/patterns + exclusions) and `--dry-run`.
- Global token-bucket rate limiter with jitter (concurrency and rate as independent flags).

**Technical detail.** The coordinator owns the frontier and a results channel; workers pull dispatched work, execute, and return `(candidate, responseSignature)`. Rate limiting lives in the coordinator's dispatch loop so the aggregate rate is exact regardless of worker count. Cancellation via `context.Context`.

**Exit criteria.** `smartbuster scan <host> -w list.txt` runs concurrently, honours `--rate`/`--concurrency` independently (verified: observed aggregate rate ≤ configured), refuses out-of-scope hosts, and writes a basic result list. `-race` clean.

### Phase 1 — MVP core engine (calibration-centric)

**Objective.** The load-bearing MVP: a scan that classifies responses by a *learned baseline*, orders a frequency-ranked wordlist, recurses safely, and records everything. This alone beats naive tools on soft-404 handling.

**Deliverables.**
- **Similarity-hash service** (SimHash + TLSH) over normalised bodies.
- **Calibration subsystem** (see detail).
- **Frequency-ordered frontier** (priority = commonality prior).
- **Recursion + basic trap guards** (depth, scope, per-directory budget, content-novelty, time-per-branch).
- **Confidence model + gate.**
- **Audit log** (JSONL) and CLI result output (tree + list + plaintext).
- **Adversarial fixture test server** (built here, reused forever).

**Technical detail — calibration (the core).**
- On entering a directory, fire *N* high-entropy nonexistent paths (plus extension variants) to build a **negative baseline**: status, length distribution, word/line counts, normalised-body similarity hash, redirect target, content-type, response-time band.
- **Normalise before compare:** strip the requested path from the body, regex out tokens/nonces/timestamps/session IDs, collapse whitespace, optionally reduce HTML to tag skeleton. **Self-configure** path-stripping by first testing whether a random path is reflected in the body.
- **Compare with similarity distance, not exact hash.**
- **Derive the hit threshold from baseline self-variance** (noise floor), not a hardcoded value.
- **Vote across signals** (status class + body similarity + word-count delta + redirect match) → confidence score.
- **Detect pathologies and switch strategy:** all-identical `200` HTML shell → mark `isSPA`, warn, and (Phase 4) pivot to JS parsing; per-directory wildcard → suppress via local baseline; WAF/rate-limit onset → trigger backoff.
- **Per-directory recalibration**, cached; probe count reducible in quiet mode.

**Technical detail — traps.** Reuse the similarity hash: self-similar descent → prune; already-seen content → record alias, don't recurse; ~100% hit-rate → wildcard; response-time near timeout → tarpit. Prune when `depth ∧ per-dir budget ∧ content-novelty ∧ time-per-branch` trips. Trap "hits" fail novelty → stay low-confidence → gated out.

**Dependencies.** Phase 0.

**Exit criteria.** On the adversarial fixture suite (custom-200-404, wildcard, SPA, reflected/volatile 404, redirect-to-login, rate-limit-after-N, infinite-dir, tarpit): **zero false positives**, SPA correctly flagged, traps pruned without hanging. Audit log replays the run. Beats gobuster's false-positive count on the soft-404 fixtures.

### Phase 2 — Target profiling (tech detection, corpus, nmap)

**Objective.** Build the profile that drives selection, and turn the flat wordlist into a queryable tagged corpus. Close the detection→selection loop.

**Deliverables.**
- **Tech-detection subsystem:** passive-first (headers, cookies, root HTML, favicon mmh3, the calibration error-page fingerprint) + minimal active known-path probes; `wappalyzergo` + open-fork signatures + custom high-value rules.
- **Ruleset management:** external versioned JSON, an `update` command pulling upstream forks (pinned to a commit), a **user-overlay** directory (user rules override vendored), category toggles, rule provenance.
- **Reverse-proxy/CDN handling:** two-layer profile (edge vs backend); see past the edge via cookies/body/extension-behaviour/error page; **WAF detection** feeding stealth + calibration.
- **Tagged corpus (SQLite):** ingestion pipeline from SecLists with a declarative glob→tags/type map; dedup; commonality scoring (frequency-ordered lists; presence-across-lists); user wordlists tagged on import.
- **Selection-as-query:** compose generic + stack-specific layers ordered by `commonality × detectionConfidence` (log-linear); **runtime extension generation** (`stem × detected extensions` + backup exts like `.bak/.old/.zip/.tar.gz/.swp`); reorder-not-exclude; dedup across layers merging provenance.
- **nmap ingestion:** parse `-oX` XML → web-port targets (`http/https/http-proxy/http-alt`), `-sV` version tags (high confidence), port heuristics (8080→Tomcat, 5000→Flask, 3000→Node, 8000→Django, 9200→ES), NSE output (`http-enum`→path seeds, `http-robots.txt`, `http-favicon`, `http-server-header`, `http-title`, `ssl-cert` SANs→vhost candidates). Propagate nmap's own `conf`/method. Ingest-by-default; orchestrate opt-in; multi-host + scope; **seed vs. truth** (seeds validated by calibration, tech is a prior confirmed live).

**Dependencies.** Phase 1 (calibration validates seeds; confidence model scales boosts).

**Exit criteria.** On framework-default installs (WordPress, Drupal, Tomcat, Django/Next.js): correct high-confidence stack detection; stack-specific paths surface materially earlier than with a flat wordlist; nmap XML ingestion yields correct tags/targets/seeds; ruleset update + user overlay work.

### Phase 3 — Dynamic intelligence

**Objective.** Turn the frontier from "frequency-ordered + tech-weighted" into a live feedback loop.

**Deliverables.**
- **Log-linear scoring** combining base commonality, tech boost, structural signals, and online likelihood — tunable and explainable (decomposable per candidate).
- **Response-semantics signals:** `403`→"exists & protected" (strong boost to siblings); `200`+"Index of"→boost recursion; `401`/`500` handled distinctly.
- **Co-occurrence / association rules:** a static table (mined from a corpus **disjoint from eval targets**) + session-local learning (`login.php`→`config.php`,`admin.php`).
- **Online naming-convention learner:** character-level Markov/n-gram trained on confirmed hits; re-rank candidates matching observed case/separator/plurality/extension style.
- **Exploration vs. exploitation:** mostly-greedy with a bounded exploration budget (ε-greedy or a depth cap before yielding to breadth); configurable; interacts with stealth (greedy → fewer requests). Budget allocation follows "maximise expected new discoveries per remaining request" (the top-of-queue priority is that estimate); under concurrency the greedy choice is approximate — the top-K candidates are in flight at once, trading a little ordering precision for throughput.
- **Per-directory recalibration** wired into descent.
- **Poisoning defence:** confidence gate on all learners (only clean, high-confidence hits train / recurse).

**Dependencies.** Phases 1–2.

**Exit criteria.** Ablation shows each signal's marginal contribution to the discovery curve; learners demonstrably improve requests-to-coverage on framework installs; confidence gate verified to block soft-404 branches from training.

### Phase 4 — Discovery expansion (passive seeding + crawl/JS)

**Objective.** Add the high-value non-brute-force discovery sources and the SPA pivot.

**Deliverables.**
- **Passive seeding:** Wayback/`gau`/`waybackurls`, `robots.txt` (Disallow = high-priority guess), `sitemap.xml` — all as calibration-validated candidates.
- **Crawler + JS endpoint harvesting:** parse HTML links and run LinkFinder-style regex over JS bundles to extract API routes/endpoints; feed the frontier.
- **SPA pivot:** when calibration flags `isSPA`, shift weight from brute-force to JS/crawl discovery automatically.
- **Optional headless mode:** `playwright-go` as an out-of-process advisor for computed routes / JS-property fingerprints (heavy, opt-in) — kept out of the core binary.

**Dependencies.** Phases 1–3 (calibration's SPA detection triggers the pivot; profile guides parsing).

**Exit criteria.** On a client-side React SPA, brute-force is auto-detected as futile and JS harvesting recovers API endpoints; Wayback/robots/sitemap seeds are prioritised and correctly validated (no false positives from stale seeds).

### Phase 5 — Web UI + sessions

**Objective.** The live "watch it think" interface and session persistence, from one binary.

**Deliverables.**
- **Engine daemon:** localhost HTTP+WS server; web assets via `embed.FS`.
- **Control plane (REST):** `POST /scans`, pause/resume/stop, `PATCH` (rate/mode), and manual override — pin/exclude/boost/demote a path or branch and inject custom terms mid-scan (same capabilities from CLI and GUI; the human always outranks the engine).
- **Data plane (WebSocket):** typed event stream tapped from the coordinator.
- **Event schema** (see §6) with **sampling/aggregation** (stats coalesced ~250 ms, frontier snapshots sampled, hits coalesced under flood) — **UI stream is lossy; audit log is lossless.**
- **Visualisation:** discovered-path tree, priority-frontier view, rate/hit-rate/ETA gauges, tech profile, trap/prune events, provenance-tagged hits.
- **Sessions:** serialise/restore full state (incl. RNG seed); list/reopen in UI.
- **Server security:** localhost bind, per-session random auth token (injected into auto-opened URL), Origin validation (anti-DNS-rebinding), CSRF on control endpoints.

**Dependencies.** Phases 1–4 (events already emitted by the coordinator).

**Exit criteria.** UI shows a live scan without overwhelming the browser during a fast run; controls (pause/pin/exclude) affect the running engine; a scan can be saved and resumed; security checks verified (out-of-origin control blocked, missing token rejected).

### Phase 6 — Stealth & request engine (v2)

**Objective.** Address the three detection tiers honestly and raise fingerprint fidelity.

**Deliverables.**
- **Modes as presets:** `fast` / `normal` / `quiet` / `stealth`, each a coherent bundle; power-user overrides on top.
- **Tier 1 (timing):** global token bucket, jitter distributions (incl. bursty/human-like), time-budget pacing, **adaptive backoff** driven by calibration's WAF/`429` detection.
- **Tier 2 (request shape):** realistic ordered browser header profiles; correct header casing/order (avoid library-level tells); **stable per-session identity** (no per-request UA rotation); sensible Referer chains; request-order randomisation.
- **Tier 3 (fingerprint):** TLS ClientHello (JA3/JA4) + HTTP/2 SETTINGS/pseudo-header ordering (Akamai fingerprint) via `bogdanfinn/tls-client`; **proxy/Tor rotation** for per-IP thresholds.

**Dependencies.** Phases 0–1 (request engine), 2 (WAF detection).

**Exit criteria.** Against a fingerprinting test endpoint, `stealth` mode presents a browser-consistent JA3/JA4 + HTTP/2 fingerprint; jittered aggregate rate holds under the cap; adaptive backoff engages on simulated throttling; modes switch cleanly.

### Phase 7 — Evaluation harness + hardening + release

**Objective.** Turn "we think it's smarter" into evidence, and lock it against regressions.

**Deliverables.** Full eval harness (§9), regression CI gating on discovery-curve/requests-to-coverage metrics, expanded adversarial + concurrency test suites, docs, cross-compiled release binaries.

**Exit criteria.** Reproducible benchmark run producing discovery curves and requests-to-coverage vs. gobuster/ffuf/feroxbuster on the target corpus, with ablation deltas and significance stats; CI fails on metric regression.

---

## 9. Evaluation & testing strategy (spans all phases)

Two distinct questions, two kinds of machinery.

**A. Is it *smarter*? (benchmark / research)**

- **Ground truth requires known targets.** Three-layer corpus: **synthetic servers** (planted paths + deliberately constructed pathologies; randomised layouts to prevent overfitting), **framework default installs** (exercise the detection→selection loop), **deliberately-vulnerable real apps** (DVWA, Juice Shop, WebGoat, Mutillidae; pinned Docker tags for reproducibility).
- **Metrics:**
  - **Discovery curve** (discoveries vs. requests) — the centrepiece; essentially precision/recall over budget.
  - **Requests-to-coverage** at 50/90/100% — the headline efficiency metric.
  - **Coverage/recall at unlimited budget** — *guardrail*: prove greedy prioritisation doesn't miss long-tail paths the dumb tool eventually finds.
  - **False-positive rate** — scores calibration (dramatic win on soft-404 targets).
  - **Time-to-first-hit** and a **wasted-request breakdown** (real hits / correct negatives / false positives / redundant trap-and-dup).
- **Experimental design:**
  - **Baselines include ffuf and feroxbuster**, not just gobuster (fair comparison against autocalibration + recursion).
  - **Isolate the variable:** give every tool the same *superset* corpus; measure who finds ground-truth paths in fewest requests (isolates smartbuster's ordering/selection from "had a better list").
  - **Ablation studies:** frequency-only → +tech detection → +dynamic prioritisation → +co-occurrence/Markov → +passive seeding; quantify each feature's contribution.
  - **Variance & significance:** seeded repeated runs, mean ± CI, paired significance tests across the corpus.
  - **Overfitting guards:** train/test split on *targets*; randomised synthetic layouts; co-occurrence training data **disjoint** from eval targets.

**B. Is it *broken*? (software correctness)**

- Go unit tests for deterministic logic (scoring, corpus queries, nmap parsing, extension permutation, Markov ranker).
- **Adversarial-fixture suite** (`httptest.Server`) for calibration + traps — highest-value tests; assert zero false positives, correct SPA pivot, trap pruning. (Built in Phase 1.)
- **Concurrency invariants:** run under `-race`; property-test the global rate limiter (observed aggregate ≤ configured).

**Two payoffs from the architecture:**
1. **The audit log is the eval instrumentation** — the harness is just *ground-truth list + audit-log parser + stats/plots*.
2. **The benchmark is the regression suite** — snapshot metrics on the corpus; any tuning change that degrades the discovery curve fails CI.

## 10. Security, safety & legal

- **Recon-only:** discovers paths, never exploits.
- **Scope enforcement** hard-refuses out-of-scope hosts/paths; **dry-run** previews requests before sending.
- **Complete audit trail** proves exactly what traffic was generated (and what was not touched) — engagement reporting and reproducibility.
- **Localhost-only control server** with token auth, Origin validation, CSRF protection — the tool generates traffic, so its control surface must not be drivable by a stray local process or a malicious webpage.
- **Authorization is the operator's responsibility;** the tool's job is to make its behaviour verifiable and bounded.

## 11. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Soft-404 detection is the hardest and fails silently | Calibration is first-class (Phase 1); adversarial fixture suite gates the phase |
| Greedy prioritisation misses long-tail paths | Coverage-at-unlimited-budget guardrail metric; bounded exploration |
| Overfitting the ranker to eval apps | Train/test split on targets; randomised synthetic layouts; disjoint co-occurrence data |
| Stealth over-promises (jitter ≠ evasion) | Explicit three-tier model; fingerprint mimicry in `stealth` mode; honest docs |
| Concurrency correctness (rate limiter, races) | Coordinator-owns-state model; `-race`; rate-limiter property test |
| Tech-detection false positives waste budget | Confidence-scaled boosts; reorder-not-exclude; never hard-exclude |
| Trap/soft-404 poisoning the learners | Confidence gate: only clean high-confidence hits train/recurse |
| Scope creep (large project) | Strict phased MVP; each phase has exit criteria |
| Reverse proxy/CDN masks backend | Two-layer profile; read backend signals that pass the edge |

## 12. Milestones & sequencing

- **M1 — MVP (Phases 0–1):** calibrated, frequency-ordered, safe-recursion enumerator with audit log; already beats naive tools on soft-404s.
- **M2 — Beta (Phases 2–4):** profiling, tagged corpus, nmap ingestion, dynamic prioritisation, passive/JS discovery.
- **M3 — 1.0 (Phases 5, 7):** web UI + sessions, full eval harness, regression CI, release binaries.
- **M4 — v2 (Phase 6):** tiered stealth and TLS/HTTP-2 fingerprint fidelity.

Phases 2–4 can overlap once Phase 1's primitives (calibration, similarity hashing, confidence gate, coordinator) are stable, since each is largely an additional signal source feeding the same frontier.

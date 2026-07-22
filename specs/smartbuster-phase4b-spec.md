# smartbuster — Phase 4b Build Specification (Crawl + JS Harvesting + SPA Pivot)

*Build-ready spec for discovering paths from the target's own responses: HTML link crawling, JavaScript endpoint harvesting, and the SPA pivot that makes single-page apps enumerable. Enqueues through 4a's seed path; validated by calibration; scored by the Phase 3 scorer. Read alongside the 4a spec and the implementation plan (Phase 4).*

---

## 0. Integration contract — verify against committed code first

| # | Attachment point | Confirmed | Phase 4b action |
|---|---|---|---|
| A | `enqueueSeed` / `ensureDirChain` (4a) | confirmed | Harvested links/endpoints enqueue via the **same** path, `Provenance="crawl:html"` / `"crawl:js"` / `"headless"` |
| B | `stageDirCandidate` already-`SCANNING` branch: first-writer-wins, no merge (4a's flagged gap) | confirmed gap | **Implement the stronger merge** (union provenance, max prior), invoked by the coordinator from the inject channel (§5) — all mid-scan seeding routes through it |
| C | `ResponseSignature` drops bodies except calibration `NormBody` (Phase 1/2a) | confirmed | Add scoped body retention for **harvestable** responses only (§2); parsing happens **off** the coordinator goroutine |
| D | `Baseline.IsSPA` set by calibration (Phase 1) | confirmed | The SPA pivot keys off it (§4) |
| E | Scope enforcer + rate limiter on every request | confirmed | Crawl fetches (JS bundles, headless nav) all go through both |
| F | `applyScore` = `corpus.Score * scorer.Boost`; dynamic scorer subtree-aware | confirmed | Harvested candidates flow through unchanged; a harvested `/api/*` benefits from `/api` discoveries |
| G | **Coordinator is the single writer** of frontier/tree/baselines (Phase 1 invariant, no locks) | confirmed | Mid-scan injection **must not** lock the frontier. Add a `seedInjectCh` into the coordinator `select` loop; producers send, coordinator mutates (§5) |
| H | 4a Wayback fetch is **synchronous + upfront** on the coordinator goroutine | confirmed | Move it **off the critical path**: async goroutine, results injected via `seedInjectCh` when ready; scan starts on robots/sitemap (§5) |
| I | 4a bounds the dead-dir storm by being synchronous | confirmed | Async injection removes that bound → add a **distinct-new-directory cap** per injected batch (§5); paired with H, never separated |

## 1. Scope

**In (4b):** an HTML link crawler (piggybacked on responses), a JavaScript endpoint harvester (fetch referenced bundles + LinkFinder-style extraction), the automatic SPA pivot, scoped body retention, a crawl visited-set, an **optional out-of-process headless mode** (playwright-go), and — the load-bearing new primitive — a **concurrency-safe mid-scan seed-injection channel** shared by three producers (crawler, JS harvester, async Wayback), plus **async Wayback** (moved off the scan-start critical path) and a **distinct-new-directory blast-radius cap** on injected batches. This completes the Phase 4 discovery story: brute-force + passive seeds (4a) + active harvesting (4b), all into one calibrated, dynamically-scored frontier.

**Deferred to Phase 7 (measure-first):** inherited-baseline classification (classify against the nearest calibrated ancestor before calibrating a local dir) — the real fix for wasted calibration on dead deep chains. The cap below is its safety belt so the deferral is responsible; promote it when Phase 7 shows dead-branch calibration is costly (measure with Wayback on **and** off — its value is broader than Wayback).

**Out:** param/query fuzzing (future mode); anything in Phases 5–7.

## 2. Body retention (scoped, like `NormBody`)

Harvesting needs response bodies; the hot path must stay lean. So retain a body **only** when ALL hold: harvesting enabled; status 200; `Content-Type ∈ {text/html, application/javascript, application/json}`; size `< HARVEST_BODY_CAP` (default 512 KiB). A **harvester goroutine** (not the coordinator loop) parses it and injects discovered paths via `seedInjectCh` (§5), then discards the body — LinkFinder regex over a multi-MiB bundle is far too expensive to run on the single coordinator goroutine, and this is exactly the same async-producer shape async Wayback needs. Ordinary candidates and calibration probes are unaffected. Concretely: `ResponseSignature.HarvestBody []byte` (populated only under the above condition), mirroring the `NormBody` exception.

## 3. HTML link crawler (`internal/harvest/html.go`) — inline, near-zero extra cost

Piggyback on responses you already made: for a retained `text/html` body, extract candidate paths and enqueue them as `crawl:html` seeds.
- Parse with `golang.org/x/net/html`. Extract from: `a[href]`, `link[href]`, `script[src]`, `img[src]`, `form[action]`, and `[data-*]` URL-ish attributes.
- **Resolve** relative → absolute against the response URL; keep only same-host (scope); strip queries/fragments (path buster); dedup against the crawl visited-set.
- Enqueue each as a seed (prior `CRAWL_HTML_PRIOR` default 0.9 — a linked path almost certainly exists) via `enqueueSeed`, using `ensureDirChain` for deep links.
- `script[src]` URLs additionally feed the JS harvester (§4) — these are the bundles to mine.
- **No re-fetch** for HTML links: this rides on responses the scan already produced, so it's essentially free.

A crawl **visited-set** (per host) prevents re-processing the same page/asset; crawl depth is bounded by `MAX_DEPTH` (shared with recursion) and scope.

## 4. JavaScript endpoint harvesting + SPA pivot (`internal/harvest/js.go`) — the modern-app win

Discovered `script[src]` bundles are fetched (new requests, rate-limited + scoped) and mined for endpoints:
- **Extraction**: LinkFinder-style regex over the JS source — quoted strings that look like paths/URLs: absolute `/api/...`, relative `../...`, and URL arguments to `fetch(...)`, `axios.*(...)`, `XMLHttpRequest.open(...)`. JS is noisy, so **filter hard**: must look like a path (leading `/` or known extension), same-host or host-relative, printable, length-bounded; drop obvious non-paths (mime types, template literals with `${}`, regexes). Dedup.
- **Enqueue** survivors as `crawl:js` seeds (prior `CRAWL_JS_PRIOR` default 0.85). Endpoints from JS are frequently **real server resources** (the SPA's API), so calibration validates them normally — and because they're often JSON APIs under `/api`, they benefit from the subtree-aware scorer once one `/api/*` confirms.
- Bounded: `JS_MAX_BYTES` per bundle (default 2 MiB), `JS_FETCH_CONCURRENCY` (default = worker count, shares the pool).

**SPA pivot** — the payoff for the Phase 1 SPA detection that previously just warned:
```
onCalibrationDone(dir):
  if dir == root and baseline.IsSPA:
     emit("spa.pivot")
     # brute-force is futile: identical shell for every path.
     deprioritizeBruteForce(host)          # scale corpus-candidate scores down, don't purge
     harvestRoot(host)                       # fetch root HTML -> all script[src] -> JS harvest
```
On a client-side SPA, directory brute-force returns the shell for everything (calibration already suppresses those as non-hits), so 4b **shifts weight to harvesting**: fetch the root, mine every JS bundle for the real API surface, enqueue those endpoints. Brute-force isn't hard-stopped (some SPAs still expose server paths like `/api`, `/admin`, `/.git`) — its candidates are scored down, not removed (reorder-not-exclude). This is what turns a Phase 1 "detected but zero findings" SPA into an enumerable target.

## 5. Concurrency-safe mid-scan seed injection (the load-bearing 4b primitive)

4a seeded synchronously before dispatch (single-writer, its stated invariant). 4b introduces sources that produce seeds **while the scan runs** — and it must do so without ever locking the frontier, because the coordinator being the sole mutator of frontier/tree/baselines is the invariant the whole engine's correctness has rested on since Phase 1.

**Mechanism — one channel, three producers, coordinator stays single-writer.** Add `seedInjectCh chan SeedBatch` to the coordinator's `select` loop. Producers *send*; the coordinator *applies*. No producer touches the frontier directly; no mutex on the frontier.

```
// producers (each its own goroutine, off the coordinator loop):
//   crawler       -> parses retained HTML, sends crawl:html batches
//   js harvester  -> fetches+mines JS bundles, sends crawl:js batches   (regex too costly for the coordinator loop)
//   wayback       -> async CDX fetch off-target, sends one big wayback batch when ready
//   headless      -> (opt-in) sends captured-URL batches

// coordinator select loop gains:
case batch := <-seedInjectCh:
    applySeedBatch(batch)     // single-writer: cap-check, then per-path merge-or-enqueue
```

**`applySeedBatch` = the merge (implements 4a's flagged gap) + the cap:**
1. **Cap the batch's blast radius.** Count `distinctNewDirs` the batch would materialize via `ensureDirChain` (dirs not already in the tree). If it exceeds `MAX_NEW_DIRS_PER_BATCH` (§7), **truncate to the highest-prior seeds** and **log a `seed.capped` warning** (so Phase 7 can see whether it ever bites). This is a hard blast-radius ceiling, not a tuning knob — pre-calibration you cannot know which dirs are dead, so you cap how many a single batch may introduce, generously.
2. **Merge or enqueue, per path.** For each surviving seed: if its path already exists as a candidate (in a `NEW`, `CALIBRATING`, or `SCANNING` dir), **union `Provenance` and take `max(BasePrio)`, then rescore just that candidate** — replacing 4a's first-writer-wins in `stageDirCandidate`'s already-`SCANNING` branch. If absent, `enqueueSeed` + `ensureDirChain` as normal. "In the corpus **and** linked from the app **and** in Wayback" becomes the strong, high-priority, honestly-attributed signal it should be.

**Two producer shapes, one path — deliberately.** The crawler trickles same-host paths as pages come back; async Wayback dumps one large batch of deep, often-dead dirs at once. Routing both (and JS, and headless) through the *same* `applySeedBatch` is what proves the machinery is source-agnostic rather than accidentally crawl-shaped — build and `-race`-test it against both shapes while you're in this code.

**Async Wayback specifically (contract H).** 4a's Wayback fetch was synchronous-upfront and stalled scan start on archive.org latency (throttled 1 req/s over thousands of CDX rows = seconds-to-minutes before the main scan moved). Move it to a goroutine kicked off at scan start; the scan begins immediately on robots/sitemap seeds + corpus; the Wayback batch arrives via `seedInjectCh` mid-scan and merges in. Robots/sitemap stay synchronous-upfront (two fast on-target GETs — the wrong thing to make async). If egress is blocked, the goroutine fails gracefully (warning, no batch) without ever having delayed the scan.

## 6. Optional headless mode (`internal/harvest/headless.go`) — out-of-process, opt-in

For routes/endpoints that only exist after JS executes (client-side routers, dynamically-built URLs) that static regex misses:
- `--headless` (default **off**). Spawns **playwright-go** as a separate process/driver — the browser never links into the core binary (keeps it lean and cgo-free, as decided in the plan).
- Navigate the root (and confirmed SPA routes), let the app render, and capture: (a) actual network requests (XHR/fetch URLs — the real API calls), (b) resolved in-app routes/anchors after render.
- Feed captured URLs as `headless` seeds (prior `HEADLESS_PRIOR` 0.9 — observed live traffic), scope-filtered, calibration-validated.
- Heavy and slow, so: opt-in, bounded page/time budget, and it degrades gracefully if playwright isn't installed (warning, no headless, rest of scan proceeds). This is the `wappalyzer-next`-style escalation tier referenced back in the design discussion, reused here.

## 7. Config additions & defaults
```go
Crawl        bool   // --crawl        default true  (HTML link harvest; near-free)
JSHarvest    bool   // --js-harvest   default true  (fetches bundles; bounded)
Headless     bool   // --headless     default false (opt-in, heavy)
CrawlDepth   int    // default = MaxDepth
```
| Constant | Default | Meaning |
|---|---|---|
| `HARVEST_BODY_CAP` | 512 KiB | max body retained for parsing |
| `CRAWL_HTML_PRIOR` / `CRAWL_JS_PRIOR` / `HEADLESS_PRIOR` | 0.9 / 0.85 / 0.9 | seed priors by harvest source |
| `JS_MAX_BYTES` | 2 MiB | max JS bundle mined |
| `JS_FETCH_CONCURRENCY` | = workers | shares the pool |
| `MAX_NEW_DIRS_PER_BATCH` | 500 | blast-radius ceiling: distinct new dirs one injected seed batch may materialize; logs `seed.capped` on trip |

Async Wayback reuses 4a's `--wayback` flag and `WAYBACK_MAX`/`ARCHIVE_RATE`; 4b only changes *when* it runs (background, off the critical path) and *how* its results land (via `seedInjectCh`). No new Wayback flag.

## 8. Tests, fixtures, DoD

**Fixtures (`test/fixtures/harvest_server.go`):**
| Fixture | Presents | Expected |
|---|---|---|
| `linked_paths` | HTML with `a[href]` to a path absent from the wordlist | crawled + found; provenance `crawl:html`; no re-fetch of the page |
| `js_endpoints` | HTML → `bundle.js` containing `/api/v1/users`, `/internal/status` | endpoints extracted, enqueued, calibration-validated, found |
| `js_noise` | bundle with mime types, `${}` templates, regexes | noise filtered out; only real paths survive |
| `spa_with_api` | Phase-1 SPA shell + a JS bundle exposing `/api/*` | `spa.pivot` fires; brute-force scored down; API endpoints recovered (the SPA becomes enumerable) |
| `crawl_dedup` | a linked path that's also a corpus candidate in a SCANNING dir | merged: unioned provenance, max prior (exercises §5) |
| `offscope_links` | HTML linking to another host | out-of-scope links dropped |
| `async_wayback` (stub CDX) | slow CDX response + a big batch incl. paths already being scanned | scan starts before CDX returns; batch injected mid-scan; collisions merge |
| `dir_storm` | a seed batch naming > `MAX_NEW_DIRS_PER_BATCH` distinct new dirs | truncated to top-prior seeds; `seed.capped` logged |
| `headless_spa` (opt-in) | routes only present after JS runs | with `--headless`, captured XHR URLs become seeds; without it, gracefully skipped |

**Assertions / DoD:**
1. HTML crawling enqueues linked paths with no extra page re-fetch (assert request count: the page isn't fetched twice).
2. JS harvesting extracts real endpoints and filters noise (precision on `js_noise`).
3. **SPA pivot**: on `spa_with_api`, `spa.pivot` fires, brute-force candidates are de-prioritized (not purged — a generic term is still present), and the JS-derived API endpoints are found — i.e. a target that yielded **zero** findings in Phase 1 now yields the API surface.
4. Mid-scan dedup **merges** (provenance unioned, max prior), not first-writer-wins (§5).
5. Body retention is scoped: a non-harvestable (e.g. `image/png`, or >cap) response retains no body.
6. Scope: off-host crawled/harvested/headless URLs are dropped.
7. Headless is off by default, out-of-process, and degrades gracefully when playwright is absent.
8. **Single-writer injection**: all mid-scan seeds reach the frontier via `seedInjectCh` applied by the coordinator; assert **no mutex on the frontier** and `-race` clean with crawler + JS + Wayback producing concurrently.
9. **Async Wayback off the critical path**: with a slow stub CDX, the scan dispatches real candidates *before* CDX returns; the Wayback batch merges in mid-scan (assert first non-seed dispatch precedes CDX completion).
10. **Cap**: a batch exceeding `MAX_NEW_DIRS_PER_BATCH` is truncated to top-prior seeds and logs `seed.capped`; nothing beyond the ceiling is materialized.
11. `go build`, `go vet`, `go test -race ./...` clean; CLI smoke test on `spa_with_api` shows recovered `/api/*` endpoints with `crawl:js` provenance.

## 9. Build order & handoff
`harvest` package: body-retention plumbing (contract C) → **`seedInjectCh` + `applySeedBatch` (merge + cap) in the coordinator (§5) — build and `-race`-test this first, it's the load-bearing primitive** → `html` crawler + visited-set (producer) → `js` extractor as an off-loop producer (regex + filters) → **async Wayback producer (moves 4a's fetch off the critical path)** → SPA pivot wiring (`onCalibrationDone` + `deprioritizeBruteForce` + `harvestRoot`) → optional `headless` (out-of-process, opt-in) → config/flags. Exercise `applySeedBatch` against **both** producer shapes (trickle + big batch) under `-race`. Write the fixture server + harvester table-tests **before** the SPA-pivot/engine wiring, validating extraction and noise-filtering standalone.

Not in 4b (Phase 7, measure-first): **inherited-baseline classification** — the real fix for wasted calibration on dead deep chains. The `MAX_NEW_DIRS_PER_BATCH` cap is its safety belt in the meantime.

**Before Phase 5, report back:** the harvest/enqueue integration (esp. how the SPA pivot rebalances scores and whether brute-force is scored-down vs paused), the final body-retention condition, and any §0 deviation. **Phase 5 (web UI) consumes the event stream** the whole engine now emits — `hit`, `calibration.done`, `spa.pivot`, `tech.detected`, `frontier.snapshot`, `stats`, `trap.detected`, `throttle` — so the next handoff should also enumerate **every event type the engine currently emits and its payload shape**, since the UI's live view and the audit log both render exactly those.

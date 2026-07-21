# smartbuster — Phase 4a Build Specification (Passive Seeding)

*Build-ready spec for seeding the frontier from `robots.txt`, `sitemap.xml`, and the Wayback Machine / CDX. Seeds set priority; calibration sets truth. Feeds the frontier + scorer built in Phases 1–3. Read alongside the Phase 3 spec and the implementation plan (Phase 4).*

---

## 0. Integration contract — verify against committed code first

| # | Attachment point | Confirmed (Phase 1–3) | Phase 4a action |
|---|---|---|---|
| A | `enqueueGenerated` — pushes a candidate with immediate score, deduped per-dir against `knownPaths` | confirmed (Phase 3) | Add a sibling `enqueueSeed(seed)`; reuse the same dedup + immediate-scoring path |
| B | `engine.Candidate{Path,Type,BasePrio,Score,Depth,ParentDir,Provenance,Tags}` | confirmed | Seeds are Candidates: high `BasePrio` (§3), `Provenance="wayback|robots|sitemap"`, `Tags=["generic"]` (or `["seed"]`) |
| C | Per-directory calibration + tree of dirs (`NEW→CALIBRATING→SCANNING`) | confirmed (Phase 1) | Seeds can be deep paths → materialize the **ancestor directory chain** on demand (§4) |
| D | Scope enforcer on every candidate | confirmed | Every seed passes scope before enqueue |
| E | Global rate limiter (+ 2a fix pacing profiling requests) | confirmed | `robots`/`sitemap` fetches go through it (target host); Wayback uses a **separate** polite limiter (archive.org, different host) — §5 |
| F | `applyScore` = `corpus.Score * scorer.Boost`; scorer reads `Tags`/`ParentDir` | confirmed | Seeds flow through `applyScore` unchanged; their high `BasePrio` carries the "known path" prior |
| G | `profile`/`Baseline.IsSPA`, calibration classify | confirmed | Seeds are classified like any candidate — a stale seed that 404s today is simply not a hit (seed-vs-truth) |

## 1. Scope

**In (4a):** `robots.txt` parsing (Disallow/Allow/Sitemap), `sitemap.xml` parsing (index, nested, gzip), Wayback/CDX querying (+ optional gau-style sources behind one interface), seed normalization/scoping/dedup, ancestor-chain materialization, tiered seed priors, provenance, config toggles.

**Out (→ 4b):** live crawling of the target's own pages, HTML link extraction during the scan, JavaScript endpoint harvesting, the SPA pivot, headless. 4a seeds from *external/declared* sources; 4b harvests from the *target's own responses*.

## 2. Where seeding runs

A `seed` stage at scan start, after profiling (2a) and initial corpus selection (2b), before the main scan loop settles:
```
scanStart(target):
  profileTarget(target)                 # 2a
  seedCorpus(provisionalProfile)        # 2b (frontier gets corpus candidates)
  if cfg.Robots:   seedFromRobots(target)
  if cfg.Sitemap:  seedFromSitemap(target)      # incl. sitemaps found in robots
  if cfg.Wayback:  seedFromWayback(target)      # hits archive.org, not target
  calibrateRoot(...); runScanLoop()
```
Seeding is synchronous on the coordinator goroutine (like profiling), so all seed enqueues are single-writer. Robots/sitemap are a few target requests; Wayback is off-target. All seeds are deduped against corpus candidates already in the frontier (via `knownPaths`).

## 3. Seed model, priority, dedup

A seed is a full path (possibly deep, possibly with an extension). Convert to Candidate(s):
- `Path` = the leaf name under its immediate parent; `ParentDir` = the parent path; `Depth` = path depth.
- `Type`: extension present → `TypeFile`; trailing slash / no extension + looks dir-like → `TypeDir` (recursion-eligible); ambiguous → Phase 1 dot-heuristic.
- `BasePrio` = tiered **seed prior** (a path with external evidence beats a corpus guess):

| Source | `BasePrio` | Rationale |
|---|---|---|
| `robots.txt` **Disallow** | 0.95 | deliberately hidden = high interest |
| `sitemap.xml` | 0.90 | declared to exist (public) |
| `robots.txt` Allow / other | 0.85 | declared |
| Wayback (recent capture) | 0.85 | existed recently |
| Wayback (old capture) | 0.70 | may be stale |

- `Provenance` names the source (+ capture date for Wayback). `Tags=["generic"]` so `corpus.Score` gives no tech boost but the high `BasePrio` dominates; the dynamic scorer still applies (a seed under `/admin` still gets the semantics boost if `/admin` is protected).
- **Dedup**: against the frontier's `knownPaths` (a seed already present as a corpus candidate keeps the **max** BasePrio and unions provenance — "in corpus *and* sitemap" is a stronger signal); and across sources.
- **Query strings stripped** for path enumeration (`/x?a=1` → `/x`); the parameterized form is out of scope for a path buster (note for a future param-fuzz mode).
- **Extension/noise filtering**: drop obvious static asset noise from Wayback (`.png/.jpg/.css/.woff/...`) unless `--seed-assets`; keep `.php/.aspx/.jsp/.json/.bak/...` and extensionless.

## 4. Deep seeds & the ancestor chain (the non-obvious part)

Brute-force is breadth-first over calibrated directories; a seed like `/old/admin/config.php` names a path three levels deep that brute-force might never reach. Handle it so seeds can **extend the tree**:
```
enqueueSeed("/old/admin/config.php", prio):
  ensureDirChain("/old", "/old/admin")   # each ancestor added as a NEW dir if absent
  enqueue Candidate{Path:"config.php", ParentDir:"/old/admin", BasePrio:prio, ...}
```
- `ensureDirChain` registers each missing ancestor as a `NEW` directory (so it gets calibrated) and as a dir candidate under its parent. Bounded by `MAX_DEPTH` and scope.
- **Stale-seed pruning is automatic and correct**: if `/old` 404s today, its calibration/classification shows it isn't a live directory → the chain doesn't proceed, and `config.php` under it is never confirmed. Seed-vs-truth holds with zero special-casing: a dead historical path just fails calibration like any wrong guess.
- Ancestor directories introduced by seeds are themselves enumerated normally once calibrated — so a single deep Wayback hit can open a whole subtree the wordlist wouldn't have found.

## 5. Sources

### 5.1 robots.txt (`internal/seed/robots.go`)
Fetch `<base>/robots.txt` (through the target rate limiter + scope). Parse directives across all user-agent groups: collect **`Disallow`** and **`Allow`** paths (→ seeds; Disallow ranked highest), and **`Sitemap:`** URLs (→ feed §5.2). Ignore wildcards/patterns for enumeration (a `Disallow: /admin/*` yields the seed `/admin/`). Missing/empty robots → no error, no seeds.

### 5.2 sitemap.xml (`internal/seed/sitemap.go`)
Fetch `<base>/sitemap.xml` plus any sitemaps from robots. Handle:
- **urlset**: `<url><loc>` → seeds.
- **sitemapindex**: nested `<sitemap><loc>` → fetch each (bounded fan-out `SITEMAP_MAX_FILES`, default 20).
- **gzip**: `.xml.gz` transparently decompressed.
Scope-filter every `<loc>` to the target host; strip query strings; dedup.

### 5.3 Wayback / CDX (`internal/seed/wayback.go`)
Query the CDX API off-target: `http://web.archive.org/cdx/search/cdx?url=<host>/*&output=json&collapse=urlkey&fl=original,timestamp&limit=<N>`. Parse rows → historical URLs. Then: scope-filter to host, strip queries, asset-filter (§3), dedup, and assign BasePrio by capture recency (§3). Bounded by `WAYBACK_MAX` (default 5000 pre-filter).
- **Pluggable sources**: define `type SeedSource interface { Fetch(host string) ([]RawSeed, error) }`; Wayback is one impl. `gau`-style sources (Common Crawl index, URLScan, AlienVault OTX) are additional impls behind the same interface, enabled by flags — all off-target, all funneling into the same normalization. Only Wayback is required for 4a; the interface makes the rest additive.
- Uses a **separate polite limiter** for archive.org (default 1 req/s) — the target's rate/stealth settings don't apply to a third-party host, and you don't want to hammer archive.org.

## 6. Config additions & defaults
```go
Robots      bool     // --robots      default true
Sitemap     bool     // --sitemap     default true
Wayback     bool     // --wayback     default false (off-target network call; opt-in)
WaybackMax  int      // default 5000
SeedAssets  bool     // --seed-assets default false (keep static assets)
ExtraSeedSources []string // "commoncrawl","urlscan","otx" (4a ships wayback only)
```
| Constant | Default | Meaning |
|---|---|---|
| seed priors | see §3 table | tiered by source |
| `SITEMAP_MAX_FILES` | 20 | nested-sitemap fan-out cap |
| `WAYBACK_MAX` | 5000 | pre-filter row cap |
| `ARCHIVE_RATE` | 1 req/s | polite limiter for archive.org |

`--wayback` defaults **off** because it makes an external network call and pulls a lot; robots/sitemap default **on** (cheap, on-target, high-signal). Note the `network_configuration`: if egress is restricted in a given environment, Wayback/extra sources fail gracefully (logged warning, zero seeds) — never fatal.

## 7. Tests, fixtures, DoD

**Fixtures:** an `httptest` server serving a crafted `robots.txt` (Disallow list + a Sitemap: line), a `sitemap.xml` (urlset + a nested sitemapindex + a `.xml.gz`), and planted real paths matching some seeds; a stub CDX endpoint returning canned JSON rows (so Wayback is tested hermetically, no live archive.org).

**Assertions / DoD:**
1. **robots**: Disallow/Allow parsed into seeds; Disallow entries get the top prior; `Sitemap:` lines feed the sitemap parser.
2. **sitemap**: urlset + nested index + gzip all parsed; out-of-scope `<loc>`s dropped; fan-out capped.
3. **Wayback**: CDX JSON parsed; scope + asset + query filtering applied; recency → prior tiering; the separate archive.org limiter is used (not the target limiter).
4. **Deep seed / ancestor chain**: a seed `/a/b/c.php` materializes `/a` and `/a/b` as calibrated dirs and enqueues `c.php` under `/a/b`; a seed whose ancestor 404s is **not** confirmed (stale-seed pruning) — assert seed-vs-truth.
5. **Dedup & provenance**: a seed also present in the corpus becomes one candidate with unioned provenance and max prior.
6. **Priority**: a Disallow seed outranks generic corpus candidates in dispatch order; still calibration-validated before being reported.
7. **Graceful degradation**: missing robots/sitemap and blocked egress produce warnings, zero seeds, no crash.
8. `go build`, `go vet`, `go test -race ./...` clean; CLI smoke test on a fixture with a Disallow-hidden planted path shows it found early via the `robots` provenance.

## 8. Build order & handoff
`seed` package: `RawSeed`/`SeedSource` types → `robots` → `sitemap` (incl. gzip + index) → `wayback` (CDX + polite limiter + `SeedSource` interface) → normalization/scoping/dedup/prior-tiering → engine wiring (`enqueueSeed` + `ensureDirChain` per contract A/C; seed stage in `scanStart`; config/flags). Write the fixture servers + stub CDX and the parser/normalization table-tests **before** engine wiring.

**Before writing Phase 4b, report back:** the `enqueueSeed`/`ensureDirChain` API as built, how deep seeds interact with the calibration/tree lifecycle in practice, and any §0 deviation. Phase 4b (crawl + JS harvesting) enqueues discovered paths through the **same** `enqueueSeed` path (harvested links are just another seed source, provenance `crawl:*`), and its SPA pivot keys off `Baseline.IsSPA` — so it binds to what you finalize here.

## 9. Implementation notes (as built)

- **`enqueueSeed(sd seed.Seed)`** (`internal/engine/seed.go`) splits the seed's path into ancestor segments + a leaf, bounds total depth against `MaxDepth`, checks scope once for the full path, then calls `ensureDirChain` for the ancestors and `stageDirCandidate` for the leaf.
- **`ensureDirChain(dirs []string, prio float64, provenance string)`** materializes each missing ancestor by calling the *existing, unmodified* `enqueueChildDirectory` — the same function a live hit's own recursion already uses to spin up a child `dirState` (idempotent: a no-op if the dir already exists). It also pushes each ancestor as an ordinary `TypeDir` candidate under its own parent via `stageDirCandidate`. Both happen unconditionally and are not gated on each other: a fake `/old` still gets calibrated and its ancestor `/old/admin` still gets created immediately, rather than waiting for `/old` to first classify as a hit. This is what lets a single deep seed open a whole subtree in one pass instead of serially confirming each level — and it doesn't weaken stale-seed pruning, because the leaf's own classification is checked against *its own parent's* calibrated baseline regardless of how that parent came to exist.
- **How deep seeds interact with the lifecycle in practice**: a directory materialized this way is brand new (`dirCalibrating`, no `knownPaths`/budget yet), so a seed candidate for it can't go through `enqueueGenerated`'s existing dedup path. `stageDirCandidate` instead holds it in a new `dirState.pendingSeeds map[string]Candidate` field. `pushWordlistCandidates`/`pushCorpusCandidates` drain `pendingSeeds` on the way out: a path that matches a template entry gets that entry upgraded in place (`mergeSeedCandidate` — max `BasePrio`, `seed.UnionProvenance`'d `Provenance`) instead of pushed twice; a path the template never had is pushed as a new candidate. `candidatesTotal`/`budget` are computed *after* this merge so a matched seed is never double-counted. This merge-into-template step is also where DoD #5 (dedup against the corpus) actually happens in 4a — the corpus/wordlist template for a directory doesn't exist yet at seed time, so there is nothing already on the frontier to reconcile against.
- **Known gap flagged for Phase 4b**: `stageDirCandidate`'s branch for a directory that's already `SCANNING` falls back to `enqueueGenerated` verbatim — first-writer-wins dedup against `knownPaths`, no retroactive prior/provenance merge if a path was already pushed. 4a's own seeding is fully synchronous before any dispatch (`seedPassive` runs right after `seedRoot`, before workers start), so this branch is never exercised by 4a's tests. 4b's crawl-time seeding calls `enqueueSeed` against live `SCANNING`/`DONE` directories routinely (a harvested link can name a directory mid-scan), so it should decide then whether the stronger merge is worth the extra bookkeeping.
- **Deviation — `ctx` on `SeedSource.Fetch`**: the spec's pseudocode is `Fetch(host string) ([]RawSeed, error)`; the shipped interface is `Fetch(ctx context.Context, host string) ([]RawSeed, error)`, matching every other network call in this codebase (`httpclient.Client.Do`, `profile.Fetch`).
- **Deviation — `Config.WaybackURL`**: not in §6's config table. Overrides the CDX base URL (`""` = the real `seed.CDXBaseURL`), added so tests can point `Wayback` at a stub server — the mechanism the DoD's hermetic stub-CDX fixture actually relies on.
- **Deviation — Wayback recency cutoff is a made-up constant**: the spec's recent-vs-old tiering (§3) doesn't name a threshold. Shipped as `waybackRecentWindow = 365 * 24 * time.Hour` (`internal/seed/normalize.go`) — a capture within a year of "now" is "recent" (`BasePrio` 0.85), older is "old" (0.70). Not derived from anything in the spec; revisit if it turns out to be a bad proxy for "probably still there."
- **Provenance strings as built**: `"robots:disallow"`, `"robots:allow"`, `"sitemap"`, `"wayback:<YYYYMMDD>"` (date-only, trimmed from the CDX timestamp) — matches spec §3's `Provenance="wayback|robots|sitemap"` (+ capture date for Wayback) directly; no deviation, noted here only because Phase 4b's `provenance "crawl:*"` needs to slot in next to these.
- **Dry-run is not seed-aware**: `PreviewRequests`/`PreviewRequestsCorpus` are unchanged. Robots/sitemap/Wayback all require real network calls to produce anything, which `--dry-run` exists specifically to avoid — so a dry-run preview still reflects only the wordlist/corpus order, same as before 4a.

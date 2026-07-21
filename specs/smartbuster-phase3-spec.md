# smartbuster — Phase 3 Build Specification (Dynamic Intelligence)

*Build-ready spec for the feedback loop: scoring that adapts to what the scan discovers. This is the phase closest to the learned-enumeration research — heuristic and lightweight-statistical, no ML model. Extends the centralized scorer from 2b; feeds the same frontier. Read alongside the 2b spec and the implementation plan (Phase 3).*

---

## 0. Integration contract — verify against committed code first

The 2b handoff confirmed the key contracts. Phase 3 binds to them.

| # | Attachment point | Confirmed (2b) | Phase 3 action |
|---|---|---|---|
| A | `corpus.Score(basePrio, tags, profile, techBoostW) float64` — the single static scoring fn | confirmed | **Do not modify.** It stays the *static prior*. Phase 3 wraps it with a dynamic layer that lives in `engine` (where session state is), not in the `corpus` leaf package. |
| B | `engine.applyMatchScore(cand *engine.Candidate)` calls `corpus.Score` | confirmed | Becomes `applyScore`: `cand.Score = corpus.Score(...) * dynamicScorer.Boost(cand)`. |
| C | `Coordinator.reprioritizeIfChanged()` → `Frontier.Reprioritize(c.applyMatchScore)`, guarded by a profile signature | confirmed | Generalize the trigger: reprioritize on (profile change) **OR** (dynamic context dirty + throttle interval). §6. |
| D | Confidence model + gate (Phase 1) used for recursion | confirmed | Reuse as the **learning gate**: only high-confidence hits feed the learners (§5). |
| E | Per-directory baseline/calibration + `ParentDir` on candidates | confirmed | Response-semantics and convention signals are scoped by `ParentDir`. |
| F | `engine.Candidate` has `Tags []string` | confirmed | Phase 3 adds no required field; may cache a `dynScore` for lazy reprio (optional, §6). |
| G | Confirmed-hit path (Phase 1 `recordFinding`) | exists | Phase 3 hooks it: a confirmed hit updates the learners + marks the scorer dirty (gated by D). |

## 1. Scope

**In (Phase 3):** the `DynamicScorer`; response-semantics signals; heuristic association rules + candidate generation (backups, siblings, version bumps, a small companion table); online character-level Markov naming-convention model; exploration/exploitation control; confidence-gated learning (poisoning defense); throttled reprioritization; subtree-aware scoring.

**Out:** passive seeding / crawl / JS (Phase 4); stealth (Phase 6); any ML/LM ranker (dropped). **Note on data:** a *data-mined* "sites-with-A-have-B" co-occurrence table needs a dataset you likely don't have (same reality as the dropped ML ranker). Phase 3 therefore builds co-occurrence from **heuristic rules + a small hand-curated companion table + session-local learning** — buildable now, no dataset. A mined table is an optional future drop-in behind the same interface (§3.2).

## 2. The DynamicScorer (`internal/engine/scorer.go`)

The static prior (`corpus.Score`) is stateless; every Phase 3 signal is *stateful* (depends on what's been discovered). So the dynamic layer lives in the engine, holds session state, and combines multiplicatively with the static prior:

```go
type DynamicScorer struct {
	profile   *profile.TargetProfile
	markov    *MarkovModel            // §3.3, online
	assoc     *AssocEngine            // §3.2, rules + companion table + session
	dirCtx    map[string]*DirContext  // §3.1, per-directory response-semantics
	cfg       ScoreWeights
	mu        sync.Mutex              // coordinator-owned; lock only if scorer is touched off the main goroutine (it should not be)
}

// Boost returns the product of per-signal factors, each (1 + wᵢ·sᵢ), sᵢ∈[0,1].
func (s *DynamicScorer) Boost(c *Candidate) float64 {
	b := 1.0
	b *= 1 + s.cfg.WSem  * s.semSignal(c)   // response-semantics (per ParentDir)
	b *= 1 + s.cfg.WAssoc* s.assocSignal(c) // association
	b *= 1 + s.cfg.WConv * s.convSignal(c)  // naming-convention likelihood
	return b
}
```

Final score (in `applyScore`): `cand.Score = corpus.Score(cand.BasePrio, cand.Tags, profile, WTech) * scorer.Boost(cand)`.

Multiplicative composition is chosen over a log-linear sum only to avoid churning 2b's shipped `corpus.Score`; it is equivalent in spirit (small weights ≈ additive in log-space) and each `wᵢ` is independently tunable. If you'd rather refactor to an explicit log-linear sum, do it here in one place — but it's not required.

The scorer is **coordinator-owned state** (single-writer), consistent with the actor model — it's mutated only from `handleResult`, never from workers, so no locking is needed in practice (the mutex is a guard-rail).

## 3. Signals

### 3.1 Response-semantics (per-directory, no model — highest value/effort ratio)

`DirContext` per directory accumulates flags from classified responses:
```go
type DirContext struct {
	SawProtected bool   // a 401/403 was seen in this dir
	SawIndexOf   bool   // an open directory listing (200 + "Index of")
	HitCount     int
}
```
- On a classified `401/403`: set `SawProtected`. Then `semSignal(c)=1.0` for candidates in this dir whose term carries a **sensitive tag** (`admin, private, internal, config, backup, secret, .git, .env, api`) or matches a sensitive term list — the "exists-and-protected" insight: a locked `/admin` strongly implies neighbors worth trying.
- On `200 + "Index of"`: set `SawIndexOf`; boost the dir's **recursion** priority (feed Phase 1's recursion scorer), since open listings are high-value.
- `semSignal` defaults to 0 (neutral) when no flags apply. Cheap, stateless per-response, big payoff.

### 3.2 Association + generation (`AssocEngine`)

Two behaviors — *reweight* existing frontier candidates, and *generate* new ones — both gated by confidence (§5).

**Reweight** via `assocSignal(c)`:
- **Companion table** (`companions.yaml`, small hand-curated, ships like the ruleset): symmetric groups such as `{login.php, config.php, db.php, admin.php}`, `{index, home, default}`, `{users, user, accounts}`. If a confirmed hit is in a group, matching frontier candidates in the same group get `assocSignal=1.0`.
- **Session-local**: confirmed hits recorded; the signal is "is `c` a companion of something already found here."

**Generate** — on a confirmed hit matching a template, enqueue new high-priority candidates (deduped against the frontier):
- **Backup generation**: `config.php` → `config.php.bak/.old/.swp/~`, `config.php.1`; `<name>` → `<name>.bak`. (2b already emits some backups at selection; here it's *triggered by an actual hit*, so it fires even for paths the corpus didn't contain.)
- **Sibling/sequence**: trailing integer (`user/1` → `user/2,3`), version tokens (`/api/v1/` → `/api/v2,v3/`), date-ish patterns. Bounded (e.g. next 5).
- **Extension pivot**: found `admin.php` (a hit) → the site uses `.php`; reinforce `.php` expansion for pending stems (also feeds §3.3).

**Optional mined table**: `AssocEngine` exposes `LoadMinedTable(path)`; if you later obtain a real host→paths dataset, a mined `cooccurrence(a,b,weight)` table (the SQLite table 2b deferred) drops in behind the same `assocSignal`. Not required for Phase 3.

### 3.3 Online naming-convention Markov (`MarkovModel`)

Character-level n-gram (order `MARKOV_ORDER`, default 3) trained online on the **terminal path segment** of confirmed, high-confidence hits.
- `Train(segment string)` updates counts (gated by §5; never trained on wildcard/SPA-dir hits).
- `convSignal(c)` = normalized log-likelihood of `c`'s terminal segment under the model, squashed to `[0,1]`.
- **Cold-start guard**: return neutral 0 until `≥ MARKOV_MIN_SAMPLES` (default 8) confirmed segments — a model trained on 2 paths is noise.
- Captures case/separator/token style implicitly (a site of `get_user.php, get_role.php` scores `get_*` high). Explicit structural features (separator style, plural/singular, dominant extension) can augment it but the n-gram covers most; keep them as optional add-ons.

## 4. Exploration vs. exploitation (`internal/engine/frontier` policy)

Pure greedy (always pop max) tunnel-visions into one branch. Add bounded exploration, reproducible under the seed:
- **Primary — subtree yield cap**: a directory may run at most `SUBTREE_BURST` (default 200) consecutive requests before the coordinator round-robins to the next-best *other* directory, guaranteeing breadth. Deterministic, seed-independent.
- **Secondary — ε-greedy**: with probability `EPSILON` (default 0.05, seeded RNG) pop a candidate sampled from the frontier's mid-tier instead of the max. Off by default; enable for coverage-heavy runs.
- Interaction with stealth: greedy → fewer total requests, so `quiet`/`stealth` modes lower `EPSILON` to 0 and may raise `SUBTREE_BURST`. Interaction with the guardrail metric (Phase 7 "recall at unlimited budget"): exploration is what protects long-tail recall, so its defaults are tuned against that metric later.

## 5. Confidence-gated learning (poisoning defense)

The single rule that keeps soft-404 branches and traps from corrupting the learners:
- A confirmed hit feeds `markov.Train`, `assoc` (reweight+generate), and `dirCtx` **only if** `confidence ≥ LEARN_MIN_CONF` (default 0.8; ≥ Phase 1's `RECURSE_MIN_CONF`) **and** the hit's directory is **not** wildcard/SPA-flagged.
- Generated candidates (§3.2) are themselves subject to normal calibration before they can become hits — generation sets priority, calibration sets truth (the invariant throughout).
- Assert (test): low-confidence "hits" leave `markov` and `assoc` state unchanged.

## 6. Throttled reprioritization

Reprioritizing the whole frontier on every hit is O(N)/hit — too costly. Generalize 2b's `reprioritizeIfChanged`:
- A qualifying discovery (passes §5 and changes context) sets `scorerDirty = true`.
- The coordinator performs a full `Frontier.Reprioritize(c.applyScore)` at most once per `REPRIO_INTERVAL` (default: every 25 confirmed hits **or** 500 ms, whichever first) while dirty, then clears the flag. Profile changes (2b's trigger) also set dirty.
- **Generated candidates** (§3.2) are pushed immediately with a computed score (no full reprio needed for them).
- Optional optimization (note, not required): lazy scoring — recompute a candidate's dynamic score at pop time, re-heapify only if it moved materially; avoids full sweeps entirely. Ship the batched version first; it's simpler and correct.

## 7. Config additions & defaults (`config.go`)

```go
type ScoreWeights struct { WTech, WSem, WAssoc, WConv float64 }
```
| Constant | Default | Meaning |
|---|---|---|
| `WTech` | 2.0 | static tech-match weight (unchanged from 2b) |
| `WSem` | 1.5 | response-semantics weight |
| `WAssoc` | 1.0 | association weight |
| `WConv` | 0.8 | naming-convention weight |
| `MARKOV_ORDER` | 3 | char n-gram order |
| `MARKOV_MIN_SAMPLES` | 8 | cold-start threshold |
| `LEARN_MIN_CONF` | 0.8 | gate for feeding learners |
| `SUBTREE_BURST` | 200 | consecutive reqs per dir before yielding |
| `EPSILON` | 0.05 | ε-greedy exploration prob (0 in stealth) |
| `REPRIO_INTERVAL` | 25 hits / 500 ms | reprioritization throttle |
| generation bounds | 5 siblings, 6 backup exts | cap generated candidates per hit |

All weights CLI-overridable and, more importantly, the knobs the Phase 7 ablation will sweep.

## 8. Subtree-aware scoring (resolves the 2b deferral, pragmatically)

2b reused one Select+Expand template for all directories. Phase 3 does **not** re-run `Select` per subtree (still heavy); instead, because every dynamic signal already takes the candidate's `ParentDir` (`semSignal`, and `assoc`/`markov` state reflect hits observed so far), **scoring is now subtree-aware for free** — `/api/*` candidates get boosted by `/api` discoveries, `/admin/*` by admin discoveries. That delivers most of the value of per-subtree selection without the re-query cost. True per-subtree corpus queries remain an optional later refinement.

## 9. Tests, fixtures, DoD

**Fixtures (`test/fixtures/dynamic_server.go`):**
| Fixture | Presents | Expected |
|---|---|---|
| `protected_admin` | `/admin`→403; `/administrator`,`/admin-panel` exist | `SawProtected` boosts the neighbors; they surface earlier than baseline |
| `naming_convention` | many `get_*.php`; plants `get_secret.php` | after `MARKOV_MIN_SAMPLES`, `get_secret.php` is boosted and found earlier |
| `sequence` | `/api/v1/` exists; plants `/api/v2/` | v2 generated + found without being in the wordlist |
| `backup_trigger` | `config.php` exists; plants `config.php.bak` | backup generated on the hit + found |
| `companions` | `login.php` exists; plants `config.php`; companion table links them | `config.php` boosted after `login.php` hit |
| `soft404_poison` | branch 200s everything (low-confidence) | learners **unchanged**; no generation; no reprio thrash |
| `open_listing` | `/uploads/` → 200 "Index of" | dir flagged high-recurse; recursion prioritized |

**Assertions / DoD:**
1. Each signal, in isolation, surfaces its planted path in **fewer requests** than the 2b baseline on the same target (per-signal deltas — this **is** the Phase 7 ablation harness in embryo; record each number).
2. Generation produces calibration-validated hits for paths absent from the corpus (`/api/v2/`, `config.php.bak`).
3. **Poisoning gate**: after a low-confidence soft-404 branch, `markov`/`assoc`/`dirCtx` are provably unchanged.
4. **Reprioritization is throttled**: reprio call count is bounded by `REPRIO_INTERVAL`, not one-per-hit (assert call count over a scan).
5. **Reproducibility**: same seed → identical run including ε-greedy choices.
6. Subtree-awareness: an `/api` discovery boosts `/api/*` candidates but not unrelated ones.
7. `go build`, `go vet`, `go test -race ./...` clean; CLI smoke test on the `protected_admin` + `naming_convention` combined fixture shows planted paths front-loaded vs. the 2b run (record the requests-to-coverage improvement — this extends the DoD-8 efficiency trajectory and should show a clear step up from 2b's ~#260/831).

## 10. Build order & handoff

`scorer` skeleton + `DynamicScorer.Boost` wiring into `applyScore` → `dirCtx`/`semSignal` (cheapest, validate first) → `MarkovModel` + cold-start → `AssocEngine` (companion table + generation) → exploration policy → confidence gate → throttled reprio → fixtures + per-signal ablation tests **before** final integration. Write each signal's fixture + table-test as you add it, so ablation numbers exist from the start.

**Before Phase 4, report back:** the `DynamicScorer` API and final signal weights, how generated candidates are enqueued/deduped, and any §0 deviation. Phase 4 (Wayback/robots/sitemap seeding + JS endpoint harvesting) feeds new candidates into this same frontier and scorer — seeds set priority, calibration sets truth — so it binds to the enqueue path and the scorer you finalize here.

## 11. Implementation notes (as built)

- **`DynamicScorer` API**: `Boost(c *Candidate) float64` returns `(1 + WSem·semSignal) * (1 + WAssoc·assocSignal) * (1 + WConv·convSignal)`, called from a new `Coordinator.scoreCandidate(cand Candidate) float64` that wraps `corpus.Score(...)` — this is the one place §0 contract B's final-score formula lives, used by both push paths (`pushCorpusCandidates`, `pushWordlistCandidates`) and generation (`enqueueGenerated`). `applyMatchScore` is renamed `applyScore` and now calls `scoreCandidate` too, so `Frontier.Reprioritize(applyScore)` serves both the 2b profile-change trigger (`reprioritizeIfChanged`, unthrottled — profile changes are rare) and the new dynamic-dirty trigger (`markScorerDirty`/`runDynamicReprio`, throttled per §6). `ScoreWeights.WTech` is populated from `TechBoostW` for reporting parity only; `Boost` never reads it — `corpus.Score` already applies `TechBoostW` on its own.
- **Final default weights** (`internal/engine/config.go`): `WSem=1.5`, `WAssoc=1.0`, `WConv=0.8`, `MARKOV_ORDER=3`, `MARKOV_MIN_SAMPLES=8`, `LEARN_MIN_CONF=0.8`, `SUBTREE_BURST=200`, `EPSILON=0.05`, `REPRIO_INTERVAL=25 hits / 500ms`. All ten are `Config` fields with matching `scan` CLI flags (`-w-sem`, `-w-assoc`, `-w-conv`, `-markov-order`, `-markov-min-samples`, `-learn-min-conf`, `-subtree-burst`, `-epsilon`, `-reprio-hits`, `-reprio-interval`) — the Phase 7 ablation sweep surface the spec calls for.
- **Deviation — zero is a real value for `Epsilon` and `Weights.{WSem,WAssoc,WConv}`**: every other new numeric `Config` field follows this file's existing "`<=0` means apply the default" convention (`MarkovOrder`, `MarkovMinSamples`, `LearnMinConf`, `SubtreeBurst`, `ReprioHits`, `ReprioInterval`), but those four don't — 0 is itself meaningful ("this signal off" / "pure greedy"), exactly like `Rate`/`Jitter`/`TimePerBranch` already work in this same struct. `NewCoordinator` passes them through unmodified; the CLI flag defaults (not `NewCoordinator`) supply `1.5`/`1.0`/`0.8`/`0.05` when a flag isn't passed. This is also what makes clean per-signal ablation possible: a caller can construct `ScoreWeights{WSem: X}` with every other weight genuinely zero.
- **Generated-candidate enqueue/dedup**: `AssocEngine.Generate(cand Candidate) []Candidate` (backups for any confirmed `TypeFile` hit — broader than 2b's sensitive-stem-only static generation, per §3.2's "fires even for paths the corpus didn't contain" — plus bounded sibling/sequence candidates for a trailing-integer terminal segment, covering both plain sequences and version tokens under one regex) returns candidates with `ParentDir`/`Depth`/`Tags` inherited from the hit. The coordinator's `enqueueGenerated(dir, cand)` (`dynamic.go`) dedupes against a per-`dirState` `knownPaths` set — seeded from the directory's initial template/wordlist push, grown by every subsequent generation — and, only on a genuine miss, increments both `candidatesTotal` and `budget` (so the directory can't finish early) before scoring and pushing the candidate immediately (no reprio wait, per §6).
- **Date-ish sequence patterns are not implemented**: only a single `prefix + trailing-digits` regex (`trailingIntRe`) drives sibling generation, covering plain sequences (`"1"→"2".."6"`) and version tokens (`"v1"→"v2".."v6"`) identically — calendar-date incrementing (`"2024-01-15"→next day`) needs date arithmetic, not a naive integer bump, and was scoped out as not required by any DoD fixture.
- **Response-semantics scoping, concretely**: `SawProtected` is recorded against the *current* directory (a locked `/admin`'s 401/403 boosts its siblings — same `ParentDir`), while `SawIndexOf` is recorded against the *child* directory (`dir + "/" + cand.Path` — the newly-discovered open listing's own future candidates get boosted once it's calibrated and scanned). `semSignal` treats `SawIndexOf` as a flat boost for every candidate in that directory (not just sensitive-tagged ones, per §3.1's "open listings are high-value" being broader than the locked-dir case) — there's no separate directory-level scheduling-priority concept in the current dispatch model to hook instead.
- **A real, pre-existing coordinator characteristic surfaced by testing, not a Phase 3 bug**: `nextDispatchable` doesn't block on results before dispatching more work (only the pacer and `workCh`'s buffer gate it), so against an unbounded-rate local target the dispatch loop can pop several more candidates before a hit's result comes back and reprioritizes the frontier. Per-signal ordering tests need a modest `Rate` to keep dispatch from racing meaningfully ahead of result processing — documented in `internal/engine/dynamic_test.go` as `dynOrderingRate`.
- **`internal/types.ResponseSignature` gained `HasIndexOf bool`**, computed for *every* response in `worker.go` (not just probes): a cheap `strings.Contains(norm, "index of")` over the already-computed normalized body. This doesn't reopen 2a's "`NormBody` only for probes" hot-path tradeoff — a bool costs nothing to carry, unlike retaining the body text itself — but it is a new field on the hot path, worth flagging as a deviation from "no required field" in §0 contract F (which was about `engine.Candidate`, not `ResponseSignature`, but is the same spirit).
- **Gap vs. §3.2's own example — no `.1`-style numbered backups**: `backupsFor` always emits the same fixed 6-suffix list (`.bak`/`.old`/`.zip`/`.tar.gz`/`.swp`/`~`), and `siblingsFor` only fires when the *entire* terminal segment is `prefix+trailing-digits` — `"config.php"` doesn't match that (it ends in `p`, not a digit), so the spec's own `config.php → config.php.1` example is not produced by either mechanism as built. Nothing in the DoD fixtures exercises this specific case, so it went unnoticed until this review; a future pass adding it would most naturally extend `backupsFor` with a numbered-suffix generator for any file hit, not extend `siblingsFor`'s trailing-digit rule (which is about the *whole* segment, not appending one).
- **Subtree yield cap's actual round-robin mechanic**: `nextDispatchable`'s `held` buffer only ever forces *one* dispatch from a different directory before the throttled directory's candidates are eligible again — `recordDispatch` resets `dispatchStreak` to 1 on that single switch, so the very next pop from the (still highest-scoring) throttled directory is no longer held. In practice this interleaves one other-directory request every `SUBTREE_BURST` consecutive dispatches rather than sustaining a full switch until the other directory is itself exhausted or over its own cap. This satisfies §4's "guaranteeing breadth" intent (no directory can monopolize the frontier past its cap) more cheaply than a persistent per-directory lockout would, but is worth naming explicitly since "round-robins to the next-best other directory" could also be read as a sustained switch.
- **ε-greedy and the subtree cap use a dedicated RNG stream**, `dirRand(cfg.Seed, "\x00__epsilon__")` — not the coordinator's own `c.rng` (which seeds the pacer's jitter) and not the per-directory `dirRand(seed, dir)` calibration-token stream. This isn't spec-mandated (§4 just says "seeded RNG"), but keeps ε-greedy draws from perturbing pacer timing or calibration token sequences, so DoD #5's "same seed → identical run including ε-greedy choices" holds independently of whether exploration is enabled.
- **DoD #1's "2b baseline" is operationalized as this same codepath with the relevant weight(s) at zero**, not a separately-run 2b binary: `applyMatchScore` no longer exists once renamed to `applyScore`, so there is no standalone "2b run" left to invoke side-by-side. Since `Boost(c) == 1` whenever every weight is 0, a `ScoreWeights{}` run is behaviorally identical to 2b's static-only scoring, and this is what `internal/engine/dynamic_test.go`'s per-signal tests (`TestDynamic_ProtectedAdmin_*`, `TestDynamic_Companions_*`, `TestDynamic_NamingConvention_*`) compare against, logging the delta via `t.Logf`.
- **DoD #7's requests-to-coverage number was not reproduced**: the per-signal deltas (DoD #1) are captured with logged numbers as above, and a manual CLI smoke test confirmed the full profiling → calibration → dynamic-scoring → hit path works end-to-end, but no combined `protected_admin`+`naming_convention` fixture run was built to record a single before/after coverage-curve number comparable to 2b's "~#260/831." Worth doing explicitly before Phase 7's ablation harness leans on this number.

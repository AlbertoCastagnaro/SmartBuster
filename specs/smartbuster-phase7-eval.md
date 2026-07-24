# smartbuster — Phase 7: Evaluation & Hardening

*The capstone phase, and the one that's more research than build. Three parts: **A** — a build spec for the evaluation harness; **B** — the experiment/methodology plan (the paper's methods section); **C** — a hardening & exports punch-list that finishes the product. Parts A and C are code; Part B is how you run and interpret A. Read alongside the implementation plan (§9) and the concept doc (the efficiency thesis this phase tests).*

> **Sequencing (the letters are topical, not chronological): do Part C first, then A/B.** You evaluate the *finished* tool, not a partial one. In particular, **headless (C.2) and the real default corpus (C.1) are hard gates on the eval** — headless defines what corpus/capability you're measuring, and you can't run a fair benchmark against a 130-term placeholder corpus. The real-target shakedown (C.6) is the natural precursor to the formal matrix. Exports/docs are orthogonal to the numbers but part of "ready to use," so they belong in the same product-completion pass.

---

# Part A — Evaluation harness (build spec)

## A.0 The payoff you already built

The harness is small because two earlier decisions front-loaded the work: **the JSONL audit log is the instrumentation** (every request, timing, classification, provenance already recorded), and **every phase's DoD accreted per-signal efficiency numbers**. So the harness is essentially: *ground-truth path set + audit-log parser + stats/plots + a baseline runner*. You are not building measurement plumbing; you are reading what the engine already emits.

## A.1 Components (`/eval`)

- **Runner** (`eval/runner.go` + a small orchestration script): for each (tool, target, seed), run the tool, capture its output. smartbuster → its audit log (via `--out`); baselines (gobuster/ffuf/feroxbuster) → their stdout/JSON, normalized to `{path, status, request_number}`.
- **Ground-truth registry** (`eval/truth/`): per target, the complete set of discoverable paths. Sources: synthetic servers (you planted them), framework installs (enumerate the image), vulnerable apps (curated from docs/source). Format: a simple `target.paths` list + metadata (which pathologies it exhibits).
- **Metrics** (`eval/metrics.go`): parse a run → the metrics in §B.2. All derived from request-ordered discoveries vs. ground truth.
- **Ablation driver** (`eval/ablation.go`): run smartbuster with feature flags toggled (frequency-only → +tech → +dynamic → +cooc/markov → +seeding), one target set, produce per-feature deltas.
- **Report generator** (`eval/report.go`): emit the discovery-curve plots (SVG/PNG), the requests-to-coverage table, the ablation table, and the FP-rate table — as a self-contained HTML/markdown report and CSVs for the paper.

## A.2 Target corpus (`eval/targets/`, Docker-composed, pinned)

Three layers, each testing something different:
- **Synthetic** (`eval/targets/synthetic/`): servers with **planted** path sets and **randomized layouts** (seed-generated, so they can't be overfit), each exhibiting specific pathologies — reuse the Phase-1 adversarial fixtures + a layout generator. Exact ground truth; the controlled-conditions tier.
- **Framework installs** (pinned Docker tags): fresh WordPress, Drupal, a default Tomcat, a Django/Next.js skeleton. Ground truth = enumerate the image. Tests the detection→selection loop.
- **Vulnerable apps** (pinned tags): DVWA, OWASP Juice Shop, WebGoat, Mutillidae. Realistic structure; ground truth curated. The realism tier. **Headless caveat (Part C)**: annotate any target whose routes require JS execution, so its coverage number isn't misread as an algorithm gap while headless is stubbed.

## A.3 Harness DoD

1. One command runs the full matrix (tools × targets × seeds) reproducibly and writes the report.
2. Metrics computed correctly against a known synthetic target (unit-tested: a hand-verified discovery curve).
3. Ablation produces per-feature deltas.
4. Baselines run on the **same superset corpus** as smartbuster (§B.3).
5. `go build`/`go vet`/`go test -race ./...` clean; the report regenerates deterministically from captured run data.

---

# Part B — Experiment & methodology plan

This is the paper's methods section. It's what turns "we think it's smarter" into a defensible claim.

## B.1 Claims under test

1. **Efficiency**: smartbuster reaches coverage in *fewer requests* than gobuster/ffuf/feroxbuster on the same corpus (the central thesis — efficiency = speed + stealth).
2. **Calibration**: near-zero false positives on soft-404/catch-all/wildcard targets, where naive tools spew them.
3. **Each smart feature earns its keep** (ablation): tech-selection, dynamic prioritization, co-occurrence/Markov, and seeding each contribute measurable lift.
4. **Capability**: JS harvesting + passive seeding find paths brute-force *cannot* (SPAs, historical paths) — a coverage class, not just an ordering win.
5. **Reproducibility**: seeded uninterrupted runs reproduce (stated precisely — §B.5).

## B.2 Metrics

- **Discovery curve** (discoveries vs. requests) — the centerpiece plot; precision/recall over budget.
- **Requests-to-coverage** at 50/90/100% — the headline efficiency number.
- **Coverage/recall at unlimited budget** — the *guardrail*: proves greedy prioritization doesn't *miss* long-tail paths the dumb tool eventually finds. Report this prominently; it's the honest counter-check on your own method.
- **False-positive rate** — scores calibration; the dramatic soft-404 win.
- **Time-to-first-hit** and **wasted-request breakdown** (real hits / correct negatives / false positives / redundant trap-and-dup).

## B.3 Experimental design (the parts that make it credible)

- **Baselines include ffuf (with `-ac`) and feroxbuster**, not just gobuster — beat best-in-class or the result is a strawman.
- **Isolate the variable**: give *every* tool the same *superset* corpus (all of SecLists Web-Content); measure who finds ground-truth paths in fewest requests. This isolates smartbuster's *ordering/selection* from "had a better list."
- **Ablation** (Claim 3): frequency-only → +tech → +dynamic → +cooc/markov → +seeding; each delta attributes lift to a feature. This tells you what to keep *and* is a paper's worth of results.
- **Variance & significance**: N seeded runs per (tool, target), report mean ± CI; a **paired significance test** across the corpus, not a single-app anecdote. Use **ε=0** for reproducible ablation (§B.5).
- **Overfitting/contamination guards** — the methodological trap, and your research area, so reviewers will look:
  - **Train/test split on targets**: tune weights on a subset, evaluate on held-out unseen targets.
  - **Randomized synthetic layouts**: you can't memorize what's seed-generated.
  - **Disjoint association data**: the co-occurrence/companion table must be built from data **disjoint** from the eval targets — otherwise you've contaminated the eval.

## B.4 What to report (paper-facing)

The discovery-curve figure (smartbuster vs. 3 baselines, averaged with CI bands), the requests-to-coverage table, the ablation table, the FP-rate comparison, and the capability cases (SPA/Wayback finds brute-force missed). Each with the significance stats from B.3.

## B.5 Reproducibility & fidelity caveats (state these precisely — don't overclaim)

Carried in from the build; stating them exactly is what keeps the paper honest:
- **Seeded *uninterrupted* runs reproduce** (calibration probe tokens are a pure function of `(seed, dir)` via `dirRand`; the deterministic exploration is subtree-burst). Use these for ablation.
- **ε-greedy is not reproducible under concurrency** (RNG draw-position depends on dispatch timing). So: keep **ε=0** for reproducible runs; if evaluating exploration-on, average over N seeds rather than claiming bit-identical runs.
- **Save/resume re-seeds exploration** (the RNG-on-resume note) — irrelevant to the eval since benchmark runs are uninterrupted, but don't claim bit-exact save/resume.
- **Fingerprint fidelity** is verified as "matches tls-client's browser profile" (SETTINGS/pseudo-header/header order authoritative; the capture-test JA3 is simplified). **Validate once manually** against a live fingerprinting service (tls.peet.ws / browserleaks) to confirm the profile matches real Chrome before claiming real-world indistinguishability.

---

# Part C — Hardening & exports (punch-list)

Product-completion work; **precedes the eval** (you evaluate the finished tool). Items 1–2 are the hard eval gates. All build off `GET /api/scans/{id}/findings` (the accessor added in the 5b patch).

1. **Ship a real default corpus (eval gate).** The tool currently ships only the ~130-term embedded minimal (`embedded/`) — a placeholder, not an enumeration corpus; the `test/fixtures/seclists/` tree is ingestion-test-only. So: clone SecLists (`git clone --depth 1`, MIT-licensed → redistribution OK with attribution) and `corpus build --seclists <path>` from the **medium** lists (raft-medium + directory-list-2.3-medium — solid coverage, skip the 1.2M-line `-big`). **Embed the resulting SQLite DB** as the default (a few MB — preserves the single-binary story) with the 130-term set demoted to a "DB missing" fallback; `corpus build` stays available to regenerate/go bigger. **The `sourcemap.yaml` is the quality lever** — verify it covers `Discovery/Web-Content` (`raft-*`, `directory-list-2.3-*` marked `freq_rank`, `CMS/*` tagged per-CMS+`php`, tech/backup lists) and tags them correctly, since tag quality *is* tech-selection quality. For the eval (Part B fairness), ingest the **full** Web-Content superset so every tool shares one corpus.
2. **Headless decision (eval gate).** Resolve the stub: either **wire real `playwright-go`** (out-of-process, per the original 4b design) so computed-route SPA discovery is live, **or** explicitly scope JS-execution-only targets out of the eval corpus and document the limitation. Recommendation: for a complete tool, wire it; for the *paper*, either is defensible as long as it's stated. This gates the eval because it defines what corpus/capability is measured.
3. **Exports** (`internal/export/`):
   - **Burp/ZAP**: a discovered-URL list for site-map import. (Live proxy-passthrough already exists via 6b's `--proxy` → point it at Burp.)
   - **Markdown/HTML report**: target, detected tech profile, scan mode/config, findings tree, stats, timing — the human deliverable.
   - **JSON**: the `[]Finding` for pipelines.
   - CLI `--export burp|md|json --out <file>` and a UI download button.
4. **Ruleset default** — name a recommended upstream fork (enthec/webappanalyzer) in the README so `ruleset update` has a known-good starting point without shipping a pinned commit.
5. **Reproducibility docs** — the B.5 caveats, in the README/`--help`, so users understand exactly what "reproducible" means.
6. **Release** — README (quickstart, the modes, the corpus build step), `make` targets, cross-compiled static binaries, and a first real-target shakedown run (a real vulnerable app end-to-end) before tagging — the natural precursor to the formal benchmark.

## Phase 7 DoD

- The harness runs the full matrix and regenerates the report + CSVs deterministically.
- A real SecLists-derived default corpus is shipped (not the 130-term placeholder); the source map covers Web-Content with correct tags; the eval uses the full superset.
- A discovery curve, requests-to-coverage table, ablation table, and FP-rate comparison exist for smartbuster vs. gobuster/ffuf/feroxbuster, with CI + significance.
- The guardrail (recall-at-unlimited-budget) is reported and shows no long-tail regression.
- Exports (Burp/md/json) work from CLI and UI.
- Headless is resolved (wired or documented-out).
- The reproducibility caveats are documented; the fingerprint validated once against a live service.
- `go build`/`go vet`/`go test -race ./...` clean; a real-target end-to-end run completed.

---

## Closing note

This is the last spec. When Part A + B are done you'll have the numbers that make the claim; when Part C is done you'll have a tool a pentester can pick up and use on a real engagement. The architecture held all the way through — the same primitives (per-directory baseline, similarity hashing, confidence gate, single-writer coordinator, one event stream, seeds-set-priority/calibration-sets-truth) kept solving new problems from Phase 1 to here, which is the sign it was designed right. Go get the numbers.

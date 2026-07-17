# smartbuster — Concept & Contribution

*A context-aware, adaptive directory and file enumeration tool.*

---

## 1. The problem

Directory/file enumeration tools — gobuster, ffuf, feroxbuster, dirb — are fast but fundamentally *dumb*. You pick a wordlist and they fire it, request after request, ignoring almost everything the target reveals about itself. Three failures follow:

- **They learn nothing from the stack.** A target running WordPress on PHP behind nginx gets the same blind wordlist as a .NET app behind IIS. The tool never adapts its guesses to the technology it is clearly looking at.
- **They learn nothing from what they find.** Discovering `/api/v1/` tells a human to try `/api/v2/`; discovering `get_user.php` suggests `get_role.php`. Dumb tools discard this and keep marching through the list in a fixed order.
- **They often can't tell what exists.** They key off HTTP status codes, which modern apps routinely lie about: custom `200` "not found" pages, single-page-app catch-alls that return the same shell for every path, wildcard directories. The result is false positives, wasted requests, and a footprint that is both noisy and trivial to detect.

## 2. The idea

smartbuster reframes enumeration as an **adaptive search problem** rather than a fixed loop. It:

1. **Profiles the target** from every available signal (response headers, cookies, favicon, error pages, an existing nmap scan, `robots.txt`/`sitemap.xml`, the Wayback Machine, and the app's own HTML/JavaScript).
2. **Composes and orders candidates** from that profile, drawing the right wordlist layers and file extensions for the detected stack — and always firing the highest-probability request next.
3. **Calibrates itself** to the target's actual "not found" behaviour, so it knows what a real hit looks like on *this* server instead of trusting status codes.
4. **Re-prioritises continuously** as it discovers paths, feeding every finding back into the ordering of what to try next.

The governing objective is **efficiency: findings per request.** This is the key insight that unifies the whole design — higher efficiency means *both* a faster scan *and* a quieter, harder-to-detect footprint. Speed and stealth stop being in tension; both fall out of not wasting requests.

Just as important is what smartbuster is **not**: it achieves this with **heuristics and lightweight statistical models** — frequency priors, online character-level Markov/n-gram models, and association rules — **not** a heavyweight ML or language model. This keeps it practical and dataset-light while still capturing the spirit of learned-enumeration research.

## 3. Contributions

1. **Target-aware wordlist composition.** Wordlists are decomposed into a tagged corpus (by entry type, technology affinity, and real-world commonality). Selection becomes a *query* over that corpus driven by the detected profile — composing generic + stack-specific layers — rather than a blind pick of one flat file. File extensions are generated at runtime from the detected stack, not baked in.

2. **First-class auto-calibration.** Robust soft-404 detection is treated as a core subsystem, not an afterthought. smartbuster learns a per-directory "negative baseline," normalises out volatile content, compares with similarity hashing rather than exact matches, and derives its hit threshold from the *observed variance* of the baseline itself. It detects and adapts to SPA catch-alls, wildcards, and WAF/rate-limit pages.

3. **A unified priority frontier.** All intelligence — technology weighting, nmap seeds, recursion, response semantics (`403` means "exists and protected"), co-occurrence rules, and online naming-convention learning — expresses itself as adjustments to a single scored priority queue. One mechanism, explainable and tunable, subsumes selection, ordering, and recursion.

4. **Multi-source signal fusion.** Passive and active sources (headers, cookies, favicon hashes, nmap output, Wayback history, robots/sitemap, and JavaScript endpoint harvesting) all feed one weighted, confidence-scored target profile. Critically, external sources *set priority* while calibration *sets truth* — a seed is a strong guess, never a confirmed finding.

5. **Efficiency as the unifying objective.** Because the metric is findings-per-request, the same design decisions that make smartbuster smart also make it fast and stealthy. This is measurable (see the evaluation plan) via discovery curves and requests-to-coverage.

6. **Explainability and live observability.** Every candidate carries provenance (which signal or layer produced it), and a local web UI streams the scan in real time — the discovered-path tree growing, the priority frontier reordering, the tech profile resolving, traps being pruned. The "smart" behaviour is legible rather than magic.

7. **Layered, realistic stealth (v2).** Stealth is modelled honestly across three detection tiers — naive log/threshold, signature/heuristic (IDS/WAF rules), and client-fingerprint (Cloudflare/Akamai) — and addressed with the right tool for each: global rate limiting and jitter, realistic header profiles, and full TLS/HTTP-2 fingerprint mimicry.

## 4. Design principles

- **Reorder, don't exclude.** Signals change *priority*, they almost never remove candidates. A generic baseline is always present so stack-specific weighting never blinds the tool to `/backup`, `/.git`, `/admin`.
- **Seed vs. truth.** External sources (Wayback, robots, nmap, crawl) set candidate priority; only calibration confirms a finding. A path from the sitemap that 404s today is a good guess that failed, not a false positive.
- **Confidence gating.** Only high-confidence findings are allowed to spawn recursion or train the online learners — this single rule prevents soft-404 branches and traps from poisoning the scan.
- **One coherent event stream.** The engine emits a single stream of events; the UI renders a sampled view of it and the audit log records all of it losslessly. The observability built for the product doubles as the instrumentation for research evaluation.
- **Responsible by design.** smartbuster is a *reconnaissance* tool: it discovers paths, it does not exploit them. Scope enforcement, a dry-run mode, a complete audit trail, and a localhost-only control server make its behaviour verifiable and safe for professional engagements.

## 5. Relation to existing tools

| Tool | Selection | "Not found" handling | Adaptivity | Observability |
|---|---|---|---|---|
| gobuster | one static wordlist | status codes | none | CLI only |
| ffuf | static wordlist(s) | autocalibration + manual filters | none | CLI only |
| feroxbuster | static wordlist | status/size heuristics | recursion | CLI only |
| **smartbuster** | **profile-driven corpus query** | **learned per-directory baseline + similarity hashing** | **full priority-frontier feedback loop** | **live web UI + lossless audit** |

smartbuster's contribution is the layer above the request engine: profile-driven selection, calibrated truth, adaptive prioritisation, and legibility — delivered without a heavyweight model.

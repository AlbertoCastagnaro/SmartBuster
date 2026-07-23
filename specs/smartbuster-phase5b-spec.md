# smartbuster — Phase 5b Build Specification (Web UI)

*Build-ready spec for the local web UI that renders the frozen 5a protocol — the "watch it think" dashboard. Binds to 5a's exact REST routes and WS event catalog; adds no engine behavior. Read alongside the 5a handoff (the frozen protocol is this spec's entire input).*

---

## 0. Integration contract — the frozen 5a protocol is the input

5b consumes 5a **as delivered** (the handoff tables). Non-negotiable specifics to build against, verbatim:
- **Handshake**: token in URL fragment `#token=<64-hex>` (never sent to server). REST → `Authorization: Bearer <token>`; state-changing REST also needs `Origin: http://<host>:<port>`. WS → token via `new WebSocket(url, [token])` (`Sec-WebSocket-Protocol`); token+Origin checked before the 101.
- **JSON quirk to honor**: `engine.Config` and `engine.SessionState` serialize with **PascalCase Go field names** (`"Targets"`, `"Concurrency"`, `"Rate"`, …) and `time.Duration` fields are **int64 nanoseconds**; everything daemon-defined (`ScanStatus`, `AdjustRequest`, `PatternRequest`, `InjectRequest`, `SaveRequest`, `SessionMeta`) is `snake_case`. The scan-launch form (§8) must post PascalCase Config; status/control use snake_case. Don't "normalize" these — match the wire.
- **Event catalog**: exactly the 5a table — `scan.started/finished`, `calibration.done`, `hit` (+`Message:"alias"`), `spa.pivot`, `tech.detected` (`Tech[]`), `waf.detected` (`WAF`), `trap.detected`, `branch.pruned`, `stats` (`StatsPayload`), `frontier.snapshot` (`SnapshotPayload`), `warning` (`WarnPayload.Source`), `error` (`ErrorPayload`), `throttle`, and the **hub-synthesized** `hit.coalesced{count}`.
- **`mode` is inert until Phase 6** — render it read-only/"reserved," never as a live behavior toggle.

## 1. Scope

**In (5b):** a static Svelte SPA embedded into 5a's `embed.FS` mount; the WS connection + event-ingestion store (the load-bearing data layer); the live dashboard (gauges, discovered-path tree, **priority-frontier view**, tech profile, event log); controls wired to REST; lossy-stream client handling; the session browser + scan launcher; provenance-driven visual legibility.

**Out:** any engine change (5b is pure presentation); the real headless capability (still stubbed); export formats beyond what 5a serves (Burp/markdown exports are Phase 7/plan-level).

## 2. Tech & build integration

- **Vite + Svelte**, built to a static bundle (no SSR — it's a local dashboard). A single-page app is correct here.
- `npm run build` → `web/dist/` → copied into the Go `embed.FS` asset mount 5a left empty. Add a `make ui` / build step that runs the Vite build then `go build`, so the single binary ships the UI. Document that dev mode (`vite dev` proxying to `smartbuster serve`) is available for iteration but production is the embedded bundle.
- No runtime npm, no external CDN — everything embedded, matching the single-binary story.

## 3. The data layer (build + test this first — it's the primitive)

Everything renders from one place; get it right before any component.

- **`connection.ts`**: read `#token`, open WS with the token subprotocol, send Bearer on REST. **Auto-reconnect** with backoff on drop (re-handshake); surface connection state to the UI (connected / reconnecting / closed). On reconnect, request current status via `GET /api/scans/{id}` to resync (the stream is lossy — never assume continuity across a reconnect).
- **`store.ts`** — a reducer over the event stream maintaining derived state:
  - `stats` → replace-latest gauge state (+ a short ring buffer for sparklines).
  - `frontier.snapshot` → replace-latest top-K list.
  - `hit` → insert into the **tree** (by `Dir`/`URL`) and the **findings list**; `Message:"alias"` marks an alias (dimmed, grouped under its canonical). `hit.coalesced{count}` → bump a "+N" badge without individual rows.
  - `tech.detected` → replace the tech-profile panel (techs, confidence, layer, WAF from `waf.detected`).
  - `warning`/`error`/`trap.detected`/`branch.pruned`/`throttle`/`spa.pivot`/lifecycle → append to the **event log** with `Category` for grouping (never dropped by 5a, so the log is authoritative for these).
- The store is framework-thin (plain TS) so it's unit-testable against recorded event fixtures independent of Svelte.

## 4. Views

**4.1 Header / status bar** — target, scan state (running/paused/stopped/finished from `ScanStatus`), seed, elapsed, connection indicator, and the **SPA-pivot banner** (on `spa.pivot`: "Single-page app detected — pivoting to JS endpoint harvesting"). The pivot banner is a headline moment; make it prominent.

**4.2 Gauges (from `stats`, 400 ms)** — req/s, hit-rate, requests sent, in-flight, frontier length, dirs scanning, elapsed, and **ETA** (`ETAms`; render "unbounded" when `-1`). Sparklines from the ring buffer. This is the "is it working / how fast" glance.

**4.3 Discovered-path tree (from `hit`)** — the site tree growing live: nodes = confirmed findings, nested by directory, each showing status, confidence, and a **provenance tag** (§5). New hits animate in; open-listing dirs and `403`/protected paths are visually marked (they're the interesting ones). Click a node → pin/exclude/boost/demote it (§6).

**4.4 Priority-frontier view (from `frontier.snapshot`, 1 s) — the centerpiece.** The top-25 candidates about to be tried, with score, provenance, and directory, **reordering live** as the engine reprioritizes. This is the visualization that makes "smart" legible — you literally watch tech detection, seeds, and discoveries push candidates up the queue. Show *why* a candidate ranks where it does via its provenance tag and score. Don't bury this; it's the demo.

**4.5 Tech profile (from `tech.detected`/`waf.detected`)** — detected technologies with confidence bars, **edge vs backend layer** separation, and the WAF banner. Updates as profiling refines mid-scan. (Note: after a session resume, rule-id provenance may be absent per 5a — render gracefully without it.)

**4.6 Event log (from `warning`/`error`/`trap.detected`/`branch.pruned`/`throttle`)** — a filterable stream grouped by `Category`, with warning `Source` and error `Kind` as structured facets (this is exactly why 5a added `Category`/`WarnPayload`/`ErrorPayload` — filter on those, never parse message strings). Show `trap.detected` → `branch.pruned` as a detection→cut pair. `throttle` gets a visible "backing off (WAF/rate-limit)" indicator.

**4.7 Findings panel** — the flat confirmed-findings list (dedup/alias-aware), sortable by confidence/score, filterable by provenance, with a copy/export affordance (export the list 5a serves).

## 5. Legibility — the design principle that carries the product

Provenance is the story. Assign each provenance a stable color/tag and use it **everywhere** (tree nodes, frontier rows, findings): `wordlist`/`corpus`, `crawl:html`, `crawl:js`, `wayback`, `robots`, `sitemap`, `nmap`, `user`, `headless`, and mixed (e.g. `corpus+crawl:html` — the strong signal). When a viewer can *see* that `/api/v2/` came from a generated sibling, or that `wp-admin` jumped the queue because WordPress was detected, the intelligence stops being magic. Everything smart the engine does across Phases 2–4 should be visible in these tags. This is the single most important UI decision — prioritize it over chrome.

## 6. Controls → REST

Buttons/forms map 1:1 to 5a routes: pause/resume/stop; a live rate/concurrency adjuster (`PATCH` `AdjustRequest`, pointer fields = only send what changed); manual override (`pin`/`exclude`/`boost`/`demote` via `PatternRequest`, `inject` via `InjectRequest`) reachable both from a pattern input **and** by clicking a tree/frontier node (prefill the pattern). `mode` shown read-only with a "takes effect in stealth builds" note. All state-changing calls send `Bearer` + `Origin`. Optimistic UI is fine, but reconcile against the `ScanStatus` the route returns.

## 7. Lossy-stream client handling (honor 5a's contract)

The client must expect loss and never assume continuity: `stats`/`frontier.snapshot` are replace-latest (just overwrite — no history assumptions); `hit.coalesced{count}` means "N hits happened without individual rows" → increment a counter/badge, don't fabricate rows; on WS reconnect, re-fetch `GET /api/scans/{id}` and rebuild derived state, since arbitrary events may have been missed. The event log's non-droppable categories (warning/error/trap/lifecycle) can be treated as authoritative; the tree/findings are best-effort and reconciled on reconnect.

## 8. Session browser & scan launcher

- **Launcher**: a form → `POST /api/scans` with a PascalCase `engine.Config` (target required; expose the common knobs — wordlist path, rate, concurrency, depth, and the Phase 2–4 toggles: `--nmap` file, `--wayback`, `--crawl`, `--js-harvest`, `--active-probes`; durations sent as ns int64). A multi-scan list (`GET /api/scans`) so several can run/switch.
- **Sessions**: list (`GET /api/sessions` → `SessionMeta`), save current (`POST /save`), resume (`POST /sessions/{id}/resume` → switches to the new scan id), download (`GET /api/sessions/{id}`, heavy — lazy).

## 9. Design direction

A dense, technical, real-time **operator dashboard** — dark theme, monospace for paths/scores, calm under high update rates (no jarring reflow when `stats` ticks 400 ms or the frontier reorders each second — animate reorders smoothly, throttle layout thrash). Information density over whitespace; this is a tool for a professional watching a live scan, not a marketing page. **Apply the `frontend-design` skill** when implementing for typography/color/token discipline — this spec defines *what* renders and *from which events*; the skill governs *how it looks*.

## 10. Tests & DoD

1. **Data layer**: `store.ts` reduced over a recorded event-fixture stream produces the correct tree, findings, gauges, frontier, tech, and log — unit-tested with no browser.
2. **Handshake**: token-from-fragment + WS subprotocol + Bearer/Origin on REST all work against a running `smartbuster serve`; wrong token → visible auth failure, not a silent hang.
3. **Lossy handling**: `hit.coalesced` renders a badge not rows; replace-latest for stats/snapshot; a simulated WS drop → reconnect → `GET /api/scans/{id}` resync rebuilds state.
4. **Live render**: against a real scan on the `spa_with_api` fixture, the UI shows the SPA-pivot banner, the frontier reordering, provenance-tagged `crawl:js` findings in the tree, and the tech panel resolving.
5. **Controls**: pause/resume/stop/adjust/pin/exclude/inject each hit the right route with Bearer+Origin and reconcile against returned `ScanStatus`; `mode` is non-interactive.
6. **Build**: `make ui` produces the embedded bundle; the single `smartbuster serve` binary serves the UI with no external assets; `--open` launches it authenticated.

## 11. Build order & handoff

`connection.ts` + `store.ts` + fixtures (**test the data layer headless first**) → gauges + event log (simplest renderers) → discovered-path tree → **priority-frontier view** (the centerpiece — give it real attention) → tech panel → controls + manual override → launcher + session browser → provenance color system applied across all views → design pass with the `frontend-design` skill. Record an event-stream fixture from a real scan early (capture the WS stream to a file) so every component develops against realistic data.

**This completes Phase 5.** Report back any protocol gaps you hit (fields the UI needed that 5a doesn't emit — likely candidates: a per-finding size/status the `hit` event omits, or a scan-config echo for the header) so we can decide whether they're 5a follow-ups. **Next is Phase 6 (stealth)** — where `mode` finally becomes real (`fast`/`normal`/`quiet`/`stealth`), the TLS/HTTP-2 fingerprinting via `bogdanfinn/tls-client` lands, and the `TestCoordinator_ObservesConfiguredRate` statistical-rigor audit we deferred back in Phase 3 comes due, since the rate guarantee becomes a stealth guarantee.

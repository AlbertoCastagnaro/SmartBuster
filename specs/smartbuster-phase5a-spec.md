# smartbuster — Phase 5a Build Specification (Engine Daemon & Protocol)

*Build-ready spec for the local daemon, its REST control plane, WebSocket event streaming, the event-schema evolution, server security, and session save/resume. This is the protocol the Phase 5b web UI renders — so it must be nailed and validated before any frontend exists. Read alongside the 4b handoff (event inventory) and the implementation plan (Phase 5, §6 of the design).*

---

## 0. Integration contract — verify against committed code first

| # | Attachment point | Confirmed (4b handoff) | Phase 5a action |
|---|---|---|---|
| A | `Event{Type,Time,Dir,URL,Confidence,Message,Tech,WAF}`, emitted **only** on the coordinator goroutine | confirmed | Evolve it: add `Category` + `Payload json.RawMessage`; keep existing fields (back-compatible). §3 |
| B | `EventEmitter` interface; CLI impl; **not documented thread-safe** | confirmed | Add a `daemonEmitter` whose sink is a buffered channel to the WS **hub** (§2); coordinator still the only caller |
| C | Coordinator single-writer of frontier/tree/baselines/learners; mutation via channels (`seedInjectCh` pattern) | confirmed | Live control (pause/pin/…) enters via a **`controlCh`**, applied by the coordinator — never from an HTTP handler goroutine (§4) |
| D | Coordinator has counters (req sent, hits, in-flight, frontier len, elapsed) | confirmed | A ticker in the `select` loop emits `stats`; a sampler emits `frontier.snapshot` (§3) |
| E | `AuditRecord` path is the **lossless** per-request sink (separate from `Event`) | confirmed | Unchanged. The Event/WS stream is the **lossy** UI view; the two sinks stay distinct |
| F | `dispatchLoop` / pause-resume-cancel via `context` | confirmed | `stop` = cancel; `pause`/`resume` gate dispatch without cancelling (§4) |
| G | Session-relevant state: config, RNG seed, tree, frontier, baselines, learners (Markov/assoc/dirCtx), profile, visited-sets, counters | confirmed present across phases | Make each serializable for save/resume (§6) |

## 1. Scope

**In (5a):** `smartbuster serve` daemon (localhost HTTP + WS, `embed.FS` scaffold for later UI assets); REST control plane; WS event streaming with a non-blocking fan-out **hub**; event-schema evolution (`Category`, `Payload`, `stats`, `frontier.snapshot`, wired `branch.pruned`/`error`); server security (localhost bind, token auth, Origin validation, CSRF-safe-by-construction); session save/resume (+ CLI `--save`/`resume`).

**Out (→ 5b):** the web UI itself (framework, components, visualizations, session browser). 5a serves an empty asset mount + the protocol; 5b fills it.

**The two load-bearing concurrency primitives** (build + `-race` first, per the rhythm that's caught a bug every phase): the **hub** (coordinator → many WS clients, must never block the coordinator) and the **`controlCh`** (HTTP handlers → coordinator, preserving single-writer).

## 2. The daemon & the hub

`smartbuster serve [--port 0] [--open] [--bind 127.0.0.1]` starts an `net/http` server bound to loopback, serving REST + a WS endpoint, and (5b) embedded assets. It prints the URL **with the session token** and optionally opens a browser.

**Hub — the non-blocking fan-out (contract B):**
```
coordinator --emit(Event)--> hubIn (buffered chan, cap N) --> hub goroutine --> per-client send goroutines
```
- The coordinator's `daemonEmitter.Emit` does a **non-blocking send** to `hubIn` (buffered). If `hubIn` is full, it **drops/coalesces** (see below) — it must NEVER block, because blocking the emit blocks the coordinator, which stalls the scan. The lossless record is the audit log; the UI stream is explicitly lossy.
- The hub fans out to each connected client's bounded send buffer. A **slow client** that fills its buffer gets **lossy treatment for that client only**: `stats`/`frontier.snapshot` are *replace-latest* (keep only the newest — they're snapshots, staleness is pointless); `hit` under flood is *coalesced into a count* + the newest few; `warning`/`error`/`trap`/`spa.pivot`/lifecycle are *never dropped* (low-volume, important). One slow client never affects another and never affects the coordinator.
- Assert (test): a deliberately stalled WS client does not slow the scan and does not cause the audit log to miss anything.

## 3. Event schema evolution

Extend `Event` (back-compatible — existing fields stay):
```go
type Event struct {
	Type       string          `json:"type"`
	Category   string          `json:"category"`          // NEW: structural grouping
	Time       time.Time       `json:"time"`
	Dir        string          `json:"dir,omitempty"`
	URL        string          `json:"url,omitempty"`
	Confidence float64         `json:"confidence,omitempty"`
	Message    string          `json:"message,omitempty"`
	Tech       []TechEntry     `json:"tech,omitempty"`
	WAF        string          `json:"waf,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"` // NEW: typed struct for stats/snapshot/etc.
}
```

**`Category`** (decision #2 — kills prefix-parsing of warnings): every event carries one of `scan | calibration | discovery | tech | trap | telemetry | warning | error | control`. Warnings additionally carry a structured payload naming the source:
```go
type WarnPayload struct { Source string `json:"source"` } // "robots"|"sitemap"|"wayback"|"nmap"|"corpus"|"headless"|"seed.capped"|"spa"|"profile"
```
The human `Message` stays for display; the UI groups/filters on `Category`+`Source`, never on message prefixes.

**New event types (decision #1 — Phase 5a coordinator work):**
- `stats` (Category `telemetry`), on a ticker every `STATS_INTERVAL` (default 400 ms):
```go
type StatsPayload struct {
	ReqSent, Hits, InFlight, FrontierLen, DirsScanning int
	ReqPerSec, HitRate float64
	ElapsedMs, ETAms   int64   // ETA from remaining frontier / current rate; -1 if unbounded
}
```
- `frontier.snapshot` (Category `telemetry`), sampled every `SNAPSHOT_INTERVAL` (default 1 s), top-K by score:
```go
type SnapshotPayload struct {
	TopK []struct{ Path, Dir, Provenance string; Score float64; Depth int } // K default 25
	Total int
}
```
Both are emitted from the coordinator goroutine (single-writer-safe) and are *replace-latest* in the hub.

**Wire the two dead types (decision #3):**
- `branch.pruned` (Category `trap`): emit at the moment a branch is actually **cut** (novelty/self-similarity/tarpit/budget) — distinct from `trap.detected` (suspicion). The UI shows detection → pruning as two moments.
- `error` (Category `error`): emit on request errors (timeouts, conn resets, TLS failures) so the UI can show an error count/panel. `AuditRecord.Err` remains the lossless record; `error` is the stream signal. Payload: `{URL, Kind, Message}`.

Full type table for 5b is the 4b inventory **plus** `stats`, `frontier.snapshot`, now-live `branch.pruned`/`error`, each with `Category` and (where structured) `Payload`.

## 4. REST control plane

All state-changing control routes translate to a command on **`controlCh`**, applied by the coordinator in its `select` loop (contract C) — handlers never touch engine state directly.

| Method + path | Effect |
|---|---|
| `POST /api/scans` | start a scan (JSON = Config); returns `{id}` |
| `GET /api/scans` / `GET /api/scans/{id}` | list / status |
| `POST /api/scans/{id}/pause` \| `/resume` \| `/stop` | gate dispatch (drain in-flight) / resume / cancel (contract F) |
| `PATCH /api/scans/{id}` | live-adjust `{rate, concurrency, mode}` |
| `POST /api/scans/{id}/pin` \| `/exclude` \| `/boost` \| `/demote` | manual override on a path/pattern (§4.1) |
| `POST /api/scans/{id}/inject` | inject custom terms mid-scan (→ `enqueueSeed`, provenance `user`) |
| `GET /api/scans/{id}/events` | **WS upgrade** → the event stream |
| `POST /api/scans/{id}/save` / `GET /api/sessions` / `POST /api/sessions/{id}/resume` / `GET /api/sessions/{id}` | sessions (§6) |

**4.1 Manual override (the human outranks the engine).** `controlCh` commands mutating the frontier single-writer: `pin(pattern)` → force-try + top priority (even if not in corpus); `exclude(pattern)` → remove from frontier + add to a denylist checked at every enqueue; `boost/demote(pattern, factor)` → a persistent score multiplier applied in `scoreCandidate` (like SPA damping); `inject(terms)` → user seeds. Patterns are glob/prefix. All are also reachable from the CLI (same commands, same `controlCh`), so no capability lives only in the GUI.

## 5. Server security (the daemon can launch traffic — lock it down)

- **Loopback bind only** (`127.0.0.1`); refuse `--bind` to non-loopback without an explicit `--i-know-this-is-remote` flag (and even then, warn).
- **Per-session random token** (32 bytes, printed on `serve`, injected into the auto-opened URL as `#token=…`). Every REST + WS request must present it in an `Authorization: Bearer` header (WS: via the first message or a `Sec-WebSocket-Protocol` token, not a query string — avoids token-in-URL logging).
- **Origin validation** on the WS upgrade and every state-changing REST call: reject if `Origin` isn't the known loopback origin. This is the anti-**DNS-rebinding** defense — a malicious page in your browser resolving `127.0.0.1:port` still fails the Origin check.
- **CSRF-safe by construction:** auth is a **custom `Authorization` header**, never a cookie, so a cross-origin page can't ride ambient credentials. Origin check is the belt to that suspenders.
- Rationale to keep in the code comments: this daemon can initiate scans against arbitrary hosts; a drive-by page triggering one would be a serious incident. These four together close it.

## 6. Session save/resume

Serialize the full resumable state to a JSON session file (inspectable, matches the audit ethos): `{version, config, seed, tree, frontier(candidates+scores), baselines, learners(markov,assoc,dirCtx), profile, visitedSets, counters, wafState}`. 
- **Save**: `POST /save`, CLI `--save f.json`, optional periodic autosave (`--autosave 30s`).
- **Resume**: `POST /sessions/{id}/resume`, CLI `smartbuster resume f.json` → rebuild the coordinator from state and continue; **RNG seed restored** → reproducibility holds across a save/resume boundary.
- The tricky parts are the **frontier heap** and the **learners** — give each a serializable form (`MarshalState`/`LoadState`); assert **round-trip fidelity** (serialize→load→deep-equal on the reconstructable fields). Session ≠ audit: session is a resumable *snapshot*; audit is the append-only *record*. Note the overlap so their schemas evolve together.

## 7. Config & CLI additions

**As built, split across two places rather than one flat struct** — see §10's note on why: `Serve`/`Port`/`Open`/`Bind`/`ResumePath` describe how the *daemon or CLI subcommand* is invoked, not any one scan's parameters, so they live as `flag.FlagSet` locals in `cmd/smartbuster/main.go`'s `runServe`/`runResume` (plus `daemon.Options` for the `Start`-time subset) instead of on `engine.Config`. `SavePath`/`Autosave` genuinely are a scan's own parameters (the CLI `scan` and `resume` subcommands both take them) and do live on `engine.Config`:
```go
// internal/engine/config.go — Config
SavePath string        // --save; CLI-only (the daemon's save path is POST .../save's body, §4)
Autosave time.Duration // --autosave; default 30s once --save is set, else inert

// cmd/smartbuster/main.go — CLI flag locals, and internal/daemon.Options
Port, Bind, Open, AllowRemote  // `serve` subcommand flags / daemon.Options
ResumePath                     // `resume <file>` subcommand's positional arg
```
| Constant | Default | Where |
|---|---|---|
| `STATS_INTERVAL` | 400 ms | `internal/engine/config.go` (`StatsInterval`) |
| `SNAPSHOT_INTERVAL` | 1 s | `internal/engine/config.go` (`SnapshotInterval`) |
| `SNAPSHOT_TOPK` | 25 | `internal/engine/config.go` (`SnapshotTopK`) |
| `HUB_IN_CAP` | 1024 | `internal/daemon/hub.go` (`HubInCap`) |
| `CLIENT_BUF` | 256 | `internal/daemon/hub.go` (`ClientBufCap`) |

(Constants split by package rather than one table: `STATS_INTERVAL`/`SNAPSHOT_INTERVAL`/`SNAPSHOT_TOPK` are the coordinator's own dispatch-loop ticker/sampler parameters — `internal/engine` has no dependency on `internal/daemon` — while `HUB_IN_CAP`/`CLIENT_BUF` are hub-internal buffer sizes with no meaning outside it.)

## 8. Tests, fixtures, DoD

1. **Hub non-blocking**: a stalled WS client does **not** slow the scan (assert scan completion time ≈ unimpaired) and the audit log is **complete**; `stats`/`snapshot` are replace-latest for that client; lifecycle/warning/error events are never dropped.
2. **Single-writer control**: `pause` halts dispatch and in-flight drains; `resume` continues; `stop` cancels; `pin`/`exclude`/`boost`/`inject` mutate the frontier correctly — all via `controlCh`, `-race` clean with concurrent HTTP calls + a running scan.
3. **Schema**: `Category` populated on every event; `stats` fires on cadence; `frontier.snapshot` returns top-K; `branch.pruned` fires at actual pruning (distinct from `trap.detected`); `error` fires on a forced request error; warnings carry `WarnPayload.Source` (no prefix-parsing needed).
4. **Security**: missing/wrong token → 401 on REST and WS; cross-`Origin` upgrade → rejected (DNS-rebinding sim); state-changing REST without the Bearer header → rejected; non-loopback bind refused without the explicit flag.
5. **Sessions**: save → resume → scan continues; **same seed reproduces** the continuation; round-trip fidelity on frontier + learners (deep-equal); resume of a mid-scan session re-emits a coherent initial `stats`/`snapshot`.
6. `go build`, `go vet`, `go test -race ./...` clean; `smartbuster serve` prints URL+token, REST reachable via `curl` with the token, WS streams events to a test client.

## 9. Build order & handoff

`daemonEmitter` + **hub** (build + `-race` first against a stalled client) → **`controlCh`** + coordinator command handling (`-race` with concurrent control calls) → event-schema evolution (`Category`/`Payload`, `stats`/`snapshot` emitters, wire `branch.pruned`/`error`) → REST routes → security (token/Origin/bind) → sessions (serialize/resume + round-trip test) → `serve` CLI + empty `embed.FS` asset mount. Build and race-test the hub and `controlCh` **before** the REST layer — they're the load-bearing concurrency, same discipline that caught 4b's channel bugs.

**Before writing Phase 5b, report back:** the **frozen protocol** — the exact REST routes (request/response JSON) and the **complete WS event catalog** (every `type`, its `category`, and its `payload` struct) — plus how the token/Origin handshake works for a browser client, and any §0 deviation. 5b renders precisely this; the protocol is 5b's entire input, so it must be final before the UI is built against it.

**Handoff filed:** reported in the build session that shipped this phase (routes, event catalog, and handshake exactly as implemented — see §10 below for the deviations that report called out). `go build`, `go vet`, and `go test -race ./...` are clean across the whole repo; the hub and `controlCh` each have dedicated `-race` tests plus an integration test running a real scan through a real hub with a permanently-stalled client attached.

## 10. Implementation notes (as built)

- **The hub is per-scan, not one daemon-wide instance.** §2's diagram reads naturally as a single hub; in practice `GET /api/scans/{id}/events` needs to upgrade into *that scan's* event stream specifically; a daemon running several scans concurrently would otherwise have no way to route to one and not the others. `ScanManager.Start`/`Resume` each build a fresh `*daemon.Hub` (its own `Run` goroutine, its own client registry) bound to that one `*engine.Coordinator` via `hub.NewEmitter()`; the hub's `Run` context is cancelled when the coordinator's `Run` returns. `HubInCap`/`ClientBufCap` are still per-hub-instance, i.e. effectively per-scan, matching the spec's intent (bounding one scan's fan-out) even though there can be many hubs live in one daemon process.
- **Token-over-WS uses `Sec-WebSocket-Protocol`, not "the first message."** Spec §5 offered both; `Sec-WebSocket-Protocol` was chosen because it's checked during the handshake itself (`websocket.Server.Handshake`, before the 101 response) — symmetric with the Origin check, and a rejected token never even completes an upgrade rather than upgrading and then closing. Browser client contract: `new WebSocket(url, [token])` (the token as the WS sub-protocol); server-side, `internal/daemon/ws.go`'s `wsHandshake` validates `config.Protocol` (already parsed to one element by the time the handshake func runs) with a constant-time compare, alongside `checkOrigin`.
- **`hit.coalesced` is a new, hub-only synthetic event type**, not in the 4b inventory or §3's "new event types" list: `EventType = "hit.coalesced"`, `Category: discovery`, `Payload: {"count": N}`. Spec §2 says a hit flood "coalesces into a count + the newest few" but doesn't give that count a wire shape — a real one was needed to actually deliver it to a client. It is **never emitted by the coordinator**; only `internal/daemon/hub.go`'s per-client buffering synthesizes it, only for the one client whose hit buffer (`ClientBufCap`, 256) overflowed. 5b should treat it as informational ("N hits weren't shown individually to you specifically because your connection fell behind"), not as a discovery to render in a findings list.
- **`branch.pruned` fires at three concrete call sites**, corresponding to spec §3 decision #3's "novelty/self-similarity/tarpit/budget": (1) `handleHit`'s duplicate-content/novelty-gate branch (`coordinator.go`, fires once per aliased hit — same cardinality as the existing alias `hit` event it accompanies); (2) `checkTarpit`, the same instant `trap.detected` fires for a tarpit cap, so the two arrive back-to-back rather than with a gap (spec's "two moments" language is about them being distinct *types*, not necessarily separated in time — for tarpit specifically, detection and cutting are the same event); (3) `nextDispatchable`'s `PER_DIR_BUDGET`-exhausted discard, guarded by a new `dirState.budgetPruned` bool so it fires exactly once per directory instead of once per discarded candidate.
- **Deviation — `exclude`'s enforcement point is dispatch-time, not "every enqueue."** §4.1 says the denylist is "checked at every enqueue"; it's actually checked once, at `nextDispatchable`'s existing budget/scope discard site, plus an immediate `Frontier.RemoveMatching` sweep of whatever's already queued when the exclude command lands. Checking at every one of the half-dozen push call sites was tried first and rejected: `pushWordlistCandidates`/`pushCorpusCandidates` compute `candidatesTotal` from a slice length *before* pushing, so silently dropping a candidate at push time there — without also adjusting the total — strands the directory's `candidatesAccountedFor` short of `candidatesTotal` forever, hanging the scan. The dispatch-time gate is externally equivalent (an excluded candidate is never requested) without that hazard; reasoning is in `internal/engine/control.go`'s `isExcluded` doc comment.
- **Deviation — `PATCH .../{id}`'s `mode` field is accepted, stored, and echoed back in `ScanStatus.Mode`, but has no behavioral effect yet.** No stealth/aggressive tier concept exists in the engine before Phase 7 (the implementation plan's own stealth-mode section is explicitly future work); inventing behavior for it now would be speculative. `rate` and `concurrency` are fully live: `rate` calls `httpclient.Limiter.SetRate` directly; `concurrency` raises `Coordinator.concurrencyCap` (an atomic, checked at `dispatchLoop`'s sole dispatch point) and, if the new value exceeds how many `RunWorker` goroutines have been spawned so far, spawns the shortfall. Lowering `concurrency` never kills a goroutine — it just throttles future dispatch below the new cap; the existing idle workers simply have nothing to do until (if ever) the cap rises again.
- **`pin` reuses `boost`'s multiplicative override path** (`overrideMultiplier` in `control.go`) with a reserved constant, `PinScore = 1000.0` — comfortably above every other scoring tier (corpus priors ≈0–5, `nmapSeedScore` is 2.0) — rather than a separate absolute-priority mechanism. For a literal (non-glob) pattern, `pin` *additionally* force-inserts it via the ordinary `enqueueSeed` path (provenance `"user:pin"`) so "even if not in corpus" is actually satisfied, not just scored-if-it-happens-to-exist; a glob pattern only reweights matches, since there's no single concrete path to insert.
- **`stop` = cancel is implemented by having `Coordinator.Run` wrap its caller's `ctx` in an internal `context.WithCancel` child**, storing the `cancel` func on the coordinator. A `CtrlStop` command simply calls it. This reuses the exact shutdown path an external `ctx` cancellation already takes (workers unblock, `dispatchLoop` returns, `workCh` closes, `wg.Wait()` completes) rather than inventing a second "stopped" flag that would have needed its own draining logic to avoid a `resultsCh`-full deadlock against workers that are still mid-flight.
- **Session round-trip is not bit-exact everywhere — see the fidelity test's own scope.** `profile.TargetProfile.Tech`'s per-vote rule-id provenance and layer-vote tally are unexported fields that `encoding/json` silently drops; a resumed scan's tech/WAF/confidence values survive, but that audit trail doesn't. The coordinator's top-level RNG streams (`c.rng`, `c.epsilonRNG`) are re-seeded fresh from `Config.Seed` on resume rather than restored mid-stream — only per-directory probe-token generation (`dirRand`, a pure function of `(seed, dir)`, independent of any stream position) is exactly reproduced, which is what `TestSession_SaveResumeContinuesAndReproducesSeed` actually asserts on (same finding set as an uninterrupted run), matching §8 DoD #5's "same seed reproduces the continuation." A directory that was still `CALIBRATING` at save time restarts calibration from scratch on resume rather than round-tripping partial probe results (cheap — at most `N_PROBES * len(ExtSet)`, ~15 requests — and avoids serializing in-flight probe state).
- **Session storage on disk: one JSON file per session** under a directory (`--session-dir`, default a per-user config dir via `os.UserConfigDir()`), named `<id>.json`; a session's `id` in `GET/POST /api/sessions/...` is that filename's stem. Not literally specified by §6's text (which only names the wire schema); `internal/daemon/sessions.go`'s `SessionStore` is the concrete on-disk layout. `POST .../save`'s request body may set `{"name": "..."}` to choose the id explicitly; it defaults to the scan's own id.
- **WS transport is `golang.org/x/net/websocket`**, not `gorilla/websocket` or a hand-rolled implementation — it was already available as a subpackage of `golang.org/x/net`, an existing direct dependency, so no new entry was needed in `go.mod`/`go.sum`. Its `Server.Handshake` hook is what makes the token/Origin-before-101-response ordering straightforward.
- **Routing uses Go's standard-library `net/http.ServeMux`** (1.22+'s method+pattern+`{wildcard}` matching, e.g. `"POST /api/scans/{id}/pause"`, `r.PathValue("id")`) — no external router dependency, matching the rest of the project's minimal-dependency posture.

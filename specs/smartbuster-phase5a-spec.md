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
```go
Serve       bool          // `smartbuster serve`
Port        int           // default 0 = OS-assigned
Open        bool          // --open (launch browser)
Bind        string        // default 127.0.0.1
SavePath    string        // --save
Autosave    time.Duration // --autosave; 0 = off
ResumePath  string        // `smartbuster resume <file>`
```
| Constant | Default | Meaning |
|---|---|---|
| `STATS_INTERVAL` | 400 ms | `stats` heartbeat cadence |
| `SNAPSHOT_INTERVAL` | 1 s | `frontier.snapshot` cadence |
| `SNAPSHOT_TOPK` | 25 | candidates per snapshot |
| `HUB_IN_CAP` / `CLIENT_BUF` | 1024 / 256 | hub inbound + per-client buffers |

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

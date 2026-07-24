# smartbuster — Phase 6b Build Specification (TLS/HTTP-2 Fingerprint Mimicry + Proxies)

*Build-ready spec for tier-3 stealth: browser TLS + HTTP-2 fingerprint mimicry via `bogdanfinn/tls-client`, header-order fidelity, and proxy support (single/Tor + a lean rotation seam). Plugs into 6a's `HTTPDoer` seam behind the `stealth` preset. This completes Phase 6. Read alongside the 6a handoff and the stealth design discussion (tier-3).*

---

## 0. Integration contract — verify against committed code

| # | Attachment point | Confirmed (6a) | 6b action |
|---|---|---|---|
| A | `HTTPDoer{ Do(ctx, Request) (Response, error) }`; worker routes through it | confirmed | Add a `tls-client` impl behind it |
| B | `Preset.TLSProfile`/`Proxies`/`HTTP2FP` fields (present, unused) | confirmed | Activate them; `stealth` preset sets a browser profile |
| C | **A second concrete `httpClient`** serves profile/seed/harvest on-target requests, *outside* the `HTTPDoer` boundary | confirmed (6a report) | **THE key fix: route these through the fingerprint client too** — the fingerprint must be identical across every on-target request (§4) |
| D | 6a header profiles = values/set/identity only (order untouched) | confirmed | Add header-**order** fidelity (tls-client preserves order) — completes tier-2 (§3) |
| E | Global rate limiter / AIMD in `Limiter`; single-owner | confirmed | Proxy dialing sits under the same pacing; unchanged control model |
| F | Coordinator single-writer; requests built on its goroutine | confirmed | `--proxy` is a static single upstream set at client construction — no per-request selection, nothing new in the control model (§5) |

## 1. Scope

**In (6b):** `bogdanfinn/tls-client` behind `HTTPDoer`; coherent **BrowserProfile** bundles (TLS ClientHello / JA3-JA4 **+** HTTP-2 SETTINGS/pseudo-header order **+** header values **+** header order, all one browser); fingerprint applied **consistently across all on-target requests** (candidate, profiling, seed, harvest-fetch); a `--fingerprint` axis (orthogonal to timing mode; default-on in `stealth`); and a single **opt-in `--proxy` upstream** (http/https/socks5).

**Not built (deliberately):** no Tor flag/circuit rotation (Tor exit IPs are publicly enumerable and widely pre-blocked — free rotation to *known-bad* IPs, which fights the fingerprint), no proxy pool / round-robin / `ProxyProvider` abstraction, no health-checking. The honest model is "bring your own trusted proxy" — one string, empty by default → direct connection. Users who want a good egress IP (residential, corporate, or their own Tor SOCKS) point `--proxy` at it themselves.

## 2. The fingerprint client (`internal/httpclient/tlsclient.go`)

`bogdanfinn/tls-client` wraps `utls` (TLS ClientHello → JA3/JA4) **plus** a patched HTTP/2 stack (SETTINGS frame values/order, `WINDOW_UPDATE`, pseudo-header order, priority — the Akamai fingerprint). It ships browser profiles (`Chrome_120`, `Firefox_117`, `Safari_16_0`, …).

- Implement `HTTPDoer` via a `tls-client` instance selected by `Preset.TLSProfile`.
- **Coherence is the whole point:** a `BrowserProfile` is a *bundle* — Chrome TLS fingerprint **with** Chrome HTTP/2 settings **with** Chrome header values **with** Chrome header order. A Chrome JA3 wearing Firefox headers is itself a tell. So unify 6a's `HeaderProfile` and 6b's `TLSProfile` into one `BrowserProfile` key; selecting a profile sets all four layers together.
```go
type BrowserProfile struct {
	Name         string            // "chrome"|"firefox"|"safari"
	TLSClient    profiles.ClientProfile // bogdanfinn profile (TLS+HTTP2)
	Headers      []HeaderKV        // ordered (§3)
}
```

## 3. Header-order fidelity (completes 6a's deferred tier-2)

`net/http`'s header map couldn't preserve order (6a's scope note). `tls-client` can: supply the header list as an **ordered** slice per profile and let the client emit them in browser order (including `Host`/pseudo-header placement). This is the layer that turns "realistic header values" (6a) into "indistinguishable from the browser" (6b).

## 4. Consistency across ALL on-target requests (the load-bearing fix)

This is the contract-C fix and the thing most likely to be done wrong. In a fingerprinting-WAF session, **every** request to the target must present the *same* browser fingerprint — candidate requests (worker), the profiling fetch + favicon (2a), robots/sitemap fetches (4a), JS bundle + SPA-root harvest fetches (4b). If any of these still go through Go's stock `net/http`, the WAF sees one Chrome client and one Go client hitting the same host — worse than no mimicry.

- **Unify on `HTTPDoer`.** When fingerprinting is active, the coordinator's second concrete `httpClient` (profile/seed/harvest) is replaced by the same `tls-client`-backed `HTTPDoer`. Off-target requests (Wayback→archive.org CDX) are exempt — they're a different host and can stay on the plain client.
- Assert (test): with `--fingerprint chrome`, a captured candidate request and a captured profiling/harvest request present the **identical** JA3/JA4 + HTTP-2 fingerprint.

## 5. Proxy support (`internal/httpclient/proxy.go`)

One thing, opt-in: `--proxy <url>` (http/https/socks5), passed to `tls-client` at construction. Covers Burp passthrough (`http://127.0.0.1:8080`), a corporate/residential egress, or a manually-run Tor SOCKS (`socks5://127.0.0.1:9050`) — the user brings the IP they trust. Empty (default) → direct connection.

- No pool, no rotation, no `ProxyProvider` interface, no Tor control protocol. `Config.Proxy string` → the client's upstream. That's the entire feature.
- **Rationale (documented):** Tor exit nodes are a public, enumerable list that mature WAFs block by default, so automated Tor rotation is free rotation *to already-suspicious IPs* — it undercuts the fingerprint rather than helping. Real reputation evasion needs residential/mobile IPs, which are the user's to supply, not ours to build infrastructure for. "Bring your own proxy" is the ready-to-use answer.
- **Fingerprint stays stable regardless of proxy:** one browser identity per session; the proxy only changes the egress IP.

## 6. Fingerprint as an axis (not just a mode)

Fingerprinting is orthogonal to the timing modes: `--fingerprint <profile>` enables it independently, and the `stealth` preset turns it on by default (`TLSProfile="chrome"`). You can run `normal` timing with a browser fingerprint (look like a browser without going slow) or `stealth` without it (rare). `fast`/`normal`/`quiet` default to the plain `net/http` `HTTPDoer` unless `--fingerprint` is set; `stealth` defaults it on.

## 7. Config & defaults
```go
Fingerprint string   // --fingerprint "chrome"|"firefox"|"safari"|""(off); stealth preset sets "chrome"
Proxy       string   // --proxy <url> (http/https/socks5); "" = direct
```
| Default | Meaning |
|---|---|
| stealth `TLSProfile` | `chrome` (latest bundled) |
| proxy | none (direct) |

## 8. Tests & DoD

Fingerprint fidelity is genuinely hard to test hermetically — do it with a **local capture server**, not a live third party:
1. **TLS fidelity**: a local TLS server captures the received `ClientHello`, computes JA3/JA4, and asserts it matches the profile's expected value — and that it is **not** Go's default `net/http` JA3.
2. **HTTP-2 fidelity**: the same server records the HTTP/2 `SETTINGS` frame + pseudo-header order and asserts it matches the profile (the Akamai fingerprint).
3. **Header order**: captured request headers are in the profile's browser order (contract D).
4. **Consistency (§4)**: candidate + profiling + harvest requests to the capture server present **identical** fingerprints; Wayback→archive.org is exempt (plain client).
5. **Proxy**: `--proxy` routes through a local proxy (assert the proxy saw the request); no config → direct connection; the presented fingerprint is unchanged whether or not a proxy is set.
6. **Axis**: `--fingerprint` works in `normal` mode; `stealth` enables it by default; `fast` without it is unchanged.
7. `go build`, `go vet`, `go test -race ./...` clean. (Note: `bogdanfinn/tls-client` is pure-Go/cgo-free — confirm it doesn't break the single-static-binary story; if it does, gate it behind a build tag.)

## 9. Build order & handoff

`BrowserProfile` bundle (unify TLS+HTTP2+header-values+order) → `tls-client` `HTTPDoer` impl → **route ALL on-target requests through it when fingerprinting is active (contract C — the consistency fix, do this deliberately, it's the easy thing to miss)** → header-order fidelity → single `--proxy` upstream plumbing → preset/`--fingerprint` wiring → the local-capture fidelity tests. Build the capture server first so fidelity is measurable from the start.

**This completes Phase 6 — the full stealth stack.** Report back the `BrowserProfile`/`ProxyProvider` APIs and any deviation. **Next is Phase 7 (evaluation & hardening)** — the benchmark corpus, the discovery-curve/requests-to-coverage metrics vs. gobuster/ffuf/feroxbuster, the ablation harness (which, per §1 of every phase, has been accreting DoD efficiency numbers all along), the regression CI, and the reproducibility caveats we've been banking (ε-greedy under concurrency, RNG-on-resume) stated precisely. Two carry-ins for Phase 7 scoping: the **headless capability is still a stub** (annotate/exclude JS-execution-only targets from the corpus, or wire real playwright first), and the **Burp/markdown exports** land there (the `GET /findings` endpoint from the 5b patch is their accessor).

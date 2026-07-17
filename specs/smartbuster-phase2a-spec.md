# smartbuster — Phase 2a Build Specification (Target Profiling)

*Build-ready spec for the `TargetProfile` and the signals that populate it: technology detection + nmap ingestion. Consumed by Phase 2b (corpus & selection). Read alongside the implementation plan (Phase 2) and the Phase 1 spec.*

---

## 0. Integration contract — verify against committed Phase 1 code first

Phase 2a attaches to Phase 1 internals. **Before building, confirm each assumption below against the code as committed** (Claude Code may have deviated from the Phase 1 spec). Where reality differs, adjust 2a to the code, not the code to 2a.

| # | Attachment point | Phase 2a assumption | If different… |
|---|---|---|---|
| A | `Calibrate(dir, probes)` supplies extensions via a package const `EXT_SET` | 2a **parameterizes** it: `Calibrate(dir, exts []string, probes)`; caller passes `profile.ExtensionsForStack()` (falling back to the Phase 1 fixed set before the profile exists) | adapt the call site 2a threads extensions through |
| B | Coordinator has a single scan-start point (`run()` / `seedRootDirectory()`) before real dispatch | 2a inserts a `profileTarget()` step here, once per target | hook wherever the first request for a target originates |
| C | Phase 1 only issues random calibration probes; there is **no** real `GET /` fetch | 2a adds a **profile fetch** (real root GET + `/favicon.ico`) whose result carries full headers + a body sample | if a root fetch already exists, reuse it |
| D | `ResponseSignature` is compact and intentionally drops headers/body | 2a introduces a separate `ProfileResponse` (headers map + body sample) for the **few** profiling requests only; the hot path stays compact | keep regular requests compact regardless |
| E | Frontier `Score = BasePrio`; a `Reprioritize(fn)` hook exists but is unused | 2a **does not** score per-candidate by tech yet (candidates aren't tagged until 2b). 2a only (i) supplies extensions, (ii) seeds nmap/NSE paths as high-priority candidates, (iii) stores the profile for 2b to consume | if no hook exists, 2b adds it; 2a doesn't need it |
| F | `Config` struct exists and is threaded through | 2a extends it (ruleset path, nmap file, active-probe toggle, mode) | extend in place |
| G | An event emitter exists (`events.go`) | 2a adds `tech.detected` and `waf.detected` event types and reuses the emitter | reuse whatever emit API exists |
| H | Scope enforcer gates every candidate/target | 2a routes nmap multi-host targets and all seeds through it | reuse |

**Sequencing consequence of C+A:** at scan start the order is (1) profile fetch + fast passive signals → provisional profile, (2) derive extensions, (3) calibrate the root dir *using those extensions*, (4) slower signals (favicon, optional active probes, nmap merge) refine the profile, (5) the stored profile is available to 2b. Calibration must not run for the root until the provisional profile exists, so the right extensions are probed.

## 1. Scope

**In scope (2a)**
- `TargetProfile` data model and the profile store on the coordinator.
- Passive tech detection: headers, cookies, HTML, favicon hash, reuse of the calibration error-page fingerprint.
- `wappalyzergo` integration over the profile fetch.
- Confidence fusion; edge-vs-backend layering; WAF detection.
- Ruleset management: embedded default + external versioned dir + user overlay + `ruleset update` + category toggles + rule provenance.
- nmap `-oX` ingestion: tech tags, multi-port targets, port heuristics, NSE seeds, ssl-cert vhosts; ingest (default) + orchestrate (opt-in).
- `profile.ExtensionsForStack()` feeding calibration.

**Deferred to 2b:** tagged SQLite corpus, selection-as-query, per-candidate tech scoring, runtime `stem × ext` permutation of the wordlist. (2a produces the profile; 2b consumes its tags.)

## 2. Data model (`internal/profile/types.go`)

```go
package profile

type Layer int
const ( LayerBackend Layer = iota; LayerEdge; LayerUnknown )

type Source int
const ( SrcHeader Source = iota; SrcCookie; SrcHTML; SrcFavicon; SrcErrorPage; SrcWappalyzer; SrcActiveProbe; SrcNmap )

// Tech is one detected technology with fused confidence.
type Tech struct {
	Name       string    // "PHP", "WordPress", "nginx", "Apache Tomcat"
	Category   string    // "language","server","cms","framework","waf","proxy"
	Version    string    // "" if unknown
	Confidence float64   // [0,1], fused across sources
	Layer      Layer
	Sources    []Source  // provenance: every signal that voted for it
}

type ServiceTarget struct {
	BaseURL string        // "https://host:8443"
	Port    int
	Scheme  string
}

type TargetProfile struct {
	Host      string
	Services  []ServiceTarget          // >1 when nmap finds multiple web ports
	Tech      map[string]*Tech         // keyed by Name
	WAF       string                   // vendor name, "" if none
	IsSPA     bool                     // read from calibration's root baseline
	VHosts    []string                 // ssl-cert SANs etc. (candidates, not scanned yet)
}

// ExtensionsForStack returns the calibration/probe extension set for the
// detected backend, always including generic + backup extensions.
func (p *TargetProfile) ExtensionsForStack() []string
// MatchScore(tags) is defined here but USED in 2b: returns a [0,1] boost factor
// for a candidate carrying `tags`, given the profile's techs and confidences.
func (p *TargetProfile) MatchScore(tags []string) float64
```

`ExtensionsForStack` mapping (backend tech → extensions), always unioned with generic/backup:

| Backend | Extensions added |
|---|---|
| PHP | `.php .phtml .php5` |
| ASP.NET | `.aspx .asmx .ashx .asp` |
| Java/Tomcat | `.jsp .do .action` |
| (always) generic | `"" .html .txt .json` |
| (always) backup | `.bak .old .zip .tar.gz .swp .~` |

Extensions carry weights later (2b); in 2a the set is what calibration probes and what 2b will permute.

## 3. Profiling orchestration (`internal/profile/profile.go`)

```
profileTarget(target) *TargetProfile:
  p = &TargetProfile{Host: target.Host, Services: [rootService(target)]}
  # 3.1 profile fetch (real GET), via the worker pool, IsProbe=false, ProfileFetch=true
  root = profileFetch(target.BaseURL + "/")            # ProfileResponse: headers,cookies,body
  applyPassiveSignals(p, root)                          # §4.1-4.3 headers/cookies/html
  detectWAF(p, root)                                    # §5
  runWappalyzer(p, root)                                # §4.5

  emit("tech.detected", p.snapshot())                   # provisional; refined below

  # 3.2 slower/opt-in signals (do not block root calibration)
  go func:
    fav = profileFetch(target.BaseURL + "/favicon.ico")
    applyFaviconSignal(p, fav)                          # §4.4
    applyErrorPageSignal(p, rootBaselineOf(target))     # §4.6 reuse calibration
    if cfg.ActiveProbes: applyActiveProbes(p, target)   # §4.7
    emit("tech.detected", p.snapshot())

  # 3.3 nmap merge (if provided) happens once, before scan start (§7)
  return p
```

The **provisional** profile (3.1) is enough to derive `ExtensionsForStack()` for root calibration. Favicon/active/nmap refinements (3.2–3.3) update the stored profile in place; because per-candidate tech scoring is 2b, mid-scan refinement is safe (it changes 2b's future boosts, not past decisions).

## 4. Signal sources

Each source calls `p.vote(name, category, layer, source, confidence, version)`, which upserts a `Tech` and fuses confidence (§4.8).

### 4.1 Headers (`SrcHeader`)
Parse `Server`, `X-Powered-By`, `X-AspNet-Version`, `X-Generator`, `X-Drupal-Cache`, `Via`, `X-Served-By`. Regex→tech table. `Server`/`Via` and CDN markers lean `LayerEdge`; `X-Powered-By`/`X-AspNet` lean `LayerBackend`. Confidence 0.7 (headers are often spoofed/stripped).

### 4.2 Cookies (`SrcCookie`) — high signal, backend-leaking
Session-cookie **names** map to backend tech; these usually pass through proxies, so `LayerBackend`, confidence 0.85:

| Cookie name | Tech |
|---|---|
| `PHPSESSID` | PHP |
| `JSESSIONID` | Java |
| `ASP.NET_SessionId`, `.ASPXAUTH` | ASP.NET |
| `laravel_session`, `XSRF-TOKEN`+`laravel_session` | Laravel (PHP) |
| `csrftoken`+`sessionid` | Django (Python) |
| `connect.sid` | Express (Node) |
| `wordpress_*`, `wp-settings-*` | WordPress (PHP) |

### 4.3 HTML (`SrcHTML`)
Over the profile-fetch body: `<meta name="generator">`, script/link `src`/`href` path patterns (`/wp-content/`,`/wp-includes/`→WordPress; `/_next/`→Next.js; `/sites/default/files/`→Drupal; `/media/jui/`→Joomla), and comment markers. Confidence 0.75.

### 4.4 Favicon (`SrcFavicon`)
`GET /favicon.ico` → mmh3 (32-bit, the Shodan convention: base64-encode body then hash) → lookup in the favicon-hash table (part of the ruleset). Exact match → confidence 0.9. Miss → no vote.

### 4.5 wappalyzergo (`SrcWappalyzer`)
Feed the profile fetch (final URL, headers, cookies, body) to `wappalyzergo`. It returns tech names + categories using its bundled fingerprints. Map its categories to ours; confidence 0.8 for its matches. This provides breadth; §4.1–4.4 provide the high-value pentest-specific depth.

### 4.6 Error-page fingerprint (`SrcErrorPage`) — free, reused from calibration
The root `Baseline` already captured a normalized representative not-found page. Match it against known framework error templates (Tomcat page, ASP.NET yellow-screen, Werkzeug debugger, Laravel Ignition, default Apache/nginx 404). Match → confidence 0.85, `LayerBackend`. **No extra request** — this consumes calibration's output.

### 4.7 Active known-path probes (`SrcActiveProbe`) — opt-in, confirmation only
Only if `cfg.ActiveProbes` AND a candidate tech sits in `[0.4,0.75)` confidence (worth confirming). Fire the specific confirmer: WordPress→`/wp-login.php`, Joomla→`/administrator/`, Spring Boot→`/actuator`, Drupal→`/user/login`, `.git`→`/.git/HEAD`, .NET→`/web.config`. A non-baseline response confirms → raise to 0.95. All go through scope + rate limiter.

### 4.8 Confidence fusion
Per tech, fuse source confidences with **noisy-OR**: `conf = 1 - Π(1 - cᵢ)`. Cap 0.99. Record all voting sources in `Tech.Sources`. Layer is decided by majority of source-implied layers, ties → `LayerUnknown`.

### 4.9 Reverse-proxy / CDN layering
If an edge marker is present (`Server: cloudflare`, `Via`, `X-Served-By`, CDN cookies), keep edge techs in the profile but **derive `ExtensionsForStack` and 2b's selection from `LayerBackend` techs only**. When headers are scrubbed by the edge, backend detection falls through to §4.2 (cookies), §4.3 (body), §4.6 (error page) — the signals a proxy doesn't rewrite.

## 5. WAF detection (`internal/profile/waf.go`)
A lightweight wafw00f-style matcher over the profile fetch (and any observed block page): signatures for Cloudflare (`cf-ray`, `__cfduid`/`cf_clearance`, `Server: cloudflare`), Akamai (`akamai`, `AkamaiGHost`), Imperva/Incapsula (`incap_ses`, `visid_incap`), AWS WAF (`x-amzn-*`, challenge body), Sucuri (`x-sucuri-id`). Set `p.WAF`, emit `waf.detected`. **Feeds two subsystems:** calibration (a WAF challenge page can masquerade as a hit — calibration's WAF-onset guard should key off `p.WAF != ""`), and Phase 6 stealth (turn on mimicry).

## 6. Ruleset management (`internal/profile/ruleset.go`)
- **Format:** JSON files — `headers.json`, `cookies.json`, `html.json`, `favicons.json`, `errorpages.json`, `active_probes.json`, `waf.json`. Each entry: `{ "tech","category","layer","pattern"|"hash","confidence" }`.
- **Layered load (later overrides earlier):** embedded default snapshot (`embed.FS`, so the binary works offline) < system ruleset dir < user overlay dir (`~/.config/smartbuster/rules/`). User rules override by `(source,tech,pattern)` key; nothing in the vendored set is edited.
- **`smartbuster ruleset update`:** git-pull a pinned commit of the chosen upstream fork (enthec/webappanalyzer or the wappalyzergo signature set) into the system dir; record the commit in a `ruleset.lock` for reproducibility.
- **Category toggles:** `--rules-off analytics,marketing` (default off for those, since pentest cares about server/framework/cms/language/waf).
- **Provenance:** every vote records the rule id; surfaced in `tech.detected` and the audit log.
- Note: `wappalyzergo` carries its **own** bundled fingerprints; this ruleset is the smartbuster-specific overlay (favicon hashes, error-page templates, pentest confirmers) plus the header/cookie tables above.

## 7. nmap ingestion (`internal/profile/nmap.go`)
- **Input:** `--nmap scan.xml` (nmap `-oX`). Parse with `encoding/xml` into host→ports→service/script structs.
- **Extraction & merge into the profile:**
  - Web services (`service.name ∈ {http,https,http-proxy,http-alt,https-alt}`) → `ServiceTarget`s (multi-port scanning).
  - `-sV` version (`service.product`+`version`, e.g. "Apache Tomcat" / "nginx") → `vote(..., SrcNmap, conf)` where **conf follows nmap's own `conf` and `method`**: `method="probe"` & `conf>=8` → 0.9; `method="table"` (port-only guess) → 0.4.
  - Port heuristics (only when no `-sV`): 8080→Tomcat/proxy, 8443→alt-HTTPS, 3000→Node, 5000→Flask, 8000→Django, 9200→Elasticsearch, 9000→PHP-FPM/SonarQube. conf 0.35.
  - NSE outputs: `http-enum` paths → **frontier seeds** (high priority, `Provenance:"nmap:http-enum"`, validated by calibration like any candidate); `http-robots.txt` → seeds; `http-server-header`/`http-title` → tech; `http-favicon` → favicon id; `ssl-cert` SANs → `p.VHosts` (vhost candidates, not auto-scanned in 2a).
- **Ingest vs orchestrate:** ingest is default. `--run-nmap` (opt-in) shells out to run `nmap -sV --script http-enum,http-headers,ssl-cert -oX -`; requires nmap on PATH; note privilege caveats. 
- **Scope + multi-host:** an nmap file may span a subnet; **every** derived target passes the scope enforcer; out-of-scope hosts are dropped with a warning.
- **Seed vs truth:** nmap tech is a *prior* (confirmed/overridden by live signals §4); nmap paths are *seeds* (confirmed by calibration). Never reported as findings without classification.

## 8. Config additions (`config.go`)
```go
RulesetDir   string        // default: OS config dir; embedded snapshot as fallback
UserRulesDir string        // default: ~/.config/smartbuster/rules
NmapFile     string        // --nmap
RunNmap      bool          // --run-nmap (opt-in orchestrate)
ActiveProbes bool          // default false (passive-only unless asked)
FaviconProbe bool          // default true
RulesOff     []string      // default: ["analytics","marketing","tracking"]
```
| Constant | Default | Meaning |
|---|---|---|
| `PROFILE_FETCH_TIMEOUT` | 8s | per profiling request |
| `ACTIVE_PROBE_CONF_LO/HI` | 0.4 / 0.75 | confidence band that triggers a confirmer |
| `NMAP_PROBE_CONF` / `NMAP_TABLE_CONF` | 0.9 / 0.4 | version-probe vs port-table confidence |
| favicon hash algo | mmh3-32 (base64 body) | Shodan convention |

## 9. Tests, fixtures, DoD

**Fixtures (`test/fixtures/profile_server.go` + `test/fixtures/nmap/*.xml`):**
| Fixture | Presents | Expected |
|---|---|---|
| `php_apache` | `Server: Apache`, `PHPSESSID`, `.php` links | backend=PHP high-conf; exts include `.php` |
| `wordpress` | `/wp-content/` links, `wordpress_*` cookie, `/wp-login.php` | WordPress+PHP; active probe (if on) confirms |
| `dotnet_iis` | `Server: IIS`, `X-AspNet-Version`, `.ASPXAUTH` | ASP.NET; exts include `.aspx` |
| `behind_cdn` | `Server: cloudflare`+`cf-ray`, backend `PHPSESSID` leaks | edge=Cloudflare, backend=PHP separated; WAF set; exts from backend |
| `spa_react` | identical shell (as Phase 1 SPA) | `IsSPA` read from baseline; profile still built from headers/JS |
| `favicon_known` | serves a known-hash favicon | favicon vote fires at 0.9 |
| `waf_challenge` | Cloudflare challenge page | `waf.detected`; not miscounted as tech-only |
| `nmap_basic.xml` | `-sV` Apache/PHP + `http-enum` paths | tech tags + path seeds + service target |
| `nmap_multiport.xml` | 80 + 8443 web services | two `ServiceTarget`s |

**Assertions / DoD:**
1. On framework fixtures: correct backend tech at high confidence; `ExtensionsForStack()` returns the right set (verified feeding into root calibration).
2. `behind_cdn`: edge/backend layers separated; selection/extension source is backend; WAF recorded.
3. Confidence fusion is noisy-OR (two 0.7 sources → ~0.91), provenance lists all sources.
4. nmap XML → correct tags (with nmap-conf-scaled confidence), path seeds enter the frontier and are calibration-validated, multi-port yields multiple targets, out-of-scope hosts dropped.
5. Ruleset: embedded default works offline; user overlay overrides a vendored rule; `--rules-off` suppresses a category; `ruleset update` writes a lock; rule provenance surfaces in `tech.detected`.
6. `--active-probes=false` sends **zero** confirmer requests (passive-only honored); `=true` fires only for techs in the confidence band.
7. `go build ./...` and `go test -race ./...` clean.
8. Integration: a scan against `php_apache` calibrates the root with PHP extensions (i.e., 2a actually changed calibration's probe set vs. Phase 1's fixed one).

## 10. Build order & handoff
`profile/types` → `ruleset` (load/embed/overlay) → signal matchers (`headers`,`cookies`,`html`,`favicon`,`errorpage`,`waf`) → `wappalyzergo` adapter → `nmap` → `profile.go` orchestration → engine wiring (profile-fetch path in worker per contract C/D; `Calibrate` extension param per A; scan-start hook per B; new events per G). Write the fixture servers + nmap XML fixtures and the matcher table-tests **before** the engine wiring, so detection is validated standalone (same discipline as Phase 1's calibration).

**Before writing Phase 2b, report back:** the final `TargetProfile` API as built (esp. `ExtensionsForStack` and `MatchScore` signatures), whether the frontier `Reprioritize`/scoring hook exists and its shape, and any deviation from contract §0 — 2b's corpus selection and per-candidate scoring bind directly to those.

// harvest.go is the coordinator-orchestration counterpart of package
// internal/harvest (mirroring seed.go's role for package internal/seed):
// that package holds pure extraction logic with no Coordinator dependency,
// this file wires it into the scan loop as mid-scan seedInjectCh producers
// (spec §3, §4, §5) plus the SPA pivot (spec §4).
package engine

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/harvest"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/seed"
)

// visitedSet is the crawl visited-set (spec §3): guards against re-fetching
// the same JS bundle/page URL discovered from multiple pages. It's touched
// by producer goroutines concurrently (never the coordinator's own
// frontier/tree/baselines state), so it owns its own mutex rather than
// relying on single-writer discipline the way the rest of the coordinator
// does.
type visitedSet struct {
	mu   sync.Mutex
	seen map[string]bool
}

// markVisited reports whether url is newly seen, marking it visited either
// way.
func (v *visitedSet) markVisited(url string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen[url] {
		return false
	}
	v.seen[url] = true
	return true
}

// snapshot/restore round-trip the visited set (Phase 5a session save/
// resume, spec §6's "visitedSets"): called only while no harvest producer
// goroutine is running concurrently — buildSnapshot runs on the coordinator
// goroutine via controlCh (contract C), and restoreSnapshot runs before
// Run() ever spawns one — so the mutex here is a guard-rail, not something
// either path strictly needs.
func (v *visitedSet) snapshot() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]string, 0, len(v.seen))
	for k := range v.seen {
		out = append(out, k)
	}
	return out
}

func (v *visitedSet) restore(keys []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, k := range keys {
		v.seen[k] = true
	}
}

// harvestResponse is handleReal's Phase 4b hook (spec §2, §3): a retained
// body gets handed to a tracked background producer goroutine for parsing —
// never parsed inline, since LinkFinder-style regex over a multi-MiB bundle
// is far too expensive for the coordinator's own loop. Probes never reach
// here (handleResult routes IsProbe to collectProbe before handleReal is
// ever called), so only genuine candidate/harvest-fetch responses are
// crawled.
func (c *Coordinator) harvestResponse(pageURL string, sig ResponseSignature) {
	body := sig.HarvestBody
	switch sig.ContentType {
	case "text/html":
		if !c.config.Crawl && !c.config.JSHarvest {
			return
		}
		if !c.crawlVisited.markVisited(pageURL) {
			return
		}
		c.spawnHarvest(func() { c.harvestHTML(pageURL, body) })
	case "application/javascript", "application/json":
		if !c.config.JSHarvest {
			return
		}
		if !c.crawlVisited.markVisited(pageURL) {
			return
		}
		c.spawnHarvest(func() { c.harvestJSBody(pageURL, body) })
	}
}

// harvestHTML is the HTML link crawler (spec §3), always run off the
// coordinator goroutine (spawnHarvest). It never re-fetches pageURL itself —
// body is the one already retained off a response the scan made for some
// other reason — so crawling genuinely rides for free on responses the scan
// already produced.
func (c *Coordinator) harvestHTML(pageURL string, body []byte) {
	links, scripts := harvest.ExtractHTML(body, pageURL)

	if c.config.Crawl {
		var raws []seed.RawSeed
		for _, l := range links {
			if p, ok := c.resolveSameHost(l); ok {
				raws = append(raws, seed.RawSeed{Path: p, Source: "crawl:html"})
			}
		}
		if len(raws) > 0 {
			c.sendSeedBatch(seed.Normalize(raws, seed.NormalizeOptions{}))
		}
	}

	if c.config.JSHarvest {
		for _, s := range scripts {
			if _, ok := c.resolveSameHost(s); ok {
				c.requestHarvestFetch(s)
			}
		}
	}
}

// harvestJSBody is the JS endpoint harvester's extraction half (spec §4),
// always run off the coordinator goroutine (spawnHarvest): mine already-
// fetched JS/JSON source for endpoint-shaped paths and turn survivors into
// crawl:js seeds. Absolute (leading '/') paths resolve against the target's
// own root — the SPA's own API, not wherever the bundle happens to be
// hosted; relative paths resolve against the bundle's own URL.
func (c *Coordinator) harvestJSBody(scriptURL string, body []byte) {
	paths := harvest.ExtractJSPaths(body)
	if len(paths) == 0 {
		return
	}
	base, err := url.Parse(scriptURL)
	if err != nil {
		return
	}

	var raws []seed.RawSeed
	for _, p := range paths {
		var abs string
		if strings.HasPrefix(p, "/") {
			abs = c.target + p
		} else {
			u, err := base.Parse(p)
			if err != nil {
				continue
			}
			abs = u.String()
		}
		if rp, ok := c.resolveSameHost(abs); ok {
			raws = append(raws, seed.RawSeed{Path: rp, Source: "crawl:js"})
		}
	}
	if len(raws) > 0 {
		c.sendSeedBatch(seed.Normalize(raws, seed.NormalizeOptions{}))
	}
}

// resolveSameHost parses raw, keeps it only if its host matches the
// target's own (spec §3: "keep only same-host") and it clears the general
// scope enforcer (contract E, spec §0 contract D), and returns its path
// stripped of query/fragment (path buster, spec §3).
func (c *Coordinator) resolveSameHost(raw string) (path string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host != c.targetHost {
		return "", false
	}
	p := u.Path
	if p == "" || p == "/" {
		return "", false
	}
	if !c.scope.InScope(c.target + p) {
		return "", false
	}
	return p, true
}

// requestHarvestFetch is a producer goroutine's half of the harvest-fetch
// handoff (spec §4, §5): hand a URL worth fetching (a JS bundle) to the
// coordinator, which alone dispatches it through the paced/scoped pipeline
// (enqueueHarvestFetch). Selecting on runCtx.Done() keeps a producer from
// leaking past scan cancellation if the coordinator stops draining.
func (c *Coordinator) requestHarvestFetch(rawURL string) {
	select {
	case c.harvestFetchCh <- rawURL:
	case <-c.runCtx.Done():
	}
}

// enqueueHarvestFetch is requestHarvestFetch's coordinator-side landing
// point (spec §4, §5) — the dispatchLoop select case for harvestFetchCh
// does nothing but call this — and harvestRoot's direct call (already on
// the coordinator goroutine, so it skips the channel entirely rather than
// deadlocking a synchronous send back to itself). Scope + visited-set dedup
// happen here, once, regardless of which path reached it.
func (c *Coordinator) enqueueHarvestFetch(rawURL string) {
	if !c.scope.InScope(rawURL) {
		return
	}
	if !c.crawlVisited.markVisited(rawURL) {
		return
	}
	c.harvestFetchQueue = append(c.harvestFetchQueue, WorkItem{
		URL: rawURL, IsHarvestFetch: true,
		Headers: httpclient.BuildHeaders(c.headerProfile.Load(), ""), // no referer chain for harvest fetches (spec §5 only covers ordinary candidates)
	})
}

// handleHarvestFetchResult is handleResult's branch for a completed harvest
// fetch (spec §4, §5): audit it like any other request, then — off the
// coordinator goroutine — mine it as HTML or JS/JSON by whatever
// Content-Type it actually came back as (a JS-bundle fetch and the
// SPA-pivot root fetch share this one path; only the URL requested differs).
func (c *Coordinator) handleHarvestFetchResult(res WorkResult) {
	c.auditSink.WriteRequest(AuditRecord{
		Time: time.Now(), Method: "GET", URL: res.Item.URL, Provenance: "harvest-fetch",
		Signature: res.Signature, Err: res.Err,
	})
	if res.Err != nil {
		c.emit(Event{Type: EventError, URL: res.Item.URL, Message: res.Err.Error(),
			Payload: payloadFor(ErrorPayload{URL: res.Item.URL, Kind: classifyRequestErr(res.Err), Message: res.Err.Error()})})
		return
	}
	if res.Signature.HarvestBody == nil {
		return
	}
	fetchedURL, body := res.Item.URL, res.Signature.HarvestBody
	switch res.Signature.ContentType {
	case "text/html":
		c.spawnHarvest(func() { c.harvestHTML(fetchedURL, body) })
	case "application/javascript", "application/json":
		c.spawnHarvest(func() { c.harvestJSBody(fetchedURL, body) })
	}
}

// spaPivot is the SPA pivot (spec §4): fired once, from finishCalibration,
// when root calibrates as an SPA (baseline.IsSPA). Brute-force is futile
// against an identical shell for every path, so weight shifts to harvesting
// instead of stopping — deprioritizeBruteForce scales scores down rather
// than purging candidates (a generic term surviving is still useful), and
// harvestRoot fetches the root page so its script bundles get mined even
// though no ordinary candidate ever requests "/" itself.
func (c *Coordinator) spaPivot() {
	c.emit(Event{Type: EventSPAPivot, URL: c.target})
	c.deprioritizeBruteForce()
	if c.config.Crawl || c.config.JSHarvest {
		c.harvestRoot()
	}
}

// deprioritizeBruteForce sets spaMode (consulted by scoreCandidate for the
// rest of the scan, spec §4) and rescores whatever's already queued so the
// effect is immediate, not just for future pushes.
func (c *Coordinator) deprioritizeBruteForce() {
	c.spaMode = true
	c.frontier.Reprioritize(c.applyScore)
}

// seedHeadlessAsync is the opt-in headless tier (spec §6): kicked off at
// scan start, off the critical path exactly like async Wayback, since a
// real browser navigation is heavy and slow. Captured URLs land via
// seedInjectCh like any other producer. A missing/failed driver degrades
// gracefully — a warning, no headless seeds, the rest of the scan
// unaffected — never delaying or aborting the scan that's already running.
func (c *Coordinator) seedHeadlessAsync(ctx context.Context) {
	if !c.config.Headless {
		return
	}
	c.spawnHarvest(func() {
		runner, err := harvest.NewPlaywrightRunner()
		if err != nil {
			c.sendWarning("headless", "headless: "+err.Error())
			return
		}
		urls, err := runner.Capture(ctx, c.target+"/", nil)
		if err != nil {
			c.sendWarning("headless", "headless: "+err.Error())
			return
		}
		var raws []seed.RawSeed
		for _, u := range urls {
			if p, ok := c.resolveSameHost(u); ok {
				raws = append(raws, seed.RawSeed{Path: p, Source: "headless"})
			}
		}
		if len(raws) > 0 {
			c.sendSeedBatch(seed.Normalize(raws, seed.NormalizeOptions{}))
		}
	})
}

// harvestRoot requests the root page through the same paced harvest-fetch
// pipeline every other fetch uses (spec §4: "fetch root HTML -> all
// script[src] -> JS harvest"). Called synchronously from spaPivot, which
// already runs on the coordinator goroutine (via finishCalibration), so it
// calls enqueueHarvestFetch directly rather than round-tripping through
// harvestFetchCh — a synchronous channel send back to the same goroutine
// that would need to receive it would deadlock.
func (c *Coordinator) harvestRoot() {
	c.enqueueHarvestFetch(c.target + "/")
}

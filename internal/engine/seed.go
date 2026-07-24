// seed.go is the coordinator-orchestration counterpart of package
// internal/seed (mirroring dynamic.go's role for scorer.go/markov.go/
// assoc.go): that package holds pure fetch/parse/normalize logic with no
// Coordinator dependency, this file wires it into the scan loop (spec §2,
// §4).
package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/harvest"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/seed"
)

// seedPassiveSync runs Phase 4a's on-target passive-seeding sources —
// robots.txt and sitemap.xml — normalized/deduped/prior-tiered together and
// then handed to enqueueSeed one at a time. Synchronous on the coordinator
// goroutine, like profileTarget — called from Run() right after seedRoot(),
// so root's dirState already exists and every seed's ancestor-chain
// materialization (ensureDirChain, §4) has somewhere to land.
//
// Wayback is deliberately NOT here (spec §5, contract H): it's an
// off-target archive.org query that can take seconds-to-minutes under its
// own polite rate limit, and robots/sitemap are "two fast on-target GETs —
// the wrong thing to make async" per the same contract. See
// seedWaybackAsync.
func (c *Coordinator) seedPassiveSync(ctx context.Context) {
	var raws []seed.RawSeed

	if c.config.Robots {
		res, err := seed.FetchRobots(ctx, c.target, c.seedOpts())
		if err != nil {
			c.emitWarning("robots", "robots: "+err.Error())
		}
		raws = append(raws, res.Seeds...)
		if c.config.Sitemap {
			seeds, err := c.fetchSitemapSeeds(ctx, res.Sitemaps)
			if err != nil {
				c.emitWarning("sitemap", "sitemap: "+err.Error())
			}
			raws = append(raws, seeds...)
		}
	} else if c.config.Sitemap {
		seeds, err := c.fetchSitemapSeeds(ctx, nil)
		if err != nil {
			c.emitWarning("sitemap", "sitemap: "+err.Error())
		}
		raws = append(raws, seeds...)
	}

	if len(raws) == 0 {
		return
	}
	for _, sd := range seed.Normalize(raws, seed.NormalizeOptions{SeedAssets: c.config.SeedAssets}) {
		c.enqueueSeed(sd)
	}
}

// seedWaybackAsync kicks off the CDX fetch in its own goroutine and returns
// immediately (spec §5, contract H): the scan starts dispatching real
// candidates off robots/sitemap/corpus right away rather than stalling on
// archive.org latency. The result — one big batch of often-deep, often-dead
// dirs — lands via seedInjectCh like any other producer once it's ready;
// MAX_NEW_DIRS_PER_BATCH (capBatch) is this batch shape's safety belt,
// paired with this move and never separated (contract I). A fetch failure
// degrades gracefully: a warning batch, no seeds, no delay ever imposed on
// the scan that was already underway.
func (c *Coordinator) seedWaybackAsync(ctx context.Context) {
	if !c.config.Wayback {
		return
	}
	c.spawnHarvest(func() {
		w := &seed.Wayback{BaseURL: c.config.WaybackURL, Max: c.config.WaybackMax, Pace: c.archivePace}
		raws, err := w.Fetch(ctx, c.profileState.Host)
		if err != nil {
			c.sendWarning("wayback", "wayback: "+err.Error())
			return
		}
		if len(raws) == 0 {
			return
		}
		c.sendSeedBatch(seed.Normalize(raws, seed.NormalizeOptions{SeedAssets: c.config.SeedAssets}))
	})
}

func (c *Coordinator) fetchSitemapSeeds(ctx context.Context, extraSitemaps []string) ([]seed.RawSeed, error) {
	return seed.FetchSitemaps(ctx, c.target, extraSitemaps, seed.SitemapOptions{
		Options: c.seedOpts(), Host: c.profileState.Host,
	})
}

func (c *Coordinator) seedOpts() seed.Options {
	return seed.Options{Client: c.httpClient, InScope: c.scope.InScope, Pace: c.paceProfileRequest}
}

// archivePace waits one archivePacer tick — the *separate* archive.org
// limiter (spec §5.3) — before a Wayback CDX request, mirroring
// paceProfileRequest's use of c.pacer for the target.
func (c *Coordinator) archivePace() {
	<-c.archivePacer.C()
	c.archivePacer.Advance()
}

// enqueueSeed is the frontier entry point for every seeding source, passive
// (spec §0 contract A) or active (Phase 4b, §5): a full, possibly deep path
// becomes a Candidate under its immediate parent, with every missing
// ancestor directory materialized first via ensureDirChain (§4) — the
// non-obvious part — so the frontier can extend into a subtree the
// wordlist/corpus would never reach on its own. Both applySeedBatch (4b's
// mid-scan injection) and seedPassiveSync (4a's scan-start seeding) funnel
// through this one function.
func (c *Coordinator) enqueueSeed(sd seed.Seed) {
	path := strings.Trim(sd.Path, "/")
	if path == "" {
		return
	}
	segments := strings.Split(path, "/")
	maxDepth := c.config.MaxDepth
	if isHarvestProvenance(sd.Provenance) {
		maxDepth = c.crawlDepth // spec §7 CrawlDepth: crawl/JS/headless seeds get their own depth cap, default = MaxDepth
	}
	if len(segments) > maxDepth {
		return // bounded by MAX_DEPTH / CrawlDepth (spec §4, §7)
	}
	if !c.scope.InScope(c.target + "/" + path) {
		return // spec §0 contract D
	}

	ancestors := seedAncestorDirs(path)
	dir := ""
	if n := len(ancestors); n > 0 {
		dir = ancestors[n-1]
	}
	c.ensureDirChain(ancestors, sd.BasePrio, sd.Provenance)

	typ := TypeFile
	if sd.IsDirHint {
		typ = TypeDir
	}
	c.stageDirCandidate(dir, Candidate{
		Path: segments[len(segments)-1], Type: typ, BasePrio: sd.BasePrio,
		Tags: []string{"generic"}, Depth: len(segments), ParentDir: dir, Provenance: sd.Provenance,
	})
}

// seedAncestorDirs returns the ordered ancestor directory chain a seed's
// path passes through (spec §4), e.g. "/old/admin/secret.php" ->
// ["/old", "/old/admin"] — everything ensureDirChain must materialize
// before the seed's own leaf candidate can land. Also used by capBatch
// (spec §5) to count a batch's blast radius before any dir is touched, so
// the two stay in exact agreement about what "one new directory" means.
func seedAncestorDirs(trimmedPath string) []string {
	segments := strings.Split(trimmedPath, "/")
	dir := ""
	var ancestors []string
	for _, seg := range segments[:len(segments)-1] {
		dir += "/" + seg
		ancestors = append(ancestors, dir)
	}
	return ancestors
}

// isHarvestProvenance reports whether a seed/candidate's provenance names an
// active-harvest source (spec §4 SPA pivot, §7 CrawlDepth): crawl:html,
// crawl:js, or headless. Provenance strings are "+"-unioned (spec §3), so a
// substring check is correct even after a merge.
func isHarvestProvenance(provenance string) bool {
	return strings.Contains(provenance, "crawl:") || strings.Contains(provenance, "headless")
}

// ensureDirChain materializes every ancestor directory a deep seed passes
// through (spec §4): each missing ancestor is registered exactly as a live
// hit's own recursion would (enqueueChildDirectory — idempotent, a no-op if
// the dir already exists), so its calibration starts immediately rather than
// waiting on the ancestor above it to first be confirmed a hit. Each
// ancestor is *also* pushed as an ordinary TypeDir candidate under its own
// parent, so it gets requested and classified like any other candidate (and
// shows up as a genuine Finding in its own right if it classifies as a hit).
// Neither of those is gated on the other: a stale ancestor's own calibrated
// baseline still governs whether anything beneath it — including the seed's
// real leaf — ever classifies as a hit. That's what makes stale-seed
// pruning automatic (spec §4): if the whole /old prefix is fake, requests
// under it will diverge from its own baseline no differently than any other
// wrong guess, so nothing beneath it is ever confirmed.
func (c *Coordinator) ensureDirChain(dirs []string, prio float64, provenance string) {
	parent := ""
	for i, d := range dirs {
		c.enqueueChildDirectory(d, i+1, parent)
		name := strings.TrimPrefix(d, parent+"/")
		c.stageDirCandidate(parent, Candidate{
			Path: name, Type: TypeDir, BasePrio: prio, Tags: []string{"generic"},
			Depth: i + 1, ParentDir: parent, Provenance: provenance,
		})
		parent = d
	}
}

// stageDirCandidate is enqueueSeed/ensureDirChain's per-directory landing
// point (spec §0 contract A): if dir's baseline already exists (SCANNING),
// it merges into an already-queued candidate at the same path if one
// exists, or otherwise falls through to enqueueGenerated's ordinary
// dedup+immediate-scoring path (mergeOrEnqueueGenerated, spec §0 contract B
// — this is 4a's flagged first-writer-wins gap, now fixed: every mid-scan
// seed source, crawler/JS/Wayback/headless alike, routes through
// applySeedBatch -> enqueueSeed -> here). Otherwise it holds cand in
// ds.pendingSeeds until pushCandidates folds it into the directory's
// template once calibration finishes (spec §4) — the only way a
// still-materializing ancestor (or root, mid-calibration, when the seed
// stage itself runs) can receive a seed at all.
//
// A dir already marked DONE is reopened, but only if cand turns out to be a
// genuinely new candidate (spec §5: a harvest producer runs concurrently
// with the rest of the scan, so a small directory's own wordlist/corpus
// candidates can easily finish — candidatesAccountedFor reaching
// candidatesTotal — before that producer's goroutine ever gets scheduled).
// A DONE dir, by construction, can never still have a queued candidate at
// some other path (accountedFor >= candidatesTotal means everything
// dispatched or discarded already), so mergeOrEnqueueGenerated's only two
// possible outcomes here are "moot" (path already known, nothing queued to
// merge into — leave DONE alone) or "genuinely new" (enqueueGenerated
// succeeds, bumping candidatesTotal/budget — reopen, since there's now a
// legitimately unaccounted candidate that needs its own dispatch+result
// before maybeFinishDir can correctly re-mark it DONE). Reopening
// unconditionally here was an earlier bug: a moot merge changes nothing,
// so nothing would ever re-trigger maybeFinishDir, leaving the dir stuck
// in SCANNING with an empty queue forever — pendingHarvest is what keeps
// the whole scan alive long enough for a genuine reopen to resolve.
func (c *Coordinator) stageDirCandidate(dir string, cand Candidate) {
	ds := c.dirs[dir]
	if ds == nil {
		return // dir never existed — nothing to reopen or land cand in
	}
	if ds.state == dirDone {
		// Tentatively reopen before calling mergeOrEnqueueGenerated:
		// enqueueGenerated itself refuses to add anything while
		// ds.state == dirDone (its own guard against adding to a finished
		// dir on the ordinary hit-driven path), so the add must be allowed
		// to proceed as SCANNING first — then revert if it turns out moot.
		ds.state = dirScanning
		if !c.mergeOrEnqueueGenerated(dir, cand) {
			ds.state = dirDone
		}
		return
	}
	if ds.state == dirScanning {
		c.mergeOrEnqueueGenerated(dir, cand)
		return
	}
	if ds.pendingSeeds == nil {
		ds.pendingSeeds = make(map[string]Candidate)
	}
	if existing, ok := ds.pendingSeeds[cand.Path]; ok {
		if cand.BasePrio > existing.BasePrio {
			existing.BasePrio = cand.BasePrio
		}
		existing.Provenance = seed.UnionProvenance(existing.Provenance, cand.Provenance)
		ds.pendingSeeds[cand.Path] = existing
		return
	}
	ds.pendingSeeds[cand.Path] = cand
}

// mergeOrEnqueueGenerated is stageDirCandidate's SCANNING/DONE landing
// point (spec §0 contract B, the flagged 4a gap this fixes): a late-arriving
// seed whose path dir already knows about merges into the matching frontier
// candidate in place — union Provenance, max BasePrio, rescore — instead of
// enqueueGenerated's ordinary first-writer-wins dedup silently dropping it.
// "In the corpus and linked from the app and in Wayback" becomes the strong,
// honestly-attributed signal it should be (spec §5). If the path was already
// dispatched (no longer queued in the frontier), there's nothing left to
// merge into — moot, not an error — and this returns false, same as a
// no-op merge; only a genuinely new candidate (enqueueGenerated) returns
// true, which is what stageDirCandidate's DONE branch needs to decide
// whether reopening the directory is warranted.
func (c *Coordinator) mergeOrEnqueueGenerated(dir string, cand Candidate) bool {
	ds := c.dirs[dir]
	if ds != nil && ds.knownPaths != nil && ds.knownPaths[cand.Path] {
		c.frontier.UpdateMatching(dir, cand.Path, func(existing *Candidate) {
			if cand.BasePrio > existing.BasePrio {
				existing.BasePrio = cand.BasePrio
			}
			existing.Provenance = seed.UnionProvenance(existing.Provenance, cand.Provenance)
			c.applyScore(existing)
		})
		return false
	}
	return c.enqueueGenerated(dir, cand)
}

// SeedBatch is one mid-scan seed injection sent over seedInjectCh (spec §5):
// a producer's collected seeds, applied by the coordinator — the sole
// reader/writer of the frontier — as its single-writer invariant requires.
// Warning carries a producer-side error/notice to surface as an
// EventWarning; it's routed through here rather than emitted directly by
// the producer goroutine so every c.emit() call, like every frontier
// mutation, stays exclusively on the coordinator goroutine.
type SeedBatch struct {
	Seeds      []seed.Seed
	Warning    string
	WarnSource string // WarnPayload.Source for Warning (spec §3 decision #2); set by whichever producer called sendWarning
}

// applySeedBatch is seedInjectCh's sole consumer (spec §5) — the coordinator
// select loop's case for it does nothing but call this. It caps the batch's
// blast radius (capBatch, contract I) and then merges or enqueues each
// surviving seed via the same enqueueSeed path every seed source shares;
// the stronger SCANNING-branch merge now lives in stageDirCandidate itself
// (mergeOrEnqueueGenerated above), so no separate merge logic belongs here.
func (c *Coordinator) applySeedBatch(batch SeedBatch) {
	if batch.Warning != "" {
		c.emitWarning(batch.WarnSource, batch.Warning)
	}
	for _, sd := range c.capBatch(batch.Seeds) {
		c.enqueueSeed(sd)
	}
}

// capBatch enforces MAX_NEW_DIRS_PER_BATCH (spec §5, contract I): seeds are
// considered highest-BasePrio first, and the batch is truncated at the
// first point where materializing their ancestor chains would introduce
// more than the cap's worth of directories brand-new to the tree. This is a
// hard blast-radius ceiling, not a tuning knob — pre-calibration there's no
// way to know which of a large batch's directories are dead, so this bounds
// how many a single batch may introduce at once, generously. Async Wayback
// (seedWaybackAsync) is the batch shape this exists for; paired with moving
// that fetch off the critical path and never separated from it (contract I).
func (c *Coordinator) capBatch(seeds []seed.Seed) []seed.Seed {
	if len(seeds) == 0 {
		return nil
	}
	sorted := append([]seed.Seed(nil), seeds...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].BasePrio > sorted[j].BasePrio })

	newDirs := make(map[string]bool)
	for i, sd := range sorted {
		for _, d := range seedAncestorDirs(strings.Trim(sd.Path, "/")) {
			if _, exists := c.dirs[d]; !exists {
				newDirs[d] = true
			}
		}
		if len(newDirs) > harvest.MaxNewDirsPerBatch {
			c.emitWarning("seed.capped", fmt.Sprintf(
				"seed.capped: batch truncated to %d/%d seeds (MAX_NEW_DIRS_PER_BATCH=%d)", i, len(sorted), harvest.MaxNewDirsPerBatch))
			return sorted[:i]
		}
	}
	return sorted
}

// sendSeedBatch and sendWarning are the producer-side half of seedInjectCh
// (spec §5): every harvest producer goroutine (crawler, JS harvester, async
// Wayback, headless) calls these — never c.emit or c.frontier directly —
// to hand results back to the coordinator, which is the only goroutine that
// ever applies them (applySeedBatch). Both block on the channel send but
// select on the scan's own ctx so a producer never leaks past scan
// cancellation.
func (c *Coordinator) sendSeedBatch(seeds []seed.Seed) {
	if len(seeds) == 0 {
		return
	}
	c.injectBatch(SeedBatch{Seeds: seeds})
}

func (c *Coordinator) sendWarning(source, msg string) {
	c.injectBatch(SeedBatch{Warning: msg, WarnSource: source})
}

func (c *Coordinator) injectBatch(batch SeedBatch) {
	select {
	case c.seedInjectCh <- batch:
	case <-c.runCtx.Done():
	}
}

// spawnHarvest launches fn as a tracked background producer goroutine
// (spec §5, §9): pendingHarvest is incremented before the goroutine starts
// and decremented when it returns, so dispatchLoop's termination check
// (frontier empty, nothing in flight) also waits for any in-progress
// harvest work — parsing a retained body, fetching a JS bundle, an async
// Wayback query — to either deliver its batch or give up, rather than
// ending the scan out from under it.
func (c *Coordinator) spawnHarvest(fn func()) {
	c.pendingHarvest.Add(1)
	go func() {
		defer c.pendingHarvest.Add(-1)
		fn()
	}()
}

// mergeSeedCandidate looks up pending for cand.Path (spec §3: "a seed also
// present in the corpus becomes one candidate with unioned provenance and
// max prior") and, if found, upgrades cand in place and removes it from
// pending so the caller's leftover loop doesn't push it a second time.
func mergeSeedCandidate(pending map[string]Candidate, cand *Candidate) {
	sd, ok := pending[cand.Path]
	if !ok {
		return
	}
	if sd.BasePrio > cand.BasePrio {
		cand.BasePrio = sd.BasePrio
	}
	cand.Provenance = seed.UnionProvenance(cand.Provenance, sd.Provenance)
	delete(pending, cand.Path)
}

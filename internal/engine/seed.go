// seed.go is the coordinator-orchestration counterpart of package
// internal/seed (mirroring dynamic.go's role for scorer.go/markov.go/
// assoc.go): that package holds pure fetch/parse/normalize logic with no
// Coordinator dependency, this file wires it into the scan loop (spec §2,
// §4).
package engine

import (
	"context"
	"strings"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/seed"
)

// seedPassive runs the Phase 4a passive-seeding stage (spec §2): robots.txt,
// sitemap.xml, and (opt-in) Wayback/CDX, normalized/deduped/prior-tiered
// together and then handed to enqueueSeed one at a time. Synchronous on the
// coordinator goroutine, like profileTarget — called from Run() right after
// seedRoot(), so root's dirState already exists and every seed's
// ancestor-chain materialization (ensureDirChain, §4) has somewhere to land.
func (c *Coordinator) seedPassive(ctx context.Context) {
	var raws []seed.RawSeed

	if c.config.Robots {
		res, err := seed.FetchRobots(ctx, c.target, c.seedOpts())
		if err != nil {
			c.emit(Event{Type: EventWarning, Message: "robots: " + err.Error()})
		}
		raws = append(raws, res.Seeds...)
		if c.config.Sitemap {
			seeds, err := c.fetchSitemapSeeds(ctx, res.Sitemaps)
			if err != nil {
				c.emit(Event{Type: EventWarning, Message: "sitemap: " + err.Error()})
			}
			raws = append(raws, seeds...)
		}
	} else if c.config.Sitemap {
		seeds, err := c.fetchSitemapSeeds(ctx, nil)
		if err != nil {
			c.emit(Event{Type: EventWarning, Message: "sitemap: " + err.Error()})
		}
		raws = append(raws, seeds...)
	}

	if c.config.Wayback {
		w := &seed.Wayback{BaseURL: c.config.WaybackURL, Max: c.config.WaybackMax, Pace: c.archivePace}
		seeds, err := w.Fetch(ctx, c.profileState.Host)
		if err != nil {
			c.emit(Event{Type: EventWarning, Message: "wayback: " + err.Error()})
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

func (c *Coordinator) fetchSitemapSeeds(ctx context.Context, extraSitemaps []string) ([]seed.RawSeed, error) {
	return seed.FetchSitemaps(ctx, c.target, extraSitemaps, seed.SitemapOptions{
		Options: c.seedOpts(), Host: c.profileState.Host,
	})
}

func (c *Coordinator) seedOpts() seed.Options {
	return seed.Options{Client: c.client, InScope: c.scope.InScope, Pace: c.paceProfileRequest}
}

// archivePace waits one archivePacer tick — the *separate* archive.org
// limiter (spec §5.3) — before a Wayback CDX request, mirroring
// paceProfileRequest's use of c.pacer for the target.
func (c *Coordinator) archivePace() {
	<-c.archivePacer.C()
	c.archivePacer.Advance()
}

// enqueueSeed is Phase 4a's frontier entry point for every passive-seeding
// source (spec §0 contract A): a full, possibly deep path becomes a
// Candidate under its immediate parent, with every missing ancestor
// directory materialized first via ensureDirChain (§4) — the non-obvious
// part — so the frontier can extend into a subtree the wordlist/corpus
// would never reach on its own.
func (c *Coordinator) enqueueSeed(sd seed.Seed) {
	path := strings.Trim(sd.Path, "/")
	if path == "" {
		return
	}
	segments := strings.Split(path, "/")
	if len(segments) > c.config.MaxDepth {
		return // bounded by MAX_DEPTH (spec §4)
	}
	if !c.scope.InScope(c.target + "/" + path) {
		return // spec §0 contract D
	}

	dir := ""
	var ancestors []string
	for _, seg := range segments[:len(segments)-1] {
		dir += "/" + seg
		ancestors = append(ancestors, dir)
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
// it reuses enqueueGenerated's dedup+immediate-scoring path directly;
// otherwise it holds cand in ds.pendingSeeds until pushCandidates folds it
// into the directory's template once calibration finishes (spec §4) — the
// only way a still-materializing ancestor (or root, mid-calibration, when
// the seed stage itself runs) can receive a seed at all.
//
// Known gap for Phase 4b to revisit: the SCANNING branch dedups by
// ds.knownPaths like enqueueGenerated always has — first writer wins, no
// retroactive BasePrio/provenance merge if a path was already pushed. That
// merge only happens for the (4a-exercised) pending-seed path above, via
// mergeSeedCandidate. 4b calls enqueueSeed against live SCANNING/DONE
// directories (harvested links arrive mid-scan), so it should decide then
// whether the stronger merge is worth the extra bookkeeping.
func (c *Coordinator) stageDirCandidate(dir string, cand Candidate) {
	ds := c.dirs[dir]
	if ds == nil || ds.state == dirDone {
		return // too late — dir vanished or finished already (only reachable via 4b-style late seeding)
	}
	if ds.state == dirScanning {
		c.enqueueGenerated(dir, cand)
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

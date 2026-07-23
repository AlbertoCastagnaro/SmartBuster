package engine

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/calibration"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/corpus"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
)

// PreviewRequestsCorpus is PreviewRequests' spec §0 contract G counterpart
// for a scan that doesn't pass -w: the corpus-selected+expanded order. Since
// dry-run sends no requests, it can't run the real target-profiling fetch
// that would drive tech-boosted ordering — the preview reflects the
// profile-less (generic+backup only) order a scan would start from before
// any signal is known.
func PreviewRequestsCorpus(target string, cfg Config, seed int64) ([]string, error) {
	target = strings.TrimRight(target, "/")
	rng := dirRand(seed, "")

	db, err := openCorpusDBFor(cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	techBoostW := cfg.TechBoostW
	if techBoostW <= 0 {
		techBoostW = corpus.DefaultTechBoostW
	}
	p := &profile.TargetProfile{}
	cands, err := corpus.Select(p, corpus.SelectConfig{DB: db, MaxCandidates: cfg.CorpusMax, TechBoostW: techBoostW})
	if err != nil {
		return nil, err
	}
	expanded := corpus.Expand(cands, p, techBoostW)
	// The real scan dispatches from a Score-ordered max-heap (Frontier), not
	// push order — sort here too so the preview matches actual dispatch
	// order.
	sort.SliceStable(expanded, func(i, j int) bool { return expanded[i].Score > expanded[j].Score })

	urls := make([]string, 0, len(calibration.ExtSet)*calibration.NProbes+len(expanded))
	for _, ext := range calibration.ExtSet {
		for i := 0; i < calibration.NProbes; i++ {
			urls = append(urls, target+"/"+randToken(rng, 12)+ext)
		}
	}
	for _, c := range expanded {
		urls = append(urls, target+"/"+c.Path)
	}
	return urls, nil
}

func openCorpusDBFor(cfg Config) (*sql.DB, error) {
	if cfg.CorpusDB == "" {
		return corpus.Default()
	}
	return corpus.Open(cfg.CorpusDB)
}

// loadCorpusTemplate builds c.corpusTemplate once, right after the
// provisional profile exists (spec §0 sequencing: "2b corpus.Select
// (provisionalProfile) seeds the frontier"), unless -w was given (spec §0
// contract G: -w bypasses the corpus). The template is reused verbatim for
// every directory's push (root and recursion children alike, spec §7's
// "inherit the corpus term set for their directory"), mirroring how
// c.wordlist is loaded once and reused for every directory today — true
// per-subtree Select() scoping is a Phase 3 concern, not 2b's.
//
// Failures here (missing/corrupt corpus DB) are non-fatal: they're
// surfaced as a warning event and the scan falls back to having no
// candidates from this source, rather than aborting a scan that could
// still proceed with, say, nmap-seeded paths.
func (c *Coordinator) loadCorpusTemplate() {
	if len(c.wordlist) > 0 {
		return
	}
	db, err := c.openCorpusDB()
	if err != nil {
		c.emitWarning("corpus", "corpus: "+err.Error())
		return
	}
	defer db.Close()

	cands, err := corpus.Select(c.profileState, corpus.SelectConfig{
		DB: db, MaxCandidates: c.config.CorpusMax, TechBoostW: c.techBoostW,
	})
	if err != nil {
		c.emitWarning("corpus", "corpus: "+err.Error())
		return
	}
	c.corpusTemplate = corpus.Expand(cands, c.profileState, c.techBoostW)
}

// openCorpusDB opens the configured prebuilt corpus DB, or builds the
// embedded zero-setup one when none is configured (spec §2, §8).
func (c *Coordinator) openCorpusDB() (*sql.DB, error) {
	return openCorpusDBFor(c.config)
}

// pushCorpusCandidates pushes c.corpusTemplate for dir once its baseline
// exists (spec §7), scoring each against the coordinator's current profile
// state at push time. Root additionally gets any nmap-seeded paths (spec
// §7 of Phase 2a), exactly as pushWordlistCandidates does.
func (c *Coordinator) pushCorpusCandidates(dir string, ds *dirState) {
	pending := ds.pendingSeeds
	ds.pendingSeeds = nil

	ds.knownPaths = make(map[string]bool, len(c.corpusTemplate)+len(pending))
	for _, tc := range c.corpusTemplate {
		cand := Candidate{
			Path:       tc.Path,
			Type:       CandidateType(tc.Type), // corpus.TermType shares engine.CandidateType's int encoding (spec §2 DDL comment)
			BasePrio:   tc.BasePrio,
			Tags:       tc.Tags,
			Depth:      ds.depth + 1,
			ParentDir:  dir,
			Provenance: tc.Provenance,
		}
		mergeSeedCandidate(pending, &cand) // spec §3: a seed at this path upgrades it in place (max prio, unioned provenance)
		cand.Score = c.scoreCandidate(cand)
		ds.knownPaths[tc.Path] = true
		c.frontier.Push(cand)
	}
	// Whatever's left in pending after the loop above didn't match any
	// template entry — a genuinely new path the seed named (the deep-seed
	// case, spec §4).
	leftoverSeeds := len(pending)
	for path, cand := range pending {
		cand.Score = c.scoreCandidate(cand)
		ds.knownPaths[path] = true
		c.frontier.Push(cand)
	}

	ds.candidatesTotal = len(c.corpusTemplate) + leftoverSeeds
	if dir == "" {
		ds.candidatesTotal += len(c.nmapSeeds)
	}
	if ds.candidatesTotal == 0 {
		ds.state = dirDone
		return
	}
	ds.budget = ds.candidatesTotal
	if c.config.PerDirBudget > 0 {
		ds.budget = c.config.PerDirBudget + leftoverSeeds
		if dir == "" {
			ds.budget += len(c.nmapSeeds)
		}
	}

	if dir == "" {
		for _, ns := range c.nmapSeeds {
			ds.knownPaths[ns.Path] = true
			c.frontier.Push(Candidate{
				Path:       ns.Path,
				Type:       TypeFullPath,
				BasePrio:   1.0,
				Score:      nmapSeedScore,
				Depth:      ds.depth + 1,
				ParentDir:  dir,
				Provenance: ns.Provenance,
			})
		}
	}
}

// scoreCandidate is spec §0 contract B's final-score formula: the static
// prior (corpus.Score) times the Phase 3 dynamic layer's multiplicative
// Boost. cand must already have ParentDir/Tags/Path set — Boost reads them.
// c.scorer is nil only before profileTarget has run, which is earlier than
// any candidate is ever scored, but the nil check keeps this safe to call
// unconditionally from any push path.
func (c *Coordinator) scoreCandidate(cand Candidate) float64 {
	score := corpus.Score(cand.BasePrio, cand.Tags, c.profileState, c.techBoostW)
	if c.scorer != nil {
		score *= c.scorer.Boost(&cand)
	}
	// spec §4 SPA pivot: once root calibrates as an SPA, brute-force
	// candidates are scaled down (not purged) in favor of harvested ones —
	// reorder-not-exclude, so a generic brute-force term is still reachable.
	if c.spaMode && !isHarvestProvenance(cand.Provenance) {
		score *= SPABruteForceScoreDown
	}
	score *= c.overrideMultiplier(&cand) // spec §4.1: pin/boost/demote
	return score
}

// applyScore is the Frontier.Reprioritize callback (spec §0 contract B,
// renamed from 2b's applyMatchScore): recomputes Score as the static prior
// times the current dynamic Boost, so resident candidates re-rank after
// profile refinement or a qualifying discovery without needing a second
// Select().
func (c *Coordinator) applyScore(cand *Candidate) {
	cand.Score = c.scoreCandidate(*cand)
}

// reprioritizeIfChanged fires Frontier.Reprioritize(applyScore) at spec §7's
// profile-change trigger — once the provisional profile is finalized, and
// whenever RefineAfterCalibration mutates it — guarded against thrash so an
// unchanged profile is a no-op. This is unthrottled (profile changes happen
// at most a couple of times per scan); the dynamic-context-dirty trigger
// (spec §6, markScorerDirty/runDynamicReprio in dynamic.go) is the one that
// needs throttling.
func (c *Coordinator) reprioritizeIfChanged() {
	if c.profileState == nil {
		return
	}
	sig := profileSignature(c.profileState)
	if sig == c.lastReprioSig {
		return
	}
	c.lastReprioSig = sig
	c.frontier.Reprioritize(c.applyScore)
}

// profileSignature is a deterministic summary of p's detected techs and
// confidences, used only to detect whether reprioritizeIfChanged's second
// call is redundant.
func profileSignature(p *profile.TargetProfile) string {
	names := make([]string, 0, len(p.Tech))
	for n := range p.Tech {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "%s:%.4f;", n, p.Tech[n].Confidence)
	}
	return b.String()
}

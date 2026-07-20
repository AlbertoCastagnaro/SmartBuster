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
		c.emit(Event{Type: EventWarning, Message: "corpus: " + err.Error()})
		return
	}
	defer db.Close()

	cands, err := corpus.Select(c.profileState, corpus.SelectConfig{
		DB: db, MaxCandidates: c.config.CorpusMax, TechBoostW: c.techBoostW,
	})
	if err != nil {
		c.emit(Event{Type: EventWarning, Message: "corpus: " + err.Error()})
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
	ds.candidatesTotal = len(c.corpusTemplate)
	if dir == "" {
		ds.candidatesTotal += len(c.nmapSeeds)
	}
	if ds.candidatesTotal == 0 {
		ds.state = dirDone
		return
	}
	ds.budget = ds.candidatesTotal
	if c.config.PerDirBudget > 0 {
		ds.budget = c.config.PerDirBudget
		if dir == "" {
			ds.budget += len(c.nmapSeeds)
		}
	}

	for _, tc := range c.corpusTemplate {
		c.frontier.Push(Candidate{
			Path:       tc.Path,
			Type:       CandidateType(tc.Type), // corpus.TermType shares engine.CandidateType's int encoding (spec §2 DDL comment)
			BasePrio:   tc.BasePrio,
			Score:      corpus.Score(tc.BasePrio, tc.Tags, c.profileState, c.techBoostW),
			Tags:       tc.Tags,
			Depth:      ds.depth + 1,
			ParentDir:  dir,
			Provenance: tc.Provenance,
		})
	}

	if dir == "" {
		for _, seed := range c.nmapSeeds {
			c.frontier.Push(Candidate{
				Path:       seed.Path,
				Type:       TypeFullPath,
				BasePrio:   1.0,
				Score:      nmapSeedScore,
				Depth:      ds.depth + 1,
				ParentDir:  dir,
				Provenance: seed.Provenance,
			})
		}
	}
}

// applyMatchScore is the Frontier.Reprioritize callback (spec §7): it
// recomputes Score with the shared corpus.Score formula against the
// coordinator's current profile state, so resident candidates re-rank
// after profile refinement without needing a second Select().
func (c *Coordinator) applyMatchScore(cand *Candidate) {
	cand.Score = corpus.Score(cand.BasePrio, cand.Tags, c.profileState, c.techBoostW)
}

// reprioritizeIfChanged fires Frontier.Reprioritize(applyMatchScore) at
// spec §7's two trigger points — once the provisional profile is
// finalized, and whenever RefineAfterCalibration mutates it — guarded
// against thrash so an unchanged profile is a no-op (spec §7).
func (c *Coordinator) reprioritizeIfChanged() {
	if c.profileState == nil {
		return
	}
	sig := profileSignature(c.profileState)
	if sig == c.lastReprioSig {
		return
	}
	c.lastReprioSig = sig
	c.frontier.Reprioritize(c.applyMatchScore)
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

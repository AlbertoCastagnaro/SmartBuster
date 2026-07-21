package engine

import (
	"strings"
	"sync"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
)

// DirContext accumulates response-semantics flags for one directory (spec
// §3.1) — the highest value/effort signal in Phase 3, built entirely from
// classification the coordinator already computes, no model needed.
type DirContext struct {
	SawProtected bool // a 401/403 was classified as a hit in this dir
	SawIndexOf   bool // a 200 + "Index of" listing was found at this dir
	HitCount     int
}

// sensitiveTerms are the tags/keywords spec §3.1 names as implying a locked
// neighbor is worth trying: admin, private, internal, config, backup,
// secret, .git, .env, api.
var sensitiveTerms = map[string]bool{
	"admin": true, "private": true, "internal": true, "config": true,
	"backup": true, "secret": true, ".git": true, ".env": true, "api": true,
}

// isSensitiveCandidate reports whether c carries a sensitive tag or its
// terminal path segment contains a sensitive keyword (spec §3.1: "carries a
// sensitive tag ... or matches a sensitive term list").
func isSensitiveCandidate(c *Candidate) bool {
	for _, t := range c.Tags {
		if sensitiveTerms[strings.ToLower(t)] {
			return true
		}
	}
	seg := strings.ToLower(terminalSegment(c.Path))
	for term := range sensitiveTerms {
		if strings.Contains(seg, term) {
			return true
		}
	}
	return false
}

// terminalSegment returns the last '/'-delimited path segment. Candidate.Path
// is already a single segment relative to ParentDir in this codebase's model,
// so this is mostly identity — kept explicit because the signals below are
// specified in terms of "the terminal path segment" (spec §3.1, §3.3).
func terminalSegment(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// extOf returns a terminal segment's extension (including the leading dot),
// "" if it has none.
func extOf(path string) string {
	seg := terminalSegment(path)
	if i := strings.LastIndexByte(seg, '.'); i >= 0 {
		return seg[i:]
	}
	return ""
}

// DynamicScorer is the Phase 3 dynamic layer (spec §2): stateful, per-scan
// signals that combine multiplicatively with the static corpus.Score prior.
// It is coordinator-owned, single-writer state — mutated only from
// handleResult and its callees, never from workers — so mu is a guard-rail
// per the spec's own note, not something normal operation needs to take.
type DynamicScorer struct {
	profile *profile.TargetProfile
	markov  *MarkovModel
	assoc   *AssocEngine
	dirCtx  map[string]*DirContext
	cfg     ScoreWeights
	mu      sync.Mutex
}

// NewDynamicScorer builds a DynamicScorer bound to p (mutated in place by
// profile refinement over the scan's lifetime, so the pointer stays valid).
func NewDynamicScorer(p *profile.TargetProfile, cfg ScoreWeights, markovOrder, markovMinSamples int) *DynamicScorer {
	return &DynamicScorer{
		profile: p,
		markov:  NewMarkovModel(markovOrder, markovMinSamples),
		assoc:   NewAssocEngine(),
		dirCtx:  make(map[string]*DirContext),
		cfg:     cfg,
	}
}

// Boost returns the product of per-signal factors, each (1 + wᵢ·sᵢ), sᵢ∈[0,1]
// (spec §2).
func (s *DynamicScorer) Boost(c *Candidate) float64 {
	b := 1.0
	b *= 1 + s.cfg.WSem*s.semSignal(c)
	b *= 1 + s.cfg.WAssoc*s.assoc.assocSignal(c)
	b *= 1 + s.cfg.WConv*s.markov.convSignal(terminalSegment(c.Path))
	return b
}

func (s *DynamicScorer) dirContext(dir string) *DirContext {
	dc, ok := s.dirCtx[dir]
	if !ok {
		dc = &DirContext{}
		s.dirCtx[dir] = dc
	}
	return dc
}

// semSignal is spec §3.1: a directory that turned out to be an open listing
// boosts every candidate under it broadly ("open listings are high-value");
// a directory that turned out locked (401/403) boosts only its
// sensitive-tagged siblings ("a locked /admin strongly implies neighbors
// worth trying").
func (s *DynamicScorer) semSignal(c *Candidate) float64 {
	dc, ok := s.dirCtx[c.ParentDir]
	if !ok {
		return 0
	}
	if dc.SawIndexOf {
		return 1.0
	}
	if dc.SawProtected && isSensitiveCandidate(c) {
		return 1.0
	}
	return 0
}

// ObserveProtected records a classified 401/403 in dir (spec §3.1); dir is
// where the protected response was seen, so its siblings (same ParentDir)
// are what benefit from semSignal.
func (s *DynamicScorer) ObserveProtected(dir string) {
	s.dirContext(dir).SawProtected = true
}

// ObserveIndexOf records a 200 + "Index of" listing found at childDir (spec
// §3.1) — childDir is the directory just discovered to be open, so its own
// future candidates (once calibrated and scanned) are what benefit.
func (s *DynamicScorer) ObserveIndexOf(childDir string) {
	s.dirContext(childDir).SawIndexOf = true
}

// RecordHit bumps dir's DirContext.HitCount (spec §3.1's DirContext field);
// called once per qualifying (confidence-gated) confirmed hit.
func (s *DynamicScorer) RecordHit(dir string) {
	s.dirContext(dir).HitCount++
}

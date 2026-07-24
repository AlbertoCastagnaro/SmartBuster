// session.go implements Phase 5a save/resume (spec §6): a JSON snapshot of
// every session-relevant piece of Coordinator state (spec §0 contract G),
// captured on the coordinator goroutine (via controlCh, so it never races a
// running scan) and used to reconstruct a fresh Coordinator that continues
// from exactly that point.
//
// Session ≠ audit: the audit log is the append-only lossless record of
// every request ever made; a session is a resumable snapshot of engine
// state at one instant. Their schemas overlap (both ultimately describe
// the same scan) but serve different purposes and evolve independently.
package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

// SessionVersion is bumped whenever SessionState's shape changes
// incompatibly; Load rejects a file whose Version it doesn't understand.
const SessionVersion = 1

// DirSnapshot is one directory's serializable tree/budget state (spec §6's
// "tree"). A directory still CALIBRATING when saved is not snapshotted at
// this granularity — see restoreSnapshot — since a handful of in-flight
// probe results aren't worth the complexity of round-tripping; it simply
// restarts calibration on resume.
type DirSnapshot struct {
	Path                   string
	Depth                  int
	State                  string // "scanning"|"done" ("calibrating" dirs are restarted instead, see restoreSnapshot)
	BranchStart            time.Time
	CandidatesTotal        int
	CandidatesAccountedFor int
	RequestsDispatched     int
	Budget                 int
	WildcardSuspect        bool
	Capped                 bool
	BudgetPruned           bool
	KnownPaths             []string
}

// WAFSample is one ring-buffer entry from spec §6's "wafState".
type WAFSample struct {
	Status int
	Novel  bool
}

// SessionState is the full resumable snapshot (spec §6): every field
// contract G lists as session-relevant — config, RNG seed (part of
// Config), tree, frontier (candidates+scores), baselines, learners
// (Markov/assoc/dirCtx), profile, visitedSets, counters, wafState.
type SessionState struct {
	Version int       `json:"version"`
	Target  string    `json:"target"`
	Config  Config    `json:"config"`
	SavedAt time.Time `json:"saved_at"`

	Frontier    []Candidate         `json:"frontier"`
	Dirs        []DirSnapshot       `json:"dirs"`
	Baselines   map[string]Baseline `json:"baselines"`
	Findings    []Finding           `json:"findings"`
	SeenContent map[uint64][]string `json:"seen_content"`

	Profile *profile.TargetProfile `json:"profile,omitempty"`

	Markov MarkovState           `json:"markov"`
	Assoc  AssocState            `json:"assoc"`
	DirCtx map[string]DirContext `json:"dir_ctx"`

	VisitedSet []string `json:"visited_set"`

	StatsReqSent int `json:"stats_req_sent"`
	StatsHits    int `json:"stats_hits"`

	WAFRing []WAFSample `json:"waf_ring"`

	// AIMD adaptive-rate state (Phase 6a upgrade of spec §6's "wafState"):
	// the Limiter's current rate plus enough of its trigger bookkeeping to
	// resume mid-backoff/mid-recovery exactly where a live scan left off.
	AIMDRate        float64   `json:"aimd_rate"`
	AIMDLastTrigger time.Time `json:"aimd_last_trigger"`
	AIMDTriggered   bool      `json:"aimd_triggered"`

	NmapSeeds   []profile.NmapSeed `json:"nmap_seeds"`
	SPAMode     bool               `json:"spa_mode"`
	RootRefined bool               `json:"root_refined"`
}

// Save is CtrlSnapshot's caller-side entry point: submits a snapshot
// request over controlCh (contract C — buildSnapshot itself only ever runs
// on the coordinator goroutine, alongside every other frontier/dirs
// mutation) and waits for the result.
//
// This used to be able to hang forever: SubmitControl's buffered controlCh
// accepts the CtrlSnapshot command even after Run has returned (nothing
// checked that), so a caller with no ctx deadline (e.g. a plain HTTP
// request context) would block on <-result indefinitely, since
// applyControl — the only thing that ever sends to Result — never runs
// again. The <-c.done case closes that: once Run has returned, Save fails
// fast with ErrScanNotRunning instead of waiting on a reply that can now
// never arrive.
func (c *Coordinator) Save(ctx context.Context) (SessionState, error) {
	result := make(chan SessionState, 1)
	if err := c.SubmitControl(ctx, ControlCmd{Kind: CtrlSnapshot, Result: result}); err != nil {
		return SessionState{}, err
	}
	select {
	case snap := <-result:
		return snap, nil
	case <-ctx.Done():
		return SessionState{}, ctx.Err()
	case <-c.done:
		return SessionState{}, ErrScanNotRunning
	}
}

// buildSnapshot runs exclusively on the coordinator goroutine (called from
// applyControl's CtrlSnapshot case), so every field it reads is read
// without racing its only writer.
func (c *Coordinator) buildSnapshot() SessionState {
	dirs := make([]DirSnapshot, 0, len(c.dirs))
	for path, ds := range c.dirs {
		state := "scanning"
		if ds.state == dirDone {
			state = "done"
		}
		if ds.state == dirCalibrating {
			state = "calibrating"
		}
		known := make([]string, 0, len(ds.knownPaths))
		for p := range ds.knownPaths {
			known = append(known, p)
		}
		dirs = append(dirs, DirSnapshot{
			Path: path, Depth: ds.depth, State: state, BranchStart: ds.branchStart,
			CandidatesTotal: ds.candidatesTotal, CandidatesAccountedFor: ds.candidatesAccountedFor,
			RequestsDispatched: ds.requestsDispatched, Budget: ds.budget,
			WildcardSuspect: ds.wildcardSuspect, Capped: ds.capped, BudgetPruned: ds.budgetPruned,
			KnownPaths: known,
		})
	}

	baselines := make(map[string]Baseline, len(c.baselines))
	for k, v := range c.baselines {
		baselines[k] = *v
	}

	seenContent := make(map[uint64][]string, len(c.seenContent))
	for k, v := range c.seenContent {
		seenContent[k] = append([]string(nil), v...)
	}

	var markovState MarkovState
	var assocState AssocState
	dirCtx := make(map[string]DirContext)
	if c.scorer != nil {
		markovState = c.scorer.markov.MarshalState()
		assocState = c.scorer.assoc.MarshalState()
		for k, v := range c.scorer.dirCtx {
			dirCtx[k] = *v
		}
	}

	wafRing := make([]WAFSample, len(c.wafRing))
	for i, s := range c.wafRing {
		wafRing[i] = WAFSample{Status: s.status, Novel: s.novel}
	}
	aimdRate, aimdLastTrigger, aimdTriggered := c.limiter.AIMDState()

	return SessionState{
		Version: SessionVersion, Target: c.target, Config: c.config, SavedAt: time.Now(),
		Frontier: c.frontier.All(), Dirs: dirs, Baselines: baselines,
		Findings: append([]Finding(nil), c.findings...), SeenContent: seenContent,
		Profile: c.profileState,
		Markov:  markovState, Assoc: assocState, DirCtx: dirCtx,
		VisitedSet:   c.crawlVisited.snapshot(),
		StatsReqSent: c.statsReqSent, StatsHits: c.statsHits,
		WAFRing:         wafRing,
		AIMDRate:        aimdRate,
		AIMDLastTrigger: aimdLastTrigger,
		AIMDTriggered:   aimdTriggered,
		NmapSeeds:       append([]profile.NmapSeed(nil), c.nmapSeeds...), SPAMode: c.spaMode, RootRefined: c.rootRefined,
	}
}

// NewCoordinatorFromSnapshot rebuilds a Coordinator from a saved
// SessionState (spec §6 resume): state.Config (including its RNG seed)
// round-trips verbatim through NewCoordinator, so per-directory probe
// token reproducibility (dirRand is a pure function of (seed, dir)) holds
// exactly across the save/resume boundary. wl must be the same wordlist
// Config.Wordlist names (re-loaded by the caller — a session file doesn't
// inline the wordlist itself, matching how Config only ever stores its
// path).
//
// Deviation from spec: the coordinator's own top-level RNG streams
// (c.rng, c.epsilonRNG) are NOT restored bit-exact — they're re-seeded
// fresh from Config.Seed, the same starting state a brand-new run would
// have, not the exact mid-stream position a live scan would be at. Only
// dirRand's per-directory sequences (which is what governs calibration
// probe token reproducibility, the property spec §8 DoD #5 actually tests)
// are exactly reproduced.
func NewCoordinatorFromSnapshot(state SessionState, wl []wordlist.Entry, sc *scope.Scope, opts ...Option) (*Coordinator, error) {
	if state.Version != SessionVersion {
		return nil, fmt.Errorf("session version %d unsupported (want %d)", state.Version, SessionVersion)
	}
	c, err := NewCoordinator(state.Target, wl, state.Config, sc, opts...)
	if err != nil {
		return nil, err
	}
	c.restoreSnapshot(state)
	c.resumed = true
	return c, nil
}

// restoreSnapshot installs state onto a freshly-constructed c, before Run()
// is ever called — so, like buildSnapshot, nothing here races a running
// scan (there isn't one yet).
func (c *Coordinator) restoreSnapshot(state SessionState) {
	c.frontier = NewFrontier()
	for _, cand := range state.Frontier {
		c.frontier.Push(cand)
	}

	c.dirs = make(map[string]*dirState, len(state.Dirs))
	var toRecalibrate []DirSnapshot
	for _, d := range state.Dirs {
		if d.State == "calibrating" {
			// Cheap to redo (N_PROBES*len(ExtSet) requests) and avoids
			// round-tripping partial in-flight probe results — see the
			// DirSnapshot doc comment.
			toRecalibrate = append(toRecalibrate, d)
			continue
		}
		ds := &dirState{
			path: d.Path, depth: d.Depth, branchStart: d.BranchStart,
			candidatesTotal: d.CandidatesTotal, candidatesAccountedFor: d.CandidatesAccountedFor,
			requestsDispatched: d.RequestsDispatched, budget: d.Budget,
			wildcardSuspect: d.WildcardSuspect, capped: d.Capped, budgetPruned: d.BudgetPruned,
			state: dirScanning,
		}
		if d.State == "done" {
			ds.state = dirDone
		}
		ds.knownPaths = make(map[string]bool, len(d.KnownPaths))
		for _, p := range d.KnownPaths {
			ds.knownPaths[p] = true
		}
		c.dirs[d.Path] = ds
	}

	c.baselines = make(map[string]*Baseline, len(state.Baselines))
	for k, v := range state.Baselines {
		b := v
		c.baselines[k] = &b
	}

	c.findings = append([]Finding(nil), state.Findings...)
	c.seenContent = make(map[uint64][]string, len(state.SeenContent))
	for k, v := range state.SeenContent {
		c.seenContent[k] = append([]string(nil), v...)
	}

	c.profileState = state.Profile
	if c.profileState != nil {
		c.extSet = c.profileState.ExtensionsForStack()
	}
	c.scorer = NewDynamicScorer(c.profileState, c.scoreWeights, c.markovOrder, c.markovMinSamples)
	c.scorer.markov.LoadState(state.Markov)
	c.scorer.assoc.LoadState(state.Assoc)
	for k, v := range state.DirCtx {
		dc := v
		c.scorer.dirCtx[k] = &dc
	}

	c.crawlVisited.restore(state.VisitedSet)

	c.statsReqSent = state.StatsReqSent
	c.statsHits = state.StatsHits

	c.wafRing = make([]wafSample, len(state.WAFRing))
	for i, s := range state.WAFRing {
		c.wafRing[i] = wafSample{status: s.Status, novel: s.Novel}
	}
	c.limiter.SetAIMDState(state.AIMDRate, state.AIMDLastTrigger, state.AIMDTriggered)

	c.nmapSeeds = append([]profile.NmapSeed(nil), state.NmapSeeds...)
	c.spaMode = state.SPAMode
	c.rootRefined = state.RootRefined

	// Deterministic, no network (corpus.Select reads the local DB): safe to
	// rebuild here rather than persist verbatim. loadCorpusTemplate itself
	// no-ops when Config.Wordlist bypassed the corpus (spec §0 contract G).
	c.loadCorpusTemplate()

	for _, d := range toRecalibrate {
		c.startCalibration(d.Path, d.Depth, d.BranchStart)
	}
}

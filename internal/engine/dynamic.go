// dynamic.go is the coordinator-orchestration counterpart of scorer.go,
// markov.go, and assoc.go — mirroring corpus_select.go's role for the
// corpus package: those files hold pure signal logic with no Coordinator
// dependency, this one wires them into the scan loop (spec §5, §6).
package engine

import "time"

// learnableHit is spec §5's poisoning defense: every Phase 3 learner update
// (dirCtx, markov, assoc) is gated behind this single check, so a soft-404
// or wildcard branch can't corrupt any of them.
func (c *Coordinator) learnableHit(cls Classification, dir string) bool {
	if cls.Confidence < c.learnMinConf {
		return false
	}
	if ds := c.dirs[dir]; ds != nil && ds.wildcardSuspect {
		return false
	}
	if b := c.baselines[dir]; b != nil && b.IsSPA {
		return false
	}
	return true
}

// learnFromHit is spec §5's single gate for every Phase 3 learner: a
// confirmed, non-alias hit (the caller, handleHit, only reaches this after
// its own novelty check) feeds dirCtx (§3.1), markov (§3.3), and assoc
// reweight+generate (§3.2) only if learnableHit clears it.
func (c *Coordinator) learnFromHit(res WorkResult, cls Classification, dir string) {
	if c.scorer == nil || !c.learnableHit(cls, dir) {
		return
	}
	cand := res.Item.Candidate
	sig := res.Signature

	if sig.Status == 401 || sig.Status == 403 {
		c.scorer.ObserveProtected(dir)
	}
	if sig.Status >= 200 && sig.Status < 300 && sig.HasIndexOf {
		c.scorer.ObserveIndexOf(dir + "/" + cand.Path)
	}
	c.scorer.RecordHit(dir)

	c.scorer.markov.Train(terminalSegment(cand.Path))
	c.scorer.assoc.RecordHit(dir, cand.Path)
	for _, gen := range c.scorer.assoc.Generate(cand) {
		c.enqueueGenerated(dir, gen)
	}

	c.markScorerDirty()
}

// enqueueGenerated pushes a Phase 3 generated candidate (spec §3.2) into
// dir's frontier if it isn't already known — dedup'd against the initial
// template push and anything already generated. Growing candidatesTotal and
// budget keeps the directory from finishing before the generated candidate
// gets a chance to dispatch. Generated candidates get a real computed score
// immediately (spec §6: "no full reprio needed for them").
func (c *Coordinator) enqueueGenerated(dir string, cand Candidate) bool {
	ds := c.dirs[dir]
	if ds == nil || ds.state == dirDone {
		return false
	}
	if ds.knownPaths == nil {
		ds.knownPaths = make(map[string]bool)
	}
	if ds.knownPaths[cand.Path] {
		return false
	}
	ds.knownPaths[cand.Path] = true
	ds.candidatesTotal++
	ds.budget++

	cand.Depth = ds.depth + 1
	cand.Score = c.scoreCandidate(cand)
	c.frontier.Push(cand)
	return true
}

// markScorerDirty implements spec §6's throttled reprioritization: a
// qualifying discovery marks the frontier dirty, but the actual
// Frontier.Reprioritize sweep runs at most once per REPRIO_INTERVAL (25
// confirmed hits or 500ms, whichever first) — reprioritizing on every hit is
// O(N)/hit, too costly for a large frontier.
func (c *Coordinator) markScorerDirty() {
	c.scorerDirty = true
	c.hitsSinceReprio++
	if c.hitsSinceReprio >= c.reprioHits || time.Since(c.lastDynReprio) >= c.reprioInterval {
		c.runDynamicReprio()
	}
}

// runDynamicReprio performs the actual sweep and resets both throttle
// counters; a no-op if nothing is dirty (e.g. called from a stray timer
// tick with no new discovery since the last sweep).
func (c *Coordinator) runDynamicReprio() {
	if !c.scorerDirty {
		return
	}
	c.frontier.Reprioritize(c.applyScore)
	c.scorerDirty = false
	c.hitsSinceReprio = 0
	c.lastDynReprio = time.Now()
	c.dynReprioCount++
}

// popNext is spec §4's exploration hook: with probability epsilon, sample a
// candidate from the frontier's mid-tier instead of always taking the max,
// so pure greedy descent doesn't tunnel-vision into one branch. epsilon is
// 0 (pure greedy, the default zero value) unless explicitly configured —
// stealth modes keep it that way; coverage-heavy runs raise it.
func (c *Coordinator) popNext() (Candidate, bool) {
	if c.epsilon > 0 && c.frontier.Len() > 2 && c.epsilonRNG.Float64() < c.epsilon {
		if cand, ok := c.frontier.SampleMidTier(c.epsilonRNG); ok {
			return cand, true
		}
	}
	if c.frontier.Empty() {
		return Candidate{}, false
	}
	if c.orderJitter {
		return c.frontier.PopBand(c.orderJitterRNG, orderJitterBand), true
	}
	return c.frontier.Pop(), true
}

// recordDispatch tracks the subtree yield cap's consecutive-dispatch streak
// (spec §4): dispatching from a new directory resets it, which is what lets
// nextDispatchable's burst check eventually let a throttled directory's own
// candidates through again.
func (c *Coordinator) recordDispatch(dir string) {
	if dir == c.lastDispatchDir {
		c.dispatchStreak++
	} else {
		c.lastDispatchDir = dir
		c.dispatchStreak = 1
	}
}

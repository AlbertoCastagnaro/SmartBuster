// control.go is Phase 5a's second load-bearing concurrency primitive (spec
// §0 contract C, §4): controlCh carries every externally-triggered mutation
// — pause/resume/stop, live rate/concurrency/mode adjustment, and the
// manual frontier overrides (pin/exclude/boost/demote/inject) — from an
// HTTP handler (or the CLI) goroutine to the coordinator goroutine, which
// alone applies them. This preserves the same single-writer invariant
// seedInjectCh already gives the frontier: an HTTP handler never touches
// c.frontier, c.paused, c.overrides, or the worker pool directly.
package engine

import (
	"context"
	"path"
	"strings"
	"sync/atomic"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/seed"
)

type ControlKind string

const (
	CtrlPause   ControlKind = "pause"
	CtrlResume  ControlKind = "resume"
	CtrlStop    ControlKind = "stop"
	CtrlAdjust  ControlKind = "adjust" // PATCH .../{id}: rate/concurrency/mode
	CtrlPin     ControlKind = "pin"
	CtrlExclude ControlKind = "exclude"
	CtrlBoost   ControlKind = "boost"
	CtrlDemote  ControlKind = "demote"
	CtrlInject  ControlKind = "inject"

	// CtrlSnapshot is session save's entry point (spec §6): like every
	// other control command, the snapshot itself is only ever built on the
	// coordinator goroutine (buildSnapshot, session.go) — Result is how the
	// answer gets back to the caller.
	CtrlSnapshot ControlKind = "snapshot"
)

// ControlCmd is one manual-override command (spec §4, §4.1). Only the
// fields relevant to Kind are read; the rest are zero-valued.
type ControlCmd struct {
	Kind ControlKind

	// CtrlAdjust's optional fields: nil means "leave unchanged", mirroring
	// PATCH's partial-update semantics.
	SetRate        *float64
	SetConcurrency *int
	SetMode        *string

	// CtrlPin/CtrlExclude/CtrlBoost/CtrlDemote: Pattern is glob/prefix
	// (spec §4.1), matched against a candidate's "dir/path" — see
	// matchOverridePattern. Factor is Boost/Demote's multiplier.
	Pattern string
	Factor  float64

	// CtrlInject: user-supplied seed paths (-> enqueueSeed, provenance "user").
	Terms []string

	// CtrlSnapshot: where applyControl sends the built SessionState. Buffered
	// (cap >= 1) by the caller so the coordinator's send never blocks.
	Result chan SessionState
}

// SubmitControl is controlCh's producer-side entry point: the daemon's REST
// handlers (or the CLI) call this — never c.frontier/c.paused/c.overrides
// directly — so every control mutation is applied on the coordinator
// goroutine (contract C). It returns once the command is queued, not once
// it's applied; ctx lets a caller give up rather than block forever against
// a scan whose dispatchLoop has already returned.
func (c *Coordinator) SubmitControl(ctx context.Context, cmd ControlCmd) error {
	select {
	case c.controlCh <- cmd:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// applyControl is controlCh's sole consumer — dispatchLoop's select case for
// it does nothing but call this — so every mutation below runs exclusively
// on the coordinator goroutine.
func (c *Coordinator) applyControl(cmd ControlCmd) {
	switch cmd.Kind {
	case CtrlPause:
		c.paused = true
	case CtrlResume:
		c.paused = false
	case CtrlStop:
		if c.cancel != nil {
			c.cancel()
		}
	case CtrlAdjust:
		c.applyAdjust(cmd)
	case CtrlPin:
		c.applyPin(cmd.Pattern)
	case CtrlExclude:
		c.applyExclude(cmd.Pattern)
	case CtrlBoost:
		c.applyOverride(cmd.Pattern, cmd.Factor, ovBoost)
	case CtrlDemote:
		c.applyOverride(cmd.Pattern, cmd.Factor, ovDemote)
	case CtrlInject:
		for _, term := range cmd.Terms {
			if term == "" {
				continue
			}
			c.enqueueSeed(seed.Seed{Path: term, Provenance: "user", BasePrio: 1.0})
		}
	case CtrlSnapshot:
		if cmd.Result != nil {
			cmd.Result <- c.buildSnapshot()
		}
	}
}

// applyAdjust implements PATCH .../{id} (spec §4): rate takes effect on the
// Limiter immediately; concurrency raises the live dispatch cap and, if
// that's now above how many RunWorker goroutines exist, spawns the
// shortfall (lowering it never kills a goroutine — see the concurrencyCap
// field doc on Coordinator). mode is stored for status reporting only; no
// engine behavior keys off it yet (handoff deviation).
func (c *Coordinator) applyAdjust(cmd ControlCmd) {
	if cmd.SetRate != nil {
		c.limiter.SetRate(*cmd.SetRate)
	}
	if cmd.SetConcurrency != nil && *cmd.SetConcurrency > 0 {
		atomic.StoreInt32(&c.concurrencyCap, int32(*cmd.SetConcurrency))
		if grow := *cmd.SetConcurrency - c.workerCount; grow > 0 {
			for i := 0; i < grow; i++ {
				c.workersWG.Add(1)
				go func() {
					defer c.workersWG.Done()
					RunWorker(c.runCtx, c.workCh, c.resultsCh, c.client, c.harvestEnabled)
				}()
			}
			c.workerCount = *cmd.SetConcurrency
		}
	}
	if cmd.SetMode != nil {
		c.mode = *cmd.SetMode
	}
}

func (c *Coordinator) concurrencyLimit() int {
	return int(atomic.LoadInt32(&c.concurrencyCap))
}

// overrideKind distinguishes the three persistent-override flavors
// (pin reuses ovBoost with PinScore — see applyPin).
type overrideKind int

const (
	ovBoost overrideKind = iota
	ovDemote
	ovExclude
)

type override struct {
	pattern string
	kind    overrideKind
	factor  float64
}

// applyPin implements spec §4.1's pin: a persistent forced-top-priority
// override (via the same multiplicative path boost/demote use, factor
// PinScore) plus, for a literal (non-glob) pattern, a forced insertion
// through the ordinary seed path — "even if not in corpus" means a pin
// candidate that was never in the wordlist/corpus template still has to
// exist as a real frontier entry, not just a scoring rule with nothing to
// apply to.
func (c *Coordinator) applyPin(pattern string) {
	c.overrides = append(c.overrides, override{pattern: pattern, kind: ovBoost, factor: PinScore})
	c.frontier.Reprioritize(c.applyScore)
	if !isGlobPattern(pattern) {
		c.enqueueSeed(seed.Seed{Path: pattern, Provenance: "user:pin", BasePrio: 1.0})
	}
}

// applyExclude implements spec §4.1's exclude: added to the denylist every
// future push checks (pushCandidate), and swept out of whatever's already
// queued right now. Each removed candidate is accounted for exactly like
// nextDispatchable's own budget/scope discards, so its directory can still
// reach dirDone — an exclude must not leave candidatesAccountedFor
// permanently short of candidatesTotal.
func (c *Coordinator) applyExclude(pattern string) {
	c.overrides = append(c.overrides, override{pattern: pattern, kind: ovExclude})
	removed := c.frontier.RemoveMatching(func(cand Candidate) bool {
		return matchOverridePattern(pattern, cand.ParentDir, cand.Path)
	})
	for _, cand := range removed {
		if ds := c.dirs[cand.ParentDir]; ds != nil {
			ds.candidatesAccountedFor++
			c.maybeFinishDir(ds)
		}
	}
}

// applyOverride implements spec §4.1's boost/demote: a persistent score
// multiplier (or its reciprocal) applied in scoreCandidate, the same
// choke-point pattern SPA damping already uses — so it composes with every
// other signal instead of bypassing them.
func (c *Coordinator) applyOverride(pattern string, factor float64, kind overrideKind) {
	if factor <= 0 {
		factor = 1
	}
	c.overrides = append(c.overrides, override{pattern: pattern, kind: kind, factor: factor})
	c.frontier.Reprioritize(c.applyScore)
}

// isExcluded is the denylist check spec §4.1 calls "checked at every
// enqueue" — enforced at nextDispatchable's single discard choke point
// (alongside the existing budget/scope discards) rather than scattered
// across every push call site. That's deliberate, not a shortcut: several
// push sites (pushWordlistCandidates, pushCorpusCandidates) compute
// candidatesTotal from a slice length before pushing, so silently dropping
// a candidate at push time there would leave candidatesAccountedFor
// permanently short of candidatesTotal and strand the directory in
// dirScanning forever. Checking once at dispatch — where every candidate
// that reaches this point already has a finalized candidatesTotal/budget —
// gets the same externally-visible behavior (an excluded candidate is
// never requested) without that hazard. applyExclude additionally sweeps
// anything already queued out of the frontier immediately, so this is the
// backstop for candidates enqueued after the exclude took effect.
func (c *Coordinator) isExcluded(dir, path string) bool {
	for _, ov := range c.overrides {
		if ov.kind == ovExclude && matchOverridePattern(ov.pattern, dir, path) {
			return true
		}
	}
	return false
}

// overrideMultiplier folds every matching boost/demote/pin override into
// scoreCandidate's result. Multiple matching overrides compose
// multiplicatively, same as every other scoreCandidate signal.
func (c *Coordinator) overrideMultiplier(cand *Candidate) float64 {
	m := 1.0
	for _, ov := range c.overrides {
		if ov.kind == ovExclude || !matchOverridePattern(ov.pattern, cand.ParentDir, cand.Path) {
			continue
		}
		switch ov.kind {
		case ovBoost:
			m *= ov.factor
		case ovDemote:
			m /= ov.factor
		}
	}
	return m
}

// matchOverridePattern is spec §4.1's "glob/prefix" pattern match: pattern
// is checked against both the full "dir/path" and the bare terminal path,
// so a user can target either "/admin/config.php" or just "config.php"
// wherever it appears. A trailing '*' additionally matches as a plain
// string prefix (path.Match's '*' doesn't cross '/', so "/admin/*" alone
// wouldn't otherwise reach "/admin/sub/x") — that's the "prefix" half of
// "glob/prefix"; path.Match covers the "glob" half.
func matchOverridePattern(pattern, dir, p string) bool {
	full := strings.TrimPrefix(dir+"/"+p, "/")
	if pattern == full || pattern == p {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok && strings.HasPrefix(full, prefix) {
		return true
	}
	if ok, err := path.Match(pattern, full); err == nil && ok {
		return true
	}
	if ok, err := path.Match(pattern, p); err == nil && ok {
		return true
	}
	return false
}

func isGlobPattern(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

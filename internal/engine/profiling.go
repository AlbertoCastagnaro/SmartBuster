package engine

import (
	"context"
	"os"
	"sync"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
)

// sharedWappalyzer is loaded once per process (its fingerprint DB is large
// and immutable) rather than once per Coordinator/target.
var (
	sharedWappalyzerOnce sync.Once
	sharedWappalyzer     *profile.Wappalyzer
)

func getSharedWappalyzer() *profile.Wappalyzer {
	sharedWappalyzerOnce.Do(func() {
		w, err := profile.NewWappalyzer()
		if err == nil {
			sharedWappalyzer = w
		}
	})
	return sharedWappalyzer
}

const nmapSeedScore = 2.0 // above BasePrio's max of 1.0: nmap-confirmed paths jump the queue

// extSetGrew reports whether newExts contains an extension absent from old
// — the trigger for re-calibrating root after profile refinement.
func extSetGrew(old, newExts []string) bool {
	oldSet := make(map[string]bool, len(old))
	for _, e := range old {
		oldSet[e] = true
	}
	for _, e := range newExts {
		if !oldSet[e] {
			return true
		}
	}
	return false
}

// DiscoveredServices returns same-host web service base URLs nmap revealed
// beyond this coordinator's own target (spec §7) — e.g. an alternate HTTPS
// port. The CLI uses this to auto-expand a scan across a host's other
// in-scope ports; empty if profiling didn't run or nmap wasn't ingested.
func (c *Coordinator) DiscoveredServices() []string {
	if c.profileState == nil {
		return nil
	}
	var out []string
	for _, svc := range c.profileState.Services {
		if svc.BaseURL != c.target {
			out = append(out, svc.BaseURL)
		}
	}
	return out
}

// profileOpts builds the profile.Options shared by ProfileTarget and
// RefineAfterCalibration.
func (c *Coordinator) profileOpts() profile.Options {
	return profile.Options{
		Ruleset:      c.ruleset,
		Wappalyzer:   c.wappalyzer,
		ActiveProbes: c.config.ActiveProbes,
		FaviconProbe: c.config.FaviconProbe,
		InScope:      c.scope.InScope,
		Pace:         c.paceProfileRequest,
	}
}

// paceProfileRequest waits one pacer tick before an active-probe request
// (spec §4.7: "All go through scope + rate limiter").
func (c *Coordinator) paceProfileRequest() {
	<-c.pacer.C()
	c.pacer.Advance()
}

// profileTarget is the scan-start hook (spec §0 contract B): build the
// provisional TargetProfile, derive the calibration extension set from it,
// and merge any nmap data — all before root calibration begins.
func (c *Coordinator) profileTarget(ctx context.Context) {
	c.profileState = profile.ProfileTarget(ctx, c.client, c.target, c.profileOpts())
	c.extSet = c.profileState.ExtensionsForStack()
	// Phase 3 (spec §0 contract B): the dynamic layer binds to the same
	// profileState pointer RefineAfterCalibration mutates in place below,
	// so it stays valid without re-wiring for the rest of the scan.
	c.scorer = NewDynamicScorer(c.profileState, c.scoreWeights, c.markovOrder, c.markovMinSamples)
	c.ingestNmap(ctx)
	c.loadCorpusTemplate()
	c.reprioritizeIfChanged() // spec §7(a): the provisional profile is finalized
	c.emitTechDetected()
}

// ingestNmap implements spec §7: parse --nmap (or run --run-nmap), merge
// only the data for this coordinator's own target host into the profile
// and frontier — other hosts an nmap file might describe are informational
// only in Phase 2a (not auto-scanned).
func (c *Coordinator) ingestNmap(ctx context.Context) {
	if c.config.NmapFile == "" && !c.config.RunNmap {
		return
	}

	var data []byte
	var err error
	if c.config.RunNmap {
		data, err = profile.RunNmap(ctx, c.profileState.Host)
	} else {
		data, err = os.ReadFile(c.config.NmapFile)
	}
	if err != nil {
		c.emit(Event{Type: EventWarning, Message: "nmap: " + err.Error()})
		return
	}

	results, warnings, err := profile.IngestNmap(data, func(host string) bool {
		return c.scope.InScope("https://" + host + "/")
	})
	for _, w := range warnings {
		c.emit(Event{Type: EventWarning, Message: w})
	}
	if err != nil {
		c.emit(Event{Type: EventWarning, Message: "nmap: " + err.Error()})
		return
	}

	for _, r := range results {
		if r.Host != c.profileState.Host {
			continue
		}
		for _, v := range r.Votes {
			c.profileState.VoteNmap(v)
		}
		c.profileState.VHosts = append(c.profileState.VHosts, r.VHosts...)
		c.profileState.Services = mergeServices(c.profileState.Services, r.Services)
		c.nmapSeeds = append(c.nmapSeeds, r.Seeds...)
	}
}

func mergeServices(existing []profile.ServiceTarget, add []profile.ServiceTarget) []profile.ServiceTarget {
	seen := make(map[string]bool, len(existing))
	for _, s := range existing {
		seen[s.BaseURL] = true
	}
	for _, s := range add {
		if !seen[s.BaseURL] {
			seen[s.BaseURL] = true
			existing = append(existing, s)
		}
	}
	return existing
}

// emitTechDetected fires spec §0 contract G's events and (if the audit
// sink supports it) persists provenance (spec §6).
func (c *Coordinator) emitTechDetected() {
	if c.profileState == nil {
		return
	}
	entries := techEntries(c.profileState)
	c.emit(Event{Type: EventTechDetected, Tech: entries})
	if c.profileState.WAF != "" {
		c.emit(Event{Type: EventWAFDetected, WAF: c.profileState.WAF})
	}
	if ts, ok := c.auditSink.(TechAuditSink); ok {
		ts.WriteTechProfile(c.profileState.Host, entries, c.profileState.WAF)
	}
}

func techEntries(p *profile.TargetProfile) []TechEntry {
	snap := p.Snapshot()
	out := make([]TechEntry, 0, len(snap))
	for _, t := range snap {
		out = append(out, TechEntry{
			Name:       t.Name,
			Category:   t.Category,
			Version:    t.Version,
			Confidence: t.Confidence,
			Layer:      layerString(t.Layer),
			Sources:    sourceStrings(t.Sources),
			RuleIDs:    t.RuleIDs(),
		})
	}
	return out
}

func layerString(l profile.Layer) string {
	switch l {
	case profile.LayerBackend:
		return "backend"
	case profile.LayerEdge:
		return "edge"
	default:
		return "unknown"
	}
}

func sourceStrings(sources []profile.Source) []string {
	out := make([]string, len(sources))
	for i, s := range sources {
		out[i] = sourceString(s)
	}
	return out
}

func sourceString(s profile.Source) string {
	switch s {
	case profile.SrcHeader:
		return "header"
	case profile.SrcCookie:
		return "cookie"
	case profile.SrcHTML:
		return "html"
	case profile.SrcFavicon:
		return "favicon"
	case profile.SrcErrorPage:
		return "errorpage"
	case profile.SrcWappalyzer:
		return "wappalyzer"
	case profile.SrcActiveProbe:
		return "activeprobe"
	case profile.SrcNmap:
		return "nmap"
	default:
		return "unknown"
	}
}

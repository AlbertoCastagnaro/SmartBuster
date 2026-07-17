// Package profile builds a TargetProfile for one scan target: passive tech
// detection (headers, cookies, HTML, favicon, wappalyzergo), WAF detection,
// and nmap ingestion. Phase 2a produces the profile; Phase 2b's corpus
// selection and per-candidate scoring consume it (spec §0).
package profile

import "strings"

type Layer int

const (
	LayerBackend Layer = iota
	LayerEdge
	LayerUnknown
)

type Source int

const (
	SrcHeader Source = iota
	SrcCookie
	SrcHTML
	SrcFavicon
	SrcErrorPage
	SrcWappalyzer
	SrcActiveProbe
	SrcNmap
)

// Tech is one detected technology with fused confidence (spec §2, §4.8).
type Tech struct {
	Name       string // "PHP", "WordPress", "nginx", "Apache Tomcat"
	Category   string // "language","server","cms","framework","waf","proxy"
	Version    string // "" if unknown
	Confidence float64
	Layer      Layer
	Sources    []Source

	// ruleIDs records the provenance of every vote (spec §6: "every vote
	// records the rule id; surfaced in tech.detected and the audit log").
	// Not part of the spec's Tech field list; exposed via RuleIDs() so the
	// exported struct shape stays exactly as specified.
	ruleIDs []string
}

// RuleIDs returns the rule ids that voted for this Tech, in vote order.
func (t *Tech) RuleIDs() []string {
	return t.ruleIDs
}

type ServiceTarget struct {
	BaseURL string
	Port    int
	Scheme  string
}

// TargetProfile is built once per target by profileTarget and stored on the
// coordinator for Phase 2b. Every mutation happens on the coordinator's
// single goroutine (see profile.go's sequencing note), so no internal
// locking is needed here.
type TargetProfile struct {
	Host     string
	Services []ServiceTarget
	Tech     map[string]*Tech
	WAF      string
	IsSPA    bool
	VHosts   []string

	// layerVotes counts, per tech name, how many sources implied each
	// Layer — internal bookkeeping for the majority-vote rule in §4.8;
	// not part of the public API.
	layerVotes map[string][3]int
}

func newTargetProfile(host string) *TargetProfile {
	return &TargetProfile{
		Host:       host,
		Tech:       make(map[string]*Tech),
		layerVotes: make(map[string][3]int),
	}
}

// vote upserts a Tech and fuses confidence via noisy-OR (spec §4.8):
// conf = 1 - Π(1 - cᵢ). Layer is decided by majority of source-implied
// layers seen so far; ties resolve to LayerUnknown.
func (p *TargetProfile) vote(name, category string, layer Layer, source Source, confidence float64, version, ruleID string) {
	if name == "" {
		return
	}
	t, ok := p.Tech[name]
	if !ok {
		t = &Tech{Name: name, Category: category, Layer: LayerUnknown}
		p.Tech[name] = t
	}
	if t.Category == "" {
		t.Category = category
	}
	if version != "" && t.Version == "" {
		t.Version = version
	}
	t.Confidence = 1 - (1-t.Confidence)*(1-confidence)
	if t.Confidence > 0.99 {
		t.Confidence = 0.99
	}
	t.Sources = append(t.Sources, source)
	if ruleID != "" {
		t.ruleIDs = append(t.ruleIDs, ruleID)
	}

	votes := p.layerVotes[name]
	votes[layer]++
	p.layerVotes[name] = votes
	t.Layer = majorityLayer(votes)
}

func majorityLayer(votes [3]int) Layer {
	backend, edge := votes[LayerBackend], votes[LayerEdge]
	switch {
	case backend > edge:
		return LayerBackend
	case edge > backend:
		return LayerEdge
	default:
		return LayerUnknown
	}
}

// stackExtensions maps a backend-tech keyword (matched case-insensitively
// against a substring of the tech name) to the extensions it implies (spec
// §2 table).
var stackExtensions = []struct {
	keyword string
	exts    []string
}{
	{"php", []string{".php", ".phtml", ".php5"}},
	{"asp.net", []string{".aspx", ".asmx", ".ashx", ".asp"}},
	{"iis", []string{".aspx", ".asmx", ".ashx", ".asp"}},
	{"java", []string{".jsp", ".do", ".action"}},
	{"tomcat", []string{".jsp", ".do", ".action"}},
}

var genericExtensions = []string{"", ".html", ".txt", ".json"}
var backupExtensions = []string{".bak", ".old", ".zip", ".tar.gz", ".swp", ".~"}

// ExtensionsForStack returns the calibration/probe extension set for the
// detected backend, always unioned with generic + backup extensions (spec
// §2). Only LayerBackend/LayerUnknown techs contribute stack-specific
// extensions; LayerEdge techs (proxies/CDNs) never do (spec §4.9).
func (p *TargetProfile) ExtensionsForStack() []string {
	seen := make(map[string]bool)
	var exts []string
	add := func(list []string) {
		for _, e := range list {
			if !seen[e] {
				seen[e] = true
				exts = append(exts, e)
			}
		}
	}

	for _, t := range p.Tech {
		if t.Layer == LayerEdge {
			continue
		}
		name := strings.ToLower(t.Name)
		for _, se := range stackExtensions {
			if strings.Contains(name, se.keyword) {
				add(se.exts)
			}
		}
	}
	add(genericExtensions)
	add(backupExtensions)
	return exts
}

// MatchScore returns a [0,1] boost factor for a candidate carrying tags,
// given the profile's detected techs and their confidences. Defined here
// per spec §2; consumed by Phase 2b's per-candidate scoring.
func (p *TargetProfile) MatchScore(tags []string) float64 {
	best := 0.0
	for _, tag := range tags {
		for name, t := range p.Tech {
			if strings.EqualFold(name, tag) || strings.EqualFold(t.Category, tag) {
				if t.Confidence > best {
					best = t.Confidence
				}
			}
		}
	}
	return best
}

// Snapshot returns a shallow copy of the profile's Tech map, safe for an
// event payload or audit record without exposing the live map.
func (p *TargetProfile) Snapshot() map[string]Tech {
	out := make(map[string]Tech, len(p.Tech))
	for k, v := range p.Tech {
		out[k] = *v
	}
	return out
}

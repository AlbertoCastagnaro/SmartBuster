package seed

import (
	"strings"
	"time"
)

// BasePrio tiers (spec §3 table): a path with external evidence beats a
// corpus guess. Deliberately-hidden (robots Disallow) beats declared-public
// (sitemap), beats merely-allowed/historical.
const (
	PrioRobotsDisallow = 0.95
	PrioSitemap        = 0.90
	PrioRobotsAllow    = 0.85
	PrioWaybackRecent  = 0.85
	PrioWaybackOld     = 0.70

	// Phase 4b active-harvest priors (spec §7): CRAWL_HTML_PRIOR,
	// CRAWL_JS_PRIOR, HEADLESS_PRIOR. Co-located with the passive-seeding
	// priors above since Normalize's basePrio (and its cross-source
	// dedup/merge) is the single place every seed source's prior tiering
	// happens, active or passive alike.
	PrioCrawlHTML = 0.90
	PrioCrawlJS   = 0.85
	PrioHeadless  = 0.90
)

// waybackRecentWindow is the recency cutoff for Wayback's recent-vs-old
// prior tier (spec §3): a capture within this window of "now" counts as
// "recent" (BasePrio 0.85); older counts as "old" (0.70). The spec doesn't
// mandate an exact figure — a year is a reasonable proxy for "probably still
// there."
const waybackRecentWindow = 365 * 24 * time.Hour

// assetNoiseExt is spec §3's static-asset noise dropped from Wayback by
// default (--seed-assets keeps it): a historical CDX dump is mostly CSS/JS/
// image cruft that isn't worth a path-buster's attention.
var assetNoiseExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".ico": true, ".css": true, ".js": true, ".woff": true, ".woff2": true,
	".ttf": true, ".eot": true, ".mp4": true, ".mp3": true, ".webp": true,
	".bmp": true,
}

// NormalizeOptions configures Normalize (spec §3, §6).
type NormalizeOptions struct {
	SeedAssets bool      // --seed-assets: keep static-asset noise from Wayback
	Now        time.Time // recency cutoff reference; zero = time.Now()
}

// Normalize turns every source's RawSeeds into the final, deduped,
// prior-tiered Seed list the engine consumes via enqueueSeed (spec §3):
// query strings stripped, Wayback asset noise dropped (unless SeedAssets),
// BasePrio assigned per source (+ capture recency for Wayback), and
// duplicates across sources merged into one Seed carrying the max BasePrio
// and the union of every source that named it.
func Normalize(raws []RawSeed, opts NormalizeOptions) []Seed {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	byPath := make(map[string]*Seed)
	var order []string
	for _, raw := range raws {
		p := stripQuery(raw.Path)
		if p == "" || p == "/" {
			continue
		}
		if !opts.SeedAssets && strings.HasPrefix(raw.Source, "wayback") && assetNoiseExt[strings.ToLower(extOf(p))] {
			continue
		}

		prio := basePrio(raw.Source, now)
		if s, ok := byPath[p]; ok {
			if prio > s.BasePrio {
				s.BasePrio = prio
			}
			s.Provenance = UnionProvenance(s.Provenance, raw.Source)
			continue
		}
		byPath[p] = &Seed{Path: p, IsDirHint: isDirHint(p), BasePrio: prio, Provenance: raw.Source}
		order = append(order, p)
	}

	out := make([]Seed, 0, len(order))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	return out
}

// UnionProvenance "+"-joins a and b's already-"+"-joined provenance tags,
// deduped and order-preserving (spec §3: "unions provenance"). Exported so
// engine's own dedup-against-corpus merge (a seed landing on a path the
// wordlist/corpus template already has, spec §3) reuses the exact same
// joining convention instead of duplicating it.
func UnionProvenance(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	seen := make(map[string]bool)
	var out []string
	for _, part := range strings.Split(a+"+"+b, "+") {
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return strings.Join(out, "+")
}

func basePrio(source string, now time.Time) float64 {
	switch {
	case source == "robots:disallow":
		return PrioRobotsDisallow
	case source == "sitemap":
		return PrioSitemap
	case source == "robots:allow":
		return PrioRobotsAllow
	case source == "crawl:html":
		return PrioCrawlHTML
	case source == "crawl:js":
		return PrioCrawlJS
	case source == "headless":
		return PrioHeadless
	case strings.HasPrefix(source, "wayback:"):
		if isRecentCapture(strings.TrimPrefix(source, "wayback:"), now) {
			return PrioWaybackRecent
		}
		return PrioWaybackOld
	default:
		return PrioRobotsAllow
	}
}

func isRecentCapture(dateStr string, now time.Time) bool {
	if len(dateStr) > 8 {
		dateStr = dateStr[:8] // tolerate a full CDX timestamp, not just waybackDate's already-trimmed form
	}
	t, err := time.Parse("20060102", dateStr)
	if err != nil {
		return false // unparseable timestamp: treat conservatively as old
	}
	return now.Sub(t) <= waybackRecentWindow
}

// isDirHint mirrors wordlist.Load's Phase 1 dot-heuristic (spec §3): a
// terminal segment without a '.' looks directory-like.
func isDirHint(p string) bool {
	return !strings.Contains(terminalSegment(p), ".")
}

func extOf(p string) string {
	seg := terminalSegment(p)
	if i := strings.LastIndexByte(seg, '.'); i >= 0 {
		return seg[i:]
	}
	return ""
}

func terminalSegment(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func stripQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

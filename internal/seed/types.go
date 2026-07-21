// Package seed implements Phase 4a passive seeding: turning robots.txt,
// sitemap.xml, and the Wayback Machine/CDX into frontier seeds. Like
// internal/wordlist and internal/corpus, this is a leaf package — it does
// not import package engine (which imports seed for enqueueSeed, spec §0
// contract A) — so engine converts a seed.Seed into an engine.Candidate the
// same way it already converts wordlist.Entry and corpus.Candidate.
package seed

import "context"

// RawSeed is one path discovered by a source, before normalization, query
// stripping, asset filtering, prior tiering, and cross-source dedup (spec
// §3) — Normalize's input.
type RawSeed struct {
	Path   string // path (may still carry a query string), as declared/observed
	Source string // "robots:disallow" | "robots:allow" | "sitemap" | "wayback:<capture-date>"
}

// Seed is one fully normalized, prior-tiered, deduped seed — Normalize's
// output, and what enqueueSeed (engine-side) turns into a Candidate (spec
// §3, §4).
type Seed struct {
	Path       string  // normalized path, query string stripped, e.g. "/old/admin/config.php"
	IsDirHint  bool    // trailing slash / extensionless -> looks dir-like (Phase 1 dot-heuristic, spec §3)
	BasePrio   float64 // tiered seed prior (spec §3 table), max across dedup
	Provenance string  // "+"-joined source tags, unioned across dedup (spec §3)
}

// SeedSource is the pluggable interface external passive sources implement
// (spec §5.3); Wayback is the only implementation shipped in 4a. Additional
// gau-style sources (Common Crawl, URLScan, OTX) can be added later behind
// this same interface without touching normalization or engine wiring.
//
// Deviation from the spec's `Fetch(host string) ([]RawSeed, error)`
// pseudocode: ctx is added so a caller can cancel a slow off-target query,
// matching every other network call in this codebase (e.g.
// httpclient.Client.Do, profile.Fetch).
type SeedSource interface {
	Fetch(ctx context.Context, host string) ([]RawSeed, error)
}

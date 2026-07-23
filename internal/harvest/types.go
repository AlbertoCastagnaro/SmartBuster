// Package harvest implements Phase 4b active discovery: mining paths out of
// the target's own responses (HTML link crawling, JS endpoint extraction)
// and, opt-in, a headless-browser capture tier. Like internal/seed, this is
// a leaf package — it holds pure parse/extract logic with no Coordinator
// dependency, so engine converts its output into seed.RawSeed the same way
// it already converts wordlist.Entry, corpus.Candidate, and seed.Seed.
package harvest

const (
	// HarvestBodyCap is spec §7's HARVEST_BODY_CAP: the max body size the
	// coordinator retains off a piggybacked candidate response for parsing
	// (spec §2).
	HarvestBodyCap = 512 * 1024

	// JSMaxBytes is spec §7's JS_MAX_BYTES: the max size of a JS bundle (or
	// the SPA-pivot root page) fetched and mined by the JS harvester (spec
	// §4).
	JSMaxBytes = 2 * 1024 * 1024

	// MaxNewDirsPerBatch is spec §7's MAX_NEW_DIRS_PER_BATCH: the
	// blast-radius ceiling on how many directories brand-new to the tree a
	// single injected seed batch may materialize (spec §5, contract I).
	MaxNewDirsPerBatch = 500
)

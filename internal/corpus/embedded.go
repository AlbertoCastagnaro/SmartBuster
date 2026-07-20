package corpus

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
)

// embeddedFS bundles the zero-setup minimal corpus (a hand-curated
// raft-small-ish + a few CMS lists, spec §2) and the default source map
// `corpus build` falls back to when --source-map isn't given (spec §3).
//
//go:embed embedded/lists/*.txt embedded/sourcemap.yaml embedded/default_sourcemap.yaml
var embeddedFS embed.FS

// Default builds the zero-setup in-memory corpus DB from the embedded
// minimal wordlists + source map (spec §2: "An embedded minimal corpus
// ... is bundled via embed.FS so the tool works with zero setup").
func Default() (*sql.DB, error) {
	sub, err := fs.Sub(embeddedFS, "embedded")
	if err != nil {
		return nil, fmt.Errorf("corpus: embedded fs: %w", err)
	}
	smData, err := fs.ReadFile(sub, "sourcemap.yaml")
	if err != nil {
		return nil, fmt.Errorf("corpus: embedded source map: %w", err)
	}
	sm, err := ParseSourceMap(smData)
	if err != nil {
		return nil, fmt.Errorf("corpus: embedded source map: %w", err)
	}

	db, err := Open(":memory:")
	if err != nil {
		return nil, err
	}
	if _, err := Ingest(db, sub, "", sm, HashBytes(smData)); err != nil {
		db.Close()
		return nil, fmt.Errorf("corpus: embedded ingest: %w", err)
	}
	return db, nil
}

// DefaultSourceMap returns the bundled default source map for
// `smartbuster corpus build` against a real SecLists checkout (spec §3),
// used when the operator doesn't pass --source-map.
func DefaultSourceMap() (*SourceMap, error) {
	data, err := embeddedFS.ReadFile("embedded/default_sourcemap.yaml")
	if err != nil {
		return nil, fmt.Errorf("corpus: default source map: %w", err)
	}
	return ParseSourceMap(data)
}

// DefaultSourceMapBytes returns the raw bytes of the bundled default
// source map, so a caller building a DB can hash them for ingest_meta the
// same way it would hash a user-supplied sourcemap.yaml.
func DefaultSourceMapBytes() ([]byte, error) {
	return embeddedFS.ReadFile("embedded/default_sourcemap.yaml")
}

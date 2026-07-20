package corpus

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver, registers as "sqlite"
)

const schemaDDL = `
CREATE TABLE IF NOT EXISTS terms (
  id      INTEGER PRIMARY KEY,
  term    TEXT NOT NULL,
  type    INTEGER NOT NULL,
  weight  REAL NOT NULL,
  UNIQUE(term, type)
);
CREATE TABLE IF NOT EXISTS term_tags (
  term_id INTEGER NOT NULL REFERENCES terms(id),
  tag     TEXT NOT NULL,
  PRIMARY KEY(term_id, tag)
);
CREATE TABLE IF NOT EXISTS ingest_meta (
  key TEXT PRIMARY KEY, value TEXT
);
CREATE INDEX IF NOT EXISTS idx_term_tags_tag ON term_tags(tag);
CREATE INDEX IF NOT EXISTS idx_terms_weight  ON terms(weight DESC);
`

// Open opens (creating if absent) a corpus DB file at path and ensures its
// schema exists. path == ":memory:" builds a transient in-memory DB, used
// for the embedded default corpus (spec §2).
func Open(path string) (*sql.DB, error) {
	if path == "" {
		path = ":memory:"
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("corpus: open %s: %w", path, err)
	}
	// modernc.org/sqlite serializes internally per-connection; a single
	// connection avoids "database is locked" errors on concurrent writers
	// during ingest and keeps read/write ordering simple for the read-only
	// scan-time path too.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaDDL); err != nil {
		db.Close()
		return nil, fmt.Errorf("corpus: migrate %s: %w", path, err)
	}
	return db, nil
}

// SetMeta upserts a key/value pair into ingest_meta (spec §3 step 6).
func SetMeta(db *sql.DB, key, value string) error {
	_, err := db.Exec(`INSERT INTO ingest_meta(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// GetMeta reads a value from ingest_meta; ok is false if key is absent.
func GetMeta(db *sql.DB, key string) (value string, ok bool, err error) {
	err = db.QueryRow(`SELECT value FROM ingest_meta WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

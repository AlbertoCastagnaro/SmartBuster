package corpus

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxTermLen caps a single term's length during cleaning (spec §3 step 3).
const maxTermLen = 512

// occKey identifies one terms row: dedup and upsert both key on (term,type)
// per spec §3 step 5.
type occKey struct {
	term string
	typ  TermType
}

// IngestResult summarizes one ingestion run (spec §3 step 6).
type IngestResult struct {
	Files          int
	Terms          int
	SecListsCommit string // "" if seclistsRoot isn't a git checkout
}

// Ingest walks fsys per sm's globs, cleans and commonality-scores every
// matched line (spec §4), and upserts the result into db, deduping by
// (term,type) and unioning tags (spec §3 step 5). gitRoot, if non-empty, is
// the real OS directory fsys was rooted at — used only to look up a
// SecLists commit hash (step 6); pass "" when fsys isn't backed by a git
// checkout (e.g. the embedded corpus). sourceMapHash is recorded into
// ingest_meta for reproducibility — pass HashBytes of the raw
// sourcemap.yaml bytes the caller loaded.
func Ingest(db *sql.DB, fsys fs.FS, gitRoot string, sm *SourceMap, sourceMapHash string) (IngestResult, error) {
	tagsByKey := make(map[occKey]map[string]bool)
	filesByTerm := make(map[string]map[string]bool) // term -> set of file paths containing it, any type

	var totalFiles int
	rankPos := make(map[string]int)
	haveRank := make(map[string]bool)
	var rankListLen int

	for _, rule := range sm.Rules {
		matches, err := fs.Glob(fsys, rule.Glob)
		if err != nil {
			return IngestResult{}, fmt.Errorf("corpus: glob %q: %w", rule.Glob, err)
		}
		sort.Strings(matches)
		for _, path := range matches {
			lines, err := readCleanLines(fsys, path)
			if err != nil {
				return IngestResult{}, fmt.Errorf("corpus: read %q: %w", path, err)
			}
			totalFiles++

			// Rank signal (spec §4, primary): the *first* freq_rank list
			// encountered supplies rank_score via its line order; a second
			// freq_rank: true rule (unusual, but not forbidden) is ignored
			// for ranking so the signal stays well-defined.
			if rule.FreqRank && rankListLen == 0 {
				rankListLen = len(lines)
				for i, term := range lines {
					if !haveRank[term] {
						haveRank[term] = true
						rankPos[term] = i
					}
				}
			}

			seen := make(map[string]bool, len(lines))
			for _, term := range lines {
				if !seen[term] {
					seen[term] = true
					if filesByTerm[term] == nil {
						filesByTerm[term] = make(map[string]bool)
					}
					filesByTerm[term][path] = true
				}
				k := occKey{term: term, typ: rule.Type}
				if tagsByKey[k] == nil {
					tagsByKey[k] = make(map[string]bool)
				}
				for _, t := range rule.Tags {
					tagsByKey[k][t] = true
				}
			}
		}
	}

	if totalFiles == 0 {
		return IngestResult{}, fmt.Errorf("corpus: no files matched any source-map glob")
	}

	// Corroboration signal (spec §4): presence = distinct files containing
	// the term / totalFiles, then max-normalized across the whole term set
	// so a term corroborated by many lists (but absent from the freq_rank
	// list) still lands at a meaningful weight rather than a near-zero
	// fraction of totalFiles.
	maxPresence := 0.0
	presenceOf := make(map[string]float64, len(filesByTerm))
	for term, files := range filesByTerm {
		p := float64(len(files)) / float64(totalFiles)
		presenceOf[term] = p
		if p > maxPresence {
			maxPresence = p
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return IngestResult{}, fmt.Errorf("corpus: begin: %w", err)
	}
	defer tx.Rollback()

	nTerms, err := upsertOccurrences(tx, tagsByKey, func(term string) float64 {
		rank := 0.0
		if haveRank[term] && rankListLen > 0 {
			rank = 1 - float64(rankPos[term])/float64(rankListLen)
		}
		norm := 0.0
		if maxPresence > 0 {
			norm = presenceOf[term] / maxPresence
		}
		return clampWeight(0.7*rank + 0.3*norm)
	})
	if err != nil {
		return IngestResult{}, err
	}

	var commit string
	if gitRoot != "" {
		commit, _ = gitCommit(gitRoot)
	}
	if err := writeIngestMeta(tx, commit, sourceMapHash); err != nil {
		return IngestResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return IngestResult{}, fmt.Errorf("corpus: commit: %w", err)
	}
	return IngestResult{Files: totalFiles, Terms: nTerms, SecListsCommit: commit}, nil
}

// ImportUserList adds a user-supplied flat wordlist to db, tagged tags,
// typed typ (spec §8: `corpus import <file> --tags a,b --type dir|file`).
// Like wordlist.Load, line order is taken as the commonality signal (most
// common word first) since a hand-curated list carries no other ranking
// evidence.
func ImportUserList(db *sql.DB, path string, tags []string, typ TermType) (int, error) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	lines, err := readCleanLines(os.DirFS(dir), base)
	if err != nil {
		return 0, fmt.Errorf("corpus: import %q: %w", path, err)
	}
	if len(lines) == 0 {
		return 0, fmt.Errorf("corpus: import %q: no usable lines", path)
	}

	tagsByKey := make(map[occKey]map[string]bool, len(lines))
	tagSet := make(map[string]bool, len(tags))
	for _, t := range tags {
		tagSet[t] = true
	}
	n := len(lines)
	weights := make(map[string]float64, n)
	for i, term := range lines {
		weights[term] = clampWeight(1 - float64(i)/float64(n))
		tagsByKey[occKey{term, typ}] = tagSet
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("corpus: begin: %w", err)
	}
	defer tx.Rollback()

	count, err := upsertOccurrences(tx, tagsByKey, func(term string) float64 { return weights[term] })
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("corpus: commit: %w", err)
	}
	return count, nil
}

// upsertOccurrences writes one terms row (weight from weightOf) and its
// unioned term_tags rows per (term,type) key (spec §3 step 5: dedup by
// (term,type), union tags).
func upsertOccurrences(tx *sql.Tx, tagsByKey map[occKey]map[string]bool, weightOf func(term string) float64) (int, error) {
	upsertTerm, err := tx.Prepare(`INSERT INTO terms(term, type, weight) VALUES(?, ?, ?)
		ON CONFLICT(term, type) DO UPDATE SET weight = excluded.weight`)
	if err != nil {
		return 0, fmt.Errorf("corpus: prepare upsert term: %w", err)
	}
	defer upsertTerm.Close()

	selectID, err := tx.Prepare(`SELECT id FROM terms WHERE term = ? AND type = ?`)
	if err != nil {
		return 0, fmt.Errorf("corpus: prepare select id: %w", err)
	}
	defer selectID.Close()

	upsertTag, err := tx.Prepare(`INSERT OR IGNORE INTO term_tags(term_id, tag) VALUES(?, ?)`)
	if err != nil {
		return 0, fmt.Errorf("corpus: prepare upsert tag: %w", err)
	}
	defer upsertTag.Close()

	n := 0
	for k, tags := range tagsByKey {
		weight := weightOf(k.term)
		if _, err := upsertTerm.Exec(k.term, int(k.typ), weight); err != nil {
			return 0, fmt.Errorf("corpus: upsert term %q: %w", k.term, err)
		}
		var id int64
		if err := selectID.QueryRow(k.term, int(k.typ)).Scan(&id); err != nil {
			return 0, fmt.Errorf("corpus: lookup term %q: %w", k.term, err)
		}
		for tag := range tags {
			if _, err := upsertTag.Exec(id, tag); err != nil {
				return 0, fmt.Errorf("corpus: tag term %q: %w", k.term, err)
			}
		}
		n++
	}
	return n, nil
}

func writeIngestMeta(tx *sql.Tx, seclistsCommit, sourceMapHash string) error {
	set := func(key, value string) error {
		if value == "" {
			return nil
		}
		_, err := tx.Exec(`INSERT INTO ingest_meta(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
		return err
	}
	if err := set("seclists_commit", seclistsCommit); err != nil {
		return fmt.Errorf("corpus: ingest_meta: %w", err)
	}
	if err := set("source_map_hash", sourceMapHash); err != nil {
		return fmt.Errorf("corpus: ingest_meta: %w", err)
	}
	if err := set("ingested_at", time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("corpus: ingest_meta: %w", err)
	}
	return nil
}

// readCleanLines reads name from fsys and applies spec §3 step 3's
// cleaning: trim, drop comments/blanks, drop lines with embedded control
// characters, cap length.
func readCleanLines(fsys fs.FS, name string) ([]string, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if hasControlChar(line) {
			continue
		}
		if len(line) > maxTermLen {
			line = line[:maxTermLen]
		}
		out = append(out, line)
	}
	return out, scanner.Err()
}

func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 {
			return true
		}
	}
	return false
}

func clampWeight(w float64) float64 {
	switch {
	case w < 0.01:
		return 0.01
	case w > 1.0:
		return 1.0
	default:
		return w
	}
}

// gitCommit returns seclistsRoot's HEAD commit if it's a git checkout
// (spec §3 step 6); ok is false otherwise (not an error — most operators
// won't have cloned SecLists with a full .git history).
func gitCommit(dir string) (commit string, ok bool) {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return "", false
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// HashBytes returns the hex-encoded SHA-256 of data, used to record the
// source-map hash into ingest_meta (spec §3 step 6), mirroring
// wordlist.Hash's convention for the wordlist file.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

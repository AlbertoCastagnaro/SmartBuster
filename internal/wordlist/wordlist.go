// Package wordlist loads a flat, frequency-ordered wordlist into candidate
// templates the coordinator turns into per-directory engine.Candidates. It
// deliberately has no dependency on package engine (which itself needs to
// import wordlist), so it stays a leaf package.
package wordlist

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// EntryType is a lightweight, engine-independent mirror of the shape
// distinction engine.CandidateType needs (TypeDir vs TypeFile); the
// coordinator converts it when building Candidates.
type EntryType int

const (
	EntryDir EntryType = iota
	EntryFile
)

// Entry is one line from the wordlist: the word plus its inferred candidate
// type and commonality-derived base priority.
type Entry struct {
	Word     string
	Type     EntryType
	BasePrio float64
}

// Load reads a newline-delimited wordlist (most common word first), skipping
// blank lines and lines starting with '#'. BasePrio is the normalized
// inverse rank: rank 0 -> 1.0, decreasing for later entries.
//
// A word containing '.' is given type EntryFile (e.g. "config.php"); a word
// without a dot is EntryDir. Type is only a hint for how the request is
// shaped — actual recursion eligibility is confirmed from the live
// response, not assumed from word shape alone.
func Load(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open wordlist: %w", err)
	}
	defer f.Close()

	var words []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words = append(words, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read wordlist: %w", err)
	}
	if len(words) == 0 {
		return nil, fmt.Errorf("wordlist %q is empty", path)
	}

	n := len(words)
	entries := make([]Entry, n)
	for i, w := range words {
		typ := EntryDir
		if strings.Contains(w, ".") {
			typ = EntryFile
		}
		entries[i] = Entry{
			Word:     w,
			Type:     typ,
			BasePrio: 1.0 - float64(i)/float64(n),
		}
	}
	return entries, nil
}

// Hash returns the hex-encoded SHA-256 of the wordlist file, recorded in the
// audit log header so a run can be replayed against the exact same list.
func Hash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("hash wordlist: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

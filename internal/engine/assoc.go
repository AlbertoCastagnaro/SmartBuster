package engine

import (
	"embed"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed companions.yaml
var embeddedCompanionsFS embed.FS

type companionDoc struct {
	Groups [][]string `yaml:"groups"`
}

func loadEmbeddedCompanions() [][]string {
	data, err := embeddedCompanionsFS.ReadFile("companions.yaml")
	if err != nil {
		return nil
	}
	var doc companionDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil
	}
	return doc.Groups
}

// backupSuffixes mirrors corpus.Expand's own 6-suffix list (spec §7: "6
// backup exts"). Phase 3 generation fires from what was actually confirmed,
// not from a static sensitivity table (spec §3.2's deviation from 2b: "it
// fires even for paths the corpus didn't contain").
var backupSuffixes = []string{".bak", ".old", ".zip", ".tar.gz", ".swp", "~"}

// trailingIntRe splits a terminal segment into a prefix and its trailing
// integer, e.g. "v1" -> ("v","1"), "1" -> ("","1"). Covers both plain
// sequences and version tokens with the same rule (spec §3.2).
var trailingIntRe = regexp.MustCompile(`^(.*?)(\d+)$`)

// AssocEngine is spec §3.2: a small hand-curated companion table plus
// session-local confirmed-hit tracking, used both to reweight existing
// frontier candidates (assocSignal) and to generate new ones on a hit
// (Generate).
type AssocEngine struct {
	companions map[string]map[string]bool    // term -> companion terms, both directions
	hitTerms   map[string]map[string]bool    // dir -> confirmed terminal segments
	hitExts    map[string]map[string]bool    // dir -> confirmed extensions ("extension pivot")
	mined      map[string]map[string]float64 // optional data-mined cooccurrence table
}

// NewAssocEngine loads the embedded companion table and returns a fresh,
// session-local engine.
func NewAssocEngine() *AssocEngine {
	return &AssocEngine{
		companions: expandCompanionGroups(loadEmbeddedCompanions()),
		hitTerms:   make(map[string]map[string]bool),
		hitExts:    make(map[string]map[string]bool),
	}
}

func expandCompanionGroups(groups [][]string) map[string]map[string]bool {
	out := make(map[string]map[string]bool)
	for _, g := range groups {
		for _, term := range g {
			if out[term] == nil {
				out[term] = make(map[string]bool)
			}
			for _, other := range g {
				if other != term {
					out[term][other] = true
				}
			}
		}
	}
	return out
}

// LoadMinedTable loads an optional data-mined cooccurrence(a,b,weight) table
// (spec §3.2): a future drop-in behind the same assocSignal interface, not
// required for — or used by — Phase 3 itself (no such dataset exists yet).
// Format: one "a,b,weight" CSV row per line.
func (a *AssocEngine) LoadMinedTable(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("assoc: load mined table: %w", err)
	}
	table := make(map[string]map[string]float64)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) != 3 {
			continue
		}
		w, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64)
		if err != nil {
			continue
		}
		term, other := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if table[term] == nil {
			table[term] = make(map[string]float64)
		}
		table[term][other] = w
	}
	a.mined = table
	return nil
}

// RecordHit records dir's confirmed terminal segment and extension (spec
// §3.2's "session-local" tracking). The caller gates this to qualifying
// hits (spec §5).
func (a *AssocEngine) RecordHit(dir, path string) {
	seg := terminalSegment(path)
	if a.hitTerms[dir] == nil {
		a.hitTerms[dir] = make(map[string]bool)
	}
	a.hitTerms[dir][seg] = true
	if ext := extOf(path); ext != "" {
		if a.hitExts[dir] == nil {
			a.hitExts[dir] = make(map[string]bool)
		}
		a.hitExts[dir][ext] = true
	}
}

// assocSignal is spec §3.2's reweight: 1.0 if c is a companion (via the
// hand-curated table, or the optional mined table) of a confirmed hit in the
// same directory; a smaller "extension pivot" boost (spec §3.2's "found
// admin.php -> reinforce .php expansion for pending stems") if c shares an
// extension with a confirmed hit in the same directory without being a
// companion; 0 otherwise.
func (a *AssocEngine) assocSignal(c *Candidate) float64 {
	seg := terminalSegment(c.Path)
	if hits, ok := a.hitTerms[c.ParentDir]; ok {
		for term := range hits {
			if a.companions[term][seg] {
				return 1.0
			}
			if a.mined != nil {
				if w := a.mined[term][seg]; w > 0 {
					if w > 1 {
						w = 1
					}
					return w
				}
			}
		}
	}
	if ext := extOf(c.Path); ext != "" {
		if exts, ok := a.hitExts[c.ParentDir]; ok && exts[ext] {
			return 0.6
		}
	}
	return 0
}

// Generate is spec §3.2's candidate generation: on a confirmed hit, produce
// backups (for file hits) and bounded sibling/sequence candidates, for the
// coordinator to dedupe against the frontier and enqueue. Generated
// candidates inherit cand's ParentDir/Depth/Tags.
func (a *AssocEngine) Generate(cand Candidate) []Candidate {
	var out []Candidate
	if cand.Type == TypeFile {
		out = append(out, backupsFor(cand)...)
	}
	out = append(out, siblingsFor(cand)...)
	return out
}

// backupsFor emits <path>.bak/.old/.zip/.tar.gz/.swp/~ for any confirmed
// file hit (spec §3.2: "<name> -> <name>.bak", broader than 2b's
// sensitive-stem-only static generation).
func backupsFor(cand Candidate) []Candidate {
	out := make([]Candidate, 0, len(backupSuffixes))
	for _, suf := range backupSuffixes {
		out = append(out, Candidate{
			Path: cand.Path + suf, Type: TypeFile, BasePrio: cand.BasePrio,
			Tags: cand.Tags, Depth: cand.Depth, ParentDir: cand.ParentDir,
			Provenance: "generated:backup:" + cand.Path,
		})
	}
	return out
}

// siblingsFor emits up to GenSiblingBound sibling candidates for a hit whose
// terminal segment ends in a trailing integer (spec §3.2): plain sequences
// ("1" -> "2".."6") and version tokens ("v1" -> "v2".."v6") both match the
// same prefix+digits rule. Zero-padding is preserved when present.
func siblingsFor(cand Candidate) []Candidate {
	m := trailingIntRe.FindStringSubmatch(cand.Path)
	if m == nil {
		return nil
	}
	prefix, numStr := m[1], m[2]
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return nil
	}
	width := len(numStr)

	out := make([]Candidate, 0, GenSiblingBound)
	for i := 1; i <= GenSiblingBound; i++ {
		s := strconv.Itoa(n + i)
		if len(s) < width {
			s = strings.Repeat("0", width-len(s)) + s
		}
		out = append(out, Candidate{
			Path: prefix + s, Type: cand.Type, BasePrio: cand.BasePrio,
			Tags: cand.Tags, Depth: cand.Depth, ParentDir: cand.ParentDir,
			Provenance: "generated:sibling:" + cand.Path,
		})
	}
	return out
}

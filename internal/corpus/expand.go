package corpus

import (
	"sort"
	"strings"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
)

// stemDecayFactor is the per-extra-extension decay applied when a stem
// expands to multiple files, so the first (stack-primary) extension in
// p.ExtensionsForStack() ranks above later, less-likely ones (spec §6).
const stemDecayFactor = 0.9

// BackupSensitiveBoost is spec §6's BACKUP_SENSITIVE_BOOST default: a
// readable backup of a sensitive file is worth trying above its peers.
const BackupSensitiveBoost = 1.3

// backupSuffixes are appended directly to a sensitive file's path (spec
// §6), e.g. "config.php" -> "config.php.bak", "config.php~".
var backupSuffixes = []string{".bak", ".old", ".zip", ".tar.gz", ".swp", "~"}

// sensitiveStems are sensitive regardless of extension (spec §6): a term
// matches by its filename stem (before the first '.').
var sensitiveStems = map[string]bool{
	"config": true, "settings": true, "database": true, "backup": true,
}

// sensitiveFullNames are sensitive as an exact filename (spec §6): these
// don't reduce to one of sensitiveStems by stripping an extension.
var sensitiveFullNames = map[string]bool{
	".env": true, "wp-config.php": true, "web.config": true,
}

// stackNeutralExtensions mirrors profile package's generic+backup
// extension sets (asserted stable by profile/types_test.go's
// ExtensionsForStack DoD table). Anything ExtensionsForStack() returns
// beyond this set means a real backend stack was detected (spec §6:
// "only when a backend stack is detected").
var stackNeutralExtensions = map[string]bool{
	"": true, ".html": true, ".txt": true, ".json": true,
	".bak": true, ".old": true, ".zip": true, ".tar.gz": true, ".swp": true, ".~": true,
}

// Expand runs spec §6 on Select's raw output: ambiguous generic-word
// dot-heuristic reclassification, stem x ExtensionsForStack expansion,
// backup-file generation for sensitive names, and final dedup (collapsing
// duplicate Paths across layers, keeping max Score and unioning
// tags/provenance).
func Expand(cands []Candidate, p *profile.TargetProfile, techBoostW float64) []Candidate {
	exts := p.ExtensionsForStack()
	stackDetected := hasBackendStack(exts)

	var out []Candidate
	for _, c := range cands {
		effType := c.Type
		isGeneric := containsTag(c.Tags, "generic")
		if isGeneric && (c.Type == TypeDir || c.Type == TypeFile) {
			// spec §6: directory-list terms mix dir- and file-shaped words
			// under one declared source-map type; re-derive the real shape
			// from the word itself (Phase 1's dot heuristic) rather than
			// trusting the blanket per-glob type.
			if strings.Contains(c.Path, ".") {
				effType = TypeFile
			} else {
				effType = TypeDir
			}
		}

		switch effType {
		case TypeFile, TypeFullPath:
			cc := c
			cc.Type = effType
			out = append(out, cc)
			out = append(out, backupsFor(cc, p, techBoostW)...)

		case TypeDir:
			cc := c
			cc.Type = TypeDir
			out = append(out, cc)
			if isGeneric && stackDetected {
				// spec §6: only when a backend stack is detected, also try
				// an extensionless generic word as a stem (config ->
				// config.php) — additive, the dir form is still tried too.
				out = append(out, expandStem(c, exts, p, techBoostW)...)
			}

		case TypeStem:
			out = append(out, expandStem(c, exts, p, techBoostW)...)
		}
	}
	return dedup(out)
}

// expandStem emits stem+ext for every ext in exts (spec §6), each
// inheriting BasePrio decayed by its position so the stack-primary
// extension (first in exts) ranks above later ones, plus any backup
// variants of the resulting file.
func expandStem(c Candidate, exts []string, p *profile.TargetProfile, techBoostW float64) []Candidate {
	out := make([]Candidate, 0, len(exts))
	decay := 1.0
	for _, ext := range exts {
		basePrio := c.BasePrio * decay
		cc := Candidate{
			Path:       c.Path + ext,
			Type:       TypeFile,
			BasePrio:   basePrio,
			Score:      Score(basePrio, c.Tags, p, techBoostW),
			Tags:       c.Tags,
			Provenance: c.Provenance + ":stem",
		}
		out = append(out, cc)
		out = append(out, backupsFor(cc, p, techBoostW)...)
		decay *= stemDecayFactor
	}
	return out
}

// backupsFor emits <path>.bak/.old/.zip/.tar.gz/.swp/~ for a sensitive
// file candidate, boosted by BackupSensitiveBoost (spec §6).
func backupsFor(c Candidate, p *profile.TargetProfile, techBoostW float64) []Candidate {
	if c.Type != TypeFile || !isSensitive(c.Path) {
		return nil
	}
	basePrio := c.BasePrio * BackupSensitiveBoost
	out := make([]Candidate, 0, len(backupSuffixes))
	for _, suf := range backupSuffixes {
		out = append(out, Candidate{
			Path:       c.Path + suf,
			Type:       TypeFile,
			BasePrio:   basePrio,
			Score:      Score(basePrio, c.Tags, p, techBoostW),
			Tags:       c.Tags,
			Provenance: c.Provenance + ":backup",
		})
	}
	return out
}

func isSensitive(path string) bool {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	if sensitiveFullNames[base] {
		return true
	}
	stem := base
	if i := strings.Index(base, "."); i >= 0 {
		stem = base[:i]
	}
	return sensitiveStems[stem]
}

func hasBackendStack(exts []string) bool {
	for _, e := range exts {
		if !stackNeutralExtensions[e] {
			return true
		}
	}
	return false
}

func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// dedup collapses duplicate Paths (spec §6 last bullet): the surviving
// candidate keeps the max Score (and the Type/BasePrio that produced it),
// with tags and provenance unioned across every contributing occurrence.
func dedup(cands []Candidate) []Candidate {
	order := make([]string, 0, len(cands))
	best := make(map[string]Candidate, len(cands))
	tagSet := make(map[string]map[string]bool, len(cands))
	provSet := make(map[string]map[string]bool, len(cands))

	anyDir := make(map[string]bool, len(cands))
	for _, c := range cands {
		cur, ok := best[c.Path]
		if !ok {
			order = append(order, c.Path)
			best[c.Path] = c
			tagSet[c.Path] = map[string]bool{}
			provSet[c.Path] = map[string]bool{}
		} else if c.Score > cur.Score {
			cur.Score, cur.BasePrio, cur.Type = c.Score, c.BasePrio, c.Type
			best[c.Path] = cur
		}
		if c.Type == TypeDir {
			anyDir[c.Path] = true
		}
		for _, t := range c.Tags {
			tagSet[c.Path][t] = true
		}
		if c.Provenance != "" {
			provSet[c.Path][c.Provenance] = true
		}
	}

	out := make([]Candidate, 0, len(order))
	for _, path := range order {
		c := best[path]
		// A path that was ever tried as a directory must stay recursion-
		// eligible even if a same-path file/stem expansion (e.g. the bare
		// "" extension reproducing the dir's own path) scored higher —
		// losing TypeDir here would silently and permanently disable
		// recursion into a path that's genuinely a directory.
		if anyDir[path] {
			c.Type = TypeDir
		}
		c.Tags = sortedKeys(tagSet[path])
		c.Provenance = strings.Join(sortedKeys(provSet[path]), ", ")
		out = append(out, c)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

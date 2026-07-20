package corpus

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
)

// DefaultTechBoostW is spec §5.4 / §8's TECH_BOOST_W default.
const DefaultTechBoostW = 2.0

// SelectConfig configures corpus.Select (spec §5, §8).
type SelectConfig struct {
	DB            *sql.DB
	MaxCandidates int     // Select LIMIT; 0 = unbounded (medium corpus default, spec §5.2)
	TechBoostW    float64 // spec TECH_BOOST_W; <= 0 defaults to DefaultTechBoostW
}

// Select queries db for every term tagged with p's backend-layer tech tags,
// "generic", or "backup" (spec §5.1-§5.4), weight-ordered and optionally
// bounded. Candidates come back BasePrio/Score set but not yet
// extension-expanded or backup-generated — call Expand next (spec §10's
// build order keeps selection and expansion as separate stages).
//
// Deviation from the spec's `func Select(p *profile.TargetProfile, cfg
// SelectConfig) []Candidate` pseudocode signature: Select is DB-backed, so
// it also returns an error, and the DB handle travels via cfg.DB rather
// than an implicit global — see the Phase 3 handoff note (spec §10).
func Select(p *profile.TargetProfile, cfg SelectConfig) ([]Candidate, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("corpus: Select: cfg.DB is nil")
	}
	techBoostW := cfg.TechBoostW
	if techBoostW <= 0 {
		techBoostW = DefaultTechBoostW
	}

	tags := tagSetFor(p)
	placeholders := make([]string, len(tags))
	args := make([]interface{}, len(tags))
	for i, t := range tags {
		placeholders[i] = "?"
		args[i] = t
	}

	query := fmt.Sprintf(`
		SELECT t.term, t.type, t.weight, group_concat(DISTINCT tt.tag) AS tags
		FROM terms t JOIN term_tags tt ON tt.term_id = t.id
		WHERE tt.tag IN (%s)
		GROUP BY t.id
		ORDER BY t.weight DESC`, strings.Join(placeholders, ","))
	if cfg.MaxCandidates > 0 {
		query += fmt.Sprintf(" LIMIT %d", cfg.MaxCandidates)
	}

	rows, err := cfg.DB.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("corpus: select: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var term string
		var typ int
		var weight float64
		var tagCSV string
		if err := rows.Scan(&term, &typ, &weight, &tagCSV); err != nil {
			return nil, fmt.Errorf("corpus: select: scan: %w", err)
		}
		termTags := strings.Split(tagCSV, ",")
		out = append(out, Candidate{
			Path:       term,
			Type:       TermType(typ),
			BasePrio:   weight,
			Score:      Score(weight, termTags, p, techBoostW),
			Tags:       termTags,
			Provenance: "corpus:" + strings.Join(termTags, "+"),
		})
	}
	return out, rows.Err()
}

// Score is spec §5.4/§7's shared formula: Score = BasePrio * (1 +
// TECH_BOOST_W * MatchScore(Tags)). Exported so engine's Reprioritize hook
// (applyMatchScore) recomputes Score identically after profile refinement
// instead of duplicating the formula (spec §7).
func Score(basePrio float64, tags []string, p *profile.TargetProfile, techBoostW float64) float64 {
	return basePrio * (1 + techBoostW*p.MatchScore(tags))
}

// tagSetFor builds spec §5.1's selection tag set: every backend/unknown-
// layer tech's name and category (split into lowercase words too, so a
// multi-word tech name like "Apache Tomcat" still matches the single-word
// corpus tags "apache"/"tomcat"), unioned with "generic" (always selected)
// and "backup" (backups are worth trying regardless of detected stack).
// Edge-layer techs (proxies/CDNs) contribute nothing, matching
// TargetProfile.ExtensionsForStack's own layer gate (spec §4.9).
func tagSetFor(p *profile.TargetProfile) []string {
	seen := map[string]bool{"generic": true, "backup": true}
	tags := []string{"generic", "backup"}
	add := func(s string) {
		s = strings.ToLower(s)
		if s != "" && !seen[s] {
			seen[s] = true
			tags = append(tags, s)
		}
	}
	for _, t := range p.Tech {
		if t.Layer == profile.LayerEdge {
			continue
		}
		add(t.Name)
		add(t.Category)
		for _, w := range tagWords(t.Name) {
			add(w)
		}
	}
	return tags
}

// tagWords splits s into lowercase alphanumeric words, e.g. "Apache Tomcat"
// -> ["apache", "tomcat"].
func tagWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

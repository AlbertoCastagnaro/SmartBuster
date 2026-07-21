package engine

import (
	"math"
	"strings"
)

// markovAlphabetSize is the Laplace-smoothing denominator: bytes range over
// 256 values, so this is a conservative (if slightly loose, since real path
// text uses far fewer distinct bytes) prior over the alphabet size.
const markovAlphabetSize = 256

// MarkovModel is the online character-level n-gram naming-convention model
// (spec §3.3): trained on the terminal path segment of confirmed,
// high-confidence hits, order chars of context predicting the next
// character.
type MarkovModel struct {
	order      int
	minSamples int
	counts     map[string]map[byte]int
	totals     map[string]int
	trained    int
}

// NewMarkovModel builds an untrained model. order<=0 and minSamples<=0 fall
// back to the spec §7 defaults.
func NewMarkovModel(order, minSamples int) *MarkovModel {
	if order <= 0 {
		order = DefaultMarkovOrder
	}
	if minSamples <= 0 {
		minSamples = DefaultMarkovMinSamples
	}
	return &MarkovModel{
		order:      order,
		minSamples: minSamples,
		counts:     make(map[string]map[byte]int),
		totals:     make(map[string]int),
	}
}

// pad brackets segment with `order` start markers and one end marker, so the
// model also learns start-of-segment and end-of-segment structure, not just
// interior digrams.
func (m *MarkovModel) pad(segment string) string {
	return strings.Repeat("^", m.order) + segment + "$"
}

// Train updates n-gram counts from one confirmed terminal segment (spec
// §3.3). The caller is responsible for gating this to high-confidence,
// non-wildcard/SPA hits (spec §5) — Train itself has no opinion on that.
func (m *MarkovModel) Train(segment string) {
	if segment == "" {
		return
	}
	s := m.pad(segment)
	for i := 0; i+m.order < len(s); i++ {
		ctx := s[i : i+m.order]
		next := s[i+m.order]
		if m.counts[ctx] == nil {
			m.counts[ctx] = make(map[byte]int)
		}
		m.counts[ctx][next]++
		m.totals[ctx]++
	}
	m.trained++
}

// convSignal returns segment's likelihood under the model, in [0,1] (spec
// §3.3). Cold-start guard: neutral 0 until minSamples confirmed segments
// have been trained — a model trained on a couple of paths is noise.
func (m *MarkovModel) convSignal(segment string) float64 {
	if m.trained < m.minSamples || segment == "" {
		return 0
	}
	s := m.pad(segment)
	var sumLogP float64
	n := 0
	for i := 0; i+m.order < len(s); i++ {
		ctx := s[i : i+m.order]
		next := s[i+m.order]
		total := m.totals[ctx]
		c := m.counts[ctx][next]
		p := float64(c+1) / float64(total+markovAlphabetSize) // Laplace smoothing
		sumLogP += math.Log(p)
		n++
	}
	if n == 0 {
		return 0
	}
	// exp(mean log-probability) is the geometric-mean per-character
	// likelihood: each factor is a probability in (0,1), so the product —
	// and thus this — is already bounded in (0,1) with no extra squashing.
	return math.Exp(sumLogP / float64(n))
}

package engine

import (
	"container/heap"
	"math/rand"
	"sort"
)

// Frontier is a max-heap of Candidates ordered by Score (highest first).
// Phase 1 Score = BasePrio; Reprioritize is the hook later signal sources
// (tech weighting, co-occurrence, ...) plug into without changing this API.
type Frontier struct {
	items candidateHeap
}

func NewFrontier() *Frontier {
	f := &Frontier{}
	heap.Init(&f.items)
	return f
}

func (f *Frontier) Push(c Candidate) {
	heap.Push(&f.items, c)
}

// Pop removes and returns the highest-Score candidate. Panics if empty;
// callers must check Len()/Empty() first.
func (f *Frontier) Pop() Candidate {
	return heap.Pop(&f.items).(Candidate)
}

func (f *Frontier) Len() int {
	return f.items.Len()
}

func (f *Frontier) Empty() bool {
	return f.items.Len() == 0
}

// Reprioritize applies fn to every queued candidate in place and re-heapifies.
// Unused in Phase 1.
func (f *Frontier) Reprioritize(fn func(*Candidate)) {
	for i := range f.items {
		fn(&f.items[i])
	}
	heap.Init(&f.items)
}

// SampleMidTier removes and returns a candidate sampled uniformly from the
// frontier's middle third by Score (spec §4's ε-greedy exploration): pure
// greedy descent always takes the max, which can tunnel-vision into one
// branch, so this gives the coordinator a way to occasionally pick
// something else instead. ok is false only if the frontier is empty.
func (f *Frontier) SampleMidTier(rng *rand.Rand) (Candidate, bool) {
	n := len(f.items)
	if n == 0 {
		return Candidate{}, false
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return f.items[order[i]].Score > f.items[order[j]].Score })

	lo, hi := n/3, (2*n)/3
	if hi <= lo {
		lo, hi = 0, n
	}
	idx := order[lo+rng.Intn(hi-lo)]
	c := f.items[idx]
	heap.Remove(&f.items, idx)
	return c, true
}

// orderJitterBand is spec §5's "near-equal scores" tolerance for
// PopBand: candidates scoring within this fraction of the current max are
// eligible for the seeded shuffle, not just the single highest-scoring one.
const orderJitterBand = 0.05

// PopBand pops a candidate chosen uniformly at random (from rng) among
// those currently within orderJitterBand of the frontier's max Score (spec
// §5's OrderJitter): a seeded tie-break among near-equal top candidates,
// not a full reshuffle — priority dispatch still goes to the top tier, just
// not always to literally the single highest scorer within it. Falls back
// to a strict Pop when there's nothing to jitter between (0 or 1 items, or
// a non-positive max score, where a fractional band isn't meaningful).
func (f *Frontier) PopBand(rng *rand.Rand, bandFrac float64) Candidate {
	n := len(f.items)
	if n <= 1 {
		return f.Pop()
	}
	max := f.items[0].Score // heap root: always the current max
	if max <= 0 {
		return f.Pop()
	}
	thresh := max * (1 - bandFrac)
	band := make([]int, 0, 4)
	for i, it := range f.items {
		if it.Score >= thresh {
			band = append(band, i)
		}
	}
	idx := band[rng.Intn(len(band))]
	c := f.items[idx]
	heap.Remove(&f.items, idx)
	return c
}

// UpdateMatching finds the still-queued candidate at (dir, path) — if any —
// applies fn to it in place, and re-heapifies just that element. Returns
// false if no such candidate is currently queued (already dispatched, or
// never existed): the caller then falls back to enqueueing a new one. This
// is the mid-scan seed merge's landing point (spec §0 contract B, §5): a
// linear scan is fine here since it only runs for a late-arriving seed's
// dedup check, not the hot dispatch path.
func (f *Frontier) UpdateMatching(dir, path string, fn func(*Candidate)) bool {
	for i := range f.items {
		if f.items[i].ParentDir == dir && f.items[i].Path == path {
			fn(&f.items[i])
			heap.Fix(&f.items, i)
			return true
		}
	}
	return false
}

// All returns a copy of every resident candidate, in no particular order
// (Phase 5a session save, spec §6: the frontier's full serializable form).
func (f *Frontier) All() []Candidate {
	return append(candidateHeap(nil), f.items...)
}

// RemoveMatching removes every queued candidate for which pred returns true
// and returns them (spec §4.1's exclude override: swept out of the frontier
// immediately, not just denylisted going forward). Re-heapifies once at the
// end rather than per-removal, since a single exclude call can match many
// candidates.
func (f *Frontier) RemoveMatching(pred func(Candidate) bool) []Candidate {
	var removed []Candidate
	kept := make(candidateHeap, 0, len(f.items))
	for _, c := range f.items {
		if pred(c) {
			removed = append(removed, c)
		} else {
			kept = append(kept, c)
		}
	}
	f.items = kept
	heap.Init(&f.items)
	return removed
}

// TopK returns a copy of the k highest-Score queued candidates without
// mutating the heap (spec §3's frontier.snapshot sampler calls this
// periodically and must not disturb dispatch order).
func (f *Frontier) TopK(k int) []Candidate {
	items := append(candidateHeap(nil), f.items...)
	sort.Slice(items, func(i, j int) bool { return items[i].Score > items[j].Score })
	if len(items) > k {
		items = items[:k]
	}
	return items
}

type candidateHeap []Candidate

func (h candidateHeap) Len() int            { return len(h) }
func (h candidateHeap) Less(i, j int) bool  { return h[i].Score > h[j].Score } // max-heap
func (h candidateHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *candidateHeap) Push(x interface{}) { *h = append(*h, x.(Candidate)) }

func (h *candidateHeap) Pop() interface{} {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

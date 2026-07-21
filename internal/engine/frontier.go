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

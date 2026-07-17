package engine

import "container/heap"

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

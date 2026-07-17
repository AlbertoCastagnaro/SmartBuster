package engine

import "testing"

func TestFrontierPopsHighestScoreFirst(t *testing.T) {
	f := NewFrontier()
	f.Push(Candidate{Path: "low", Score: 0.1})
	f.Push(Candidate{Path: "high", Score: 0.9})
	f.Push(Candidate{Path: "mid", Score: 0.5})

	order := []string{"high", "mid", "low"}
	for _, want := range order {
		if f.Empty() {
			t.Fatalf("frontier emptied early, expected %q next", want)
		}
		got := f.Pop()
		if got.Path != want {
			t.Fatalf("got %q, want %q", got.Path, want)
		}
	}
	if !f.Empty() {
		t.Fatal("expected frontier empty after draining all pushes")
	}
}

func TestFrontierLenTracksPushPop(t *testing.T) {
	f := NewFrontier()
	if f.Len() != 0 {
		t.Fatalf("expected 0, got %d", f.Len())
	}
	f.Push(Candidate{Path: "a", Score: 1})
	f.Push(Candidate{Path: "b", Score: 2})
	if f.Len() != 2 {
		t.Fatalf("expected 2, got %d", f.Len())
	}
	f.Pop()
	if f.Len() != 1 {
		t.Fatalf("expected 1, got %d", f.Len())
	}
}

func TestFrontierReprioritize(t *testing.T) {
	f := NewFrontier()
	f.Push(Candidate{Path: "a", Score: 0.1})
	f.Push(Candidate{Path: "b", Score: 0.2})

	// Flip scores so "a" should now come out first.
	f.Reprioritize(func(c *Candidate) {
		if c.Path == "a" {
			c.Score = 0.9
		}
	})

	got := f.Pop()
	if got.Path != "a" {
		t.Fatalf("expected reprioritized candidate 'a' first, got %q", got.Path)
	}
}

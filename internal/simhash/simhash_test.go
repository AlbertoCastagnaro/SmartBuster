package simhash

import (
	"strings"
	"testing"
)

func shingles(text string) []string {
	words := strings.Fields(text)
	if len(words) < 2 {
		return words
	}
	out := make([]string, 0, len(words)-1)
	for i := 0; i < len(words)-1; i++ {
		out = append(out, words[i]+" "+words[i+1])
	}
	return out
}

func TestIdenticalInputsProduceZeroDistance(t *testing.T) {
	a := SimHash(shingles("the quick brown fox jumps over the lazy dog"))
	b := SimHash(shingles("the quick brown fox jumps over the lazy dog"))
	if a != b {
		t.Fatalf("expected identical fingerprints, got %x vs %x", a, b)
	}
	if d := Hamming(a, b); d != 0 {
		t.Fatalf("expected 0 hamming distance, got %d", d)
	}
}

func TestSimilarInputsAreClose(t *testing.T) {
	a := SimHash(shingles("error 404 page not found on this server today"))
	b := SimHash(shingles("error 404 page not found on this server yesterday"))
	d := Hamming(a, b)
	if d > 12 {
		t.Fatalf("expected small hamming distance for near-identical text, got %d", d)
	}
}

func TestDissimilarInputsAreFar(t *testing.T) {
	a := SimHash(shingles("error 404 page not found on this server"))
	b := SimHash(shingles("welcome to the admin dashboard control panel overview"))
	d := Hamming(a, b)
	if d < 10 {
		t.Fatalf("expected large hamming distance for unrelated text, got %d", d)
	}
}

func TestEmptyInputIsDeterministic(t *testing.T) {
	a := SimHash(nil)
	b := SimHash(nil)
	if a != b || a != 0 {
		t.Fatalf("expected empty input to always produce 0, got %x and %x", a, b)
	}
}

func TestHammingSelfIsZero(t *testing.T) {
	a := SimHash(shingles("some arbitrary content for hashing purposes"))
	if Hamming(a, a) != 0 {
		t.Fatalf("hamming distance to self must be 0")
	}
}

func TestWeightingByFrequencyShiftsFingerprint(t *testing.T) {
	// A shingle repeated many times should pull the fingerprint measurably
	// toward its own bit pattern versus a single occurrence.
	base := shingles("alpha beta gamma delta epsilon zeta")
	repeated := append([]string{}, base...)
	for i := 0; i < 20; i++ {
		repeated = append(repeated, "alpha beta")
	}
	h1 := SimHash(base)
	h2 := SimHash(repeated)
	if h1 == h2 {
		t.Fatalf("expected repeated shingle to change the fingerprint")
	}
}

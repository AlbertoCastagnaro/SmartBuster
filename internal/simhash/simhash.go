// Package simhash implements a 64-bit Charikar SimHash over word shingles,
// used to compare response bodies by similarity rather than exact match.
package simhash

import (
	"math/bits"

	"github.com/cespare/xxhash/v2"
)

// SimHash computes a 64-bit fingerprint over the given shingles. Duplicate
// shingles must appear once per occurrence in the slice (not deduplicated)
// so that a shingle's frequency naturally weights its contribution to each
// bit's vote, per the Charikar construction.
func SimHash(shingles []string) uint64 {
	var votes [64]int
	for _, s := range shingles {
		h := xxhash.Sum64String(s)
		for i := 0; i < 64; i++ {
			if h&(1<<uint(i)) != 0 {
				votes[i]++
			} else {
				votes[i]--
			}
		}
	}
	var fp uint64
	for i := 0; i < 64; i++ {
		if votes[i] > 0 {
			fp |= 1 << uint(i)
		}
	}
	return fp
}

// Hamming returns the Hamming distance between two fingerprints.
func Hamming(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

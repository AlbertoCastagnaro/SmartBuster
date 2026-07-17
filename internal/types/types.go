// Package types holds the data-model structs shared between calibration
// and engine (and, from Phase 2 on, other analysis subsystems that need to
// read a ResponseSignature without depending on calibration's algorithms).
//
// It exists specifically to avoid an import cycle: calibration.Calibrate
// and calibration.Classify need these types, and engine already depends on
// calibration (worker.go calls Normalize), so the types can't live in
// engine. An earlier version defined them directly in package calibration
// and had engine re-export them as aliases — that worked, but conflated
// "calibration the algorithm" with "calibration the type owner," and would
// have forced any future package that only wants ResponseSignature (e.g. a
// Phase 2 tech-detection package reading headers/bodies) into a logic
// dependency on calibration it doesn't need. This package has no logic of
// its own, only data, so both calibration and engine can sit above it
// without a cycle, and so can whatever Phase 2 adds.
package types

import "time"

// ResponseSignature: compact per-response fingerprint. Computed IN THE WORKER
// (normalization needs the requested path; keeps large bodies off the channel).
type ResponseSignature struct {
	Status      int
	BodyLen     int
	WordCount   int
	SimHash     uint64 // over the normalized body
	RawBodyHash uint64 // xxhash of normalized body, for exact dedup
	RedirectTo  string // normalized redirect target ("" if none)
	ContentType string
	SetCookie   bool
	Reflected   bool // requested token echoed in body (diagnostic)
	Elapsed     time.Duration

	// NormBody is the normalized body text, populated only for calibration
	// probes (Phase 2a addition; see internal/profile's handoff report for
	// why). Left empty for ordinary candidate responses so the hot path
	// stays exactly as compact as Phase 1 designed it.
	NormBody string
}

// Baseline: learned "not found" profile for one directory.
type Baseline struct {
	Dir         string
	Samples     []ResponseSignature
	NoiseFloor  int // max intra-baseline Hamming + margin
	LenMean     float64
	LenStdDev   float64
	RepStatus   int // representative not-found status
	RepSimHash  uint64
	RepRedirect string
	IsWildcard  bool
	IsSPA       bool

	// RepBody is the representative (medoid) probe's normalized body text —
	// Phase 2a's error-page tech fingerprint (spec §4.6) reuses it instead
	// of making an extra request. See ResponseSignature.NormBody.
	RepBody string
}

type Classification struct {
	IsHit      bool
	Confidence float64 // [0,1]
	Reason     string
}

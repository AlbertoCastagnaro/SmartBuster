package engine

import (
	"github.com/AlbertoCastagnaro/SmartBuster/internal/types"
)

type CandidateType int

const (
	TypeDir CandidateType = iota
	TypeFile
	TypeStem // extension appended at runtime (Phase 2)
	TypeFullPath
)

// Candidate: one path to test.
type Candidate struct {
	Path       string // relative to base, e.g. "admin" or "config.php"
	Type       CandidateType
	BasePrio   float64 // commonality prior (Phase 1: normalized wordlist rank)
	Score      float64 // effective frontier priority
	Depth      int
	ParentDir  string // directory this lives under; "" = root
	Provenance string // "wordlist" | "recursion:/admin" | ...
}

// ResponseSignature, Baseline, and Classification are defined in the leaf
// package internal/types (see its doc comment) and aliased here so the
// field-level contract matches spec §2 from every caller's view, and so
// engine's own extensive use of these bare names is unchanged.
type ResponseSignature = types.ResponseSignature
type Baseline = types.Baseline
type Classification = types.Classification

type Finding struct {
	URL         string
	Status      int
	Size        int
	Confidence  float64
	Provenance  string
	ContentHash uint64
	Aliases     []string
}

// Channel messages.
type WorkItem struct {
	Candidate Candidate
	URL       string
	IsProbe   bool   // calibration probe vs. real candidate
	ProbeTag  string // groups probes belonging to one directory
}

type WorkResult struct {
	Item      WorkItem
	Signature ResponseSignature
	Err       error
}

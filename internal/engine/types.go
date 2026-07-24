package engine

import (
	"net/http"

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
	BasePrio   float64 // commonality prior (Phase 1: normalized wordlist rank; Phase 2b: corpus weight)
	Score      float64 // effective frontier priority
	Depth      int
	ParentDir  string   // directory this lives under; "" = root
	Provenance string   // "wordlist" | "recursion:/admin" | "corpus:php+wordpress" | ...
	Tags       []string // corpus tech tags (Phase 2b spec §0 contract D); nil/["generic"] for -w candidates
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
	Candidate      Candidate
	URL            string
	IsProbe        bool   // calibration probe vs. real candidate
	ProbeTag       string // groups probes belonging to one directory
	IsHarvestFetch bool   // Phase 4b: a JS-bundle or SPA-pivot root fetch requested by a harvest producer (spec §4, §5) — dispatched through the same paced pipeline as ordinary candidates, routed by content-type once the result comes back (see handleHarvestFetchResult)

	// Headers is the full request header set (profile + referer, spec §5),
	// computed once by the coordinator at construction time (buildHeaders)
	// so the worker stays a stateless (item, network) -> WorkResult
	// executor — it never reads mode/profile/referer policy itself, only
	// forwards whatever the coordinator already decided.
	Headers http.Header
}

type WorkResult struct {
	Item      WorkItem
	Signature ResponseSignature
	Err       error
}

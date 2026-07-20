// Package corpus is the Phase 2b tagged corpus store: a SQLite-backed
// database of terms (directories/files/stems/fullpaths) tagged by the tech
// stack they belong to, a commonality weight, and the selection/expansion
// logic that turns a profile.TargetProfile into scored candidates.
//
// Like internal/wordlist, this is a leaf package: it does not import
// package engine (which imports corpus for selection — see spec §0
// contract E), so engine converts corpus.Candidate into engine.Candidate
// the same way it already converts wordlist.Entry.
package corpus

// TermType mirrors engine.CandidateType's ordering (dir,file,stem,fullpath)
// without depending on package engine; the DB's terms.type column stores
// these same integer values (spec §2 DDL comment).
type TermType int

const (
	TypeDir TermType = iota
	TypeFile
	TypeStem
	TypeFullPath
)

// Candidate is one selected/expanded term ready to be turned into an
// engine.Candidate (spec §5.3, §6). ParentDir and Depth are filled in by
// the caller (engine), since Select has no notion of where in the scan
// tree it's being seeded — the same division of responsibility
// wordlist.Entry has today.
type Candidate struct {
	Path       string
	Type       TermType
	BasePrio   float64
	Score      float64
	Tags       []string
	Provenance string // "corpus:" + join(tags), unioned across dedup (spec §6)
}

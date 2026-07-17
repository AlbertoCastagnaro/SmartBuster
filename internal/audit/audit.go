// Package audit writes the append-only JSONL audit log: one line per
// request (probes included), written for every result before any
// UI/console output (spec §11). This file is the system of record and the
// eval instrumentation.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
)

// Writer implements engine.AuditSink.
type Writer struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// New creates (or truncates) the audit log at path.
func New(path string) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create audit log: %w", err)
	}
	return &Writer{file: f, enc: json.NewEncoder(f)}, nil
}

func (w *Writer) Close() error {
	return w.file.Close()
}

// Header is the run-start record: config, target(s), RNG seed, wordlist
// path + hash, and tool version, so a run can be replayed (spec §11).
// UserAgent is recorded once here rather than duplicated on every entry
// line, since Phase 1 uses one constant User-Agent for the whole run.
type Header struct {
	Type         string    `json:"type"`
	TS           time.Time `json:"ts"`
	Version      string    `json:"version"`
	Targets      []string  `json:"targets"`
	Wordlist     string    `json:"wordlist"`
	WordlistHash string    `json:"wordlist_hash"`
	UserAgent    string    `json:"user_agent"`
	Seed         int64     `json:"seed"`
	Concurrency  int       `json:"concurrency"`
	Rate         float64   `json:"rate"`
	Jitter       float64   `json:"jitter"`
	MaxDepth     int       `json:"max_depth"`
	RequestTOMs  int64     `json:"request_timeout_ms"`
	DryRun       bool      `json:"dry_run"`
}

// ReadHeader reads back the first line of an audit log at path as a
// Header, so a run can be reconstructed and replayed from just the log
// file (spec §11) — the counterpart to WriteHeader.
func ReadHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, fmt.Errorf("read audit header: %w", err)
	}
	defer f.Close()

	var h Header
	if err := json.NewDecoder(f).Decode(&h); err != nil {
		return Header{}, fmt.Errorf("read audit header: %w", err)
	}
	if h.Type != "header" {
		return Header{}, fmt.Errorf("read audit header: first line of %q is not a header record", path)
	}
	return h, nil
}

func (w *Writer) WriteHeader(h Header) error {
	h.Type = "header"
	if h.TS.IsZero() {
		h.TS = time.Now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(h)
}

// entry mirrors the per-request JSONL schema from spec §11.
type entry struct {
	TS         time.Time        `json:"ts"`
	Method     string           `json:"method"`
	URL        string           `json:"url"`
	Status     int              `json:"status"`
	Size       int              `json:"size"`
	ElapsedMS  int64            `json:"elapsed_ms"`
	IsProbe    bool             `json:"is_probe"`
	ParentDir  string           `json:"parent_dir"`
	Provenance string           `json:"provenance"`
	Classified *classifiedEntry `json:"classified,omitempty"`
	SimHash    string           `json:"sim_hash"`
	RawHash    string           `json:"raw_hash"`
	Error      string           `json:"error,omitempty"`
}

type classifiedEntry struct {
	IsHit       bool    `json:"is_hit"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
	BaselineDir string  `json:"baseline_dir"`
	Hamming     int     `json:"hamming"`
	NoiseFloor  int     `json:"noise_floor"`
}

// WriteRequest implements engine.AuditSink. It never returns an error or
// panics on a write failure — the audit log must not be able to crash a
// scan — instead surfacing the failure on stderr.
func (w *Writer) WriteRequest(rec engine.AuditRecord) {
	ts := rec.Time
	if ts.IsZero() {
		ts = time.Now()
	}
	e := entry{
		TS:         ts,
		Method:     rec.Method,
		URL:        rec.URL,
		Status:     rec.Signature.Status,
		Size:       rec.Signature.BodyLen,
		ElapsedMS:  rec.Signature.Elapsed.Milliseconds(),
		IsProbe:    rec.IsProbe,
		ParentDir:  rec.ParentDir,
		Provenance: rec.Provenance,
		SimHash:    fmt.Sprintf("0x%x", rec.Signature.SimHash),
		RawHash:    fmt.Sprintf("0x%x", rec.Signature.RawBodyHash),
	}
	if rec.Err != nil {
		e.Error = rec.Err.Error()
	}
	if rec.Classified != nil {
		e.Classified = &classifiedEntry{
			IsHit:       rec.Classified.IsHit,
			Confidence:  rec.Classified.Confidence,
			Reason:      rec.Classified.Reason,
			BaselineDir: rec.BaselineDir,
			Hamming:     rec.Hamming,
			NoiseFloor:  rec.NoiseFloor,
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(e); err != nil {
		fmt.Fprintf(os.Stderr, "audit: write failed: %v\n", err)
	}
}

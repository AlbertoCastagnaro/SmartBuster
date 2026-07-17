package engine

import "time"

type EventType string

const (
	EventScanStarted     EventType = "scan.started"
	EventCalibrationDone EventType = "calibration.done"
	EventHit             EventType = "hit"
	EventWarning         EventType = "warning"
	EventThrottle        EventType = "throttle"
	EventTrapDetected    EventType = "trap.detected"
	EventBranchPruned    EventType = "branch.pruned"
	EventScanFinished    EventType = "scan.finished"
	EventError           EventType = "error"
)

// Event is the coordinator's single typed event stream (concept.md §6): the
// audit log persists all of it, results are distilled from it, and (Phase 5)
// a UI would tap a sampled subset of it.
type Event struct {
	Type       EventType
	Time       time.Time
	Dir        string
	URL        string
	Confidence float64
	Message    string
}

// EventEmitter receives engine events synchronously; calls must be fast.
type EventEmitter interface {
	Emit(Event)
}

type EventFunc func(Event)

func (f EventFunc) Emit(e Event) { f(e) }

type noopEmitter struct{}

func (noopEmitter) Emit(Event) {}

// AuditRecord carries everything the audit log needs for one request (spec
// §11). WriteRequest is called for EVERY result, probe or real, before any
// UI/console output.
type AuditRecord struct {
	Time        time.Time
	Method      string
	URL         string
	IsProbe     bool
	ParentDir   string
	Provenance  string
	Signature   ResponseSignature
	Err         error
	Classified  *Classification // nil for probes: no baseline exists yet
	BaselineDir string
	Hamming     int
	NoiseFloor  int
}

// AuditSink receives one AuditRecord per request. Defined here, rather than
// depending on the concrete package audit, so engine never imports audit;
// cmd wires the two together at startup. This avoids an audit<->engine
// import cycle (audit's writer needs engine's types to serialize them).
type AuditSink interface {
	WriteRequest(AuditRecord)
}

type noopAuditSink struct{}

func (noopAuditSink) WriteRequest(AuditRecord) {}

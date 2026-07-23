package engine

import (
	"encoding/json"
	"time"
)

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

	// EventTechDetected and EventWAFDetected are Phase 2a additions (spec
	// §0 contract G): emitted once after the provisional profile is built,
	// and again after root calibration refines it (favicon/error-page/
	// active-probe signals, nmap merge).
	EventTechDetected EventType = "tech.detected"
	EventWAFDetected  EventType = "waf.detected"

	// EventSPAPivot is Phase 4b's SPA pivot (spec §4): fired once, when root
	// calibrates as an SPA, right before brute-force is deprioritized and
	// root's script bundles get harvested. URL carries the target.
	EventSPAPivot EventType = "spa.pivot"

	// EventStats and EventSnapshot are Phase 5a additions (spec §3): a
	// telemetry heartbeat and a periodic top-K frontier sample, both emitted
	// from the coordinator's own select loop (single-writer-safe) so a
	// daemon UI has something to poll besides discrete discovery events.
	EventStats    EventType = "stats"
	EventSnapshot EventType = "frontier.snapshot"
)

// Category is spec §3 decision #2's structural grouping: every event
// carries exactly one, so a UI can group/filter without parsing Message
// prefixes. Warnings additionally carry a WarnPayload naming their source.
type Category string

const (
	CategoryScan        Category = "scan"
	CategoryCalibration Category = "calibration"
	CategoryDiscovery   Category = "discovery"
	CategoryTech        Category = "tech"
	CategoryTrap        Category = "trap"
	CategoryTelemetry   Category = "telemetry"
	CategoryWarning     Category = "warning"
	CategoryError       Category = "error"
	CategoryControl     Category = "control"
)

// eventCategories is the single source of truth mapping EventType ->
// Category (spec §3): c.emit consults this so every call site gets a
// correct Category without having to set it by hand. EventWarning is
// deliberately absent — every warning is emitted through sendWarning/
// c.emit(Event{..., Category: CategoryWarning, ...}) at its call site
// because it also needs a WarnPayload, which this map can't provide.
var eventCategories = map[EventType]Category{
	EventScanStarted:     CategoryScan,
	EventScanFinished:    CategoryScan,
	EventCalibrationDone: CategoryCalibration,
	EventHit:             CategoryDiscovery,
	EventSPAPivot:        CategoryDiscovery,
	EventTechDetected:    CategoryTech,
	EventWAFDetected:     CategoryTech,
	EventTrapDetected:    CategoryTrap,
	EventBranchPruned:    CategoryTrap,
	EventStats:           CategoryTelemetry,
	EventSnapshot:        CategoryTelemetry,
	EventWarning:         CategoryWarning,
	EventError:           CategoryError,
	EventThrottle:        CategoryControl,
}

// WarnPayload names an EventWarning's source (spec §3 decision #2): the
// human Message stays for display, but the UI groups/filters warnings on
// this instead of parsing message prefixes.
type WarnPayload struct {
	Source string `json:"source"` // "robots"|"sitemap"|"wayback"|"nmap"|"corpus"|"headless"|"seed.capped"|"spa"|"profile"
}

// StatsPayload is the telemetry heartbeat (spec §3), emitted on
// STATS_INTERVAL by a ticker in the coordinator's dispatch loop.
type StatsPayload struct {
	ReqSent, Hits, InFlight, FrontierLen, DirsScanning int
	ReqPerSec, HitRate                                 float64
	ElapsedMs, ETAms                                   int64 // ETA from remaining frontier / current rate; -1 if unbounded
}

// SnapshotPayload is the periodic top-K frontier sample (spec §3), emitted
// on SNAPSHOT_INTERVAL by a sampler in the coordinator's dispatch loop.
type SnapshotPayload struct {
	TopK  []SnapshotEntry
	Total int
}

type SnapshotEntry struct {
	Path       string
	Dir        string
	Provenance string
	Score      float64
	Depth      int
}

// ErrorPayload carries a request error's detail (spec §3 decision #3):
// AuditRecord.Err remains the lossless record; this is the stream signal.
type ErrorPayload struct {
	URL     string
	Kind    string // "timeout"|"connreset"|"tls"|"other"
	Message string
}

// HitPayload is the `hit` event's structured detail (phase 5b follow-up:
// the wire Event carried Confidence but no Provenance/Status/Size, even
// though Finding — the internal record a hit becomes — has all three; the
// 5b web UI could only render a provenance tag by opportunistically
// correlating a hit's URL against recent frontier.snapshot sightings,
// which is necessarily partial (snapshots sample only the top 25 every
// ~1s). This carries the same fields Finding does, straight from the
// Candidate/response that produced the hit, for both the canonical and
// alias case — an alias is still a real, distinctly-dispatched request
// with its own candidate/response.
type HitPayload struct {
	Provenance string
	Status     int
	Size       int
}

// payloadFor marshals v (always one of the typed payload structs above, all
// trivially marshalable) into Event.Payload. An error here would mean a
// non-marshalable type was passed by mistake — a programmer error, not a
// runtime condition callers need to handle — so it's silently dropped
// rather than threaded through every c.emit call site.
func payloadFor(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// TechEntry is the audit/event-friendly projection of one profile.Tech,
// kept as a plain struct (rather than importing package profile's type
// directly into the event/audit surface) so engine and audit don't need to
// depend on profile's internals beyond what they display.
type TechEntry struct {
	Name       string
	Category   string
	Version    string
	Confidence float64
	Layer      string
	Sources    []string
	RuleIDs    []string
}

// Event is the coordinator's single typed event stream (concept.md §6): the
// audit log persists all of it, results are distilled from it, and (Phase 5)
// a daemon fans a sampled subset of it out to WS clients — this struct's
// json tags ARE the wire format (spec §3), so they're part of the frozen
// protocol Phase 5b renders against, not just an internal convenience.
type Event struct {
	Type       EventType `json:"type"`
	Category   Category  `json:"category"` // spec §3 decision #2: structural grouping, never inferred from Message
	Time       time.Time `json:"time"`
	Dir        string    `json:"dir,omitempty"`
	URL        string    `json:"url,omitempty"`
	Confidence float64   `json:"confidence,omitempty"`
	Message    string    `json:"message,omitempty"`

	// Tech and WAF carry the tech.detected / waf.detected payload (spec
	// §0 contract G); zero-valued for every other event type.
	Tech []TechEntry `json:"tech,omitempty"`
	WAF  string      `json:"waf,omitempty"`

	// Payload is a typed struct (WarnPayload/StatsPayload/SnapshotPayload/
	// ErrorPayload/HitPayload) for the event types that carry one (spec §3
	// NEW); empty for every other event type.
	Payload json.RawMessage `json:"payload,omitempty"`
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

// TechAuditSink is an optional extension an AuditSink may implement to
// persist tech-detection provenance (spec §6: "surfaced in tech.detected
// and the audit log"). Kept separate from AuditSink so existing sinks
// don't need to implement it.
type TechAuditSink interface {
	WriteTechProfile(host string, tech []TechEntry, waf string)
}

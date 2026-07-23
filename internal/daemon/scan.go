package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

// ScanState is a scan's daemon-tracked lifecycle state — distinct from
// (and coarser than) the coordinator's own internal paused/dispatching
// state, which isn't queryable from outside the coordinator goroutine.
type ScanState string

const (
	ScanRunning  ScanState = "running"
	ScanPaused   ScanState = "paused"
	ScanStopped  ScanState = "stopped"
	ScanFinished ScanState = "finished"
)

// ScanStatus is GET /api/scans/{id}'s response body (spec §4).
type ScanStatus struct {
	ID         string     `json:"id"`
	Target     string     `json:"target"`
	State      ScanState  `json:"state"`
	Seed       int64      `json:"seed"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Findings   int        `json:"findings"`
	Mode       string     `json:"mode,omitempty"`
}

// Scan is one tracked scan: its coordinator, its dedicated event hub (spec
// §4: each scan gets its own WS stream, so §4's GET .../{id}/events can
// upgrade into exactly that scan's events, not a daemon-wide firehose), and
// the daemon-tracked lifecycle state REST handlers read/mutate.
type Scan struct {
	ID     string
	Target string
	Config engine.Config

	co        *engine.Coordinator
	hub       *Hub
	hubCancel context.CancelFunc

	mu       sync.Mutex
	state    ScanState
	started  time.Time
	finished time.Time
	mode     string
}

func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Control submits cmd to the scan's coordinator (spec §4 contract C: the
// only mutation path, applied exclusively on the coordinator goroutine) and
// updates the daemon-tracked ScanState to match — pause/resume/stop are the
// only kinds that change it; the rest (adjust/pin/exclude/boost/demote/
// inject) don't affect lifecycle state.
func (s *Scan) Control(ctx context.Context, cmd engine.ControlCmd) error {
	if err := s.co.SubmitControl(ctx, cmd); err != nil {
		return err
	}
	s.mu.Lock()
	switch cmd.Kind {
	case engine.CtrlPause:
		s.state = ScanPaused
	case engine.CtrlResume:
		if s.state == ScanPaused {
			s.state = ScanRunning
		}
	case engine.CtrlStop:
		s.state = ScanStopped
	case engine.CtrlAdjust:
		if cmd.SetMode != nil {
			s.mode = *cmd.SetMode
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Scan) Status() ScanStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ScanStatus{
		ID: s.ID, Target: s.Target, State: s.state, Seed: s.Config.Seed,
		StartedAt: s.started, Findings: len(s.co.Findings()), Mode: s.mode,
	}
	if !s.finished.IsZero() {
		f := s.finished
		st.FinishedAt = &f
	}
	return st
}

// ScanManager tracks every scan this daemon process has started, by id.
type ScanManager struct {
	mu    sync.Mutex
	scans map[string]*Scan
}

func NewScanManager() *ScanManager {
	return &ScanManager{scans: make(map[string]*Scan)}
}

// newHub builds a hub and starts its Run loop, tied to its own cancellable
// context — shared by Start and Resume so both wire the coordinator->hub
// path (spec §2 contract B) identically.
func newHub() (*Hub, context.CancelFunc) {
	hub := NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	return hub, cancel
}

// Start builds a coordinator wired to its own hub (spec §2 contract B) and
// runs it in a background goroutine, returning immediately with a Scan
// handle — POST /api/scans returns {id} without waiting for the scan to
// finish.
func (m *ScanManager) Start(target string, wl []wordlist.Entry, cfg engine.Config, sc *scope.Scope, auditSink engine.AuditSink) (*Scan, error) {
	id, err := randomID()
	if err != nil {
		return nil, fmt.Errorf("generate scan id: %w", err)
	}

	hub, hubCancel := newHub()
	opts := []engine.Option{engine.WithEventEmitter(hub.NewEmitter())}
	if auditSink != nil {
		opts = append(opts, engine.WithAuditSink(auditSink))
	}
	co, err := engine.NewCoordinator(target, wl, cfg, sc, opts...)
	if err != nil {
		hubCancel()
		return nil, err
	}

	scan := &Scan{ID: id, Target: target, Config: cfg, co: co, hub: hub, hubCancel: hubCancel, state: ScanRunning, started: time.Now()}
	m.adopt(scan, auditSink)
	return scan, nil
}

// Resume rebuilds a coordinator from a saved SessionState (spec §6) and
// tracks it exactly like a freshly-Start-ed scan, under a new scan id (the
// session id and the scan id it resumes into are deliberately distinct —
// one session can be resumed more than once).
func (m *ScanManager) Resume(state engine.SessionState, wl []wordlist.Entry, sc *scope.Scope, auditSink engine.AuditSink) (*Scan, error) {
	id, err := randomID()
	if err != nil {
		return nil, fmt.Errorf("generate scan id: %w", err)
	}

	hub, hubCancel := newHub()
	opts := []engine.Option{engine.WithEventEmitter(hub.NewEmitter())}
	if auditSink != nil {
		opts = append(opts, engine.WithAuditSink(auditSink))
	}
	co, err := engine.NewCoordinatorFromSnapshot(state, wl, sc, opts...)
	if err != nil {
		hubCancel()
		return nil, err
	}

	scan := &Scan{ID: id, Target: state.Target, Config: state.Config, co: co, hub: hub, hubCancel: hubCancel, state: ScanRunning, started: time.Now()}
	m.adopt(scan, auditSink)
	return scan, nil
}

// adopt registers scan and starts the background goroutine that runs it to
// completion and updates its lifecycle state — the part Start and Resume
// share.
func (m *ScanManager) adopt(scan *Scan, auditSink engine.AuditSink) {
	m.mu.Lock()
	m.scans[scan.ID] = scan
	m.mu.Unlock()

	co, hubCancel := scan.co, scan.hubCancel
	go func() {
		co.Run(context.Background())
		hubCancel()
		if closer, ok := auditSink.(io.Closer); ok {
			closer.Close()
		}
		scan.mu.Lock()
		if scan.state != ScanStopped {
			scan.state = ScanFinished
		}
		scan.finished = time.Now()
		scan.mu.Unlock()
	}()
}

func (m *ScanManager) Get(id string) (*Scan, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.scans[id]
	return s, ok
}

func (m *ScanManager) List() []*Scan {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Scan, 0, len(m.scans))
	for _, s := range m.scans {
		out = append(out, s)
	}
	return out
}

// BuildScope defaults the allowlist to target's own host, exactly like the
// CLI's buildScope (cmd/smartbuster/main.go) — recursion can never wander
// off-host by default even for a daemon-started scan.
func BuildScope(target string, cfg engine.Config) (*scope.Scope, error) {
	sc := cfg.Scope
	if len(sc.AllowHosts) == 0 {
		u, err := url.Parse(target)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("target %q: no host", target)
		}
		sc.AllowHosts = []string{u.Hostname()}
	}
	return scope.New(sc)
}

// server.go wires the REST control plane (spec §4) on top of ScanManager,
// Security, and SessionStore. Every state-changing route requires both the
// bearer token and a matching Origin; read-only GETs require only the
// token (spec §5).
package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

type Server struct {
	mux      *http.ServeMux
	sec      *Security
	scans    *ScanManager
	sessions *SessionStore
}

func NewServer(sec *Security, sessions *SessionStore) *Server {
	s := &Server{mux: http.NewServeMux(), sec: sec, scans: NewScanManager(), sessions: sessions}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Scans exposes the ScanManager so `serve`'s CLI wiring (and tests) can
// adopt a resumed session's Scan (spec §6) without a second registry.
func (s *Server) Scans() *ScanManager { return s.scans }

func (s *Server) routes() {
	mut := func(h http.HandlerFunc) http.Handler { return s.sec.RequireToken(s.sec.RequireOrigin(h)) }
	ro := func(h http.HandlerFunc) http.Handler { return s.sec.RequireToken(h) }

	s.mux.Handle("POST /api/scans", mut(s.handleStartScan))
	s.mux.Handle("GET /api/scans", ro(s.handleListScans))
	s.mux.Handle("GET /api/scans/{id}", ro(s.handleGetScan))
	s.mux.Handle("POST /api/scans/{id}/pause", mut(s.handlePause))
	s.mux.Handle("POST /api/scans/{id}/resume", mut(s.handleResume))
	s.mux.Handle("POST /api/scans/{id}/stop", mut(s.handleStop))
	s.mux.Handle("PATCH /api/scans/{id}", mut(s.handleAdjust))
	s.mux.Handle("POST /api/scans/{id}/pin", mut(s.handlePin))
	s.mux.Handle("POST /api/scans/{id}/exclude", mut(s.handleExclude))
	s.mux.Handle("POST /api/scans/{id}/boost", mut(s.handleBoost))
	s.mux.Handle("POST /api/scans/{id}/demote", mut(s.handleDemote))
	s.mux.Handle("POST /api/scans/{id}/inject", mut(s.handleInject))
	// Token+Origin are validated inside the WS handshake itself (ws.go),
	// not this REST middleware — a browser can't attach an Authorization
	// header (or, for that matter, a custom middleware's response) to a WS
	// upgrade request the way it can a normal fetch.
	s.mux.Handle("GET /api/scans/{id}/events", s.handleEvents())
	// Phase 5b follow-up: the WS event stream is lossy by design (spec §2),
	// but until this route existed a reconnect's resync (GET .../{id}) could
	// only report a findings *count* — never rebuild the tree/findings list
	// a dropped connection actually missed. This closes that: an
	// authoritative, on-demand read of what handleHit has confirmed so far.
	s.mux.Handle("GET /api/scans/{id}/findings", ro(s.handleGetFindings))
	s.mux.Handle("POST /api/scans/{id}/save", mut(s.handleSaveScan))
	s.mux.Handle("GET /api/sessions", ro(s.handleListSessions))
	s.mux.Handle("POST /api/sessions/{id}/resume", mut(s.handleResumeSession))
	s.mux.Handle("GET /api/sessions/{id}", ro(s.handleGetSession))

	// Everything else falls through to the (5b) static asset mount — no
	// token required, same as any static web page; the UI itself carries
	// the token client-side (from the URL fragment) for its own API calls.
	s.mux.Handle("/", AssetHandler())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeControlError maps a control-plane error to its HTTP status:
// engine.ErrScanNotRunning means the scan's Coordinator.Run has already
// returned (finished/stopped) — a client retrying or racing a "finished"
// transition is a normal, expected condition, not a server fault, so it's
// 409 Conflict rather than the blanket 500 every other control error gets.
func writeControlError(w http.ResponseWriter, err error) {
	if errors.Is(err, engine.ErrScanNotRunning) {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	httpError(w, http.StatusInternalServerError, err.Error())
}

// --- POST /api/scans ---

func (s *Server) handleStartScan(w http.ResponseWriter, r *http.Request) {
	var cfg engine.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if len(cfg.Targets) == 0 {
		httpError(w, http.StatusBadRequest, "Targets must have at least one entry")
		return
	}
	target := strings.TrimRight(cfg.Targets[0], "/")

	var wl []wordlist.Entry
	if cfg.Wordlist != "" {
		var err error
		wl, err = wordlist.Load(cfg.Wordlist)
		if err != nil {
			httpError(w, http.StatusBadRequest, "wordlist: "+err.Error())
			return
		}
	}

	sc, err := BuildScope(target, cfg)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	auditSink, err := openAuditSink(cfg)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	scan, err := s.scans.Start(target, wl, cfg, sc, auditSink)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": scan.ID})
}

// --- GET /api/scans, GET /api/scans/{id} ---

func (s *Server) handleListScans(w http.ResponseWriter, r *http.Request) {
	list := s.scans.List()
	out := make([]ScanStatus, len(list))
	for i, sc := range list {
		out[i] = sc.Status()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getScanOr404(w http.ResponseWriter, r *http.Request) (*Scan, bool) {
	scan, ok := s.scans.Get(r.PathValue("id"))
	if !ok {
		httpError(w, http.StatusNotFound, "scan not found")
		return nil, false
	}
	return scan, true
}

func (s *Server) handleGetScan(w http.ResponseWriter, r *http.Request) {
	scan, ok := s.getScanOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, scan.Status())
}

// handleGetFindings serves GET /api/scans/{id}/findings: the authoritative
// findings list (engine.Coordinator.Findings, safe to call at any point —
// including after Run has returned), so a client resyncing after a WS
// reconnect can rebuild its tree/findings from something more than a count.
func (s *Server) handleGetFindings(w http.ResponseWriter, r *http.Request) {
	scan, ok := s.getScanOr404(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, scan.co.Findings())
}

// --- pause / resume / stop ---

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.simpleControl(w, r, engine.CtrlPause)
}
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.simpleControl(w, r, engine.CtrlResume)
}
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.simpleControl(w, r, engine.CtrlStop)
}

func (s *Server) simpleControl(w http.ResponseWriter, r *http.Request, kind engine.ControlKind) {
	scan, ok := s.getScanOr404(w, r)
	if !ok {
		return
	}
	if err := scan.Control(r.Context(), engine.ControlCmd{Kind: kind}); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scan.Status())
}

// --- PATCH /api/scans/{id} ---

// AdjustRequest is PATCH .../{id}'s body (spec §4): every field optional —
// nil means "leave unchanged".
type AdjustRequest struct {
	Rate        *float64 `json:"rate,omitempty"`
	Concurrency *int     `json:"concurrency,omitempty"`
	Mode        *string  `json:"mode,omitempty"`
}

func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	scan, ok := s.getScanOr404(w, r)
	if !ok {
		return
	}
	var req AdjustRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	cmd := engine.ControlCmd{Kind: engine.CtrlAdjust, SetRate: req.Rate, SetConcurrency: req.Concurrency, SetMode: req.Mode}
	if err := scan.Control(r.Context(), cmd); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scan.Status())
}

// --- pin / exclude / boost / demote ---

// PatternRequest is pin/exclude/boost/demote's shared body shape (spec
// §4.1): Factor is only meaningful for boost/demote.
type PatternRequest struct {
	Pattern string  `json:"pattern"`
	Factor  float64 `json:"factor,omitempty"`
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	s.patternControl(w, r, engine.CtrlPin)
}
func (s *Server) handleExclude(w http.ResponseWriter, r *http.Request) {
	s.patternControl(w, r, engine.CtrlExclude)
}
func (s *Server) handleBoost(w http.ResponseWriter, r *http.Request) {
	s.patternControl(w, r, engine.CtrlBoost)
}
func (s *Server) handleDemote(w http.ResponseWriter, r *http.Request) {
	s.patternControl(w, r, engine.CtrlDemote)
}

func (s *Server) patternControl(w http.ResponseWriter, r *http.Request, kind engine.ControlKind) {
	scan, ok := s.getScanOr404(w, r)
	if !ok {
		return
	}
	var req PatternRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Pattern == "" {
		httpError(w, http.StatusBadRequest, "pattern is required")
		return
	}
	cmd := engine.ControlCmd{Kind: kind, Pattern: req.Pattern, Factor: req.Factor}
	if err := scan.Control(r.Context(), cmd); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- inject ---

// InjectRequest is POST .../inject's body (spec §4): user-supplied terms,
// -> enqueueSeed with provenance "user".
type InjectRequest struct {
	Terms []string `json:"terms"`
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	scan, ok := s.getScanOr404(w, r)
	if !ok {
		return
	}
	var req InjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Terms) == 0 {
		httpError(w, http.StatusBadRequest, "terms is required")
		return
	}
	if err := scan.Control(r.Context(), engine.ControlCmd{Kind: engine.CtrlInject, Terms: req.Terms}); err != nil {
		writeControlError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- events (WS) ---

func (s *Server) handleEvents() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scan, ok := s.getScanOr404(w, r)
		if !ok {
			return
		}
		scan.hub.EventsHandler(s.sec).ServeHTTP(w, r)
	})
}

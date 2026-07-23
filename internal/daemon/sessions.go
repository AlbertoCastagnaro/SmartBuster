// sessions.go implements spec §6's session save/resume REST surface on top
// of engine's SessionState (internal/engine/session.go): a JSON file per
// session, human-inspectable (matches the audit ethos), named by an id
// that's just its filename stem.
package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

// SessionMeta is one entry of GET /api/sessions' response (spec §4): a
// lightweight projection — a session file's full SessionState can be large
// (the whole frontier + baselines), so listing doesn't force a client to
// pull all of them just to pick one.
type SessionMeta struct {
	ID      string    `json:"id"`
	Target  string    `json:"target"`
	Seed    int64     `json:"seed"`
	SavedAt time.Time `json:"saved_at"`
}

// SessionStore persists SessionState as one JSON file per session under Dir.
type SessionStore struct {
	Dir string
}

func NewSessionStore(dir string) (*SessionStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("session dir: %w", err)
	}
	return &SessionStore{Dir: dir}, nil
}

// sessionIDRe keeps a session id safely usable as a bare filename component
// — no path traversal, no surprising characters.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (s *SessionStore) path(id string) (string, error) {
	if id == "" || !sessionIDRe.MatchString(id) {
		return "", fmt.Errorf("invalid session id %q (must match [A-Za-z0-9_-]+)", id)
	}
	return filepath.Join(s.Dir, id+".json"), nil
}

// Save writes state to <Dir>/<id>.json, human-inspectable (spec §6:
// "matches the audit ethos"), overwriting any existing session of the same
// id.
func (s *SessionStore) Save(id string, state engine.SessionState) (string, error) {
	p, err := s.path(id)
	if err != nil {
		return "", err
	}
	f, err := os.Create(p)
	if err != nil {
		return "", fmt.Errorf("save session: %w", err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		return "", fmt.Errorf("save session: %w", err)
	}
	return p, nil
}

func (s *SessionStore) Get(id string) (engine.SessionState, error) {
	p, err := s.path(id)
	if err != nil {
		return engine.SessionState{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return engine.SessionState{}, fmt.Errorf("session %q: %w", id, err)
	}
	defer f.Close()
	var state engine.SessionState
	if err := json.NewDecoder(f).Decode(&state); err != nil {
		return engine.SessionState{}, fmt.Errorf("session %q: %w", id, err)
	}
	return state, nil
}

// List enumerates every saved session. A file that fails to parse (e.g.
// hand-edited into something invalid) is skipped rather than failing the
// whole listing.
func (s *SessionStore) List() ([]SessionMeta, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var out []SessionMeta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		state, err := s.Get(id)
		if err != nil {
			continue
		}
		out = append(out, SessionMeta{ID: id, Target: state.Target, Seed: state.Config.Seed, SavedAt: state.SavedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out, nil
}

// --- REST handlers ---

// SaveRequest is POST .../save's optional body (spec §4): Name defaults to
// the scan's own id when omitted.
type SaveRequest struct {
	Name string `json:"name,omitempty"`
}

func (s *Server) handleSaveScan(w http.ResponseWriter, r *http.Request) {
	scan, ok := s.getScanOr404(w, r)
	if !ok {
		return
	}
	var req SaveRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
	}
	name := req.Name
	if name == "" {
		name = scan.ID
	}

	state, err := scan.co.Save(r.Context())
	if err != nil {
		writeControlError(w, err)
		return
	}
	path, err := s.sessions.Save(name, state)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": name, "path": path})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	list, err := s.sessions.List()
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	state, err := s.sessions.Get(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "session not found")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleResumeSession(w http.ResponseWriter, r *http.Request) {
	state, err := s.sessions.Get(r.PathValue("id"))
	if err != nil {
		httpError(w, http.StatusNotFound, "session not found")
		return
	}

	var wl []wordlist.Entry
	if state.Config.Wordlist != "" {
		wl, err = wordlist.Load(state.Config.Wordlist)
		if err != nil {
			httpError(w, http.StatusBadRequest, "wordlist: "+err.Error())
			return
		}
	}
	sc, err := BuildScope(state.Target, state.Config)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	auditSink, err := openAuditSink(state.Config)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}

	scan, err := s.scans.Resume(state, wl, sc, auditSink)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": scan.ID})
}

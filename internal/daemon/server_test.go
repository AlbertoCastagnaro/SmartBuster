package daemon_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/daemon"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
	"golang.org/x/net/websocket"
)

const testOrigin = "http://127.0.0.1:9"
const testPort = 9 // arbitrary; NewSecurity just needs a fixed bind:port pair, matched below

func newTestServer(t *testing.T) (*httptest.Server, *daemon.Security, string) {
	t.Helper()
	token, err := daemon.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	sec := daemon.NewSecurity(token, "127.0.0.1", testPort)
	sessions, err := daemon.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := daemon.NewServer(sec, sessions)
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	return httpSrv, sec, token
}

func writeWordlistFile(t *testing.T, words []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "words.txt")
	if err := os.WriteFile(p, []byte(strings.Join(words, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func doReq(t *testing.T, method, url, token, origin string, body any) *http.Response {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	defer resp.Body.Close()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return v
}

// TestServer_TokenRequiredOnEveryRoute is spec §8 DoD #4: missing/wrong
// token -> 401 on REST.
func TestServer_TokenRequiredOnEveryRoute(t *testing.T) {
	httpSrv, _, token := newTestServer(t)

	cases := []struct {
		method, path string
	}{
		{"GET", "/api/scans"},
		{"POST", "/api/scans"},
		{"GET", "/api/sessions"},
	}
	for _, c := range cases {
		resp := doReq(t, c.method, httpSrv.URL+c.path, "", testOrigin, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with no token: got %d, want 401", c.method, c.path, resp.StatusCode)
		}

		resp2 := doReq(t, c.method, httpSrv.URL+c.path, "wrong-token", testOrigin, nil)
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with wrong token: got %d, want 401", c.method, c.path, resp2.StatusCode)
		}
	}

	// Sanity: the same route succeeds with the right token.
	resp := doReq(t, "GET", httpSrv.URL+"/api/scans", token, testOrigin, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/scans with a valid token: got %d, want 200", resp.StatusCode)
	}
}

// TestServer_OriginRejectedOnStateChangingRoutesOnly is spec §8 DoD #4:
// cross-Origin upgrade/REST rejected (DNS-rebinding sim) on state-changing
// calls; a mismatched Origin must NOT affect read-only GETs.
func TestServer_OriginRejectedOnStateChangingRoutesOnly(t *testing.T) {
	httpSrv, _, token := newTestServer(t)

	resp := doReq(t, "POST", httpSrv.URL+"/api/scans", token, "http://evil.example", engine.Config{Targets: []string{"http://127.0.0.1:1"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST /api/scans with a forged Origin: got %d, want 403", resp.StatusCode)
	}

	roResp := doReq(t, "GET", httpSrv.URL+"/api/scans", token, "http://evil.example", nil)
	roResp.Body.Close()
	if roResp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/scans (read-only) with a mismatched Origin: got %d, want 200 (Origin is only checked on state-changing routes)", roResp.StatusCode)
	}

	noOriginResp := doReq(t, "GET", httpSrv.URL+"/api/scans", token, "", nil)
	noOriginResp.Body.Close()
	if noOriginResp.StatusCode != http.StatusOK {
		t.Errorf("request with no Origin header at all: got %d, want 200 (non-browser clients have no Origin to forge)", noOriginResp.StatusCode)
	}
}

func startTestScan(t *testing.T, httpSrv *httptest.Server, token string, target string, words []string) string {
	t.Helper()
	cfg := engine.Config{
		Targets: []string{target}, Wordlist: writeWordlistFile(t, words),
		Seed: 1, Rate: 40, Concurrency: 3, MaxDepth: 4,
	}
	resp := doReq(t, "POST", httpSrv.URL+"/api/scans", token, testOrigin, cfg)
	if resp.StatusCode != http.StatusOK {
		body := decodeJSON[map[string]string](t, resp)
		t.Fatalf("POST /api/scans: got %d, body=%v", resp.StatusCode, body)
	}
	out := decodeJSON[map[string]string](t, resp)
	if out["id"] == "" {
		t.Fatalf("POST /api/scans: response had no id: %v", out)
	}
	return out["id"]
}

// TestServer_StartListGetScan exercises the REST scan lifecycle's read
// surface end to end.
func TestServer_StartListGetScan(t *testing.T) {
	httpSrv, _, token := newTestServer(t)
	fx := fixtures.NewHonest()
	defer fx.Close()

	id := startTestScan(t, httpSrv, token, fx.URL, []string{"admin", "backup", "noise1", "noise2"})

	getResp := doReq(t, "GET", httpSrv.URL+"/api/scans/"+id, token, testOrigin, nil)
	status := decodeJSON[daemon.ScanStatus](t, getResp)
	if status.ID != id || status.Target != fx.URL {
		t.Errorf("GET /api/scans/%s: got %+v", id, status)
	}

	listResp := doReq(t, "GET", httpSrv.URL+"/api/scans", token, testOrigin, nil)
	list := decodeJSON[[]daemon.ScanStatus](t, listResp)
	var found bool
	for _, s := range list {
		if s.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("expected scan %s in GET /api/scans list: %+v", id, list)
	}

	missResp := doReq(t, "GET", httpSrv.URL+"/api/scans/does-not-exist", token, testOrigin, nil)
	missResp.Body.Close()
	if missResp.StatusCode != http.StatusNotFound {
		t.Errorf("GET on an unknown scan id: got %d, want 404", missResp.StatusCode)
	}
}

// TestServer_PauseResumeStop exercises the control routes end to end
// through real HTTP requests (the coordinator-side pause/resume/stop
// semantics themselves are covered by control_test.go's race tests).
func TestServer_PauseResumeStop(t *testing.T) {
	httpSrv, _, token := newTestServer(t)
	fx := fixtures.NewHonest()
	defer fx.Close()

	words := make([]string, 30)
	for i := range words {
		words[i] = fmt.Sprintf("ctlword%02d", i)
	}
	id := startTestScan(t, httpSrv, token, fx.URL, words)

	pauseResp := doReq(t, "POST", httpSrv.URL+"/api/scans/"+id+"/pause", token, testOrigin, nil)
	pauseStatus := decodeJSON[daemon.ScanStatus](t, pauseResp)
	if pauseStatus.State != daemon.ScanPaused {
		t.Errorf("after /pause: state=%q, want %q", pauseStatus.State, daemon.ScanPaused)
	}

	resumeResp := doReq(t, "POST", httpSrv.URL+"/api/scans/"+id+"/resume", token, testOrigin, nil)
	resumeStatus := decodeJSON[daemon.ScanStatus](t, resumeResp)
	if resumeStatus.State != daemon.ScanRunning {
		t.Errorf("after /resume: state=%q, want %q", resumeStatus.State, daemon.ScanRunning)
	}

	stopResp := doReq(t, "POST", httpSrv.URL+"/api/scans/"+id+"/stop", token, testOrigin, nil)
	stopStatus := decodeJSON[daemon.ScanStatus](t, stopResp)
	if stopStatus.State != daemon.ScanStopped {
		t.Errorf("after /stop: state=%q, want %q", stopStatus.State, daemon.ScanStopped)
	}
}

// TestServer_AdjustPinExcludeBoostDemoteInject exercises PATCH and the
// manual-override routes' HTTP surface (request shape, 200/400 status).
func TestServer_AdjustPinExcludeBoostDemoteInject(t *testing.T) {
	httpSrv, _, token := newTestServer(t)
	fx := fixtures.NewHonest()
	defer fx.Close()
	id := startTestScan(t, httpSrv, token, fx.URL, []string{"admin", "backup", "noise1", "noise2", "noise3"})

	rate := 20.0
	concurrency := 5
	mode := "aggressive"
	patchReq, err := http.NewRequest("PATCH", httpSrv.URL+"/api/scans/"+id,
		bytes.NewReader(mustJSON(t, daemon.AdjustRequest{Rate: &rate, Concurrency: &concurrency, Mode: &mode})))
	if err != nil {
		t.Fatal(err)
	}
	patchReq.Header.Set("Authorization", "Bearer "+token)
	patchReq.Header.Set("Origin", testOrigin)
	patchResp, err := http.DefaultClient.Do(patchReq)
	if err != nil {
		t.Fatal(err)
	}
	adjustStatus := decodeJSON[daemon.ScanStatus](t, patchResp)
	if adjustStatus.Mode != mode {
		t.Errorf("PATCH mode: got %q, want %q", adjustStatus.Mode, mode)
	}

	for _, route := range []string{"pin", "exclude", "boost", "demote"} {
		body := daemon.PatternRequest{Pattern: "secretpath", Factor: 2}
		resp := doReq(t, "POST", httpSrv.URL+"/api/scans/"+id+"/"+route, token, testOrigin, body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("POST .../%s: got %d", route, resp.StatusCode)
		}
		resp.Body.Close()

		badResp := doReq(t, "POST", httpSrv.URL+"/api/scans/"+id+"/"+route, token, testOrigin, daemon.PatternRequest{})
		badResp.Body.Close()
		if badResp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST .../%s with an empty pattern: got %d, want 400", route, badResp.StatusCode)
		}
	}

	injectResp := doReq(t, "POST", httpSrv.URL+"/api/scans/"+id+"/inject", token, testOrigin, daemon.InjectRequest{Terms: []string{"hiddenendpoint"}})
	if injectResp.StatusCode != http.StatusOK {
		t.Errorf("POST .../inject: got %d", injectResp.StatusCode)
	}
	injectResp.Body.Close()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestServer_SaveListGetResumeSession exercises the whole session surface:
// save a running scan, list it, fetch it, and resume it into a brand-new
// running scan.
func TestServer_SaveListGetResumeSession(t *testing.T) {
	httpSrv, _, token := newTestServer(t)
	fx := fixtures.NewHonest()
	defer fx.Close()
	id := startTestScan(t, httpSrv, token, fx.URL, []string{"admin", "backup", "noise1", "noise2", "noise3", "noise4"})

	saveResp := doReq(t, "POST", httpSrv.URL+"/api/scans/"+id+"/save", token, testOrigin, daemon.SaveRequest{Name: "my-session"})
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("POST .../save: got %d", saveResp.StatusCode)
	}
	saveOut := decodeJSON[map[string]string](t, saveResp)
	if saveOut["id"] != "my-session" {
		t.Errorf("save response id: got %q, want %q", saveOut["id"], "my-session")
	}

	listResp := doReq(t, "GET", httpSrv.URL+"/api/sessions", token, testOrigin, nil)
	sessions := decodeJSON[[]daemon.SessionMeta](t, listResp)
	var found bool
	for _, s := range sessions {
		if s.ID == "my-session" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected my-session in GET /api/sessions: %+v", sessions)
	}

	getResp := doReq(t, "GET", httpSrv.URL+"/api/sessions/my-session", token, testOrigin, nil)
	state := decodeJSON[engine.SessionState](t, getResp)
	if state.Target != fx.URL {
		t.Errorf("GET /api/sessions/my-session: Target=%q, want %q", state.Target, fx.URL)
	}

	resumeResp := doReq(t, "POST", httpSrv.URL+"/api/sessions/my-session/resume", token, testOrigin, nil)
	if resumeResp.StatusCode != http.StatusOK {
		t.Fatalf("POST .../resume: got %d", resumeResp.StatusCode)
	}
	resumeOut := decodeJSON[map[string]string](t, resumeResp)
	newID := resumeOut["id"]
	if newID == "" || newID == id {
		t.Fatalf("expected a fresh scan id from resume, got %q (original was %q)", newID, id)
	}

	time.Sleep(50 * time.Millisecond)
	newStatusResp := doReq(t, "GET", httpSrv.URL+"/api/scans/"+newID, token, testOrigin, nil)
	newStatus := decodeJSON[daemon.ScanStatus](t, newStatusResp)
	if newStatus.State != daemon.ScanRunning && newStatus.State != daemon.ScanFinished {
		t.Errorf("resumed scan status: got %q, want running or finished", newStatus.State)
	}
}

// TestServer_WSHandshake_TokenAndOrigin is spec §8 DoD #4's WS half: wrong
// token -> reject; cross-Origin -> reject; both correct -> the client
// actually receives events. Uses a real golang.org/x/net/websocket client
// against the real handshake path (ws.go), not a fake Transport.
func TestServer_WSHandshake_TokenAndOrigin(t *testing.T) {
	httpSrv, _, token := newTestServer(t)
	fx := fixtures.NewHonest()
	defer fx.Close()
	id := startTestScan(t, httpSrv, token, fx.URL, []string{"admin", "backup", "noise1"})

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/api/scans/" + id + "/events"

	dial := func(protocol, origin string) (*websocket.Conn, error) {
		cfg, err := websocket.NewConfig(wsURL, origin)
		if err != nil {
			return nil, err
		}
		if protocol != "" {
			cfg.Protocol = []string{protocol}
		}
		return websocket.DialConfig(cfg)
	}

	if _, err := dial("wrong-token", testOrigin); err == nil {
		t.Error("expected the WS handshake to fail with a wrong token")
	}
	if _, err := dial(token, "http://evil.example"); err == nil {
		t.Error("expected the WS handshake to fail with a mismatched Origin")
	}

	conn, err := dial(token, testOrigin)
	if err != nil {
		t.Fatalf("expected the WS handshake to succeed with a valid token+Origin: %v", err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("expected to receive at least one event frame: %v", err)
	}
	var ev engine.Event
	if err := json.Unmarshal(buf[:n], &ev); err != nil {
		t.Fatalf("event frame didn't decode as engine.Event: %v (raw=%s)", err, buf[:n])
	}
	if ev.Type == "" || ev.Category == "" {
		t.Errorf("received event missing Type/Category: %+v", ev)
	}
}

// security.go implements spec §5: the daemon can launch traffic against
// arbitrary hosts, so a drive-by page triggering a scan would be a serious
// incident. Four things close that off together — loopback-only bind, a
// per-session bearer token, Origin validation, and a token delivered via a
// custom header (never a cookie) so there's no ambient credential a
// cross-origin page could ride:
//
//   - Loopback bind only: refusing a non-loopback --bind without an
//     explicit override keeps the whole attack surface off the network.
//   - Per-session random token: without it, anything that can reach
//     127.0.0.1:port (any page in your browser, by definition) could drive
//     the API. With it, only whoever was handed the token — printed once,
//     on serve — can.
//   - Origin validation is the DNS-rebinding defense specifically: a
//     malicious page can get a browser to resolve some attacker-controlled
//     hostname to 127.0.0.1 and then fetch() it, which would carry cookies/
//     ambient auth for that origin but can NEVER forge the Origin header to
//     say anything but the page's own real origin — so this still fails
//     even though the request physically lands on our loopback port.
//   - The token lives in a custom Authorization header, never a cookie:
//     cookies are sent automatically by the browser on any request to the
//     right origin (ambient), which is exactly what CSRF exploits; a
//     custom header is never attached automatically, so a cross-origin
//     page simply has no way to make an authenticated request even before
//     the Origin check runs. Origin validation is the belt to that
//     suspenders.
package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// GenerateToken returns a new 32-byte per-session random token, hex-encoded
// (spec §5): printed once on `serve` and injected into the auto-opened
// URL's fragment (`#token=...` — a fragment, not a query string, so it's
// never sent to the server or logged by it). Every REST request must then
// present it via `Authorization: Bearer <token>`; every WS request via a
// Sec-WebSocket-Protocol value equal to it (browsers can't set arbitrary
// headers on a WS handshake).
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Security holds one daemon instance's token and the Origin(s) its own
// served UI is expected to use.
type Security struct {
	Token          string
	AllowedOrigins map[string]bool
}

// NewSecurity builds Security for a daemon bound to bind:port — the only
// Origin a legitimate same-origin browser client of this daemon's own
// (5b) UI could ever present.
func NewSecurity(token, bind string, port int) *Security {
	if bind == "" {
		bind = "127.0.0.1"
	}
	origins := map[string]bool{
		fmt.Sprintf("http://%s:%d", bind, port): true,
	}
	if bind == "127.0.0.1" {
		origins[fmt.Sprintf("http://localhost:%d", port)] = true
	}
	return &Security{Token: token, AllowedOrigins: origins}
}

// checkToken validates the REST Authorization: Bearer header. Constant-time
// compare so response timing can't be used to brute-force the token
// byte-by-byte.
func (s *Security) checkToken(r *http.Request) bool {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return false
	}
	got := strings.TrimPrefix(auth, prefix)
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

// checkWSProtocolToken validates the WS handshake's Sec-WebSocket-Protocol
// token (already parsed into a single-element slice by the time a
// websocket.Server.Handshake func runs — see ws.go).
func (s *Security) checkWSProtocolToken(protocols []string) bool {
	if len(protocols) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(protocols[0]), []byte(s.Token)) == 1
}

// checkOrigin implements the DNS-rebinding defense: reject only when an
// Origin header is present AND doesn't match. A browser always sends
// Origin on a cross-origin fetch/XHR and on every WS handshake, so a
// missing Origin means a non-browser client (curl, the CLI, a test) — the
// token is that caller's real gate; Origin specifically targets the
// browser-driven attack.
func (s *Security) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	return s.AllowedOrigins[origin]
}

// RequireToken is REST middleware applied to every route (spec §5: "Every
// REST + WS request must present it").
func (s *Security) RequireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.checkToken(r) {
			http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireOrigin is REST middleware applied only to state-changing routes
// (spec §5: "Origin validation on the WS upgrade and every state-changing
// REST call").
func (s *Security) RequireOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.checkOrigin(r) {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ValidateBind implements spec §5's loopback-only bind: refuses a
// non-loopback address unless allowRemote (--i-know-this-is-remote) was
// explicitly given, and reports whether a warning should still be printed
// even when explicitly allowed.
func ValidateBind(bind string, allowRemote bool) (warn bool, err error) {
	if bind == "" || bind == "localhost" {
		return false, nil
	}
	ip := net.ParseIP(bind)
	if ip != nil && ip.IsLoopback() {
		return false, nil
	}
	if !allowRemote {
		return false, fmt.Errorf("refusing to bind non-loopback address %q without --i-know-this-is-remote (this daemon can initiate scans against arbitrary hosts; exposing its control plane off loopback is dangerous)", bind)
	}
	return true, nil
}

// daemon.go is `smartbuster serve`'s entry point (spec §2, §7): binds the
// loopback listener, generates the per-session token, and wires Security +
// SessionStore + Server together. cmd/smartbuster's serve subcommand calls
// Start then Serve.
package daemon

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

// Options configures Start (spec §7's Config additions).
type Options struct {
	Bind        string // default 127.0.0.1
	Port        int    // 0 = OS-assigned
	SessionDir  string // "" = a per-user config directory
	AllowRemote bool   // --i-know-this-is-remote
}

// Daemon is a bound, not-yet-serving `smartbuster serve` instance —
// Start separates binding (so the caller can print the URL/token) from
// Serve (which blocks accepting connections).
type Daemon struct {
	Server   *Server
	Security *Security
	Listener net.Listener
	Bind     string
	Port     int

	// URL is this daemon's own origin (http://bind:port); TokenURL is the
	// same with the session token in the fragment (spec §5: "#token=..." —
	// a fragment, never sent to the server or logged by it), exactly what
	// gets printed and, with --open, launched in a browser.
	URL      string
	TokenURL string
}

// Start validates Bind (spec §5's loopback-only rule), binds the listener,
// generates a fresh per-session token, and wires Security/SessionStore/
// Server. It does not accept connections yet — call Serve for that.
func Start(opts Options) (*Daemon, error) {
	bind := opts.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	warn, err := ValidateBind(bind, opts.AllowRemote)
	if err != nil {
		return nil, err
	}
	if warn {
		fmt.Fprintf(os.Stderr, "smartbuster: WARNING — binding to non-loopback address %q; this daemon can initiate scans against arbitrary hosts, exposing its control plane off loopback is dangerous\n", bind)
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(opts.Port)))
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		ln.Close()
		return nil, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		ln.Close()
		return nil, err
	}

	token, err := GenerateToken()
	if err != nil {
		ln.Close()
		return nil, err
	}
	sec := NewSecurity(token, bind, port)

	sessionDir := opts.SessionDir
	if sessionDir == "" {
		sessionDir = defaultSessionDir()
	}
	sessions, err := NewSessionStore(sessionDir)
	if err != nil {
		ln.Close()
		return nil, err
	}

	srv := NewServer(sec, sessions)

	url := fmt.Sprintf("http://%s:%d", bind, port)
	return &Daemon{
		Server: srv, Security: sec, Listener: ln, Bind: bind, Port: port,
		URL: url, TokenURL: url + "/#token=" + token,
	}, nil
}

// Serve blocks, accepting connections until the listener is closed or ctx
// (via a caller-managed shutdown) stops it.
func (d *Daemon) Serve() error {
	return http.Serve(d.Listener, d.Server)
}

func defaultSessionDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "smartbuster-sessions"
	}
	return filepath.Join(dir, "smartbuster", "sessions")
}

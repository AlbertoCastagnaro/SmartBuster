// Package fixtures provides the adversarial httptest servers used to
// validate calibration and trap-handling in isolation, per spec §14. Each
// constructor returns one fixture; tests spin up exactly the ones they need.
package fixtures

import (
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"
)

// Fixture wraps an httptest.Server with the set of "real" (planted) paths a
// correct scanner should find on it, for recall/false-positive assertions.
type Fixture struct {
	*httptest.Server
	RealPaths []string
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// NewHard404 returns a server whose "not found" page is a custom, static
// 200 template (status codes lie) distinct from its real pages.
func NewHard404() *Fixture {
	real := map[string]string{
		"/admin":  "<html><body>Admin dashboard: manage users, settings, and logs here.</body></html>",
		"/backup": "<html><body>Backup archive listing: nightly.tar.gz, weekly.tar.gz.</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if body, ok := real[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		io.WriteString(w, "<html><body>Oops! We could not find that page. Try searching from the homepage instead.</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewReflected404 returns a server whose 200 "not found" page echoes the
// requested path back into the body.
func NewReflected404() *Fixture {
	real := map[string]string{
		"/admin": "<html><body>Welcome to the control panel. Manage every setting from here.</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if body, ok := real[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		fmt.Fprintf(w, "<html><body>Sorry, the page %s could not be located on this server.</body></html>", r.URL.Path)
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewVolatile404 returns a server whose 200 "not found" page embeds a fresh
// timestamp and error-id token on every response.
func NewVolatile404() *Fixture {
	real := map[string]string{
		"/admin": "<html><body>Admin area. Static content, always the same.</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if body, ok := real[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		fmt.Fprintf(w, "<html><body>Page not found. Error id: %s at %s</body></html>",
			randHex(16), time.Now().UTC().Format(time.RFC3339Nano))
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewWildcardDir returns a server where /files itself and every path under
// /files/ return identical content regardless of existence; paths outside
// /files 404 normally. /files is the one real (discoverable) path — there
// is no distinguishable real path under /files/ itself.
func NewWildcardDir() *Fixture {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/files" || strings.HasPrefix(r.URL.Path, "/files/") {
			io.WriteString(w, "<html><body>File Manager: access denied uniformly for every file.</body></html>")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 not found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: []string{"/files"}}
}

// NewSPA returns a server where every path returns an identical shell page,
// simulating a client-side single-page app catch-all.
func NewSPA() *Fixture {
	shell := "<html><head><title>App</title></head><body><div id=\"root\"></div><script src=\"/app.js\"></script></body></html>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, shell)
	}))
	return &Fixture{Server: srv}
}

// NewRedirect404 returns a server that 302-redirects unknown paths to
// /login and serves real content directly for planted paths.
func NewRedirect404() *Fixture {
	real := map[string]string{
		"/admin": "<html><body>Admin dashboard content, present and real.</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := real[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, body)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewRateLimited returns a server that serves normal 404s for the first
// limit requests, then 429s every request after.
func NewRateLimited(limit int) *Fixture {
	var count int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&count, 1)
		if n > int64(limit) {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, "rate limit exceeded")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 not found</body></html>")
	}))
	return &Fixture{Server: srv}
}

// NewInfiniteDir returns a server where /loop itself, and every path under
// /loop/ at any depth, return identical 200 content — a self-similar
// infinite descent (e.g. a symlink loop).
func NewInfiniteDir() *Fixture {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loop" || strings.HasPrefix(r.URL.Path, "/loop/") {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<html><body>Directory listing: contains exactly one subdirectory.</body></html>")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 not found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: []string{"/loop"}}
}

// NewTarpit returns a server that sleeps delay before responding to every
// request, simulating a slow-loris style stall.
func NewTarpit(delay time.Duration) *Fixture {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 not found (eventually)</body></html>")
	}))
	return &Fixture{Server: srv}
}

// NewHonest returns a server with ordinary 404s and a known set of real
// paths — the control case full recall is measured against.
func NewHonest() *Fixture {
	real := map[string]string{
		"/admin":      "<html><body>Admin Panel</body></html>",
		"/backup":     "<html><body>Backup Files</body></html>",
		"/config.php": "<html><body>Config file contents</body></html>",
		"/login":      "<html><body>Login Form</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := real[r.URL.Path]; ok {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

func randHex(n int) string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hexDigits[rand.Intn(len(hexDigits))]
	}
	return string(b)
}

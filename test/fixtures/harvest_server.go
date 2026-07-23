// Fixtures for Phase 4b crawl + JS harvesting + SPA pivot (spec §8): a
// target whose real, wordlist-reachable pages link to and reference paths a
// wordlist/corpus would never guess on its own — an HTML-linked page, a JS
// bundle exposing real endpoints amid deliberate noise, an off-host link,
// and a duplicate of a path the wordlist already has (the SCANNING-branch
// merge case) — plus a separate SPA target whose only real content lives
// behind its JS bundle's endpoints (the SPA pivot's payoff), and a delayed
// CDX stub for exercising async Wayback's off-the-critical-path timing.
package fixtures

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

// NewCrawlTarget returns a target whose /admin page (an ordinary
// wordlist-reachable candidate) links to /secret-page (real, absent from
// any wordlist — the crawl_dedup/linked_paths case), /shared (real, and
// also expected to be present in the caller's own wordlist — the
// SCANNING-branch merge case), an off-host link (must never be requested),
// and a <script src="/bundle.js"> bundle. bundle.js calls fetch/axios/XHR
// against two real endpoints (/api/v1/users, /internal/status) amid
// deliberate JS noise (a bare mime-type string, a template-literal
// interpolation, and a quoted fake-regex string) that must never survive
// extraction. Everything not listed 404s normally.
func NewCrawlTarget() *Fixture {
	real := map[string]struct {
		body string
		ct   string
	}{
		"/admin": {
			`<html><body>
				<a href="/secret-page">secret</a>
				<a href="/shared">shared</a>
				<a href="http://off-host.example/elsewhere">offsite</a>
				<script src="/bundle.js"></script>
			</body></html>`,
			"text/html",
		},
		"/secret-page":     {"<html><body>found only via the /admin link, never in any wordlist</body></html>", "text/html"},
		"/shared":          {"<html><body>present in both the wordlist and a crawled link</body></html>", "text/html"},
		"/api/v1/users":    {`[{"id":1,"name":"demo"}]`, "application/json"},
		"/internal/status": {`{"ok":true}`, "application/json"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/bundle.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		io.WriteString(w, `
			fetch("/api/v1/users");
			axios.get("/internal/status");
			var xhr = new XMLHttpRequest();
			xhr.open("GET", "/internal/status");
			var mime = "application/json";
			var tpl = `+"`/api/${userId}/detail`"+`;
			var pattern = "/^[a-z0-9]+$/";
		`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if entry, ok := real[r.URL.Path]; ok {
			w.Header().Set("Content-Type", entry.ct)
			io.WriteString(w, entry.body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	})
	srv := httptest.NewServer(mux)
	return &Fixture{Server: srv, RealPaths: []string{"/admin", "/secret-page", "/shared", "/api/v1/users", "/internal/status"}}
}

// NewSPATarget returns a target that presents an identical shell for every
// path (Phase 1's SPA detection, spec §4: baseline.IsSPA) except its own
// script bundle and the two real API endpoints that bundle references —
// the SPA pivot's payoff: a target that yields zero findings from
// brute-force alone becomes enumerable once its JS is mined.
func NewSPATarget() *Fixture {
	shell := `<html><head><title>App</title></head><body><div id="root"></div><script src="/app.js"></script></body></html>`
	mux := http.NewServeMux()
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		io.WriteString(w, `fetch("/api/v1/orders"); fetch("/api/v1/profile");`)
	})
	mux.HandleFunc("/api/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[{"id":1}]`)
	})
	mux.HandleFunc("/api/v1/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"demo"}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, shell) // every other path: identical shell -> calibration.Calibrate sees IsSPA
	})
	srv := httptest.NewServer(mux)
	return &Fixture{Server: srv, RealPaths: []string{"/api/v1/orders", "/api/v1/profile"}}
}

// NewStubCDXDelayed mirrors NewStubCDX (spec §7 DoD: hermetic, no live
// archive.org dependency) but sleeps delay before responding — for
// exercising contract H: the scan must dispatch real candidates before a
// slow CDX call returns, not stall on it. served, if non-nil, is closed
// the instant the (delayed) response is about to be written, so a test can
// order-check "did a real dispatch happen before this."
func NewStubCDXDelayed(target *httptest.Server, rows [][2]string, delay time.Duration, served chan<- time.Time) *httptest.Server {
	host := strings.TrimPrefix(strings.TrimPrefix(target.URL, "http://"), "https://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		if served != nil {
			served <- time.Now()
		}
		var b strings.Builder
		b.WriteString(`[["original","timestamp"]`)
		for _, row := range rows {
			fmt.Fprintf(&b, `,["http://%s%s","%s"]`, host, row[0], row[1])
		}
		b.WriteString("]")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, b.String())
	}))
}

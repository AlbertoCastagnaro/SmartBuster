// Phase 3 dynamic-scoring fixtures (spec §9): adversarial/planted servers
// exercising each DynamicScorer signal in isolation. Same package/pattern as
// server.go — each constructor returns one Fixture, tests spin up exactly
// the ones they need.
package fixtures

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// NewProtectedAdmin returns a server whose /admin is locked (403) while
// /administrator and /admin-panel are real, discoverable pages — the
// response-semantics fixture (spec §9): a locked dir should boost its
// sensitive-tagged siblings.
func NewProtectedAdmin() *Fixture {
	real := map[string]string{
		"/administrator": "<html><body>Administrator control panel access.</body></html>",
		"/admin-panel":   "<html><body>Admin panel access granted.</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/admin" {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "<html><body>403 Forbidden</body></html>")
			return
		}
		if body, ok := real[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: append(sortedKeys(real), "/admin")}
}

// NewNamingConvention returns a server serving many "get_*" pages plus a
// planted get_secret, exercising the Markov naming-convention signal (spec
// §9): once enough get_* segments are confirmed, get_secret should score as
// recognizably "in style." Deliberately extensionless (a distinct content
// type, not text/html-file-shaped) so the fixture doesn't also trigger
// Phase 3's file-backup generation (spec §3.2) — that's a separate signal,
// covered by its own fixture (NewBackupTrigger). Each body is worded
// independently of its own path segment (not "the %s endpoint") so
// Normalize's reflected-token stripping can't collapse them all into the
// same normalized content — that would make every filler after the first
// classify as an alias instead of a genuinely new hit, and aliases never
// reach the Phase 3 learners.
func NewNamingConvention() *Fixture {
	planted := []string{
		"get_user", "get_role", "get_order", "get_item", "get_status",
		"get_price", "get_stock", "get_review", "get_cart", "get_secret",
	}
	bodies := []string{
		"Returns the current user record.",
		"Returns role assignments for the account.",
		"Returns order history for the account.",
		"Returns inventory item details.",
		"Returns background job processing status.",
		"Returns current catalog pricing.",
		"Returns warehouse stock levels.",
		"Returns customer-submitted reviews.",
		"Returns shopping cart contents.",
		"Returns a planted internal payload.",
	}
	real := make(map[string]string, len(planted))
	for i, p := range planted {
		real["/"+p] = fmt.Sprintf("<html><body>%s</body></html>", bodies[i])
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if body, ok := real[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewSequence returns a server where /api and /api/v1 are real, and
// /api/v2 is planted but reachable only via sibling/sequence generation
// (spec §9) — it isn't linked from anywhere and isn't a term any wordlist
// would try on its own.
func NewSequence() *Fixture {
	real := map[string]string{
		"/api":    "<html><body>API root.</body></html>",
		"/api/v1": "<html><body>API v1 root.</body></html>",
		"/api/v2": "<html><body>API v2 root, planted.</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if body, ok := real[strings.TrimSuffix(r.URL.Path, "/")]; ok {
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewBackupTrigger returns a server where config.php is real and
// config.php.bak is planted but reachable only via backup generation (spec
// §9) — not a term any wordlist would try on its own.
func NewBackupTrigger() *Fixture {
	real := map[string]string{
		"/config.php":     "<html><body>Config file contents.</body></html>",
		"/config.php.bak": "<html><body>Config backup, planted.</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if body, ok := real[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewCompanions returns a server where login.php is real and config.php is
// planted; the embedded companion table links them (spec §9): config.php
// should be boosted, and found faster, once login.php is confirmed.
func NewCompanions() *Fixture {
	real := map[string]string{
		"/login.php":  "<html><body>Login form.</body></html>",
		"/config.php": "<html><body>Config file, planted.</body></html>",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if body, ok := real[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewSoft404Poison returns a server whose /vault branch 200s every request
// under it (a wildcard-suspect trap, spec §9): the poisoning gate must keep
// it from training the Phase 3 learners even though individual responses
// classify as hits.
func NewSoft404Poison() *Fixture {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/vault" || strings.HasPrefix(r.URL.Path, "/vault/") {
			seg := strings.TrimPrefix(r.URL.Path, "/vault/")
			io.WriteString(w, "<html><body>Vault entry: "+seg+" - access granted uniformly for every item, nothing here is a template.</body></html>")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: []string{"/vault"}}
}

// NewOpenListing returns a server where /uploads is an open directory
// listing (spec §9): the response-semantics signal should flag it as
// high-value for recursion.
func NewOpenListing() *Fixture {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/uploads" || r.URL.Path == "/uploads/" {
			io.WriteString(w, "<html><body>Index of /uploads<br>report.pdf<br>notes.txt</body></html>")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
	return &Fixture{Server: srv, RealPaths: []string{"/uploads"}}
}

// Fixtures for Phase 4a passive seeding (spec §7): a target serving
// robots.txt + sitemap.xml with real planted paths behind them (including a
// path several levels deep, to exercise the ancestor-chain), plus a stub
// CDX endpoint so the Wayback path is tested hermetically — no live
// archive.org dependency (spec §7 DoD).
package fixtures

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// NewSeedTarget returns a target whose robots.txt Disallows a path three
// levels deep (/old/admin/secret.php — real, planted), Allows a path also
// present in an ordinary wordlist (/shared — for the dedup/provenance-union
// case), and declares a Sitemap: line pointing at a urlset with one more
// real planted path (/from-sitemap). Everything else 404s normally,
// including the unplanted "/old" and "/old/admin" prefixes themselves.
func NewSeedTarget() *Fixture {
	real := map[string]string{
		"/old/admin/secret.php": "<html><body>the historical secret file is still here</body></html>",
		"/shared":               "<html><body>present in both the wordlist and robots.txt</body></html>",
		"/from-sitemap":         "<html><body>only declared in sitemap.xml</body></html>",
	}

	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "User-agent: *\nDisallow: /old/admin/secret.php\nAllow: /shared\nSitemap: %s/sitemap.xml\n", base)
	})
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset><url><loc>%s/from-sitemap</loc></url></urlset>`, base)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if body, ok := real[r.URL.Path]; ok {
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	})
	srv := httptest.NewServer(mux)
	base = srv.URL

	return &Fixture{Server: srv, RealPaths: sortedKeys(real)}
}

// NewStubCDX returns an httptest server presenting a Wayback CDX API stub:
// it ignores the query entirely and always returns rows built from
// (path, timestamp) pairs against target's own host, so tests don't depend
// on live archive.org (spec §7 DoD: "stub-CDX fixture ... tested
// hermetically").
func NewStubCDX(target *httptest.Server, rows [][2]string) *httptest.Server {
	host := strings.TrimPrefix(strings.TrimPrefix(target.URL, "http://"), "https://")
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// Fixtures for Phase 2a target profiling (spec §9). Each constructor
// returns a plain *httptest.Server presenting one detectable stack, plus
// (where relevant) the confirmer path an active probe should hit.
package fixtures

import (
	"io"
	"net/http"
	"net/http/httptest"
)

// NewPHPApache presents Server: Apache, a PHPSESSID cookie, and .php links.
func NewPHPApache() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "Apache/2.4.41 (Ubuntu)")
		w.Header().Set("Content-Type", "text/html")
		http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "0123456789abcdef0123456789abcdef"})
		if r.URL.Path == "/" {
			io.WriteString(w, `<html><body><a href="/index.php">Home</a> <a href="/about.php">About</a></body></html>`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
}

// NewWordPress presents an X-Generator: WordPress header (deliberately the
// *only* WordPress-specific signal, so its confidence lands at a single
// source's flat value — see the Phase 2a active-probe gating test, which
// needs WordPress to start inside [ActiveProbeConfLo, ActiveProbeConfHi)
// rather than already confident from stacking multiple passive signals),
// a PHPSESSID cookie (so PHP is also detected per the DoD table), and a
// distinctive (non-404) /wp-login.php for the active-probe confirmer.
func NewWordPress() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/":
			w.Header().Set("X-Generator", "WordPress 5.8")
			http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "0123456789abcdef0123456789abcdef"})
			io.WriteString(w, `<html><body>Welcome</body></html>`)
		case "/wp-login.php":
			io.WriteString(w, `<html><body><form id="loginform"><input name="log"><input name="pwd"></form></body></html>`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, "<html><body>404 Not Found</body></html>")
		}
	}))
}

// NewDotNetIIS presents Server: IIS, X-AspNet-Version, and an .ASPXAUTH cookie.
func NewDotNetIIS() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "Microsoft-IIS/10.0")
		w.Header().Set("X-AspNet-Version", "4.0.30319")
		w.Header().Set("Content-Type", "text/html")
		http.SetCookie(w, &http.Cookie{Name: ".ASPXAUTH", Value: "0123456789ABCDEF"})
		if r.URL.Path == "/" {
			io.WriteString(w, `<html><body>Welcome</body></html>`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
}

// NewBehindCDN presents a Cloudflare edge (Server + Cf-Ray) in front of a
// PHP backend that still leaks via PHPSESSID (spec §4.9: cookies pass
// through proxies headers don't).
func NewBehindCDN() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("Cf-Ray", "7d1234567890abcd-EWR")
		w.Header().Set("Content-Type", "text/html")
		http.SetCookie(w, &http.Cookie{Name: "PHPSESSID", Value: "0123456789abcdef0123456789abcdef"})
		if r.URL.Path == "/" {
			io.WriteString(w, `<html><body>Welcome</body></html>`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
}

// NewSPAReact serves an identical shell for every path (like Phase 1's SPA
// fixture, so calibration's IsSPA still fires) but also carries a header
// signal, so the profile isn't empty even though the body never varies.
func NewSPAReact() *httptest.Server {
	shell := `<html><head><title>App</title></head><body><div id="root"></div><script src="/static/js/main.js"></script></body></html>`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Powered-By", "Express")
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, shell)
	}))
}

// KnownFaviconBody is the fixed favicon body whose mmh3 hash
// (internal/profile/rulesets/favicons.json: "fav-smartbuster-fixture-sample")
// is a known ruleset entry.
const KnownFaviconBody = "smartbuster-fixture-favicon-known-v1"

// NewFaviconKnown serves KnownFaviconBody at /favicon.ico.
func NewFaviconKnown() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			io.WriteString(w, KnownFaviconBody)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			io.WriteString(w, `<html><body>Welcome</body></html>`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 Not Found</body></html>")
	}))
}

// NewWAFChallenge presents a Cloudflare "checking your browser" interstitial
// on every path (status 503).
func NewWAFChallenge() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("Cf-Ray", "7d1234567890abcd-EWR")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `<html><body>Checking your browser before accessing this site.</body></html>`)
	}))
}

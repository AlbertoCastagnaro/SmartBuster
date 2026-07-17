package engine_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// DoD §9 assertion 8: a scan against php_apache calibrates the root with
// PHP extensions — i.e. Phase 2a actually changed calibration's probe set
// vs. Phase 1's fixed one. Verified black-box (no access to Coordinator's
// unexported extSet) via two independent signals: the tech.detected event
// reports PHP at high confidence, and at least one root calibration probe
// was actually requested with a .php extension.
func TestCoordinator_ProfilesTargetAndCalibratesRootWithStackExtensions(t *testing.T) {
	fx := fixtures.NewPHPApache()
	defer fx.Close()

	_, emitter, audit := runScan(t, fx.URL, []string{"admin", "backup"}, engine.Config{
		FaviconProbe: true,
	})

	if !emitter.has(engine.EventTechDetected) {
		t.Fatal("expected a tech.detected event")
	}
	var phpConf float64
	var sawPHP bool
	emitter.mu.Lock()
	for _, ev := range emitter.events {
		if ev.Type != engine.EventTechDetected {
			continue
		}
		for _, tech := range ev.Tech {
			if tech.Name == "PHP" {
				sawPHP = true
				if tech.Confidence > phpConf {
					phpConf = tech.Confidence
				}
			}
		}
	}
	emitter.mu.Unlock()
	if !sawPHP || phpConf < 0.8 {
		t.Fatalf("PHP not reported at high confidence in tech.detected events (saw=%v conf=%v)", sawPHP, phpConf)
	}

	audit.mu.Lock()
	defer audit.mu.Unlock()
	sawPHPProbe := false
	for _, rec := range audit.records {
		if rec.IsProbe && strings.HasSuffix(rec.URL, ".php") {
			sawPHPProbe = true
			break
		}
	}
	if !sawPHPProbe {
		t.Fatal("expected at least one root calibration probe with a .php extension (profile.ExtensionsForStack() should have fed calibration)")
	}
}

// DoD §9 assertion 4 (engine-level half): nmap http-enum seeds enter the
// frontier and are validated by calibration like any other candidate — a
// seed pointing at a real path becomes a Finding with nmap provenance; the
// wordlist deliberately does not contain that path, so it can only have
// been found via the nmap seed.
func TestCoordinator_NmapSeedsEnterFrontierAndAreValidated(t *testing.T) {
	const seedPath = "secret-nmap-only-path"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/"+seedPath {
			io.WriteString(w, "<html><body>Found via nmap seed only.</body></html>")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "<html><body>404 not found</body></html>")
	}))
	defer srv.Close()

	host := mustHost(t, srv.URL)
	nmapXML := fmt.Sprintf(`<?xml version="1.0"?>
<nmaprun>
  <host>
    <address addr="%s" addrtype="ipv4"/>
    <ports>
      <port protocol="tcp" portid="80">
        <state state="open"/>
        <service name="http" product="nginx" method="probe" conf="10"/>
        <script id="http-enum" output="&#10;  /%s: Possible admin folder&#10;"/>
      </port>
    </ports>
  </host>
</nmaprun>`, host, seedPath)

	xmlPath := filepath.Join(t.TempDir(), "scan.xml")
	if err := os.WriteFile(xmlPath, []byte(nmapXML), 0o644); err != nil {
		t.Fatal(err)
	}

	co, _, _ := runScan(t, srv.URL, []string{"admin", "backup"}, engine.Config{
		NmapFile: xmlPath,
	})

	findings := findingPaths(t, co)
	if !findings["/"+seedPath] {
		t.Fatalf("expected /%s to be found via the nmap seed; findings = %v", seedPath, findings)
	}
	found := false
	for _, f := range co.Findings() {
		if strings.HasSuffix(f.URL, seedPath) {
			found = true
			if f.Provenance != "nmap:http-enum" {
				t.Fatalf("provenance = %q, want nmap:http-enum", f.Provenance)
			}
		}
	}
	if !found {
		t.Fatal("nmap-seeded finding not present in co.Findings()")
	}
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

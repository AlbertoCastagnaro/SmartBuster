package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func readNmapFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "test", "fixtures", "nmap", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func allowAll(string) bool { return true }

// DoD §9 assertion 4: nmap XML -> correct tags (nmap-conf-scaled confidence),
// path seeds enter the frontier's input (Seeds), service target recorded.
func TestIngestNmap_Basic(t *testing.T) {
	data := readNmapFixture(t, "nmap_basic.xml")
	results, warnings, err := IngestNmap(data, allowAll)
	if err != nil {
		t.Fatalf("IngestNmap: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	r := results[0]

	if len(r.Services) != 1 || r.Services[0].Port != 80 || r.Services[0].Scheme != "http" {
		t.Fatalf("Services = %+v, want one http:80 service", r.Services)
	}

	// One vote from -sV (product, method=probe conf=10 -> NmapProbeConf),
	// one from the http-server-header script (NmapTableConf, LayerEdge).
	if len(r.Votes) != 2 {
		t.Fatalf("Votes = %+v, want 2 (the -sV product and the http-server-header script)", r.Votes)
	}
	sv := r.Votes[0]
	if sv.Name != "Apache httpd" || sv.Confidence != NmapProbeConf {
		t.Fatalf("vote[0] = %+v, want Apache httpd at NmapProbeConf (method=probe, conf=10>=8)", sv)
	}
	hdr := r.Votes[1]
	if hdr.Confidence != NmapTableConf || hdr.Layer != LayerEdge {
		t.Fatalf("vote[1] = %+v, want NmapTableConf/LayerEdge from http-server-header", hdr)
	}

	wantPaths := map[string]bool{"admin": false, "backup": false, "config.php": false}
	for _, s := range r.Seeds {
		if _, ok := wantPaths[s.Path]; ok {
			wantPaths[s.Path] = true
		}
		if s.Provenance != "nmap:http-enum" {
			t.Errorf("seed %+v has unexpected provenance", s)
		}
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Errorf("expected http-enum seed %q, got seeds=%+v", p, r.Seeds)
		}
	}
}

// DoD §9 assertion 4: multi-port XML yields multiple ServiceTargets.
func TestIngestNmap_Multiport(t *testing.T) {
	data := readNmapFixture(t, "nmap_multiport.xml")
	results, _, err := IngestNmap(data, allowAll)
	if err != nil {
		t.Fatalf("IngestNmap: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	svcs := results[0].Services
	if len(svcs) != 2 {
		t.Fatalf("Services = %+v, want 2", svcs)
	}
	byPort := map[int]ServiceTarget{}
	for _, s := range svcs {
		byPort[s.Port] = s
	}
	if byPort[80].Scheme != "http" {
		t.Errorf("port 80 scheme = %q, want http", byPort[80].Scheme)
	}
	if byPort[8443].Scheme != "https" {
		t.Errorf("port 8443 scheme = %q, want https (tunnel=ssl)", byPort[8443].Scheme)
	}
}

// DoD §9 assertion 4: out-of-scope hosts are dropped with a warning.
func TestIngestNmap_OutOfScopeHostDropped(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?>
<nmaprun>
  <host>
    <address addr="10.0.0.9" addrtype="ipv4"/>
    <ports>
      <port protocol="tcp" portid="80">
        <state state="open"/>
        <service name="http" product="nginx" method="probe" conf="10"/>
      </port>
    </ports>
  </host>
</nmaprun>`)

	results, warnings, err := IngestNmap(xml, func(host string) bool { return false })
	if err != nil {
		t.Fatalf("IngestNmap: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected the out-of-scope host to be dropped, got %+v", results)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected one warning, got %v", warnings)
	}
}

// spec §7: method=table (port-only guess) scales to NmapTableConf, not
// NmapProbeConf, even when a product string is present.
func TestIngestNmap_TableMethodScalesLowerConfidence(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?>
<nmaprun>
  <host>
    <address addr="10.0.0.1" addrtype="ipv4"/>
    <ports>
      <port protocol="tcp" portid="80">
        <state state="open"/>
        <service name="http" product="nginx" method="table" conf="3"/>
      </port>
    </ports>
  </host>
</nmaprun>`)
	results, _, err := IngestNmap(xml, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(results[0].Votes) != 1 || results[0].Votes[0].Confidence != NmapTableConf {
		t.Fatalf("Votes = %+v, want NmapTableConf", results[0].Votes)
	}
}

// spec §7: port heuristics only apply when there's no -sV product string.
func TestIngestNmap_PortHeuristicWithoutSV(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?>
<nmaprun>
  <host>
    <address addr="10.0.0.1" addrtype="ipv4"/>
    <ports>
      <port protocol="tcp" portid="8080">
        <state state="open"/>
        <service name="http-alt"/>
      </port>
    </ports>
  </host>
</nmaprun>`)
	results, _, err := IngestNmap(xml, allowAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(results[0].Votes) != 1 || results[0].Votes[0].Name != "Apache Tomcat" {
		t.Fatalf("Votes = %+v, want Apache Tomcat port heuristic for 8080", results[0].Votes)
	}
}

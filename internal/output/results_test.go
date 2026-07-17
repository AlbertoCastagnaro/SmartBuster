package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
)

func sampleFindings() []engine.Finding {
	return []engine.Finding{
		{URL: "http://t/admin", Status: 200, Size: 100, Confidence: 0.9, Provenance: "wordlist"},
		{URL: "http://t/admin/config.php", Status: 200, Size: 50, Confidence: 0.95, Provenance: "recursion:/admin"},
		{URL: "http://t/backup", Status: 301, Size: 0, Confidence: 0.8, Provenance: "wordlist", Aliases: []string{"http://t/backup2"}},
	}
}

func TestBuildTreeNestsByPath(t *testing.T) {
	root := BuildTree(sampleFindings())
	admin, ok := root.Children["admin"]
	if !ok {
		t.Fatal("expected 'admin' node at root")
	}
	if admin.Finding == nil || admin.Finding.Status != 200 {
		t.Fatalf("expected admin node to carry its finding, got %+v", admin.Finding)
	}
	config, ok := admin.Children["config.php"]
	if !ok {
		t.Fatal("expected 'config.php' nested under 'admin'")
	}
	if config.Finding == nil || config.Finding.Confidence != 0.95 {
		t.Fatalf("expected nested finding preserved, got %+v", config.Finding)
	}
	if _, ok := root.Children["backup"]; !ok {
		t.Fatal("expected 'backup' node at root")
	}
}

func TestWriteTreeRendersIndentedText(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteTree(&buf, sampleFindings()); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "admin (Status: 200)") {
		t.Errorf("expected root-level admin line, got:\n%s", out)
	}
	if !strings.Contains(out, "  config.php (Status: 200)") {
		t.Errorf("expected indented nested config.php line, got:\n%s", out)
	}
}

func TestFlatListSortedByURL(t *testing.T) {
	list := FlatList(sampleFindings())
	for i := 1; i < len(list); i++ {
		if list[i-1].URL > list[i].URL {
			t.Fatalf("expected sorted URLs, got %v", list)
		}
	}
}

func TestWritePlaintextGobusterStyle(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePlaintext(&buf, sampleFindings()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %v", len(lines), lines)
	}
	want := "http://t/admin (Status: 200) [Size: 100] [conf: 0.90]"
	found := false
	for _, l := range lines {
		if l == want {
			found = true
		}
	}
	if !found {
		t.Errorf("expected line %q, got %v", want, lines)
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, sampleFindings()); err != nil {
		t.Fatal(err)
	}
	var out []engine.Finding
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 findings, got %d", len(out))
	}
	for _, f := range out {
		if f.URL == "http://t/backup" && len(f.Aliases) != 1 {
			t.Errorf("expected aliases preserved through JSON round-trip, got %v", f.Aliases)
		}
	}
}

func TestEmptyFindingsProduceNoOutput(t *testing.T) {
	var buf bytes.Buffer
	if err := WritePlaintext(&buf, nil); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected empty output for no findings, got %q", buf.String())
	}
}

package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("invalid JSON line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWriterHeaderThenEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := New(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := w.WriteHeader(Header{
		Targets: []string{"http://example.test"}, Wordlist: "list.txt",
		WordlistHash: "deadbeef", Seed: 42, Concurrency: 20,
	}); err != nil {
		t.Fatal(err)
	}

	cls := engine.Classification{IsHit: true, Confidence: 0.9, Reason: "diverges from baseline"}
	w.WriteRequest(engine.AuditRecord{
		Time: time.Now(), Method: "GET", URL: "http://example.test/admin",
		ParentDir: "", Provenance: "wordlist",
		Signature:   engine.ResponseSignature{Status: 301, BodyLen: 178, SimHash: 0xabc, RawBodyHash: 0xdef},
		Classified:  &cls,
		BaselineDir: "", Hamming: 37, NoiseFloor: 6,
	})

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines (header + 1 entry), got %d", len(lines))
	}

	header := lines[0]
	if header["type"] != "header" {
		t.Errorf("expected first line to be the header, got %v", header)
	}
	if header["seed"].(float64) != 42 {
		t.Errorf("expected seed 42 in header, got %v", header["seed"])
	}
	if header["wordlist_hash"] != "deadbeef" {
		t.Errorf("expected wordlist_hash recorded, got %v", header["wordlist_hash"])
	}

	rec := lines[1]
	if rec["url"] != "http://example.test/admin" {
		t.Errorf("unexpected url: %v", rec["url"])
	}
	if rec["status"].(float64) != 301 {
		t.Errorf("unexpected status: %v", rec["status"])
	}
	classified, ok := rec["classified"].(map[string]any)
	if !ok {
		t.Fatalf("expected classified object, got %v", rec["classified"])
	}
	if classified["is_hit"] != true {
		t.Errorf("expected is_hit true, got %v", classified["is_hit"])
	}
	if classified["hamming"].(float64) != 37 {
		t.Errorf("expected hamming 37, got %v", classified["hamming"])
	}
	if rec["sim_hash"] != "0xabc" {
		t.Errorf("expected sim_hash 0xabc, got %v", rec["sim_hash"])
	}
}

func TestWriterProbeHasNoClassifiedField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.WriteRequest(engine.AuditRecord{
		Time: time.Now(), Method: "GET", URL: "http://example.test/xk3f9a",
		IsProbe: true, ParentDir: "", Provenance: "probe",
		Signature: engine.ResponseSignature{Status: 404},
	})
	w.Close()

	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	if _, present := lines[0]["classified"]; present {
		t.Errorf("expected no classified field for a probe with no baseline yet, got %v", lines[0]["classified"])
	}
	if lines[0]["is_probe"] != true {
		t.Errorf("expected is_probe true, got %v", lines[0]["is_probe"])
	}
}

func TestWriterRecordsNetworkError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	w.WriteRequest(engine.AuditRecord{
		Time: time.Now(), Method: "GET", URL: "http://example.test/timeout",
		ParentDir: "", Provenance: "wordlist", Err: errors.New("context deadline exceeded"),
	})
	w.Close()

	lines := readLines(t, path)
	if lines[0]["error"] != "context deadline exceeded" {
		t.Errorf("expected error recorded, got %v", lines[0]["error"])
	}
}

func TestReadHeaderRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Header{
		Targets: []string{"http://example.test"}, Wordlist: "/tmp/list.txt",
		WordlistHash: "abc123", Seed: 999, Concurrency: 7, Rate: 12.5, Jitter: 0.25, MaxDepth: 3,
	}
	if err := w.WriteHeader(want); err != nil {
		t.Fatal(err)
	}
	w.WriteRequest(engine.AuditRecord{Time: time.Now(), Method: "GET", URL: "http://example.test/x"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := ReadHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Seed != want.Seed || got.Wordlist != want.Wordlist || got.WordlistHash != want.WordlistHash ||
		got.Concurrency != want.Concurrency || len(got.Targets) != 1 || got.Targets[0] != want.Targets[0] {
		t.Fatalf("ReadHeader round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestReadHeaderRejectsNonHeaderFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteRequest(engine.AuditRecord{Time: time.Now(), Method: "GET", URL: "http://example.test/x"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadHeader(path); err == nil {
		t.Fatal("expected an error when the first line isn't a header record")
	}
}

func TestWriterOneLinePerRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		w.WriteRequest(engine.AuditRecord{Time: time.Now(), Method: "GET", URL: "http://example.test/x"})
	}
	w.Close()

	lines := readLines(t, path)
	if len(lines) != 50 {
		t.Fatalf("expected 50 lines, got %d", len(lines))
	}
}

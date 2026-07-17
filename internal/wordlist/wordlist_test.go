package wordlist

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "words.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadOrdersByRankDescendingPriority(t *testing.T) {
	p := writeTemp(t, "admin\nbackup\nconfig\n")
	entries, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}
	if entries[0].Word != "admin" || entries[0].BasePrio != 1.0 {
		t.Fatalf("expected rank0 admin with BasePrio 1.0, got %+v", entries[0])
	}
	if !(entries[0].BasePrio > entries[1].BasePrio && entries[1].BasePrio > entries[2].BasePrio) {
		t.Fatalf("expected strictly decreasing BasePrio, got %+v", entries)
	}
}

func TestLoadSkipsBlankAndCommentLines(t *testing.T) {
	p := writeTemp(t, "admin\n\n# comment\nbackup\n")
	entries, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
}

func TestLoadClassifiesDottedWordsAsFile(t *testing.T) {
	p := writeTemp(t, "admin\nconfig.php\n")
	entries, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Type != EntryDir {
		t.Fatalf("expected 'admin' -> EntryDir, got %v", entries[0].Type)
	}
	if entries[1].Type != EntryFile {
		t.Fatalf("expected 'config.php' -> EntryFile, got %v", entries[1].Type)
	}
}

func TestLoadEmptyWordlistErrors(t *testing.T) {
	p := writeTemp(t, "\n# only comments\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for empty wordlist")
	}
}

func TestLoadMissingFileErrors(t *testing.T) {
	if _, err := Load("/nonexistent/path/words.txt"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestHashIsStableAndSensitiveToContent(t *testing.T) {
	p1 := writeTemp(t, "admin\nbackup\n")
	p2 := writeTemp(t, "admin\nbackup\n")
	p3 := writeTemp(t, "admin\nother\n")

	h1, err := Hash(p1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := Hash(p2)
	if err != nil {
		t.Fatal(err)
	}
	h3, err := Hash(p3)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("expected identical content to hash identically: %s vs %s", h1, h2)
	}
	if h1 == h3 {
		t.Fatalf("expected different content to hash differently")
	}
}

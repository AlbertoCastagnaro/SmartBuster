package engine

import (
	"sort"
	"testing"
)

func TestAssocEngine_CompanionReweight(t *testing.T) {
	a := NewAssocEngine()
	a.RecordHit("/", "login.php")

	companion := Candidate{Path: "config.php", ParentDir: "/"}
	if got := a.assocSignal(&companion); got != 1.0 {
		t.Errorf("expected config.php to be boosted as login.php's companion, got %v", got)
	}

	unrelated := Candidate{Path: "readme.txt", ParentDir: "/"}
	if got := a.assocSignal(&unrelated); got != 0 {
		t.Errorf("expected an unrelated term to be unboosted, got %v", got)
	}

	// Same companion, different directory: subtree-aware scoping (spec §8).
	elsewhere := Candidate{Path: "config.php", ParentDir: "/other"}
	if got := a.assocSignal(&elsewhere); got != 0 {
		t.Errorf("expected the companion boost to be scoped to the hit's directory, got %v", got)
	}
}

func TestAssocEngine_ExtensionPivot(t *testing.T) {
	a := NewAssocEngine()
	a.RecordHit("/", "admin.php") // "admin.php" IS in a companion group, but "report.*" below is not

	sameExt := Candidate{Path: "report.php", ParentDir: "/"}
	if got := a.assocSignal(&sameExt); got != 0.6 {
		t.Errorf("expected an extension-pivot boost for a same-ext, non-companion candidate, got %v", got)
	}

	diffExt := Candidate{Path: "report.aspx", ParentDir: "/"}
	if got := a.assocSignal(&diffExt); got != 0 {
		t.Errorf("expected no boost for a different extension, got %v", got)
	}
}

func TestAssocEngine_Generate_Backups(t *testing.T) {
	a := NewAssocEngine()
	hit := Candidate{Path: "report.php", Type: TypeFile, ParentDir: "/", BasePrio: 0.5, Tags: []string{"generic"}}

	gen := a.Generate(hit)
	got := make(map[string]bool)
	for _, c := range gen {
		if c.Type == TypeFile && len(c.Path) > len("report.php") {
			got[c.Path] = true
		}
	}
	for _, suf := range backupSuffixes {
		want := "report.php" + suf
		if !got[want] {
			t.Errorf("expected generated backup %q, got %v", want, gen)
		}
	}
}

func TestAssocEngine_Generate_Siblings(t *testing.T) {
	a := NewAssocEngine()

	versionHit := Candidate{Path: "v1", Type: TypeDir, ParentDir: "/api", BasePrio: 0.5}
	gen := a.Generate(versionHit)
	var paths []string
	for _, c := range gen {
		if c.Type == TypeDir {
			paths = append(paths, c.Path)
		}
	}
	sort.Strings(paths)
	want := []string{"v2", "v3", "v4", "v5", "v6"}
	if len(paths) != len(want) {
		t.Fatalf("got siblings %v, want %v", paths, want)
	}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("siblings[%d] = %q, want %q", i, paths[i], w)
		}
	}
	for _, c := range gen {
		if c.ParentDir != "/api" {
			t.Errorf("expected generated sibling to inherit ParentDir /api, got %q", c.ParentDir)
		}
	}
}

func TestAssocEngine_Generate_PlainSequenceZeroPadding(t *testing.T) {
	a := NewAssocEngine()
	hit := Candidate{Path: "007", Type: TypeFile, ParentDir: "/user"}
	gen := siblingsFor(hit)
	if len(gen) != GenSiblingBound {
		t.Fatalf("expected %d siblings, got %d: %v", GenSiblingBound, len(gen), gen)
	}
	if gen[0].Path != "008" {
		t.Errorf("expected zero-padding preserved (008), got %q", gen[0].Path)
	}
	_ = a
}

func TestAssocEngine_Generate_NoSiblingsWithoutTrailingDigits(t *testing.T) {
	hit := Candidate{Path: "config.php", Type: TypeFile, ParentDir: "/"}
	if gen := siblingsFor(hit); gen != nil {
		t.Errorf("expected no siblings for a non-numeric segment, got %v", gen)
	}
}

func TestAssocEngine_Generate_NoBackupsForDirHit(t *testing.T) {
	a := NewAssocEngine()
	hit := Candidate{Path: "uploads", Type: TypeDir, ParentDir: "/"}
	for _, c := range a.Generate(hit) {
		if c.Type == TypeFile {
			t.Errorf("did not expect a backup candidate for a directory hit, got %+v", c)
		}
	}
}

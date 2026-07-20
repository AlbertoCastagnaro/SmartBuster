package corpus

import (
	"strings"
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
)

func phpProfile() *profile.TargetProfile {
	return &profile.TargetProfile{Tech: map[string]*profile.Tech{
		"PHP": {Name: "PHP", Category: "language", Confidence: 0.85, Layer: profile.LayerBackend},
	}}
}

func TestExpand_StemYieldsExtensionVariants(t *testing.T) {
	stem := Candidate{Path: "config", Type: TypeStem, BasePrio: 0.8, Tags: []string{"php"}, Provenance: "corpus:php"}
	p := phpProfile()

	out := Expand([]Candidate{stem}, p, 2.0)

	var gotPHP bool
	for _, c := range out {
		if c.Path == "config.php" {
			gotPHP = true
			if c.Type != TypeFile {
				t.Errorf("config.php: Type = %v, want TypeFile", c.Type)
			}
		}
	}
	if !gotPHP {
		t.Errorf("expected config.php among stem expansions, got %+v", out)
	}
}

func TestExpand_GenericDotlessWordOnlyStemmedWithDetectedStack(t *testing.T) {
	generic := Candidate{Path: "config", Type: TypeDir, BasePrio: 0.5, Score: 0.5, Tags: []string{"generic"}, Provenance: "corpus:generic"}

	// No backend stack detected: "config" tried only as a dir, never
	// expanded into config.php (spec §6).
	noStack := Expand([]Candidate{generic}, &profile.TargetProfile{}, 2.0)
	for _, c := range noStack {
		if c.Path == "config.php" {
			t.Errorf("expected no config.php expansion without a detected backend stack, got %+v", noStack)
		}
	}
	foundDir := false
	for _, c := range noStack {
		if c.Path == "config" && c.Type == TypeDir {
			foundDir = true
		}
	}
	if !foundDir {
		t.Errorf("expected the bare dir candidate always tried, got %+v", noStack)
	}

	// PHP detected: "config" is additionally tried as a stem -> config.php
	// (additive, not a replacement — reorder-not-exclude).
	withStack := Expand([]Candidate{generic}, phpProfile(), 2.0)
	foundDir, foundPHP := false, false
	for _, c := range withStack {
		if c.Path == "config" && c.Type == TypeDir {
			foundDir = true
		}
		if c.Path == "config.php" {
			foundPHP = true
		}
	}
	if !foundDir {
		t.Errorf("expected the bare dir candidate still present with a detected stack, got %+v", withStack)
	}
	if !foundPHP {
		t.Errorf("expected config.php once a backend stack is detected, got %+v", withStack)
	}
}

func TestExpand_BackupGenerationForSensitiveFiles(t *testing.T) {
	c := Candidate{Path: "config.php", Type: TypeFile, BasePrio: 0.5, Tags: []string{"php"}, Provenance: "corpus:php"}
	p := phpProfile()

	out := Expand([]Candidate{c}, p, 2.0)

	var backup *Candidate
	for i := range out {
		if out[i].Path == "config.php.bak" {
			backup = &out[i]
		}
	}
	if backup == nil {
		t.Fatalf("expected config.php.bak among backup expansions, got %+v", out)
	}

	var base *Candidate
	for i := range out {
		if out[i].Path == "config.php" {
			base = &out[i]
		}
	}
	if base == nil {
		t.Fatal("expected config.php itself still present")
	}
	wantBackupBase := base.BasePrio * BackupSensitiveBoost
	if diff := backup.BasePrio - wantBackupBase; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("config.php.bak BasePrio = %v, want %v*%v = %v (BACKUP_SENSITIVE_BOOST)",
			backup.BasePrio, base.BasePrio, BackupSensitiveBoost, wantBackupBase)
	}

	// A non-sensitive file gets no backup variants.
	plain := Candidate{Path: "about.php", Type: TypeFile, BasePrio: 0.5, Tags: []string{"php"}, Provenance: "corpus:php"}
	out2 := Expand([]Candidate{plain}, p, 2.0)
	for _, oc := range out2 {
		if strings.HasSuffix(oc.Path, ".bak") {
			t.Errorf("expected no backup variants for a non-sensitive file, got %+v", out2)
		}
	}
}

func TestExpand_DedupCollapsesDuplicatePathsUnioningTagsAndProvenance(t *testing.T) {
	a := Candidate{Path: "admin", Type: TypeDir, BasePrio: 0.5, Score: 0.5, Tags: []string{"generic"}, Provenance: "corpus:generic"}
	b := Candidate{Path: "admin", Type: TypeDir, BasePrio: 0.9, Score: 0.9, Tags: []string{"wordpress"}, Provenance: "corpus:wordpress"}

	out := Expand([]Candidate{a, b}, &profile.TargetProfile{}, 2.0)

	var found *Candidate
	n := 0
	for i := range out {
		if out[i].Path == "admin" {
			found = &out[i]
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one deduped candidate for admin, got %d", n)
	}
	if !containsTag(found.Tags, "generic") || !containsTag(found.Tags, "wordpress") {
		t.Errorf("expected unioned tags [generic wordpress], got %v", found.Tags)
	}
	if !strings.Contains(found.Provenance, "corpus:generic") || !strings.Contains(found.Provenance, "corpus:wordpress") {
		t.Errorf("expected provenance to record both contributing occurrences, got %q", found.Provenance)
	}
	// Max score kept.
	if found.Score != 0.9 || found.BasePrio != 0.9 {
		t.Errorf("expected the max-score occurrence's Score/BasePrio kept, got Score=%v BasePrio=%v", found.Score, found.BasePrio)
	}
}

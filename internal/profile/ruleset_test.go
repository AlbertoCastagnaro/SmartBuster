package profile

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// DoD §9 assertion 5: embedded default works offline (no system/user dir).
func TestLoad_EmbeddedDefaultWorksOffline(t *testing.T) {
	rs, err := Load(LoadOptions{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(rs.Headers) == 0 || len(rs.Cookies) == 0 || len(rs.HTML) == 0 || len(rs.WAF) == 0 {
		t.Fatalf("embedded ruleset looks empty: %+v", rs)
	}
}

// DoD §9 assertion 5: a user overlay overrides a vendored rule by ID.
func TestLoad_UserOverlayOverridesVendoredRule(t *testing.T) {
	dir := t.TempDir()
	overlay := `[{"id":"ck-phpsessid","cookie":"PHPSESSID","tech":"PHP","category":"language","layer":"backend","confidence":0.42}]`
	if err := os.WriteFile(filepath.Join(dir, "cookies.json"), []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	rs, err := Load(LoadOptions{UserDir: dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	found := false
	for _, r := range rs.Cookies {
		if r.ID == "ck-phpsessid" {
			found = true
			if r.Confidence != 0.42 {
				t.Fatalf("overlay confidence = %v, want 0.42 (override didn't take)", r.Confidence)
			}
		}
	}
	if !found {
		t.Fatal("ck-phpsessid rule missing after overlay")
	}
	// Overlay must not have duplicated the entry or dropped unrelated ones.
	count := 0
	for _, r := range rs.Cookies {
		if r.ID == "ck-phpsessid" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ck-phpsessid appears %d times, want 1", count)
	}
}

// DoD §9 assertion 5: --rules-off suppresses a whole category.
func TestLoad_RulesOffSuppressesCategory(t *testing.T) {
	rs, err := Load(LoadOptions{RulesOff: []string{"cms"}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, r := range rs.Cookies {
		if r.Category == "cms" {
			t.Fatalf("cms cookie rule %q survived --rules-off=cms", r.ID)
		}
	}
	for _, r := range rs.HTML {
		if r.Category == "cms" {
			t.Fatalf("cms html rule %q survived --rules-off=cms", r.ID)
		}
	}
}

// DoD §9 assertion 5: `ruleset update` writes a lock file recording what it
// pulled. Exercised against a local git repo fixture — no network needed.
func TestUpdate_WritesLock(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	runGitOrFail(t, repo, "init", "-q")
	runGitOrFail(t, repo, "config", "user.email", "test@example.com")
	runGitOrFail(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "extra.json"), []byte(`[]`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOrFail(t, repo, "add", "extra.json")
	runGitOrFail(t, repo, "commit", "-q", "-m", "seed rules")

	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, out)
	}
	commit := trimNewline(string(out))

	dest := filepath.Join(t.TempDir(), "system-rules")
	if err := Update(UpdateOptions{Repo: repo, Commit: commit, Dest: dest}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	lockData, err := os.ReadFile(filepath.Join(dest, "ruleset.lock"))
	if err != nil {
		t.Fatalf("ruleset.lock missing: %v", err)
	}
	if len(lockData) == 0 {
		t.Fatal("ruleset.lock is empty")
	}
	if _, err := os.Stat(filepath.Join(dest, "extra.json")); err != nil {
		t.Fatalf("expected extra.json copied into dest: %v", err)
	}
}

func runGitOrFail(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

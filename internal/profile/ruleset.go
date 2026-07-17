package profile

import (
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

//go:embed rulesets/*.json
var embeddedRulesetFS embed.FS

// HeaderRule votes for Tech when header's value matches Pattern (spec §4.1).
type HeaderRule struct {
	ID         string  `json:"id"`
	Header     string  `json:"header"`
	Pattern    string  `json:"pattern"`
	Tech       string  `json:"tech"`
	Category   string  `json:"category"`
	Layer      string  `json:"layer"`
	Confidence float64 `json:"confidence"`
}

// CookieRule votes for Tech when a cookie name equals, or is prefixed by,
// Cookie (spec §4.2 — e.g. "wordpress_" matches "wordpress_logged_in_xxx").
type CookieRule struct {
	ID         string  `json:"id"`
	Cookie     string  `json:"cookie"`
	Tech       string  `json:"tech"`
	Category   string  `json:"category"`
	Layer      string  `json:"layer"`
	Confidence float64 `json:"confidence"`
}

// HTMLRule votes for Tech when Pattern (a regex) matches the lowercased
// profile-fetch body (spec §4.3).
type HTMLRule struct {
	ID         string  `json:"id"`
	Pattern    string  `json:"pattern"`
	Tech       string  `json:"tech"`
	Category   string  `json:"category"`
	Layer      string  `json:"layer"`
	Confidence float64 `json:"confidence"`
}

// FaviconRule votes for Tech when the mmh3-32 hash of the favicon body
// (Shodan convention: base64 the body, then hash) equals Hash (spec §4.4).
type FaviconRule struct {
	ID         string  `json:"id"`
	Hash       string  `json:"hash"`
	Tech       string  `json:"tech"`
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
}

// ErrorPageRule votes for Tech when Pattern matches the normalized
// not-found body calibration already captured (spec §4.6). Status, if
// nonzero, additionally requires the baseline's representative status.
type ErrorPageRule struct {
	ID         string  `json:"id"`
	Pattern    string  `json:"pattern"`
	Status     int     `json:"status,omitempty"`
	Tech       string  `json:"tech"`
	Category   string  `json:"category"`
	Layer      string  `json:"layer"`
	Confidence float64 `json:"confidence"`
}

// ActiveProbeRule names the confirmer path fired when Tech's fused
// confidence sits in the confirmation band (spec §4.7). Confidence is what
// the tech is raised to on a confirming (non-baseline) response.
type ActiveProbeRule struct {
	ID         string  `json:"id"`
	Tech       string  `json:"tech"`
	Path       string  `json:"path"`
	Confidence float64 `json:"confidence"`
}

// WAFRule matches a WAF vendor signature over a header, a cookie
// (name-prefix, like CookieRule), or (if BodyOnly) the response body only
// (spec §5).
type WAFRule struct {
	ID       string `json:"id"`
	Vendor   string `json:"vendor"`
	Header   string `json:"header,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
	Cookie   string `json:"cookie,omitempty"`
	BodyOnly bool   `json:"body_only,omitempty"`
}

// Ruleset is the smartbuster-specific overlay (favicon hashes, error-page
// templates, pentest confirmers, header/cookie tables). wappalyzergo's own
// bundled fingerprints are separate and used directly (spec §6 note).
type Ruleset struct {
	Headers      []HeaderRule
	Cookies      []CookieRule
	HTML         []HTMLRule
	Favicons     []FaviconRule
	ErrorPages   []ErrorPageRule
	ActiveProbes []ActiveProbeRule
	WAF          []WAFRule
}

// LoadOptions configures the layered ruleset load (spec §6): embedded
// default < SystemDir < UserDir, later layers overriding earlier ones by
// rule ID. RulesOff drops whole categories (case-insensitive) after merge.
type LoadOptions struct {
	SystemDir string
	UserDir   string
	RulesOff  []string
}

// Load builds the merged ruleset. Missing SystemDir/UserDir are silently
// skipped (not every install has them); a present but unreadable file is an
// error, since a malformed rule file the operator placed there is a
// configuration mistake worth surfacing, not silently ignoring.
func Load(opts LoadOptions) (*Ruleset, error) {
	rs := &Ruleset{}
	embeddedRead := func(name string) ([]byte, error) {
		return embeddedRulesetFS.ReadFile("rulesets/" + name)
	}
	if err := loadLayer(rs, embeddedRead, true); err != nil {
		return nil, fmt.Errorf("embedded ruleset: %w", err)
	}
	if opts.SystemDir != "" {
		if err := loadLayerDir(rs, opts.SystemDir); err != nil {
			return nil, fmt.Errorf("system ruleset: %w", err)
		}
	}
	if opts.UserDir != "" {
		if err := loadLayerDir(rs, opts.UserDir); err != nil {
			return nil, fmt.Errorf("user ruleset overlay: %w", err)
		}
	}
	applyRulesOff(rs, opts.RulesOff)
	return rs, nil
}

// loadLayerDir reads plain files directly out of dir (e.g.
// ~/.config/smartbuster/rules/cookies.json) — unlike the embedded layer,
// which nests them under "rulesets/" to match this repo's own layout.
func loadLayerDir(rs *Ruleset, dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	read := func(name string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, name))
	}
	return loadLayer(rs, read, false)
}

// loadLayer decodes each of the seven rule files (missing files in a
// non-required layer are fine — an overlay need not touch every category)
// and merges by ID: an entry whose ID already exists in rs is replaced,
// preserving that entry's position; a new ID is appended.
func loadLayer(rs *Ruleset, read func(string) ([]byte, error), required bool) error {
	tryDecode := func(name string, dst interface{}) error {
		data, err := read(name)
		if err != nil {
			if required {
				return fmt.Errorf("read %s: %w", name, err)
			}
			return nil // overlay layer: file absent is fine
		}
		if err := json.Unmarshal(data, dst); err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
		return nil
	}

	var headers []HeaderRule
	if err := tryDecode("headers.json", &headers); err != nil {
		return err
	}
	rs.Headers = mergeByID(rs.Headers, headers, func(r HeaderRule) string { return r.ID })

	var cookies []CookieRule
	if err := tryDecode("cookies.json", &cookies); err != nil {
		return err
	}
	rs.Cookies = mergeByID(rs.Cookies, cookies, func(r CookieRule) string { return r.ID })

	var html []HTMLRule
	if err := tryDecode("html.json", &html); err != nil {
		return err
	}
	rs.HTML = mergeByID(rs.HTML, html, func(r HTMLRule) string { return r.ID })

	var favicons []FaviconRule
	if err := tryDecode("favicons.json", &favicons); err != nil {
		return err
	}
	rs.Favicons = mergeByID(rs.Favicons, favicons, func(r FaviconRule) string { return r.ID })

	var errorpages []ErrorPageRule
	if err := tryDecode("errorpages.json", &errorpages); err != nil {
		return err
	}
	rs.ErrorPages = mergeByID(rs.ErrorPages, errorpages, func(r ErrorPageRule) string { return r.ID })

	var activeProbes []ActiveProbeRule
	if err := tryDecode("active_probes.json", &activeProbes); err != nil {
		return err
	}
	rs.ActiveProbes = mergeByID(rs.ActiveProbes, activeProbes, func(r ActiveProbeRule) string { return r.ID })

	var waf []WAFRule
	if err := tryDecode("waf.json", &waf); err != nil {
		return err
	}
	rs.WAF = mergeByID(rs.WAF, waf, func(r WAFRule) string { return r.ID })

	return nil
}

func mergeByID[T any](base, overlay []T, id func(T) string) []T {
	index := make(map[string]int, len(base))
	for i, r := range base {
		if k := id(r); k != "" {
			index[k] = i
		}
	}
	for _, r := range overlay {
		k := id(r)
		if k != "" {
			if i, ok := index[k]; ok {
				base[i] = r
				continue
			}
		}
		index[k] = len(base)
		base = append(base, r)
	}
	return base
}

func applyRulesOff(rs *Ruleset, rulesOff []string) {
	if len(rulesOff) == 0 {
		return
	}
	off := make(map[string]bool, len(rulesOff))
	for _, c := range rulesOff {
		off[normalizeCategory(c)] = true
	}
	rs.Headers = filterOut(rs.Headers, off, func(r HeaderRule) string { return r.Category })
	rs.Cookies = filterOut(rs.Cookies, off, func(r CookieRule) string { return r.Category })
	rs.HTML = filterOut(rs.HTML, off, func(r HTMLRule) string { return r.Category })
	rs.Favicons = filterOut(rs.Favicons, off, func(r FaviconRule) string { return r.Category })
	rs.ErrorPages = filterOut(rs.ErrorPages, off, func(r ErrorPageRule) string { return r.Category })
}

func filterOut[T any](rules []T, off map[string]bool, cat func(T) string) []T {
	out := rules[:0:0]
	for _, r := range rules {
		if !off[normalizeCategory(cat(r))] {
			out = append(out, r)
		}
	}
	return out
}

func normalizeCategory(c string) string {
	b := []byte(c)
	for i, ch := range b {
		if ch >= 'A' && ch <= 'Z' {
			b[i] = ch + 32
		}
	}
	return string(b)
}

// DefaultRulesOff is the spec's default category toggle (spec §8): pentest
// scans care about server/framework/cms/language/waf, not analytics.
var DefaultRulesOff = []string{"analytics", "marketing", "tracking"}

// Lock records the provenance of a `ruleset update` (spec §6).
type Lock struct {
	Repo      string    `json:"repo"`
	Commit    string    `json:"commit"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UpdateOptions configures `smartbuster ruleset update` (spec §6). There is
// no baked-in default upstream repo/commit: this tool must not fabricate a
// pinned commit hash it hasn't verified, so Repo and Commit are supplied by
// the operator.
type UpdateOptions struct {
	Repo   string
	Commit string
	Dest   string
}

// Update git-clones (or reuses an existing clone of) Repo into a scratch
// dir, checks out the pinned Commit, copies its *.json rule files into
// Dest, and writes Dest/ruleset.lock recording what was pulled (spec §6).
func Update(opts UpdateOptions) error {
	if opts.Repo == "" || opts.Commit == "" || opts.Dest == "" {
		return fmt.Errorf("ruleset update: repo, commit, and dest are all required")
	}
	if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
		return fmt.Errorf("ruleset update: %w", err)
	}

	scratch, err := os.MkdirTemp("", "smartbuster-ruleset-*")
	if err != nil {
		return fmt.Errorf("ruleset update: %w", err)
	}
	defer os.RemoveAll(scratch)

	if err := runGit(scratch, "clone", opts.Repo, "."); err != nil {
		return fmt.Errorf("ruleset update: clone: %w", err)
	}
	if err := runGit(scratch, "checkout", opts.Commit); err != nil {
		return fmt.Errorf("ruleset update: checkout %s: %w", opts.Commit, err)
	}

	matches, err := filepath.Glob(filepath.Join(scratch, "*.json"))
	if err != nil {
		return fmt.Errorf("ruleset update: %w", err)
	}
	for _, src := range matches {
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("ruleset update: %w", err)
		}
		dst := filepath.Join(opts.Dest, filepath.Base(src))
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return fmt.Errorf("ruleset update: %w", err)
		}
	}

	lock := Lock{Repo: opts.Repo, Commit: opts.Commit, UpdatedAt: time.Now().UTC()}
	lockData, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("ruleset update: %w", err)
	}
	if err := os.WriteFile(filepath.Join(opts.Dest, "ruleset.lock"), lockData, 0o644); err != nil {
		return fmt.Errorf("ruleset update: %w", err)
	}
	return nil
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, out)
	}
	return nil
}

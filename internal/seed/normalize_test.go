package seed

import (
	"strings"
	"testing"
	"time"
)

func TestNormalize_QueryStrippedAndPrioTiered(t *testing.T) {
	now := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC)
	raws := []RawSeed{
		{Path: "/x?a=1", Source: "robots:disallow"},
		{Path: "/y", Source: "sitemap"},
		{Path: "/z", Source: "robots:allow"},
		{Path: "/recent", Source: "wayback:20260601000000"}, // ~7 weeks before now: recent
		{Path: "/old", Source: "wayback:20200101000000"},    // years before now: old
	}
	out := Normalize(raws, NormalizeOptions{Now: now})

	byPath := map[string]Seed{}
	for _, s := range out {
		byPath[s.Path] = s
	}
	if s := byPath["/x"]; s.BasePrio != PrioRobotsDisallow {
		t.Errorf("/x: BasePrio = %v, want %v (query string should be stripped, source Disallow)", s.BasePrio, PrioRobotsDisallow)
	}
	if s := byPath["/y"]; s.BasePrio != PrioSitemap {
		t.Errorf("/y: BasePrio = %v, want %v", s.BasePrio, PrioSitemap)
	}
	if s := byPath["/z"]; s.BasePrio != PrioRobotsAllow {
		t.Errorf("/z: BasePrio = %v, want %v", s.BasePrio, PrioRobotsAllow)
	}
	if s := byPath["/recent"]; s.BasePrio != PrioWaybackRecent {
		t.Errorf("/recent: BasePrio = %v, want %v", s.BasePrio, PrioWaybackRecent)
	}
	if s := byPath["/old"]; s.BasePrio != PrioWaybackOld {
		t.Errorf("/old: BasePrio = %v, want %v", s.BasePrio, PrioWaybackOld)
	}
	if PrioRobotsDisallow <= PrioSitemap || PrioSitemap <= PrioRobotsAllow {
		t.Fatal("prio tiering constants are no longer ordered Disallow > Sitemap > Allow")
	}
}

func TestNormalize_AssetNoiseDroppedFromWaybackOnly(t *testing.T) {
	raws := []RawSeed{
		{Path: "/logo.png", Source: "wayback:20260101000000"},
		{Path: "/style.css", Source: "sitemap"}, // asset filter is Wayback-specific (spec §3): sitemap keeps it
	}
	out := Normalize(raws, NormalizeOptions{})
	if len(out) != 1 || out[0].Path != "/style.css" {
		t.Fatalf("got %+v, want only /style.css (from sitemap, not filtered)", out)
	}

	outKept := Normalize(raws, NormalizeOptions{SeedAssets: true})
	if len(outKept) != 2 {
		t.Fatalf("--seed-assets should keep the Wayback asset too: got %+v", outKept)
	}
}

func TestNormalize_DedupAcrossSources_MaxPrioUnionedProvenance(t *testing.T) {
	raws := []RawSeed{
		{Path: "/shared", Source: "robots:allow"},
		{Path: "/shared", Source: "sitemap"},
		{Path: "/shared?ignored=1", Source: "wayback:20200101"},
	}
	out := Normalize(raws, NormalizeOptions{})
	if len(out) != 1 {
		t.Fatalf("expected one deduped seed, got %+v", out)
	}
	s := out[0]
	if s.BasePrio != PrioSitemap {
		t.Errorf("BasePrio = %v, want max tier %v (sitemap beats robots:allow and old wayback)", s.BasePrio, PrioSitemap)
	}
	for _, want := range []string{"robots:allow", "sitemap", "wayback:20200101"} {
		if !containsPart(s.Provenance, want) {
			t.Errorf("Provenance %q missing %q", s.Provenance, want)
		}
	}
}

func TestNormalize_IsDirHint(t *testing.T) {
	out := Normalize([]RawSeed{
		{Path: "/old/admin", Source: "sitemap"},
		{Path: "/config.php", Source: "sitemap"},
	}, NormalizeOptions{})
	byPath := map[string]Seed{}
	for _, s := range out {
		byPath[s.Path] = s
	}
	if !byPath["/old/admin"].IsDirHint {
		t.Error("/old/admin: expected IsDirHint true (extensionless)")
	}
	if byPath["/config.php"].IsDirHint {
		t.Error("/config.php: expected IsDirHint false (has an extension)")
	}
}

func TestUnionProvenance(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"", "sitemap", "sitemap"},
		{"wordlist", "", "wordlist"},
		{"wordlist", "robots:allow", "wordlist+robots:allow"},
		{"a+b", "b+c", "a+b+c"},
	}
	for _, c := range cases {
		if got := UnionProvenance(c.a, c.b); got != c.want {
			t.Errorf("UnionProvenance(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func containsPart(joined, part string) bool {
	for _, p := range strings.Split(joined, "+") {
		if p == part {
			return true
		}
	}
	return false
}

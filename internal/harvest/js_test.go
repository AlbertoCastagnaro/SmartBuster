package harvest

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestExtractJSPaths_RealEndpointsAndCallSites(t *testing.T) {
	src := []byte(`
		fetch("/api/v1/users");
		axios.get("/internal/status");
		var xhr = new XMLHttpRequest();
		xhr.open("GET", "/internal/status");
		var rel = "../shared/config.json";
	`)
	got := ExtractJSPaths(src)
	sort.Strings(got)
	want := []string{"../shared/config.json", "/api/v1/users", "/internal/status"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractJSPaths = %v, want %v", got, want)
	}
}

func TestExtractJSPaths_FiltersNoise(t *testing.T) {
	src := []byte(`
		var mime = "application/json";
		var tpl = ` + "`/api/${userId}/detail`" + `;
		var pattern = "/^[a-z0-9]+$/";
		var short = "/a";
	`)
	got := ExtractJSPaths(src)
	for _, p := range got {
		if p == "application/json" {
			t.Errorf("bare mime type should never be extracted, got %v", got)
		}
		if p == "/api/${userId}/detail" || strings.Contains(p, "${") {
			t.Errorf("template-literal interpolation should never survive extraction, got %v", got)
		}
		if p == "/^[a-z0-9]+$/" {
			t.Errorf("regex-shaped string should never survive extraction, got %v", got)
		}
	}
	// "/a" is a genuinely short-but-valid absolute path and should survive.
	found := false
	for _, p := range got {
		if p == "/a" {
			found = true
		}
	}
	if !found {
		t.Errorf("ExtractJSPaths = %v, want it to include the short but valid /a", got)
	}
}

func TestExtractJSPaths_BareKnownExtensionSurvives(t *testing.T) {
	src := []byte(`fetch("config.json"); fetch("noext");`)
	got := ExtractJSPaths(src)
	want := []string{"config.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExtractJSPaths = %v, want %v (bare filename without a known extension must be dropped)", got, want)
	}
}

func TestExtractJSPaths_DedupsAcrossCallSites(t *testing.T) {
	src := []byte(`fetch("/api/x"); axios.get("/api/x");`)
	got := ExtractJSPaths(src)
	if len(got) != 1 {
		t.Errorf("ExtractJSPaths = %v, want exactly 1 deduped entry", got)
	}
}

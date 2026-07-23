package harvest

import (
	"reflect"
	"testing"
)

func TestExtractHTML_LinksAndScripts(t *testing.T) {
	body := []byte(`<html><body>
		<a href="/secret-page">secret</a>
		<a href="/shared">shared</a>
		<a href="http://off-host.example/elsewhere">offsite</a>
		<a href="#fragment-only">skip</a>
		<a href="javascript:void(0)">skip</a>
		<a href="mailto:a@b.com">skip</a>
		<img src="/logo.png">
		<form action="/submit"></form>
		<div data-endpoint="/api/config"></div>
		<script src="/bundle.js"></script>
	</body></html>`)

	links, scripts := ExtractHTML(body, "http://example.test/admin")

	want := []string{
		"http://example.test/secret-page",
		"http://example.test/shared",
		"http://off-host.example/elsewhere",
		"http://example.test/logo.png",
		"http://example.test/submit",
		"http://example.test/api/config",
		"http://example.test/bundle.js",
	}
	if !reflect.DeepEqual(links, want) {
		t.Errorf("links = %v, want %v", links, want)
	}
	if wantScripts := []string{"http://example.test/bundle.js"}; !reflect.DeepEqual(scripts, wantScripts) {
		t.Errorf("scripts = %v, want %v", scripts, wantScripts)
	}
}

func TestExtractHTML_ResolvesRelativeAndStripsFragment(t *testing.T) {
	body := []byte(`<a href="../sibling?x=1#frag">rel</a>`)
	links, _ := ExtractHTML(body, "http://example.test/dir/page")
	want := []string{"http://example.test/sibling?x=1"}
	if !reflect.DeepEqual(links, want) {
		t.Errorf("links = %v, want %v", links, want)
	}
}

func TestExtractHTML_DedupsWithinOnePage(t *testing.T) {
	body := []byte(`<a href="/x">1</a><a href="/x">2</a>`)
	links, _ := ExtractHTML(body, "http://example.test/")
	if len(links) != 1 {
		t.Errorf("links = %v, want exactly 1 (duplicate hrefs on one page must be deduped)", links)
	}
}

func TestExtractHTML_MalformedBaseURLReturnsNil(t *testing.T) {
	links, scripts := ExtractHTML([]byte(`<a href="/x">x</a>`), "://not-a-url")
	if links != nil || scripts != nil {
		t.Errorf("links=%v scripts=%v, want nil,nil for an unparseable base URL", links, scripts)
	}
}

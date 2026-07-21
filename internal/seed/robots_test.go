package seed

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
)

func TestParseRobots_DisallowAllowSitemap(t *testing.T) {
	body := []byte(`
User-agent: *
Disallow: /old/admin/config.php
Disallow: /admin/*
Allow: /public
Allow:
Disallow: /
Sitemap: http://example.test/sitemap.xml
# a comment line
Disallow: /secret # trailing comment
`)
	res := parseRobots(body)

	want := map[string]string{
		"/old/admin/config.php": "robots:disallow",
		"/admin/":               "robots:disallow", // wildcard truncated
		"/public":               "robots:allow",
		"/secret":               "robots:disallow",
	}
	if len(res.Seeds) != len(want) {
		t.Fatalf("got %d seeds, want %d: %+v", len(res.Seeds), len(want), res.Seeds)
	}
	for _, s := range res.Seeds {
		src, ok := want[s.Path]
		if !ok {
			t.Errorf("unexpected seed path %q", s.Path)
			continue
		}
		if s.Source != src {
			t.Errorf("seed %q: source = %q, want %q", s.Path, s.Source, src)
		}
	}
	if len(res.Sitemaps) != 1 || res.Sitemaps[0] != "http://example.test/sitemap.xml" {
		t.Errorf("Sitemaps = %v, want [http://example.test/sitemap.xml]", res.Sitemaps)
	}
}

func TestParseRobots_EmptyOrAllowAllYieldsNothing(t *testing.T) {
	res := parseRobots([]byte("User-agent: *\nDisallow:\nAllow: /\n"))
	if len(res.Seeds) != 0 {
		t.Errorf("expected no seeds from an allow-everything robots.txt, got %+v", res.Seeds)
	}
}

func newTestClient() *httpclient.Client {
	return httpclient.New(httpclient.Config{})
}

func TestFetchRobots_MissingIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	res, err := FetchRobots(context.Background(), srv.URL, Options{Client: newTestClient()})
	if err != nil {
		t.Fatalf("missing robots.txt should not be an error, got %v", err)
	}
	if len(res.Seeds) != 0 || len(res.Sitemaps) != 0 {
		t.Errorf("expected zero-value result, got %+v", res)
	}
}

func TestFetchRobots_TransportFailureIsAnError(t *testing.T) {
	// A closed listener: nothing is listening, so the request fails at the
	// transport layer, not with a 404 — this must surface as an error the
	// caller can warn on (spec §6).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close()

	_, err := FetchRobots(context.Background(), base, Options{Client: newTestClient()})
	if err == nil {
		t.Fatal("expected a transport error for an unreachable host")
	}
}

func TestFetchRobots_ParsesLiveServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			io.WriteString(w, "User-agent: *\nDisallow: /hidden\n")
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	paced := 0
	res, err := FetchRobots(context.Background(), srv.URL, Options{
		Client: newTestClient(), Pace: func() { paced++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Seeds) != 1 || res.Seeds[0].Path != "/hidden" {
		t.Fatalf("Seeds = %+v, want [{/hidden robots:disallow}]", res.Seeds)
	}
	if paced != 1 {
		t.Errorf("Pace called %d times, want 1", paced)
	}
}

func TestFetchRobots_RespectsInScope(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := FetchRobots(context.Background(), srv.URL, Options{
		Client: newTestClient(), InScope: func(string) bool { return false },
	})
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Error("expected no request when InScope refuses the target")
	}
}

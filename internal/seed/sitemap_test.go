package seed

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"sync/atomic"
	"testing"
)

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func seedPaths(seeds []RawSeed) []string {
	out := make([]string, len(seeds))
	for i, s := range seeds {
		out[i] = s.Path
	}
	sort.Strings(out)
	return out
}

// TestFetchSitemaps_IndexUrlsetGzipAndScope exercises spec §7 DoD #2 in one
// pass: a sitemapindex nesting a plain urlset and a gzip urlset, with one
// out-of-scope <loc> that must be dropped without ever being fetched.
func TestFetchSitemaps_IndexUrlsetGzipAndScope(t *testing.T) {
	// A <loc> on a different host (not backed by any listener — if the scope
	// filter failed and this test package tried to actually fetch it, the
	// DNS lookup would fail and surface as an error below).
	const outOfScope = "http://evil.test/out-of-scope"

	var base string
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<sitemapindex><sitemap><loc>%s/pages.xml</loc></sitemap><sitemap><loc>%s/pages.xml.gz</loc></sitemap></sitemapindex>`, base, base)
	})
	mux.HandleFunc("/pages.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset><url><loc>%s/from-urlset?x=1</loc></url><url><loc>%s</loc></url></urlset>`, base, outOfScope)
	})
	mux.HandleFunc("/pages.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		body := fmt.Sprintf(`<urlset><url><loc>%s/from-gzip</loc></url></urlset>`, base)
		w.Write(gzipBytes(t, body))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	host := hostOnly(t, srv.URL)
	seeds, err := FetchSitemaps(context.Background(), srv.URL, nil, SitemapOptions{
		Options: Options{Client: newTestClient()}, Host: host,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := seedPaths(seeds)
	want := []string{"/from-gzip", "/from-urlset"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("seeds = %v, want %v", got, want)
	}
	for _, s := range seeds {
		if s.Source != "sitemap" {
			t.Errorf("seed %q: source = %q, want %q", s.Path, s.Source, "sitemap")
		}
	}
}

func TestFetchSitemaps_ExtraFromRobots(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound) // no root sitemap; everything comes from robots' declared extra
	})
	var base string
	mux.HandleFunc("/extra.xml", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `<urlset><url><loc>%s/declared-in-robots</loc></url></urlset>`, base)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	seeds, err := FetchSitemaps(context.Background(), srv.URL, []string{srv.URL + "/extra.xml"}, SitemapOptions{
		Options: Options{Client: newTestClient()}, Host: hostOnly(t, srv.URL),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seeds) != 1 || seeds[0].Path != "/declared-in-robots" {
		t.Fatalf("seeds = %+v", seeds)
	}
}

func TestFetchSitemaps_FanOutCapped(t *testing.T) {
	const totalFiles = 30 // more than SitemapMaxFiles
	var base string
	mux := http.NewServeMux()
	var fetched int32
	mux.HandleFunc("/sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fetched, 1)
		var b bytes.Buffer
		b.WriteString("<sitemapindex>")
		for i := 0; i < totalFiles; i++ {
			fmt.Fprintf(&b, "<sitemap><loc>%s/child%d.xml</loc></sitemap>", base, i)
		}
		b.WriteString("</sitemapindex>")
		w.Write(b.Bytes())
	})
	for i := 0; i < totalFiles; i++ {
		i := i
		mux.HandleFunc(fmt.Sprintf("/child%d.xml", i), func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&fetched, 1)
			fmt.Fprintf(w, `<urlset><url><loc>%s/leaf%d</loc></url></urlset>`, base, i)
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base = srv.URL

	seeds, err := FetchSitemaps(context.Background(), srv.URL, nil, SitemapOptions{
		Options: Options{Client: newTestClient()}, Host: hostOnly(t, srv.URL), MaxFiles: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if int(fetched) > 10 {
		t.Errorf("fetched %d files, want <= 10 (MaxFiles)", fetched)
	}
	if len(seeds) >= totalFiles {
		t.Errorf("got %d seeds, expected the fan-out cap to leave some children unvisited", len(seeds))
	}
}

func hostOnly(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

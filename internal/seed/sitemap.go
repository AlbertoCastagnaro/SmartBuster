package seed

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"io"
	"net/url"
	"strings"
)

// SitemapMaxFiles is spec §6's SITEMAP_MAX_FILES default: the nested-
// sitemap fan-out cap.
const SitemapMaxFiles = 20

type sitemapURLSet struct {
	XMLName xml.Name     `xml:"urlset"`
	URLs    []sitemapLoc `xml:"url"`
}

type sitemapLoc struct {
	Loc string `xml:"loc"`
}

type sitemapIndex struct {
	XMLName  xml.Name     `xml:"sitemapindex"`
	Sitemaps []sitemapLoc `xml:"sitemap"`
}

// SitemapOptions configures FetchSitemaps (spec §5.2).
type SitemapOptions struct {
	Options
	Host     string // every <loc> (and nested sitemap URL) is scope-filtered to this host before being followed or emitted
	MaxFiles int    // 0 = SitemapMaxFiles
}

// FetchSitemaps fetches <base>/sitemap.xml plus any sitemap URLs robots.txt
// declared (extra), following sitemapindex nesting and transparently
// decompressing .xml.gz, up to MaxFiles total fetches (spec §5.2, §6).
// Every <loc> — sitemap or page — is scope-filtered to opts.Host before
// being followed or emitted as a seed; out-of-scope entries are dropped
// without a request. A fetch failure on any one file is not fatal to the
// others; the first error encountered (if any) is returned alongside
// whatever seeds were successfully collected (spec §6: graceful
// degradation).
func FetchSitemaps(ctx context.Context, base string, extra []string, opts SitemapOptions) ([]RawSeed, error) {
	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = SitemapMaxFiles
	}
	roots := append([]string{strings.TrimRight(base, "/") + "/sitemap.xml"}, extra...)

	f := &sitemapFetcher{ctx: ctx, opts: opts.Options, host: opts.Host, maxFiles: maxFiles, visited: map[string]bool{}}
	var seeds []RawSeed
	var firstErr error
	for _, root := range roots {
		s, err := f.fetchOne(root)
		seeds = append(seeds, s...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return seeds, firstErr
}

type sitemapFetcher struct {
	ctx      context.Context
	opts     Options
	host     string
	maxFiles int
	fetched  int
	visited  map[string]bool
}

func (f *sitemapFetcher) fetchOne(rawURL string) ([]RawSeed, error) {
	if f.fetched >= f.maxFiles || f.visited[rawURL] {
		return nil, nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Hostname(), f.host) {
		return nil, nil // unparseable or out-of-scope: drop silently, no request made
	}
	if !inScope(f.opts, rawURL) {
		return nil, nil
	}
	f.visited[rawURL] = true
	f.fetched++

	pace(f.opts)
	body, status, err := fetchBody(f.ctx, f.opts.Client, rawURL)
	if err != nil {
		return nil, err
	}
	if status != 200 || len(body) == 0 {
		return nil, nil
	}
	if isGzipContent(rawURL, body) {
		body, err = gunzip(body)
		if err != nil {
			return nil, nil // corrupt archive: skip, not fatal
		}
	}

	var idx sitemapIndex
	if xml.Unmarshal(body, &idx) == nil && len(idx.Sitemaps) > 0 {
		var seeds []RawSeed
		for _, child := range idx.Sitemaps {
			s, _ := f.fetchOne(child.Loc)
			seeds = append(seeds, s...)
		}
		return seeds, nil
	}

	var us sitemapURLSet
	if err := xml.Unmarshal(body, &us); err != nil {
		return nil, nil
	}
	var seeds []RawSeed
	for _, entry := range us.URLs {
		loc, err := url.Parse(entry.Loc)
		if err != nil || !strings.EqualFold(loc.Hostname(), f.host) {
			continue
		}
		seeds = append(seeds, RawSeed{Path: loc.Path, Source: "sitemap"})
	}
	return seeds, nil
}

// isGzipContent detects a gzip-compressed sitemap by magic bytes (the
// authoritative check) with the .xml.gz filename extension as a secondary
// signal for servers that omit/misreport Content-Encoding.
func isGzipContent(rawURL string, body []byte) bool {
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		return true
	}
	return strings.HasSuffix(strings.ToLower(rawURL), ".gz")
}

func gunzip(body []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, maxSeedBody))
}

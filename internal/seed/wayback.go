package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// CDXBaseURL is the real Wayback Machine CDX API endpoint (spec §5.3).
	CDXBaseURL = "http://web.archive.org/cdx/search/cdx"
	// WaybackMaxDefault is spec §6's WAYBACK_MAX default: the CDX row cap
	// applied before scope/asset/dedup filtering.
	WaybackMaxDefault = 5000
	// ArchiveRateDefault is spec §6's ARCHIVE_RATE default: the polite
	// limiter archive.org gets — independent of the target's own
	// rate/stealth settings, since this traffic goes to a third-party host,
	// not the target (spec §5.3).
	ArchiveRateDefault = 1.0 // req/s
)

// Wayback queries the CDX API for a host's historical URLs (spec §5.3),
// entirely off-target. BaseURL defaults to the real archive.org endpoint but
// is overridable so tests can point it at a stub server instead (spec §7:
// "hermetic ... no live archive.org dependency in CI").
type Wayback struct {
	BaseURL string       // "" = CDXBaseURL
	Client  *http.Client // nil = http.DefaultClient
	Max     int          // 0 = WaybackMaxDefault
	Pace    func()       // called once before the CDX request, via the *separate* archive.org limiter (spec §5.3) — never the target's
}

// Fetch implements SeedSource: query CDX for host and parse rows into
// RawSeeds, scope-filtered to host (spec §5.3). A query/transport failure is
// returned as an error; the caller (engine) treats it as a graceful-
// degradation warning, not fatal (spec §6).
func (w *Wayback) Fetch(ctx context.Context, host string) ([]RawSeed, error) {
	base := w.BaseURL
	if base == "" {
		base = CDXBaseURL
	}
	max := w.Max
	if max <= 0 {
		max = WaybackMaxDefault
	}
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}

	q := url.Values{}
	q.Set("url", host+"/*")
	q.Set("output", "json")
	q.Set("collapse", "urlkey")
	q.Set("fl", "original,timestamp")
	q.Set("limit", strconv.Itoa(max))
	reqURL := base + "?" + q.Encode()

	if w.Pace != nil {
		w.Pace()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wayback: CDX returned status %d", resp.StatusCode)
	}

	var rows [][]string
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("wayback: decode CDX response: %w", err)
	}
	if len(rows) <= 1 {
		return nil, nil // empty, or just the column header
	}
	rows = rows[1:] // CDX's own convention: row 0 is the ("original","timestamp") header, not data

	seeds := make([]RawSeed, 0, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		original, timestamp := row[0], row[1]
		u, err := url.Parse(original)
		if err != nil || !strings.EqualFold(u.Hostname(), host) {
			continue // scope-filter to host (spec §5.3)
		}
		seeds = append(seeds, RawSeed{Path: u.Path, Source: "wayback:" + waybackDate(timestamp)})
	}
	return seeds, nil
}

// waybackDate trims a CDX timestamp (YYYYMMDDhhmmss) to its date portion
// (spec §3: "Provenance names the source (+ capture date for Wayback)").
func waybackDate(ts string) string {
	if len(ts) >= 8 {
		return ts[:8]
	}
	return ts
}

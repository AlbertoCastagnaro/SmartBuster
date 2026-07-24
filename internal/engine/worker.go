package engine

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/cespare/xxhash/v2"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/calibration"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/harvest"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/simhash"
)

// RunWorker is a stateless HTTP executor: it never touches coordinator
// state, only (item, network) -> WorkResult. ctx cancellation is respected
// both while idle (waiting on workCh) and while trying to hand back a result.
// harvestEnabled is scan-wide (Config.Crawl || Config.JSHarvest) and gates
// body retention for ordinary candidate responses (spec §2); it never
// changes over a scan's lifetime, so passing it once here is enough. client
// is the spec §6 HTTPDoer boundary — net/http today, a tls-client
// implementation behind the stealth preset in Phase 6b — so the worker
// itself never depends on the concrete transport.
func RunWorker(ctx context.Context, workCh <-chan WorkItem, resultsCh chan<- WorkResult, client httpclient.HTTPDoer, harvestEnabled bool) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-workCh:
			if !ok {
				return
			}
			result := process(ctx, item, client, harvestEnabled)
			select {
			case resultsCh <- result:
			case <-ctx.Done():
				return
			}
		}
	}
}

func process(ctx context.Context, item WorkItem, client httpclient.HTTPDoer, harvestEnabled bool) WorkResult {
	resp, err := client.Do(ctx, httpclient.Request{URL: item.URL, Headers: item.Headers})
	if err != nil {
		return WorkResult{Item: item, Err: err}
	}
	defer resp.Body.Close()
	elapsed := resp.Elapsed

	ct := mediaType(resp)

	// Body retention is scoped (spec §2, contract C): an explicit harvest
	// fetch (JS bundle / SPA-pivot root, requested by a producer that
	// already knows the URL is worth mining) always retains up to
	// JS_MAX_BYTES; an ordinary candidate response only retains a body when
	// harvesting is enabled AND its Content-Type looks harvestable, up to
	// the smaller HARVEST_BODY_CAP. Everything else reads (and retains) at
	// calibration's usual MaxBody, unchanged from Phase 1.
	readCap := int64(calibration.MaxBody)
	harvestable := false
	switch {
	case item.IsHarvestFetch:
		readCap = harvest.JSMaxBytes
		harvestable = isHarvestableContentType(ct)
	case harvestEnabled && isHarvestableContentType(ct):
		readCap = harvest.HarvestBodyCap
		harvestable = true
	}

	body := readCapped(resp.Body, readCap)
	token := requestedToken(item)
	norm := calibration.Normalize(body, token)

	sig := ResponseSignature{
		Status:      resp.StatusCode,
		BodyLen:     len(body),
		WordCount:   calibration.WordCount(norm),
		SimHash:     simhash.SimHash(calibration.Shingles(norm)),
		RawBodyHash: xxhash.Sum64String(norm),
		RedirectTo:  normalizeRedirect(resp.Header.Get("Location")),
		ContentType: ct,
		SetCookie:   resp.Header.Get("Set-Cookie") != "",
		Reflected:   item.IsProbe && bytes.Contains(body, []byte(token)),
		Elapsed:     elapsed,
		// HasIndexOf is spec §3.1's open-directory-listing signal (Phase 3).
		// norm is already computed for every response, so this costs one
		// cheap substring scan — unlike NormBody, it doesn't retain the body.
		HasIndexOf: strings.Contains(norm, "index of"),
	}
	if item.IsProbe {
		// Calibration probes are few per directory (N_PROBES*len(exts));
		// keeping the normalized text here (unlike ordinary candidates)
		// lets Phase 2a's error-page tech signal reuse the baseline's
		// representative sample instead of an extra request.
		sig.NormBody = norm
	}
	if harvestable && resp.StatusCode == http.StatusOK && int64(len(body)) < readCap {
		sig.HarvestBody = body
	}
	return WorkResult{Item: item, Signature: sig}
}

// isHarvestableContentType is spec §2's Content-Type gate: only these three
// media types are ever worth mining for links/endpoints.
func isHarvestableContentType(ct string) bool {
	switch ct {
	case "text/html", "application/javascript", "application/json":
		return true
	default:
		return false
	}
}

// readCapped reads at most max bytes from r. A read error (e.g. connection
// reset mid-body) is not fatal: whatever was read before the error is still
// a usable partial signature.
func readCapped(r io.Reader, max int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, max))
	return b
}

// requestedToken extracts the last path segment of the requested URL — the
// word for a real candidate, the random token for a calibration probe. This
// is the token normalization strips from the body to defeat reflected-path
// soft-404s.
func requestedToken(item WorkItem) string {
	u, err := url.Parse(item.URL)
	if err != nil {
		return item.Candidate.Path
	}
	return path.Base(u.Path)
}

// normalizeRedirect reduces a Location header to just its path, so baseline
// comparison isn't thrown off by scheme/host/query variation across requests.
func normalizeRedirect(location string) string {
	if location == "" {
		return ""
	}
	u, err := url.Parse(location)
	if err != nil {
		return location
	}
	p := u.Path
	if p == "" {
		p = "/"
	}
	return p
}

func mediaType(resp httpclient.Response) string {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ct
	}
	return mt
}

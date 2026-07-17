package engine

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"

	"github.com/cespare/xxhash/v2"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/calibration"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/simhash"
)

// RunWorker is a stateless HTTP executor: it never touches coordinator
// state, only (item, network) -> WorkResult. ctx cancellation is respected
// both while idle (waiting on workCh) and while trying to hand back a result.
func RunWorker(ctx context.Context, workCh <-chan WorkItem, resultsCh chan<- WorkResult, client *httpclient.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-workCh:
			if !ok {
				return
			}
			result := process(ctx, item, client)
			select {
			case resultsCh <- result:
			case <-ctx.Done():
				return
			}
		}
	}
}

func process(ctx context.Context, item WorkItem, client *httpclient.Client) WorkResult {
	resp, elapsed, err := client.Do(ctx, item.URL)
	if err != nil {
		return WorkResult{Item: item, Err: err}
	}
	defer resp.Body.Close()

	body := readCapped(resp.Body, calibration.MaxBody)
	token := requestedToken(item)
	norm := calibration.Normalize(body, token)

	sig := ResponseSignature{
		Status:      resp.StatusCode,
		BodyLen:     len(body),
		WordCount:   calibration.WordCount(norm),
		SimHash:     simhash.SimHash(calibration.Shingles(norm)),
		RawBodyHash: xxhash.Sum64String(norm),
		RedirectTo:  normalizeRedirect(resp.Header.Get("Location")),
		ContentType: mediaType(resp),
		SetCookie:   resp.Header.Get("Set-Cookie") != "",
		Reflected:   item.IsProbe && bytes.Contains(body, []byte(token)),
		Elapsed:     elapsed,
	}
	if item.IsProbe {
		// Calibration probes are few per directory (N_PROBES*len(exts));
		// keeping the normalized text here (unlike ordinary candidates)
		// lets Phase 2a's error-page tech signal reuse the baseline's
		// representative sample instead of an extra request.
		sig.NormBody = norm
	}
	return WorkResult{Item: item, Signature: sig}
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

func mediaType(resp *http.Response) string {
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

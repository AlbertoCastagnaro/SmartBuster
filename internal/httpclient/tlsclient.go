// tlsclient.go is Phase 6b's tier-3 fingerprint client (spec §2): a
// bogdanfinn/tls-client-backed HTTPDoer that presents a coherent browser
// identity — TLS ClientHello (JA3/JA4), HTTP/2 SETTINGS/pseudo-header order
// (the Akamai fingerprint), and realistic header values *in browser order*
// (spec §3, completing 6a's deferred tier-2) — all four layers bundled
// together as one BrowserProfile, so a Chrome JA3 never wears Firefox
// headers (spec §2's coherence rule). Selected by the active preset's
// TLSProfile field; "" (every preset but stealth) keeps 6a's plain
// net/http-backed Client instead.
package httpclient

import (
	"context"
	"net/http"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// HeaderKV is one ordered header entry in a BrowserProfile's fixed header
// set (spec §3): a slice, not a map, because the whole point is the wire
// order tls-client will actually emit.
type HeaderKV struct {
	Key   string
	Value string
}

// BrowserProfile bundles every fingerprint layer one real browser presents
// together (spec §2): TLSClient carries bogdanfinn's TLS+HTTP/2 bundle
// (ClientHello, SETTINGS/pseudo-header order) for Name's browser/version;
// Headers is that same browser's realistic header set, in the order it's
// actually sent (referer, when the caller's request carries one, is
// spliced in at refererAfter — the position a real browser would put it).
type BrowserProfile struct {
	Name         string
	TLSClient    profiles.ClientProfile
	Headers      []HeaderKV
	refererAfter string // Headers[i].Key this profile's Referer follows; "" = append at the end
}

// Fingerprint profile names (Config.Fingerprint / Preset.TLSProfile, spec
// §7): coherent bundles, one per browser. Deliberately the same string
// values as the plain-client header profiles (ProfileChrome etc, headers.go)
// — spec §2 unifies 6a's HeaderProfile and 6b's TLSProfile into one key —
// but kept as a separate map here since a BrowserProfile carries strictly
// more (a TLS+HTTP2 bundle, and header order) than headers.go's value-only
// http.Header.
//
// Versions are chosen to match tls-client's bundled profile as closely as
// possible (Chrome 124 has an exact bundled profile; Firefox/Safari's UA
// strings are pinned to the nearest bundled version — 120 and 16.0
// respectively — rather than 6a's original 126/17.4) so the User-Agent
// string itself doesn't contradict the JA3 it rides on (spec §2's coherence
// rule applies to version drift, not just cross-browser mismatch).
var browserProfiles = map[string]BrowserProfile{
	ProfileChrome: {
		Name:      ProfileChrome,
		TLSClient: profiles.Chrome_124,
		Headers: []HeaderKV{
			{"sec-ch-ua", `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`},
			{"sec-ch-ua-mobile", "?0"},
			{"sec-ch-ua-platform", `"Windows"`},
			{"upgrade-insecure-requests", "1"},
			{"user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"},
			{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"},
			{"sec-fetch-site", "none"},
			{"sec-fetch-mode", "navigate"},
			{"sec-fetch-user", "?1"},
			{"sec-fetch-dest", "document"},
			{"accept-encoding", "gzip, deflate, br"},
			{"accept-language", "en-US,en;q=0.9"},
		},
		refererAfter: "sec-fetch-dest",
	},
	ProfileFirefox: {
		Name:      ProfileFirefox,
		TLSClient: profiles.Firefox_120,
		Headers: []HeaderKV{
			{"user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:120.0) Gecko/20100101 Firefox/120.0"},
			{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
			{"accept-language", "en-US,en;q=0.5"},
			{"accept-encoding", "gzip, deflate, br"},
			{"upgrade-insecure-requests", "1"},
			{"sec-fetch-dest", "document"},
			{"sec-fetch-mode", "navigate"},
			{"sec-fetch-site", "none"},
			{"sec-fetch-user", "?1"},
		},
		refererAfter: "accept-encoding",
	},
	ProfileSafari: {
		Name:      ProfileSafari,
		TLSClient: profiles.Safari_16_0,
		Headers: []HeaderKV{
			{"accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8"},
			{"accept-language", "en-US,en;q=0.9"},
			{"accept-encoding", "gzip, deflate, br"},
			{"upgrade-insecure-requests", "1"},
			{"user-agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Safari/605.1.15"},
		},
		refererAfter: "accept-encoding",
	},
}

// BrowserProfileFor returns name's bundle (spec §2), ok=false for an
// unrecognized or off ("") name — the caller's cue to stay on the plain
// net/http Client instead of constructing a TLSDoer.
func BrowserProfileFor(name string) (BrowserProfile, bool) {
	p, ok := browserProfiles[name]
	return p, ok
}

// orderedHeader builds this profile's full header set for one request,
// fhttp's magic Header-Order key (spec §3: "supply the header list as an
// ordered slice ... and let the client emit them in browser order") giving
// tls-client the wire order to actually honor. referer, when non-empty
// (spec §5 referer chains), is spliced in immediately after refererAfter —
// the position a real browser would put it — rather than tacked on the end.
func (p BrowserProfile) orderedHeader(referer string) fhttp.Header {
	h := fhttp.Header{}
	order := make([]string, 0, len(p.Headers)+1)
	for _, kv := range p.Headers {
		h.Set(kv.Key, kv.Value)
		order = append(order, kv.Key)
		if referer != "" && kv.Key == p.refererAfter {
			h.Set("referer", referer)
			order = append(order, "referer")
		}
	}
	if referer != "" && p.refererAfter == "" {
		h.Set("referer", referer)
		order = append(order, "referer")
	}
	h[fhttp.HeaderOrderKey] = order
	return h
}

// TLSDoer implements HTTPDoer via bogdanfinn/tls-client (spec §2): one
// tls-client instance, constructed once for the fingerprint's whole
// lifetime (spec §5: "one browser identity per session" — a TLS connection
// pool already committed to a JA3 can't rotate fingerprints request to
// request, and doing so would be its own tell regardless). Every layer —
// TLS ClientHello, HTTP/2 settings, header values, header order — comes
// from the single embedded BrowserProfile; the caller's req.Headers is
// consulted only for its Referer (spec §5's per-request referer chains),
// everything else about it is ignored so the profile stays the sole source
// of truth for identity (spec §2's coherence rule).
type TLSDoer struct {
	client  tlsclient.HttpClient
	profile BrowserProfile
}

// NewTLSDoer builds a TLSDoer for profileName (spec §2), routed through
// proxyURL if non-empty (spec §5: "--proxy <url> ... passed to tls-client at
// construction"; "" = direct connection, unchanged fingerprint either way).
// Redirects are never auto-followed, mirroring Client.Do — the 30x itself is
// what calibration classifies.
func NewTLSDoer(cfg Config, profileName string, proxyURL string) (*TLSDoer, error) {
	profile, ok := BrowserProfileFor(profileName)
	if !ok {
		profile = browserProfiles[ProfileChrome]
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	maxIdlePerHost := cfg.Concurrency
	if maxIdlePerHost <= 0 {
		maxIdlePerHost = DefaultPerHostConnCap
	}
	perHostCap := cfg.PerHostConnCap
	if perHostCap <= 0 {
		perHostCap = DefaultPerHostConnCap
	}

	opts := []tlsclient.HttpClientOption{
		tlsclient.WithTimeoutMilliseconds(int(cfg.RequestTimeout.Milliseconds())),
		tlsclient.WithClientProfile(profile.TLSClient),
		tlsclient.WithNotFollowRedirects(),
		tlsclient.WithTransportOptions(&tlsclient.TransportOptions{
			MaxIdleConnsPerHost: maxIdlePerHost,
			MaxConnsPerHost:     perHostCap,
			IdleConnTimeout:     durationPtr(DefaultIdleConnTimeout),
		}),
	}
	opts = withProxy(opts, proxyURL)

	c, err := tlsclient.NewHttpClient(tlsclient.NewNoopLogger(), opts...)
	if err != nil {
		return nil, err
	}
	return &TLSDoer{client: c, profile: profile}, nil
}

func durationPtr(d time.Duration) *time.Duration { return &d }

// Do issues one GET as this TLSDoer's BrowserProfile (spec §4: the same
// bundle for every on-target request — candidate, profiling, seed,
// harvest-fetch — once fingerprinting is active for the scan).
func (t *TLSDoer) Do(ctx context.Context, req Request) (Response, error) {
	fReq, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, req.URL, nil)
	if err != nil {
		return Response{}, err
	}
	referer := ""
	if req.Headers != nil {
		referer = req.Headers.Get("Referer")
	}
	fReq.Header = t.profile.orderedHeader(referer)

	start := time.Now()
	resp, err := t.client.Do(fReq)
	elapsed := time.Since(start)
	if err != nil {
		return Response{}, err
	}
	return Response{
		StatusCode: resp.StatusCode,
		Header:     convertHeader(resp.Header),
		Body:       resp.Body,
		Elapsed:    elapsed,
	}, nil
}

// convertHeader converts fhttp.Header to net/http.Header: both are
// map[string][]string under the hood, so this is a plain reinterpretation,
// not a copy — Response.Header (and Response.Cookies, which builds an
// *http.Response around it) stay exactly the same shape regardless of which
// HTTPDoer produced them.
func convertHeader(h fhttp.Header) http.Header {
	return http.Header(map[string][]string(h))
}

var _ HTTPDoer = (*TLSDoer)(nil)

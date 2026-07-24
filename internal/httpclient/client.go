// Package httpclient provides the tuned HTTP transport and the global
// rate-limiting pacer used by the coordinator's dispatch loop (spec §5).
package httpclient

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

const (
	DefaultPerHostConnCap  = 20
	DefaultRequestTimeout  = 10 * time.Second
	DefaultIdleConnTimeout = 90 * time.Second
	DefaultUserAgent       = "smartbuster/0.1"
)

// Config controls the tuned Transport.
type Config struct {
	Concurrency    int           // sizes MaxIdleConnsPerHost
	PerHostConnCap int           // MaxConnsPerHost; default 20
	RequestTimeout time.Duration // per-request timeout; default 10s
	UserAgent      string
}

// Request is what a caller asks an HTTPDoer to fetch (Phase 6a §6's client
// boundary): Headers is the full header set to apply (a header profile plus
// any per-request Referer, already merged by the caller) — net/http's
// Header is an unordered map, so faithful header *ordering* isn't attempted
// here; that's 6b's job once a tls-client implementation can actually honor
// it (spec §5 scope note).
type Request struct {
	URL     string
	Headers http.Header
}

// Response is an HTTPDoer's result. Body is the live response body stream —
// callers read it capped and close it, exactly as they did with the raw
// *http.Response before this boundary existed.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	Elapsed    time.Duration
}

// Cookies parses Set-Cookie headers, mirroring *http.Response.Cookies() —
// a callers-need-it convenience so the profile package's cookie inspection
// doesn't need its own header-parsing/http.Response construction.
func (r Response) Cookies() []*http.Cookie {
	return (&http.Response{Header: r.Header}).Cookies()
}

// HTTPDoer is the seam Phase 6b swaps a tls-client implementation behind
// (spec §6), selected by the active preset's TLSProfile/Proxies fields
// (unused in 6a). Client below is the only implementation in 6a — today's
// stock net/http behavior, unchanged.
type HTTPDoer interface {
	Do(ctx context.Context, req Request) (Response, error)
}

// Client wraps a tuned *http.Client. Redirects are never auto-followed —
// the 30x response itself is classified by calibration. Client implements
// HTTPDoer.
type Client struct {
	http      *http.Client
	userAgent string
}

func New(cfg Config) *Client {
	if cfg.PerHostConnCap <= 0 {
		cfg.PerHostConnCap = DefaultPerHostConnCap
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	maxIdlePerHost := cfg.Concurrency
	if maxIdlePerHost <= 0 {
		maxIdlePerHost = cfg.PerHostConnCap
	}
	userAgent := cfg.UserAgent
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   cfg.RequestTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: maxIdlePerHost,
		MaxConnsPerHost:     cfg.PerHostConnCap,
		IdleConnTimeout:     DefaultIdleConnTimeout,
		DisableCompression:  false,
		DisableKeepAlives:   false,
	}

	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   cfg.RequestTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		userAgent: userAgent,
	}
}

// Do issues a GET request against req.URL and returns the response and
// elapsed time. On success the caller must close resp.Body. req.Headers, if
// set, becomes the request's full header set (a header profile plus any
// per-request Referer — see package httpclient's headers.go); otherwise
// Client falls back to its own configured UserAgent alone, matching pre-6a
// (the "minimal" profile's) behavior.
func (c *Client) Do(ctx context.Context, req Request) (Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return Response{}, err
	}
	if req.Headers != nil {
		httpReq.Header = req.Headers.Clone()
	}
	if httpReq.Header.Get("User-Agent") == "" {
		httpReq.Header.Set("User-Agent", c.userAgent)
	}

	start := time.Now()
	resp, err := c.http.Do(httpReq)
	elapsed := time.Since(start)
	if err != nil {
		return Response{}, err
	}
	return Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: resp.Body, Elapsed: elapsed}, nil
}

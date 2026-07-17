// Package httpclient provides the tuned HTTP transport and the global
// rate-limiting pacer used by the coordinator's dispatch loop (spec §5).
package httpclient

import (
	"context"
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

// Client wraps a tuned *http.Client. Redirects are never auto-followed —
// the 30x response itself is classified by calibration.
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

// Do issues a GET request against url and returns the response, elapsed
// time, and error. On success the caller must close resp.Body.
func (c *Client) Do(ctx context.Context, url string) (*http.Response, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", c.userAgent)

	start := time.Now()
	resp, err := c.http.Do(req)
	elapsed := time.Since(start)
	return resp, elapsed, err
}

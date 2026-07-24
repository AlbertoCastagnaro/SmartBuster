package seed

import (
	"context"
	"io"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
)

// maxSeedBody caps how much of an on-target seed-source response
// (robots.txt, sitemap.xml) is read into memory — mirrors
// profile.maxProfileBody's rationale: an unbounded read of a hostile or
// oversized response is never worth it.
const maxSeedBody = 5 << 20 // 5 MiB

// Options configures the on-target seed sources (robots, sitemap): the
// client, target rate limiter, and scope enforcer every other on-target
// request already goes through (spec §0 contract D/E). Mirrors
// profile.Options, which the same coordinator already builds one of.
type Options struct {
	Client  *httpclient.Client
	InScope func(rawURL string) bool
	Pace    func() // called before each on-target request; nil = unpaced (e.g. direct unit tests)
}

func pace(opts Options) {
	if opts.Pace != nil {
		opts.Pace()
	}
}

func inScope(opts Options, rawURL string) bool {
	return opts.InScope == nil || opts.InScope(rawURL)
}

// fetchBody issues one GET and returns its body (capped to maxSeedBody) and
// status code.
func fetchBody(ctx context.Context, client *httpclient.Client, target string) ([]byte, int, error) {
	resp, err := client.Do(ctx, httpclient.Request{URL: target})
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxSeedBody))
	return body, resp.StatusCode, nil
}

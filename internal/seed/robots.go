package seed

import (
	"bufio"
	"bytes"
	"context"
	"strings"
)

// RobotsResult is robots.txt parsed into seeds plus any Sitemap: URLs found
// (spec §5.1), which feed the sitemap stage.
type RobotsResult struct {
	Seeds    []RawSeed
	Sitemaps []string
}

// FetchRobots fetches <base>/robots.txt through opts.Client (paced/scoped
// like every other on-target request, spec §5.1) and parses Disallow/Allow
// directives across every user-agent group into seeds — Disallow ranked
// highest (spec §3) — plus any Sitemap: URLs. A missing or empty robots.txt
// is not an error (spec §5.1): a 404 or empty body returns a zero
// RobotsResult with a nil error; only a genuine transport failure (spec §6,
// e.g. blocked egress) is returned as an error for the caller to warn on.
func FetchRobots(ctx context.Context, base string, opts Options) (RobotsResult, error) {
	target := strings.TrimRight(base, "/") + "/robots.txt"
	if !inScope(opts, target) {
		return RobotsResult{}, nil
	}
	pace(opts)
	body, status, err := fetchBody(ctx, opts.Client, target)
	if err != nil {
		return RobotsResult{}, err
	}
	if status != 200 || len(body) == 0 {
		return RobotsResult{}, nil
	}
	return parseRobots(body), nil
}

func parseRobots(body []byte) RobotsResult {
	var res RobotsResult
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		key, val, ok := splitDirective(line)
		if !ok {
			continue
		}
		switch strings.ToLower(key) {
		case "disallow":
			if p := robotsPath(val); p != "" {
				res.Seeds = append(res.Seeds, RawSeed{Path: p, Source: "robots:disallow"})
			}
		case "allow":
			if p := robotsPath(val); p != "" {
				res.Seeds = append(res.Seeds, RawSeed{Path: p, Source: "robots:allow"})
			}
		case "sitemap":
			if val != "" {
				res.Sitemaps = append(res.Sitemaps, val)
			}
		}
	}
	return res
}

func splitDirective(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// robotsPath turns a Disallow/Allow value into an enumerable seed path.
// Wildcards/patterns are ignored for enumeration (spec §5.1): "Disallow:
// /admin/*" yields the literal prefix "/admin/". A value that allows
// everything ("" or "/") yields nothing.
func robotsPath(val string) string {
	if i := strings.IndexAny(val, "*$"); i >= 0 {
		val = val[:i]
	}
	if val == "" || val == "/" {
		return ""
	}
	if !strings.HasPrefix(val, "/") {
		val = "/" + val
	}
	return val
}

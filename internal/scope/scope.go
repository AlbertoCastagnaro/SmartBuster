// Package scope enforces which hosts/paths the engine is allowed to touch.
// Consulted on every target and every candidate URL before a request is
// dispatched; out-of-scope URLs are hard-refused.
package scope

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Config declares the allow/exclude lists. Hosts may be an exact hostname,
// a "*.example.com" wildcard, or a CIDR block (for IP targets). Exclusions
// always take precedence over the allowlist.
type Config struct {
	AllowHosts      []string
	ExcludeHosts    []string
	ExcludePatterns []string // regexes matched against the URL path
}

type Scope struct {
	allowHosts   []hostMatcher
	excludeHosts []hostMatcher
	excludeRe    []*regexp.Regexp
}

type hostMatcher struct {
	exact    string
	wildcard string // suffix to match, e.g. ".example.com"
	cidr     *net.IPNet
}

func New(cfg Config) (*Scope, error) {
	s := &Scope{}
	var err error
	if s.allowHosts, err = compileHosts(cfg.AllowHosts); err != nil {
		return nil, err
	}
	if s.excludeHosts, err = compileHosts(cfg.ExcludeHosts); err != nil {
		return nil, err
	}
	for _, p := range cfg.ExcludePatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid exclude pattern %q: %w", p, err)
		}
		s.excludeRe = append(s.excludeRe, re)
	}
	return s, nil
}

func compileHosts(hosts []string) ([]hostMatcher, error) {
	var out []hostMatcher
	for _, h := range hosts {
		if _, ipnet, err := net.ParseCIDR(h); err == nil {
			out = append(out, hostMatcher{cidr: ipnet})
			continue
		}
		if strings.HasPrefix(h, "*.") {
			out = append(out, hostMatcher{wildcard: h[1:]}) // keep leading "."
			continue
		}
		out = append(out, hostMatcher{exact: strings.ToLower(h)})
	}
	return out, nil
}

func (m hostMatcher) matches(host string) bool {
	switch {
	case m.cidr != nil:
		ip := net.ParseIP(host)
		return ip != nil && m.cidr.Contains(ip)
	case m.wildcard != "":
		return strings.HasSuffix(strings.ToLower(host), m.wildcard)
	default:
		return strings.EqualFold(host, m.exact)
	}
}

// InScope reports whether rawURL may be requested: not excluded by host or
// path pattern, and matched by the allowlist when one is configured. An
// empty allowlist means "allow any host not explicitly excluded."
func (s *Scope) InScope(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()

	for _, m := range s.excludeHosts {
		if m.matches(host) {
			return false
		}
	}
	for _, re := range s.excludeRe {
		if re.MatchString(u.Path) {
			return false
		}
	}
	if len(s.allowHosts) == 0 {
		return true
	}
	for _, m := range s.allowHosts {
		if m.matches(host) {
			return true
		}
	}
	return false
}

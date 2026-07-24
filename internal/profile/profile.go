package profile

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
)

const (
	// ProfileFetchTimeout bounds each profiling request (spec §8).
	ProfileFetchTimeout = 8 * time.Second
	maxProfileBody      = 256 * 1024

	// ActiveProbeConfLo/Hi bound the confidence band worth confirming
	// (spec §4.7, §8).
	ActiveProbeConfLo = 0.4
	ActiveProbeConfHi = 0.75
)

// ProfileResponse carries full headers + a body sample for the few
// profiling requests, deliberately separate from the engine's compact
// ResponseSignature (spec §0 contract D: the hot path stays compact).
type ProfileResponse struct {
	StatusCode int
	Header     http.Header
	Cookies    []*http.Cookie
	Body       []byte
}

// Fetch issues one profiling GET, capped to maxProfileBody and
// ProfileFetchTimeout.
func Fetch(ctx context.Context, client *httpclient.Client, target string) (*ProfileResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, ProfileFetchTimeout)
	defer cancel()
	resp, err := client.Do(ctx, httpclient.Request{URL: target})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProfileBody))
	return &ProfileResponse{StatusCode: resp.StatusCode, Header: resp.Header, Cookies: resp.Cookies(), Body: body}, nil
}

// Options configures ProfileTarget/RefineAfterCalibration (spec §3, §8).
type Options struct {
	Ruleset      *Ruleset
	Wappalyzer   *Wappalyzer
	ActiveProbes bool // spec Config.ActiveProbes; default false (passive-only)
	FaviconProbe bool // spec Config.FaviconProbe; default true
	InScope      func(rawURL string) bool
	Pace         func() // called before each active-probe request (rate limiter); nil = unpaced
}

func inScope(opts Options, rawURL string) bool {
	return opts.InScope == nil || opts.InScope(rawURL)
}

// pace acquires one rate-limiter token before a profiling request, if the
// caller wired one up (nil = unpaced, e.g. in tests that construct Options
// directly). Every direct profiling request — root fetch, favicon, active
// probes — goes through this, so scan-start doesn't burst ahead of
// --rate.
func pace(opts Options) {
	if opts.Pace != nil {
		opts.Pace()
	}
}

// ProfileTarget builds the provisional TargetProfile for target: a real
// root GET plus (if enabled) a favicon GET, and every signal that doesn't
// need a calibration baseline (spec §3 steps 3.1, plus favicon from 3.2).
//
// Deviation from the spec's pseudocode (see the Phase 2a handoff report):
// the spec runs favicon/error-page/active-probe refinement in a background
// goroutine so it doesn't block root calibration. This implementation runs
// everything on the coordinator's single goroutine instead — favicon here,
// error-page and active-probe confirmation in RefineAfterCalibration once
// the root baseline exists — trading a few hundred milliseconds of
// scan-start latency for zero concurrent-mutation surface on TargetProfile.
// The provisional profile returned here is still enough to derive
// ExtensionsForStack() for root calibration, per spec §3's own note.
func ProfileTarget(ctx context.Context, client *httpclient.Client, target string, opts Options) *TargetProfile {
	p := newTargetProfile(hostOf(target))
	p.Services = append(p.Services, ServiceTarget{BaseURL: target})

	if inScope(opts, target+"/") {
		pace(opts)
		if root, err := Fetch(ctx, client, target+"/"); err == nil {
			applyHeaderSignals(p, opts.Ruleset, root.Header)
			applyCookieSignals(p, opts.Ruleset, root.Cookies)
			applyHTMLSignals(p, opts.Ruleset, root.Body)
			detectWAF(p, opts.Ruleset, root.Header, root.Cookies, root.Body)
			applyWappalyzerSignal(p, opts.Wappalyzer, root.Header, root.Body)
		}
	}

	if opts.FaviconProbe {
		favURL := target + "/favicon.ico"
		if inScope(opts, favURL) {
			pace(opts)
			if fav, err := Fetch(ctx, client, favURL); err == nil {
				applyFaviconSignal(p, opts.Ruleset, fav.Body)
			}
		}
	}

	return p
}

func hostOf(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	return u.Hostname()
}

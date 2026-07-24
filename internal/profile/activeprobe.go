package profile

import (
	"context"
	"path"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/calibration"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/simhash"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/types"
)

// RefineAfterCalibration applies the signals that need the root directory's
// calibration baseline: the error-page fingerprint (spec §4.6) and active
// confirmer probes (spec §4.7). Called once, when root calibration
// finishes — see ProfileTarget's doc comment for why this is split out
// from the initial profiling pass.
func RefineAfterCalibration(ctx context.Context, client httpclient.HTTPDoer, p *TargetProfile, opts Options, baseline *types.Baseline, repBody string, repStatus int) {
	if opts.Ruleset != nil {
		ApplyErrorPageSignal(p, opts.Ruleset, repBody, repStatus)
	}
	if opts.ActiveProbes {
		runActiveProbes(ctx, client, p, opts, baseline)
	}
}

// runActiveProbes implements spec §4.7: fire the specific confirmer for
// any tech whose fused confidence sits in [ActiveProbeConfLo,
// ActiveProbeConfHi) — worth confirming, not yet certain. A response that
// diverges from the root baseline (calibration.Classify says IsHit)
// confirms the tech, raising it to the rule's target confidence.
func runActiveProbes(ctx context.Context, client httpclient.HTTPDoer, p *TargetProfile, opts Options, baseline *types.Baseline) {
	if opts.Ruleset == nil || len(p.Services) == 0 {
		return
	}
	base := p.Services[0].BaseURL
	for _, r := range opts.Ruleset.ActiveProbes {
		t, ok := p.Tech[r.Tech]
		if !ok || t.Confidence < ActiveProbeConfLo || t.Confidence >= ActiveProbeConfHi {
			continue
		}
		probeURL := base + r.Path
		if !inScope(opts, probeURL) {
			continue
		}
		pace(opts)
		resp, err := Fetch(ctx, client, probeURL)
		if err != nil {
			continue
		}
		if confirms(resp, r.Path, baseline) {
			p.vote(r.Tech, t.Category, t.Layer, SrcActiveProbe, r.Confidence, "", r.ID)
		}
	}
}

// confirms reports whether resp diverges from the root's not-found
// baseline enough to confirm the probed path is really present. Falls
// back to a plain "not a 404" check if no baseline is available (e.g. root
// calibration produced no in-scope probes to build one from).
func confirms(resp *ProfileResponse, probePath string, baseline *types.Baseline) bool {
	if baseline == nil {
		return resp.StatusCode != 404
	}
	token := path.Base(probePath)
	norm := calibration.Normalize(resp.Body, token)
	sig := types.ResponseSignature{
		Status:  resp.StatusCode,
		BodyLen: len(resp.Body),
		SimHash: simhash.SimHash(calibration.Shingles(norm)),
	}
	return calibration.Classify(sig, *baseline).IsHit
}

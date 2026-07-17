package calibration

import (
	"math"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/simhash"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/types"
)

// ResponseSignature, Baseline, and Classification are defined in the leaf
// package internal/types (see its doc comment for why) and aliased here so
// every name in this file — and every existing caller — is unchanged.
type ResponseSignature = types.ResponseSignature
type Baseline = types.Baseline
type Classification = types.Classification

// Calibration constants (spec §13).
const (
	NProbes         = 5
	HammingMargin   = 3
	SPASelfSim      = 2
	WildcardSelfSim = 4
	LenZThreshold   = 3.0
)

// ExtSet is the Phase 1 fixed set of calibration probe extensions, used as
// the fallback before a target profile exists (spec §0 contract A) — e.g.
// --dry-run, which never runs profiling, and PreviewRequests below.
var ExtSet = []string{"", ".php", ".html"}

// Calibrate builds a negative baseline for dir from probes — N_PROBES *
// len(exts) signatures already gathered by the coordinator, grouped
// contiguously by extension (all samples for exts[0], then exts[1], ...).
// exts is supplied by the caller (Phase 2a: profile.ExtensionsForStack(),
// falling back to ExtSet before a profile exists — spec §0 contract A).
func Calibrate(dir string, exts []string, probes []ResponseSignature) Baseline {
	noiseFloor := noiseFloorFromProbes(exts, probes)

	lens := make([]float64, len(probes))
	for i, p := range probes {
		lens[i] = float64(p.BodyLen)
	}
	lenMean, lenStd := meanStdDev(lens)

	rep := medoid(probes)

	isSPA := allStatus(probes, 200) && allHTML(probes) && maxPairwiseHamming(probes) <= SPASelfSim
	isWildcard := !isSPA && !looksLikeStandard404(rep) && maxPairwiseHamming(probes) <= WildcardSelfSim

	return Baseline{
		Dir:         dir,
		Samples:     probes,
		NoiseFloor:  noiseFloor,
		LenMean:     lenMean,
		LenStdDev:   lenStd,
		RepStatus:   rep.Status,
		RepSimHash:  rep.SimHash,
		RepRedirect: rep.RedirectTo,
		IsWildcard:  isWildcard,
		IsSPA:       isSPA,
		RepBody:     rep.NormBody,
	}
}

// Classify decides whether sig diverges from baseline b enough to be a hit,
// and how confidently, per spec §6.2.
func Classify(sig ResponseSignature, b Baseline) Classification {
	if sig.RedirectTo != "" && sig.RedirectTo == b.RepRedirect {
		return Classification{IsHit: false, Confidence: 0.95, Reason: "matches baseline redirect"}
	}

	d := minHamming(sig.SimHash, b.Samples)
	lenZ := math.Abs(float64(sig.BodyLen)-b.LenMean) / math.Max(b.LenStdDev, 1)

	if b.IsWildcard && d <= b.NoiseFloor {
		return Classification{IsHit: false, Confidence: 0.9, Reason: "within wildcard-dir baseline"}
	}
	if d <= b.NoiseFloor {
		return Classification{IsHit: false, Confidence: 0.9, Reason: "within baseline noise floor"}
	}

	conf := 0.5
	if statusClass(sig.Status) != statusClass(b.RepStatus) {
		conf += 0.2
	}
	if d > 2*b.NoiseFloor {
		conf += 0.2
	}
	if lenZ > LenZThreshold {
		conf += 0.1
	}
	if conf > 0.99 {
		conf = 0.99
	}

	// 401/403 = exists-and-protected: strong signal even if body is a login page.
	if sig.Status == 401 || sig.Status == 403 {
		conf = math.Max(conf, 0.85)
	}

	return Classification{IsHit: true, Confidence: conf, Reason: "diverges from baseline"}
}

// Distance returns the minimum Hamming distance from sig to any baseline
// sample — the same value Classify uses internally to decide divergence.
// Exposed for audit-log diagnostics (spec §11: "hamming").
func Distance(sig ResponseSignature, b Baseline) int {
	return minHamming(sig.SimHash, b.Samples)
}

func statusClass(n int) int { return n / 100 }

func minHamming(h uint64, samples []ResponseSignature) int {
	min := -1
	for _, s := range samples {
		d := simhash.Hamming(h, s.SimHash)
		if min == -1 || d < min {
			min = d
		}
	}
	if min == -1 {
		return 0
	}
	return min
}

func maxPairwiseHamming(probes []ResponseSignature) int {
	maxD := 0
	for i := 0; i < len(probes); i++ {
		for j := i + 1; j < len(probes); j++ {
			if d := simhash.Hamming(probes[i].SimHash, probes[j].SimHash); d > maxD {
				maxD = d
			}
		}
	}
	return maxD
}

// probesByExt splits probes into len(exts) contiguous chunks, matching how
// the coordinator appends N_PROBES samples per extension in exts order.
func probesByExt(exts []string, probes []ResponseSignature) [][]ResponseSignature {
	numExts := len(exts)
	if numExts == 0 || len(probes) == 0 {
		return nil
	}
	chunk := len(probes) / numExts
	if chunk == 0 {
		return [][]ResponseSignature{probes}
	}
	groups := make([][]ResponseSignature, 0, numExts)
	for i := 0; i < len(probes); i += chunk {
		end := i + chunk
		if end > len(probes) {
			end = len(probes)
		}
		groups = append(groups, probes[i:end])
	}
	return groups
}

func noiseFloorFromProbes(exts []string, probes []ResponseSignature) int {
	maxD := 0
	for _, group := range probesByExt(exts, probes) {
		for i := 0; i < len(group); i++ {
			for j := i + 1; j < len(group); j++ {
				if d := simhash.Hamming(group[i].SimHash, group[j].SimHash); d > maxD {
					maxD = d
				}
			}
		}
	}
	return maxD + HammingMargin
}

func meanStdDev(vals []float64) (mean, std float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	mean = sum / float64(len(vals))

	var variance float64
	for _, v := range vals {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(len(vals))
	return mean, math.Sqrt(variance)
}

// medoid returns the sample with the minimum total distance to all others.
func medoid(probes []ResponseSignature) ResponseSignature {
	if len(probes) == 0 {
		return ResponseSignature{}
	}
	best := 0
	bestSum := -1
	for i := range probes {
		sum := 0
		for j := range probes {
			if i == j {
				continue
			}
			sum += simhash.Hamming(probes[i].SimHash, probes[j].SimHash)
		}
		if bestSum == -1 || sum < bestSum {
			bestSum = sum
			best = i
		}
	}
	return probes[best]
}

func allStatus(probes []ResponseSignature, status int) bool {
	for _, p := range probes {
		if p.Status != status {
			return false
		}
	}
	return true
}

func allHTML(probes []ResponseSignature) bool {
	for _, p := range probes {
		if p.ContentType != "text/html" {
			return false
		}
	}
	return true
}

// looksLikeStandard404 reports whether rep looks like an ordinary "not
// found" response. Phase 1: status in {404,410} is sufficient (spec §6.1);
// the wildcard test mainly needs to catch "200-for-everything under this dir."
func looksLikeStandard404(rep ResponseSignature) bool {
	return rep.Status == 404 || rep.Status == 410
}

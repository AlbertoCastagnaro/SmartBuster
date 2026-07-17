package calibration

import (
	"context"
	"io"
	"math/rand"
	"net/url"
	"strings"
	"testing"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/simhash"
	"github.com/AlbertoCastagnaro/SmartBuster/test/fixtures"
)

// --- test-local fetch helpers (deliberately independent of package engine,
// so calibration is validated in isolation per the spec's build order). ---

func fetchSignature(t *testing.T, client *httpclient.Client, rawURL, token string) ResponseSignature {
	t.Helper()
	resp, elapsed, err := client.Do(context.Background(), rawURL)
	if err != nil {
		t.Fatalf("request %s: %v", rawURL, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxBody))
	norm := Normalize(body, token)

	return ResponseSignature{
		Status:      resp.StatusCode,
		BodyLen:     len(body),
		WordCount:   WordCount(norm),
		SimHash:     simhash.SimHash(Shingles(norm)),
		RedirectTo:  testNormalizeRedirect(resp.Header.Get("Location")),
		ContentType: testMediaType(resp.Header.Get("Content-Type")),
		Elapsed:     elapsed,
	}
}

func testNormalizeRedirect(location string) string {
	if location == "" {
		return ""
	}
	u, err := url.Parse(location)
	if err != nil {
		return location
	}
	if u.Path == "" {
		return "/"
	}
	return u.Path
}

// testMediaType strips any ";charset=..." parameters, matching the base
// media type production code compares against (e.g. "text/html").
func testMediaType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.TrimSpace(contentType)
}

func randToken(rng *rand.Rand, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

func genProbes(t *testing.T, client *httpclient.Client, rng *rand.Rand, baseURL, dir string) []ResponseSignature {
	t.Helper()
	var probes []ResponseSignature
	for _, ext := range ExtSet {
		for i := 0; i < NProbes; i++ {
			token := randToken(rng, 12)
			p := dir + "/" + token + ext
			probes = append(probes, fetchSignature(t, client, baseURL+p, token))
		}
	}
	return probes
}

func newTestClient() *httpclient.Client {
	return httpclient.New(httpclient.Config{})
}

// --- fixture-driven table tests (spec §14) ---

func TestCalibration_Hard404_NoFalsePositivesFullRecall(t *testing.T) {
	fx := fixtures.NewHard404()
	defer fx.Close()
	client := newTestClient()
	rng := rand.New(rand.NewSource(1))

	baseline := Calibrate("", genProbes(t, client, rng, fx.URL, ""))

	for _, real := range fx.RealPaths {
		sig := fetchSignature(t, client, fx.URL+real, real[1:])
		c := Classify(sig, baseline)
		if !c.IsHit {
			t.Errorf("expected real path %s to be classified as a hit, got %+v", real, c)
		}
	}

	for i := 0; i < 10; i++ {
		token := randToken(rng, 10)
		sig := fetchSignature(t, client, fx.URL+"/"+token, token)
		c := Classify(sig, baseline)
		if c.IsHit {
			t.Errorf("false positive on fake path /%s: %+v", token, c)
		}
	}
}

func TestCalibration_Reflected404_NoFalsePositives(t *testing.T) {
	fx := fixtures.NewReflected404()
	defer fx.Close()
	client := newTestClient()
	rng := rand.New(rand.NewSource(2))

	baseline := Calibrate("", genProbes(t, client, rng, fx.URL, ""))

	for _, real := range fx.RealPaths {
		sig := fetchSignature(t, client, fx.URL+real, real[1:])
		if c := Classify(sig, baseline); !c.IsHit {
			t.Errorf("expected real path %s to be a hit, got %+v", real, c)
		}
	}
	for i := 0; i < 10; i++ {
		token := randToken(rng, 10)
		sig := fetchSignature(t, client, fx.URL+"/"+token, token)
		if c := Classify(sig, baseline); c.IsHit {
			t.Errorf("false positive on fake path /%s (reflected-path soft-404): %+v", token, c)
		}
	}
}

func TestCalibration_Volatile404_NoFalsePositives(t *testing.T) {
	fx := fixtures.NewVolatile404()
	defer fx.Close()
	client := newTestClient()
	rng := rand.New(rand.NewSource(3))

	baseline := Calibrate("", genProbes(t, client, rng, fx.URL, ""))

	for _, real := range fx.RealPaths {
		sig := fetchSignature(t, client, fx.URL+real, real[1:])
		if c := Classify(sig, baseline); !c.IsHit {
			t.Errorf("expected real path %s to be a hit, got %+v", real, c)
		}
	}
	for i := 0; i < 10; i++ {
		token := randToken(rng, 10)
		sig := fetchSignature(t, client, fx.URL+"/"+token, token)
		if c := Classify(sig, baseline); c.IsHit {
			t.Errorf("false positive on fake path /%s (volatile timestamp/token 404): %+v", token, c)
		}
	}
}

func TestCalibration_WildcardDir_FlaggedAndChildrenSuppressed(t *testing.T) {
	fx := fixtures.NewWildcardDir()
	defer fx.Close()
	client := newTestClient()
	rng := rand.New(rand.NewSource(4))

	baseline := Calibrate("/files", genProbes(t, client, rng, fx.URL, "/files"))
	// A directory returning byte-identical content for every path (hamming
	// distance 0) is indistinguishable, from per-directory probe shape alone,
	// from an SPA shell: isSPA is checked first with the stricter threshold
	// (SPA_SELFSIM=2) before isWildcard's !isSPA guard, so a perfectly
	// self-similar catch-all always lands on IsSPA. Both flags produce
	// identical suppression in Classify, which is the behavior that actually
	// matters here (spec §14: "children suppressed").
	if !baseline.IsWildcard && !baseline.IsSPA {
		t.Fatalf("expected /files flagged as a non-recursable catch-all (wildcard or SPA), got baseline %+v", baseline)
	}

	for i := 0; i < 5; i++ {
		token := randToken(rng, 10)
		sig := fetchSignature(t, client, fx.URL+"/files/"+token, token)
		if c := Classify(sig, baseline); c.IsHit {
			t.Errorf("expected wildcard-dir child /files/%s suppressed, got %+v", token, c)
		}
	}
}

func TestCalibration_SPA_DetectedNoHitFlood(t *testing.T) {
	fx := fixtures.NewSPA()
	defer fx.Close()
	client := newTestClient()
	rng := rand.New(rand.NewSource(5))

	baseline := Calibrate("", genProbes(t, client, rng, fx.URL, ""))
	if !baseline.IsSPA {
		t.Fatalf("expected SPA catch-all detected, got baseline %+v", baseline)
	}

	for i := 0; i < 5; i++ {
		token := randToken(rng, 10)
		sig := fetchSignature(t, client, fx.URL+"/"+token, token)
		if c := Classify(sig, baseline); c.IsHit {
			t.Errorf("expected SPA shell path /%s not reported as a hit, got %+v", token, c)
		}
	}
}

func TestCalibration_Redirect404_SuppressedAndRealFound(t *testing.T) {
	fx := fixtures.NewRedirect404()
	defer fx.Close()
	client := newTestClient()
	rng := rand.New(rand.NewSource(6))

	baseline := Calibrate("", genProbes(t, client, rng, fx.URL, ""))
	if baseline.RepRedirect != "/login" {
		t.Fatalf("expected baseline redirect target /login, got %q", baseline.RepRedirect)
	}

	token := randToken(rng, 10)
	sig := fetchSignature(t, client, fx.URL+"/"+token, token)
	c := Classify(sig, baseline)
	if c.IsHit || c.Reason != "matches baseline redirect" {
		t.Errorf("expected redirect-to-baseline suppressed via exact reason, got %+v", c)
	}

	for _, real := range fx.RealPaths {
		sig := fetchSignature(t, client, fx.URL+real, real[1:])
		if c := Classify(sig, baseline); !c.IsHit {
			t.Errorf("expected real path %s found despite redirect baseline, got %+v", real, c)
		}
	}
}

func TestCalibration_Honest_FullRecall(t *testing.T) {
	fx := fixtures.NewHonest()
	defer fx.Close()
	client := newTestClient()
	rng := rand.New(rand.NewSource(7))

	baseline := Calibrate("", genProbes(t, client, rng, fx.URL, ""))

	for _, real := range fx.RealPaths {
		sig := fetchSignature(t, client, fx.URL+real, real[1:])
		if c := Classify(sig, baseline); !c.IsHit {
			t.Errorf("expected full recall: real path %s should be a hit, got %+v", real, c)
		}
	}
	for i := 0; i < 5; i++ {
		token := randToken(rng, 10)
		sig := fetchSignature(t, client, fx.URL+"/"+token, token)
		if c := Classify(sig, baseline); c.IsHit {
			t.Errorf("false positive on fake path /%s: %+v", token, c)
		}
	}
}

// --- pure unit tests for the internal math, no network ---

func TestClassifyWithinNoiseFloorIsNotHit(t *testing.T) {
	b := Baseline{
		Samples:    []ResponseSignature{{SimHash: 0x0F0F0F0F0F0F0F0F, BodyLen: 100}},
		NoiseFloor: 5,
		LenMean:    100,
		LenStdDev:  2,
		RepStatus:  404,
	}
	sig := ResponseSignature{SimHash: 0x0F0F0F0F0F0F0F0E, BodyLen: 101, Status: 404} // hamming distance 1
	c := Classify(sig, b)
	if c.IsHit {
		t.Fatalf("expected no hit within noise floor, got %+v", c)
	}
}

func TestClassifyDivergesIsHitWithCorroboration(t *testing.T) {
	b := Baseline{
		Samples:    []ResponseSignature{{SimHash: 0x0, BodyLen: 100}},
		NoiseFloor: 2,
		LenMean:    100,
		LenStdDev:  1,
		RepStatus:  404,
	}
	sig := ResponseSignature{SimHash: ^uint64(0), BodyLen: 5000, Status: 200} // max hamming distance, huge lenZ, status class differs
	c := Classify(sig, b)
	if !c.IsHit {
		t.Fatalf("expected hit for maximally diverging response, got %+v", c)
	}
	if c.Confidence < 0.9 {
		t.Fatalf("expected high corroborated confidence, got %f", c.Confidence)
	}
}

func TestClassify401ForcesHighConfidenceFloor(t *testing.T) {
	b := Baseline{
		Samples:    []ResponseSignature{{SimHash: 0x0, BodyLen: 100}},
		NoiseFloor: 2,
		LenMean:    100,
		LenStdDev:  50,
		RepStatus:  404,
	}
	// Small divergence, otherwise low confidence, but status 401 must floor it at 0.85.
	sig := ResponseSignature{SimHash: 0x7, BodyLen: 110, Status: 401}
	c := Classify(sig, b)
	if !c.IsHit {
		t.Fatalf("expected 401 divergence to be a hit, got %+v", c)
	}
	if c.Confidence < 0.85 {
		t.Fatalf("expected confidence floored at 0.85 for 401, got %f", c.Confidence)
	}
}

func TestClassifyMatchesBaselineRedirect(t *testing.T) {
	b := Baseline{RepRedirect: "/login", NoiseFloor: 5}
	sig := ResponseSignature{RedirectTo: "/login"}
	c := Classify(sig, b)
	if c.IsHit || c.Confidence != 0.95 || c.Reason != "matches baseline redirect" {
		t.Fatalf("expected redirect-to-baseline suppression, got %+v", c)
	}
}

func TestMedoidPicksMostCentralSample(t *testing.T) {
	// Three tight samples plus one outlier: the medoid should be one of the
	// tight cluster, not the outlier.
	probes := []ResponseSignature{
		{SimHash: 0b0000},
		{SimHash: 0b0001},
		{SimHash: 0b0011},
		{SimHash: 0xFFFFFFFFFFFFFFFF}, // outlier, far from the cluster
	}
	rep := medoid(probes)
	if rep.SimHash == 0xFFFFFFFFFFFFFFFF {
		t.Fatalf("expected medoid to avoid the outlier, got %x", rep.SimHash)
	}
}

func TestLooksLikeStandard404(t *testing.T) {
	if !looksLikeStandard404(ResponseSignature{Status: 404}) {
		t.Fatal("expected 404 to look like standard 404")
	}
	if !looksLikeStandard404(ResponseSignature{Status: 410}) {
		t.Fatal("expected 410 to look like standard 404")
	}
	if looksLikeStandard404(ResponseSignature{Status: 200}) {
		t.Fatal("expected 200 to not look like standard 404")
	}
}

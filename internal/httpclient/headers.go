package httpclient

import (
	"net/http"
	"sync/atomic"
)

// Header profile names (spec §5): a realistic browser header *set* with
// correct *values*, one profile picked per scan and never rotated per
// request. Faithful header order and Host-header casing are a fingerprint
// layer net/http's Header (an unordered map) can't honor — that's 6b's job
// once a tls-client implementation actually controls wire order; 6a nails
// values + set + identity.
const (
	ProfileMinimal = "minimal"
	ProfileChrome  = "chrome"
	ProfileFirefox = "firefox"
	ProfileSafari  = "safari"
)

// headerProfiles is the fixed realistic header set per browser, values as
// of a recent stable release — good enough to pass a set/values check, not
// a live version-fingerprint service. minimal (today's Phase 1-5 behavior)
// carries no extra headers; Client.Do fills in its own DefaultUserAgent
// when Headers is nil/empty, so ProfileMinimal doesn't even need an entry.
var headerProfiles = map[string]http.Header{
	ProfileChrome: {
		"User-Agent":                {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"},
		"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8"},
		"Accept-Language":           {"en-US,en;q=0.9"},
		"Accept-Encoding":           {"gzip, deflate, br"},
		"Sec-Fetch-Dest":            {"document"},
		"Sec-Fetch-Mode":            {"navigate"},
		"Sec-Fetch-Site":            {"none"},
		"Sec-Fetch-User":            {"?1"},
		"Sec-Ch-Ua":                 {`"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`},
		"Sec-Ch-Ua-Mobile":          {"?0"},
		"Sec-Ch-Ua-Platform":        {`"Windows"`},
		"Upgrade-Insecure-Requests": {"1"},
	},
	ProfileFirefox: {
		"User-Agent":                {"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:126.0) Gecko/20100101 Firefox/126.0"},
		"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"Accept-Language":           {"en-US,en;q=0.5"},
		"Accept-Encoding":           {"gzip, deflate, br"},
		"Sec-Fetch-Dest":            {"document"},
		"Sec-Fetch-Mode":            {"navigate"},
		"Sec-Fetch-Site":            {"none"},
		"Sec-Fetch-User":            {"?1"},
		"Upgrade-Insecure-Requests": {"1"},
	},
	ProfileSafari: {
		"User-Agent":                {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15"},
		"Accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,*/*;q=0.8"},
		"Accept-Language":           {"en-US,en;q=0.9"},
		"Accept-Encoding":           {"gzip, deflate, br"},
		"Upgrade-Insecure-Requests": {"1"},
	},
}

// BuildHeaders returns the full header set for profile (falling back to
// minimal/no extra headers for an unknown name), with referer set as
// Referer when non-empty (spec §5's referer chains). Called per-request —
// cheap map lookups and a clone, not per-scan state — the identity
// *stability* (never rotating UA per request) comes from the caller always
// passing the same profile name for a whole scan (see ProfileState), not
// from anything here.
func BuildHeaders(profile, referer string) http.Header {
	h := http.Header{}
	if base, ok := headerProfiles[profile]; ok {
		h = base.Clone()
	}
	if referer != "" {
		h.Set("Referer", referer)
	}
	return h
}

// ProfileState holds the scan's single active header-profile name, safe for
// concurrent access: the coordinator goroutine is its only writer (a mode
// switch via PATCH, spec §2), but every request built for a WorkItem reads
// it — never per-request rotation, only a deliberate, infrequent, explicit
// mode change (spec §5: "stable per-session identity").
type ProfileState struct {
	v atomic.Value
}

func NewProfileState(name string) *ProfileState {
	s := &ProfileState{}
	s.v.Store(name)
	return s
}

func (s *ProfileState) Load() string {
	return s.v.Load().(string)
}

func (s *ProfileState) Store(name string) {
	s.v.Store(name)
}

package harvest

import (
	"context"
	"errors"
)

// HeadlessPageBudget and HeadlessTimeBudget bound the opt-in headless tier
// (spec §6): heavy and slow, so capped rather than left open-ended.
const (
	HeadlessPageBudget = 20 // max routes navigated per scan (root + confirmed SPA routes)
)

// HeadlessRunner is the pluggable capture backend headless mode drives
// (spec §6): navigate rootURL plus any already-confirmed SPA routes, let
// the app render, and return every URL worth seeding — captured live
// network requests (XHR/fetch — the real API calls) and resolved in-app
// routes/anchors after render. Implementations run the actual browser
// out-of-process; nothing here links a browser into the core binary.
type HeadlessRunner interface {
	Capture(ctx context.Context, rootURL string, routes []string) ([]string, error)
}

// ErrHeadlessUnavailable is returned by NewPlaywrightRunner when no
// out-of-process browser driver is available. Engine wiring treats this as
// spec §6's graceful degradation: a warning event, headless skipped, the
// rest of the scan proceeds unaffected.
var ErrHeadlessUnavailable = errors.New("headless: no browser driver available (playwright not installed)")

// NewPlaywrightRunner is the one shipped HeadlessRunner. Real browser
// automation (playwright-go driving an out-of-process Chromium) is a
// follow-up wire-in — pulling that SDK's dependency tree into this build
// wasn't exercised as part of this pass, so this always reports
// ErrHeadlessUnavailable, which is exactly the "playwright isn't installed"
// path the spec already requires callers to degrade gracefully on.
func NewPlaywrightRunner() (HeadlessRunner, error) {
	return nil, ErrHeadlessUnavailable
}

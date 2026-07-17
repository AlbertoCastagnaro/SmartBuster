package engine

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cespare/xxhash/v2"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/calibration"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/wordlist"
)

// dirLifecycle is the per-directory state machine (spec §3):
// NEW -> CALIBRATING -> SCANNING -> DONE. Real candidates for a directory
// are not dispatched until its baseline exists.
type dirLifecycle int

const (
	dirCalibrating dirLifecycle = iota
	dirScanning
	dirDone
)

const (
	wildcardWindow    = 20 // sliding window for hit-rate trap detection
	tarpitWindow      = 10 // sample count for median-elapsed tarpit detection
	wafRingSize       = 20 // ring buffer size for WAF/rate-limit onset detection
	wafSpikeThreshold = 5  // 429/403 count within the ring that trips backoff
	wafNovelRunLength = 8  // consecutive unexplained responses that trip backoff
)

type dirState struct {
	path        string
	depth       int
	state       dirLifecycle
	branchStart time.Time // inherited from the top-level ancestor; §10 TIME_PER_BRANCH

	probeQueue   []WorkItem
	probesDone   int
	probesTotal  int
	probeResults []calibration.ResponseSignature

	candidatesTotal        int
	candidatesAccountedFor int
	requestsDispatched     int
	budget                 int

	wildcardSuspect bool // §10: hit-rate trap -> stop recursing this branch
	capped          bool // §10: tarpit trap -> stop dispatching this branch

	recentHits    []bool
	recentElapsed []time.Duration
}

type wafSample struct {
	status int
	novel  bool
}

// Option configures a Coordinator at construction time.
type Option func(*Coordinator)

func WithAuditSink(s AuditSink) Option {
	return func(c *Coordinator) {
		if s != nil {
			c.auditSink = s
		}
	}
}

func WithEventEmitter(e EventEmitter) Option {
	return func(c *Coordinator) {
		if e != nil {
			c.emitter = e
		}
	}
}

// Coordinator owns all mutable scan state — the frontier, per-directory
// baselines, discovered content, and the seeded RNG — in a single goroutine.
// Workers are stateless; all cross-goroutine communication is via channels.
type Coordinator struct {
	target   string
	config   Config
	wordlist []wordlist.Entry
	scope    *scope.Scope

	client  *httpclient.Client
	limiter *httpclient.Limiter
	pacer   *httpclient.Pacer
	rng     *rand.Rand

	frontier         *Frontier
	dirs             map[string]*dirState
	calibratingOrder []string
	baselines        map[string]*Baseline
	seenContent      map[uint64][]string
	findings         []Finding

	workCh    chan WorkItem
	resultsCh chan WorkResult
	inFlight  int

	wafRing []wafSample

	auditSink AuditSink
	emitter   EventEmitter
}

// NewCoordinator builds a Coordinator for a single target. sc must not be
// nil (an empty scope.Config still produces a usable "allow everything not
// excluded" scope — see package scope); this keeps scope enforcement
// fail-fast rather than silently permissive.
func NewCoordinator(target string, wl []wordlist.Entry, cfg Config, sc *scope.Scope, opts ...Option) (*Coordinator, error) {
	if sc == nil {
		return nil, fmt.Errorf("scope must not be nil")
	}
	target = strings.TrimRight(target, "/")
	if !sc.InScope(target) {
		return nil, fmt.Errorf("target %q is out of scope", target)
	}
	if len(wl) == 0 {
		return nil, fmt.Errorf("wordlist is empty")
	}

	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.MaxDepth <= 0 {
		cfg.MaxDepth = DefaultMaxDepth
	}
	if cfg.RequestTO <= 0 {
		cfg.RequestTO = DefaultRequestTO
	}

	client := httpclient.New(httpclient.Config{
		Concurrency:    cfg.Concurrency,
		RequestTimeout: cfg.RequestTO,
	})
	limiter := httpclient.NewLimiter(cfg.Rate, cfg.Jitter)
	rng := rand.New(rand.NewSource(cfg.Seed))
	pacer := httpclient.NewPacer(limiter, rng)

	c := &Coordinator{
		target:      target,
		config:      cfg,
		wordlist:    wl,
		scope:       sc,
		client:      client,
		limiter:     limiter,
		pacer:       pacer,
		rng:         rng,
		frontier:    NewFrontier(),
		dirs:        make(map[string]*dirState),
		baselines:   make(map[string]*Baseline),
		seenContent: make(map[uint64][]string),
		workCh:      make(chan WorkItem, cfg.Concurrency),
		resultsCh:   make(chan WorkResult, cfg.Concurrency),
		auditSink:   noopAuditSink{},
		emitter:     noopEmitter{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// Findings returns the confirmed findings discovered so far. Safe to call
// after Run returns.
func (c *Coordinator) Findings() []Finding {
	return c.findings
}

// PreviewRequests returns the URLs a scan of target would send first:
// N_PROBES*len(ExtSet) root-directory calibration probes, followed by the
// root-level wordlist candidates. Deeper URLs depend on live responses and
// can't be previewed. Used by --dry-run, which must print without sending.
// Root probe tokens use the same dirRand(seed, "") derivation startCalibration
// uses, so a dry-run preview exactly matches what a real run would request.
func PreviewRequests(target string, wl []wordlist.Entry, seed int64) []string {
	target = strings.TrimRight(target, "/")
	rng := dirRand(seed, "")

	urls := make([]string, 0, len(calibration.ExtSet)*calibration.NProbes+len(wl))
	for _, ext := range calibration.ExtSet {
		for i := 0; i < calibration.NProbes; i++ {
			urls = append(urls, target+"/"+randToken(rng, 12)+ext)
		}
	}
	for _, entry := range wl {
		urls = append(urls, target+"/"+entry.Word)
	}
	return urls
}

// Run executes the scan to completion or until ctx is cancelled. Workers
// are spawned and joined here; Run does not return until every worker
// goroutine has exited.
func (c *Coordinator) Run(ctx context.Context) {
	c.emit(Event{Type: EventScanStarted, URL: c.target})
	c.seedRoot()

	var wg sync.WaitGroup
	for i := 0; i < c.config.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RunWorker(ctx, c.workCh, c.resultsCh, c.client)
		}()
	}

	c.dispatchLoop(ctx)

	close(c.workCh)
	wg.Wait()
	c.pacer.Stop()
}

func (c *Coordinator) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case res := <-c.resultsCh:
			c.handleResult(res)
		case <-c.pacer.C():
			if item, ok := c.nextDispatchable(); ok {
				c.inFlight++
				if !c.dispatch(ctx, item) {
					return // ctx cancelled while trying to hand off item
				}
			}
			c.pacer.Advance()
		}

		if c.frontier.Empty() && c.inFlight == 0 && c.allDirsDone() {
			c.emit(Event{Type: EventScanFinished})
			return
		}
	}
}

// dispatch hands item to a worker. It must never block on workCh alone: if
// every worker is meanwhile stuck trying to deliver a result (resultsCh
// full, coordinator not otherwise reading it), a bare `workCh <- item`
// deadlocks the whole coordinator. Staying willing to drain resultsCh here
// guarantees forward progress — a freed worker always eventually accepts
// item.
func (c *Coordinator) dispatch(ctx context.Context, item WorkItem) bool {
	for {
		select {
		case c.workCh <- item:
			return true
		case res := <-c.resultsCh:
			c.handleResult(res)
		case <-ctx.Done():
			return false
		}
	}
}

func (c *Coordinator) allDirsDone() bool {
	for _, ds := range c.dirs {
		if ds.state != dirDone {
			return false
		}
	}
	return true
}

func (c *Coordinator) emit(e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	c.emitter.Emit(e)
}

// nextDispatchable implements the coordinator's priority (spec §3):
// (1) pending calibration probes for any CALIBRATING dir, (2) highest-score
// candidate from a SCANNING dir. This guarantees calibration gates real
// requests.
func (c *Coordinator) nextDispatchable() (WorkItem, bool) {
	for _, d := range c.calibratingOrder {
		ds := c.dirs[d]
		if ds != nil && len(ds.probeQueue) > 0 {
			item := ds.probeQueue[0]
			ds.probeQueue = ds.probeQueue[1:]
			return item, true
		}
	}

	// This loop is the single enforcement point for PER_DIR_BUDGET and the
	// tarpit cap. heap.Pop already removed cand from the frontier; every
	// `continue` below is a permanent discard, not a requeue — a candidate
	// that loses here is gone for the rest of the scan, counted via
	// candidatesAccountedFor so the directory can still reach dirDone.
	// Consequently: any candidate that this function actually returns was
	// dispatched at requestsDispatched < budget (checked, then incremented,
	// below) — i.e. it was strictly the (requestsDispatched+1)-th dispatch
	// for that directory, always <= budget. So any WorkResult — and thus any
	// hit reaching handleHit/withinLimits — was necessarily dispatched
	// within budget already; withinLimits does not need (and must not
	// duplicate) a "budget exhausted" check of its own. Trap gates are
	// split by design, not oversight: capped (tarpit) is checked both here,
	// to stop dispatching this directory's own remaining wordlist words at
	// all, and again in withinLimits, to also stop recursing into any hit it
	// already produced; wildcardSuspect is checked only in withinLimits,
	// since spec §10 says a wildcard-suspect branch stops being recursed
	// into but its own wordlist scan keeps running.
	for !c.frontier.Empty() {
		cand := c.frontier.Pop()
		ds := c.dirs[cand.ParentDir]
		if ds == nil || ds.state != dirScanning {
			continue
		}
		if ds.capped || ds.requestsDispatched >= ds.budget {
			ds.candidatesAccountedFor++
			c.maybeFinishDir(ds)
			continue
		}
		url := c.target + cand.ParentDir + "/" + cand.Path
		if !c.scope.InScope(url) {
			ds.candidatesAccountedFor++
			c.maybeFinishDir(ds)
			continue
		}
		ds.requestsDispatched++
		return WorkItem{Candidate: cand, URL: url}, true
	}
	return WorkItem{}, false
}

func (c *Coordinator) maybeFinishDir(ds *dirState) {
	if ds.candidatesAccountedFor >= ds.candidatesTotal {
		ds.state = dirDone
	}
}

func (c *Coordinator) seedRoot() {
	c.startCalibration("", 0, time.Now())
}

// startCalibration fires N_PROBES*len(ExtSet) high-entropy nonexistent
// paths under dir to build its negative baseline (spec §6.1). Probe tokens
// come from a per-directory RNG derived from (seed, dir) — see dirRand —
// rather than the coordinator's single shared RNG, so that which directory
// happens to start calibrating first (a race between concurrent workers
// whenever multiple sibling hits recurse at once) can't perturb any other
// directory's token sequence. Without this, "same seed" would only
// guarantee "same set of directories eventually probed", not "same tokens
// per directory" — replay depends on the latter.
func (c *Coordinator) startCalibration(dir string, depth int, branchStart time.Time) {
	ds := &dirState{path: dir, depth: depth, state: dirCalibrating, branchStart: branchStart}
	c.dirs[dir] = ds
	c.calibratingOrder = append(c.calibratingOrder, dir)

	rng := dirRand(c.config.Seed, dir)
	for _, ext := range calibration.ExtSet {
		for i := 0; i < calibration.NProbes; i++ {
			token := randToken(rng, 12)
			p := dir + "/" + token + ext
			url := c.target + p
			if !c.scope.InScope(url) {
				continue
			}
			ds.probeQueue = append(ds.probeQueue, WorkItem{
				URL:      url,
				IsProbe:  true,
				ProbeTag: dir,
				Candidate: Candidate{
					Path:       token + ext,
					ParentDir:  dir,
					Depth:      depth,
					Provenance: "probe",
				},
			})
		}
	}
	ds.probesTotal = len(ds.probeQueue)

	if ds.probesTotal == 0 {
		// Nothing in scope to probe with: fall back to an empty baseline
		// rather than hang waiting for probes that will never dispatch.
		c.finishCalibration(dir, ds)
	}
}

func randToken(rng *rand.Rand, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

// dirRand returns a *rand.Rand seeded deterministically from (seed, dir),
// so a directory's probe token sequence depends only on those two values —
// never on dispatch order, goroutine scheduling, or how many other
// directories happened to start calibrating first. This is what makes a
// run genuinely replayable from just the recorded seed even when multiple
// sibling directories recurse concurrently (see startCalibration).
func dirRand(seed int64, dir string) *rand.Rand {
	h := xxhash.Sum64String(fmt.Sprintf("%d:%s", seed, dir))
	return rand.New(rand.NewSource(int64(h)))
}

func (c *Coordinator) removeCalibratingDir(dir string) {
	for i, d := range c.calibratingOrder {
		if d == dir {
			c.calibratingOrder = append(c.calibratingOrder[:i], c.calibratingOrder[i+1:]...)
			return
		}
	}
}

func (c *Coordinator) handleResult(res WorkResult) {
	c.inFlight--
	if res.Item.IsProbe {
		c.collectProbe(res)
		return
	}
	c.handleReal(res)
}

func (c *Coordinator) collectProbe(res WorkResult) {
	dir := res.Item.ProbeTag
	ds := c.dirs[dir]
	if ds == nil {
		return
	}

	c.auditSink.WriteRequest(AuditRecord{
		Time: time.Now(), Method: "GET", URL: res.Item.URL, IsProbe: true,
		ParentDir: dir, Provenance: "probe", Signature: res.Signature, Err: res.Err,
	})

	ds.probesDone++
	if res.Err == nil {
		ds.probeResults = append(ds.probeResults, res.Signature)
	}

	if ds.probesDone >= ds.probesTotal {
		c.finishCalibration(dir, ds)
	}
}

func (c *Coordinator) finishCalibration(dir string, ds *dirState) {
	baseline := calibration.Calibrate(dir, ds.probeResults)
	c.baselines[dir] = &baseline
	c.removeCalibratingDir(dir)
	ds.state = dirScanning

	c.emit(Event{Type: EventCalibrationDone, Dir: dir})
	if baseline.IsSPA {
		c.emit(Event{Type: EventWarning, Dir: dir, Message: "brute-force likely futile: SPA catch-all"})
	}
	c.pushWordlistCandidates(dir, ds)
}

// pushWordlistCandidates pushes the full wordlist for dir once its baseline
// exists (spec §9). Phase 1 Score = BasePrio.
func (c *Coordinator) pushWordlistCandidates(dir string, ds *dirState) {
	ds.candidatesTotal = len(c.wordlist)
	if ds.candidatesTotal == 0 {
		ds.state = dirDone
		return
	}
	ds.budget = ds.candidatesTotal // PER_DIR_BUDGET default = wordlist size
	if c.config.PerDirBudget > 0 {
		ds.budget = c.config.PerDirBudget
	}

	provenance := "wordlist"
	if dir != "" {
		provenance = "recursion:" + dir
	}
	for _, entry := range c.wordlist {
		typ := TypeDir
		if entry.Type == wordlist.EntryFile {
			typ = TypeFile
		}
		c.frontier.Push(Candidate{
			Path:       entry.Word,
			Type:       typ,
			BasePrio:   entry.BasePrio,
			Score:      entry.BasePrio,
			Depth:      ds.depth + 1,
			ParentDir:  dir,
			Provenance: provenance,
		})
	}
}

func (c *Coordinator) handleReal(res WorkResult) {
	dir := res.Item.Candidate.ParentDir
	ds := c.dirs[dir]

	if res.Err != nil {
		c.auditSink.WriteRequest(AuditRecord{
			Time: time.Now(), Method: "GET", URL: res.Item.URL, ParentDir: dir,
			Provenance: res.Item.Candidate.Provenance, Err: res.Err,
		})
		if ds != nil {
			ds.candidatesAccountedFor++
			c.maybeFinishDir(ds)
		}
		return
	}

	baseline := c.baselines[dir]
	var cls Classification
	var hamming, noiseFloor int
	if baseline != nil {
		cls = calibration.Classify(res.Signature, *baseline)
		hamming = calibration.Distance(res.Signature, *baseline)
		noiseFloor = baseline.NoiseFloor
	}

	c.auditSink.WriteRequest(AuditRecord{
		Time: time.Now(), Method: "GET", URL: res.Item.URL, ParentDir: dir,
		Provenance: res.Item.Candidate.Provenance, Signature: res.Signature,
		Classified: &cls, BaselineDir: dir, Hamming: hamming, NoiseFloor: noiseFloor,
	})

	if ds != nil {
		ds.recentElapsed = appendCappedDuration(ds.recentElapsed, res.Signature.Elapsed, tarpitWindow)
		c.checkTarpit(ds)
	}

	if cls.IsHit {
		c.handleHit(res, cls, dir)
	}
	if ds != nil {
		ds.recentHits = appendCappedBool(ds.recentHits, cls.IsHit, wildcardWindow)
		c.checkWildcardHitRate(ds)
	}

	c.detectWAFOnset(res)

	if ds != nil {
		ds.candidatesAccountedFor++
		c.maybeFinishDir(ds)
	}
}

// handleHit records a confirmed finding, collapsing already-seen content
// into an alias instead of recursing (novelty gate), then recurses into the
// hit as a new directory if it looks like one and clears the confidence bar.
func (c *Coordinator) handleHit(res WorkResult, cls Classification, dir string) {
	url := res.Item.URL
	hash := res.Signature.RawBodyHash

	if existing, ok := c.seenContent[hash]; ok {
		c.seenContent[hash] = append(existing, url)
		for i := range c.findings {
			if c.findings[i].ContentHash == hash {
				c.findings[i].Aliases = append(c.findings[i].Aliases, url)
				break
			}
		}
		c.emit(Event{Type: EventHit, URL: url, Dir: dir, Confidence: cls.Confidence, Message: "alias"})
		return
	}

	c.seenContent[hash] = []string{url}
	c.findings = append(c.findings, Finding{
		URL: url, Status: res.Signature.Status, Size: res.Signature.BodyLen,
		Confidence: cls.Confidence, Provenance: res.Item.Candidate.Provenance,
		ContentHash: hash,
	})
	c.emit(Event{Type: EventHit, URL: url, Dir: dir, Confidence: cls.Confidence})

	if isDirectory(res) && cls.Confidence >= RecurseMinConf && c.withinLimits(res, dir) {
		childDir := dir + "/" + res.Item.Candidate.Path
		c.enqueueChildDirectory(childDir, res.Item.Candidate.Depth, dir)
	}
}

// isDirectory decides recursion eligibility from the live response, not
// just the wordlist word's shape: TypeDir is only a hint set at load time
// (spec: a word without '.' is a plausible directory); recursion additionally
// requires the response to actually look directory-like.
func isDirectory(res WorkResult) bool {
	if res.Item.Candidate.Type != TypeDir {
		return false
	}
	sig := res.Signature
	switch {
	case sig.Status == 301 || sig.Status == 302:
		return true
	case sig.Status == 401 || sig.Status == 403:
		return true
	case sig.Status >= 200 && sig.Status < 300:
		return !isFileLikeContentType(sig.ContentType)
	default:
		return false
	}
}

func isFileLikeContentType(ct string) bool {
	switch ct {
	case "application/octet-stream", "application/zip", "application/gzip",
		"application/x-tar", "application/pdf", "application/x-rar-compressed",
		"image/png", "image/jpeg", "image/gif", "image/svg+xml", "image/webp",
		"image/x-icon", "font/woff", "font/woff2", "audio/mpeg", "video/mp4":
		return true
	default:
		return false
	}
}

// withinLimits enforces the recursion/trap guards of spec §10 (novelty is
// already handled by the caller before this is reached).
func (c *Coordinator) withinLimits(res WorkResult, dir string) bool {
	cand := res.Item.Candidate
	if cand.Depth+1 > c.config.MaxDepth {
		return false
	}
	childURL := c.target + dir + "/" + cand.Path
	if !c.scope.InScope(childURL) {
		return false
	}
	ds := c.dirs[dir]
	if ds == nil {
		return true
	}
	if ds.capped || ds.wildcardSuspect {
		return false
	}
	// PER_DIR_BUDGET's real enforcement point is nextDispatchable, which
	// already refuses to dispatch candidates beyond the parent's budget —
	// any hit reaching this point was necessarily dispatched within budget,
	// so no separate "budget exhausted" check belongs here. (An earlier
	// version checked requestsDispatched>=budget directly, but dispatch
	// routinely races ahead of result-handling even under the default
	// budget==candidatesTotal, so it tripped on every ordinary small
	// directory and blocked all recursion.)
	if c.config.TimePerBranch > 0 && time.Since(ds.branchStart) >= c.config.TimePerBranch {
		return false
	}
	return true
}

func (c *Coordinator) enqueueChildDirectory(childDir string, depth int, parentDir string) {
	if _, exists := c.dirs[childDir]; exists {
		return
	}
	branchStart := time.Now()
	if pds := c.dirs[parentDir]; pds != nil {
		branchStart = pds.branchStart
	}
	c.startCalibration(childDir, depth, branchStart)
}

// checkWildcardHitRate implements spec §10: a branch whose hit-rate stays
// >= WILDCARD_HITRATE over a window is marked wildcard-suspect and stops
// being recursed into (its own remaining wordlist scan still runs).
func (c *Coordinator) checkWildcardHitRate(ds *dirState) {
	if ds.wildcardSuspect || len(ds.recentHits) < wildcardWindow {
		return
	}
	hits := 0
	for _, h := range ds.recentHits {
		if h {
			hits++
		}
	}
	if float64(hits)/float64(len(ds.recentHits)) >= WildcardHitRate {
		ds.wildcardSuspect = true
		c.emit(Event{Type: EventTrapDetected, Dir: ds.path, Message: "wildcard-suspect: hit-rate too high, recursion stopped for this branch"})
	}
}

// checkTarpit implements spec §10: a branch whose median response time
// approaches the request timeout is deprioritized and capped (no further
// dispatch), so a slow-loris style trap can't hang the scan.
func (c *Coordinator) checkTarpit(ds *dirState) {
	if ds.capped || len(ds.recentElapsed) < tarpitWindow {
		return
	}
	if medianDuration(ds.recentElapsed) >= time.Duration(0.9*float64(c.config.RequestTO)) {
		ds.capped = true
		c.emit(Event{Type: EventTrapDetected, Dir: ds.path, Message: "tarpit-suspect: response times near timeout, branch capped"})
	}
}

// detectWAFOnset is the minimal Phase 1 ring-buffer detector (spec §6.4): a
// spike of 429/403s, or a run of responses that match neither any baseline
// nor prior findings, triggers a temporary pacing backoff.
func (c *Coordinator) detectWAFOnset(res WorkResult) {
	sig := res.Signature
	novel := !c.matchesAnyBaselineOrFinding(sig)
	c.wafRing = append(c.wafRing, wafSample{status: sig.Status, novel: novel})
	if len(c.wafRing) > wafRingSize {
		c.wafRing = c.wafRing[1:]
	}

	spike := 0
	for _, s := range c.wafRing {
		if s.status == 429 || s.status == 403 {
			spike++
		}
	}
	novelRun := 0
	for i := len(c.wafRing) - 1; i >= 0; i-- {
		if !c.wafRing[i].novel {
			break
		}
		novelRun++
	}

	if spike >= wafSpikeThreshold || novelRun >= wafNovelRunLength {
		c.triggerBackoff()
	}
}

func (c *Coordinator) matchesAnyBaselineOrFinding(sig ResponseSignature) bool {
	if _, ok := c.seenContent[sig.RawBodyHash]; ok {
		return true
	}
	for _, b := range c.baselines {
		if calibration.Distance(sig, *b) <= b.NoiseFloor {
			return true
		}
	}
	return false
}

func (c *Coordinator) triggerBackoff() {
	now := time.Now()
	if c.limiter.InBackoff(now) {
		return
	}
	c.limiter.TriggerBackoff(BackoffFactor, BackoffWindow, now)
	c.emit(Event{Type: EventThrottle, Message: "WAF/rate-limit onset detected; backing off"})
}

func appendCappedBool(buf []bool, v bool, max int) []bool {
	buf = append(buf, v)
	if len(buf) > max {
		buf = buf[len(buf)-max:]
	}
	return buf
}

func appendCappedDuration(buf []time.Duration, v time.Duration, max int) []time.Duration {
	buf = append(buf, v)
	if len(buf) > max {
		buf = buf[len(buf)-max:]
	}
	return buf
}

func medianDuration(vals []time.Duration) time.Duration {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), vals...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

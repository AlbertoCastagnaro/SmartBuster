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
	"github.com/AlbertoCastagnaro/SmartBuster/internal/corpus"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/httpclient"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/profile"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/seed"
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

	// knownPaths dedupes Phase 3 generated candidates (spec §3.2, §6)
	// against whatever's already known for this directory — the initial
	// template push, plus anything already generated.
	knownPaths map[string]bool

	// pendingSeeds holds Phase 4a seed candidates for this directory that
	// arrived before its baseline existed (spec §4: a deep seed's ancestor
	// chain is materialized eagerly, before its own calibration completes,
	// via ensureDirChain) — the only place they can land, since pushCandidates
	// hasn't built ds.knownPaths yet. Folded into the candidate set
	// pushCandidates builds once calibration finishes.
	pendingSeeds map[string]Candidate
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

	// Phase 2a target profiling (spec §0 contract B).
	profileState *profile.TargetProfile
	extSet       []string // calibration/probe extensions; profile.ExtensionsForStack() once set
	ruleset      *profile.Ruleset
	wappalyzer   *profile.Wappalyzer
	nmapSeeds    []profile.NmapSeed
	runCtx       context.Context // set at Run() entry; finishCalibration's post-calibration refine needs it
	rootRefined  bool            // RefineAfterCalibration has run for root; guards it to exactly once

	// Phase 2b corpus & selection (spec §0 contract E/F/C). corpusTemplate
	// is nil when -w was given (bypasses the corpus, spec contract G).
	corpusTemplate []corpus.Candidate
	techBoostW     float64
	lastReprioSig  string // guards Reprioritize against thrashing on an unchanged profile (spec §7)

	// Phase 3 dynamic scoring (spec §0 contract B onward). scorer is built
	// once profileState exists (profileTarget); every other field here is
	// resolved from Config at construction time (see NewCoordinator).
	scorer           *DynamicScorer
	scoreWeights     ScoreWeights
	markovOrder      int
	markovMinSamples int
	learnMinConf     float64
	subtreeBurst     int
	epsilon          float64
	epsilonRNG       *rand.Rand
	reprioHits       int
	reprioInterval   time.Duration

	scorerDirty     bool      // spec §6: set by a qualifying discovery, cleared by runDynamicReprio
	hitsSinceReprio int       // spec §6: REPRIO_INTERVAL's hit-count half
	lastDynReprio   time.Time // spec §6: REPRIO_INTERVAL's elapsed-time half
	dynReprioCount  int       // test/diagnostic only: how many times runDynamicReprio actually swept the frontier

	lastDispatchDir string // spec §4: subtree yield cap's consecutive-dispatch tracking
	dispatchStreak  int

	// Phase 4a passive seeding (spec §0 contract E, §5.3). archivePacer is a
	// polite limiter for archive.org — deliberately separate from c.pacer,
	// which paces requests to the *target* (spec §5.3: the target's own
	// rate/stealth settings have nothing to do with a third-party host).
	archivePacer *httpclient.Pacer
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
	// A -w wordlist bypasses the corpus and must be non-empty (spec §0
	// contract G); with no -w, an empty wl means "use the corpus" and is
	// expected, not an error.
	if cfg.Wordlist != "" && len(wl) == 0 {
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

	// Phase 4a (spec §5.3): archive.org gets its own polite limiter,
	// independent of cfg.Rate/cfg.Jitter — those govern the target, not a
	// third-party host.
	archiveLimiter := httpclient.NewLimiter(seed.ArchiveRateDefault, httpclient.DefaultJitter)
	archivePacer := httpclient.NewPacer(archiveLimiter, dirRand(cfg.Seed, "\x00__archive__"))

	rulesOff := cfg.RulesOff
	if rulesOff == nil {
		rulesOff = profile.DefaultRulesOff
	}
	ruleset, err := profile.Load(profile.LoadOptions{
		SystemDir: cfg.RulesetDir, UserDir: cfg.UserRulesDir, RulesOff: rulesOff,
	})
	if err != nil {
		return nil, fmt.Errorf("load ruleset: %w", err)
	}

	techBoostW := cfg.TechBoostW
	if techBoostW <= 0 {
		techBoostW = corpus.DefaultTechBoostW
	}

	// Phase 3 (spec §7): every field here follows this Config's usual
	// "<=0 means apply the default" convention, EXCEPT Weights.WSem/
	// WAssoc/WConv and Epsilon — those are used exactly as given, since 0
	// is itself a meaningful value ("this signal off" / "pure greedy"),
	// matching how Rate/Jitter/TimePerBranch already work. WTech is set
	// here purely for reporting parity (spec §10 handoff); Boost never
	// reads it.
	weights := cfg.Weights
	weights.WTech = techBoostW
	markovOrder := cfg.MarkovOrder
	if markovOrder <= 0 {
		markovOrder = DefaultMarkovOrder
	}
	markovMinSamples := cfg.MarkovMinSamples
	if markovMinSamples <= 0 {
		markovMinSamples = DefaultMarkovMinSamples
	}
	learnMinConf := cfg.LearnMinConf
	if learnMinConf <= 0 {
		learnMinConf = DefaultLearnMinConf
	}
	subtreeBurst := cfg.SubtreeBurst
	if subtreeBurst <= 0 {
		subtreeBurst = DefaultSubtreeBurst
	}
	reprioHits := cfg.ReprioHits
	if reprioHits <= 0 {
		reprioHits = DefaultReprioHits
	}
	reprioInterval := cfg.ReprioInterval
	if reprioInterval <= 0 {
		reprioInterval = DefaultReprioInterval
	}

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
		extSet:      calibration.ExtSet,
		ruleset:     ruleset,
		wappalyzer:  getSharedWappalyzer(),
		techBoostW:  techBoostW,

		scoreWeights:     weights,
		markovOrder:      markovOrder,
		markovMinSamples: markovMinSamples,
		learnMinConf:     learnMinConf,
		subtreeBurst:     subtreeBurst,
		epsilon:          cfg.Epsilon,
		epsilonRNG:       dirRand(cfg.Seed, "\x00__epsilon__"),
		reprioHits:       reprioHits,
		reprioInterval:   reprioInterval,

		archivePacer: archivePacer,
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
	c.runCtx = ctx
	c.emit(Event{Type: EventScanStarted, URL: c.target})
	c.profileTarget(ctx) // spec §0 contract B: before any real dispatch
	c.seedRoot()
	c.seedPassive(ctx) // Phase 4a: robots/sitemap/wayback (spec §2) — root already exists, so pendingSeeds has somewhere to land

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
	c.archivePacer.Stop()
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
	// held collects candidates from a directory currently over its spec §4
	// subtree yield cap: they're still valid and dispatchable, just
	// deferred one round so a different directory gets a turn first. They
	// are pushed back before this function returns via any path (found a
	// different dir to dispatch; served one from held anyway because
	// nothing else was left; or the frontier is simply empty).
	var held []Candidate
	for {
		cand, ok := c.popNext()
		if !ok {
			break
		}
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
		if c.subtreeBurst > 0 && cand.ParentDir == c.lastDispatchDir && c.dispatchStreak >= c.subtreeBurst {
			held = append(held, cand)
			continue
		}

		c.recordDispatch(cand.ParentDir)
		ds.requestsDispatched++
		for _, h := range held {
			c.frontier.Push(h)
		}
		return WorkItem{Candidate: cand, URL: url}, true
	}

	// Frontier exhausted without finding a different directory to
	// round-robin to: every remaining candidate belongs to the throttled
	// dir, so serve one anyway rather than stalling — there's nothing else
	// to switch to.
	if len(held) > 0 {
		cand := held[0]
		for _, h := range held[1:] {
			c.frontier.Push(h)
		}
		ds := c.dirs[cand.ParentDir]
		url := c.target + cand.ParentDir + "/" + cand.Path
		c.recordDispatch(cand.ParentDir)
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
	for _, ext := range c.extSet {
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
	baseline := calibration.Calibrate(dir, c.extSet, ds.probeResults)
	c.baselines[dir] = &baseline
	c.removeCalibratingDir(dir)
	ds.state = dirScanning

	c.emit(Event{Type: EventCalibrationDone, Dir: dir})
	if baseline.IsSPA {
		c.emit(Event{Type: EventWarning, Dir: dir, Message: "brute-force likely futile: SPA catch-all"})
	}

	// The root baseline is what the error-page signal and active-probe
	// confirmation need (spec §4.6, §4.7); refine here rather than in
	// profileTarget, which runs before this baseline exists. Guarded to
	// run exactly once per scan (rootRefined): if refining grows the
	// extension set (below) and root gets re-calibrated, this same
	// function runs again for dir=="" — re-refining against a second
	// baseline built from essentially the same evidence would just
	// re-vote the same rules, artificially inflating confidence via
	// noisy-OR fusion of a signal against itself.
	if dir == "" && c.profileState != nil && !c.rootRefined {
		c.rootRefined = true
		c.profileState.IsSPA = baseline.IsSPA
		profile.RefineAfterCalibration(c.runCtx, c.client, c.profileState, c.profileOpts(), &baseline, baseline.RepBody, baseline.RepStatus)
		c.reprioritizeIfChanged() // spec §7(b): RefineAfterCalibration mutated the profile
		c.emitTechDetected()

		// Refinement (error-page fingerprint, active-probe confirmation)
		// can surface backend tech root calibration didn't know about yet,
		// e.g. a PHP fingerprint only visible in the 404 page itself. If
		// that grows the extension set, the just-built baseline was
		// calibrated against too few extensions — re-run it once before
		// any real candidates are dispatched (pushWordlistCandidates
		// hasn't run yet at this point).
		if newExts := c.profileState.ExtensionsForStack(); extSetGrew(c.extSet, newExts) {
			c.extSet = newExts
			c.emit(Event{Type: EventWarning, Dir: dir, Message: "profile refinement grew the extension set; re-calibrating root"})
			c.startCalibration(dir, ds.depth, ds.branchStart)
			return
		}
	}

	c.pushCandidates(dir, ds)
}

// pushCandidates pushes dir's full candidate set once its baseline exists
// (spec §9): the corpus (spec §0 contract E), unless -w was given, in which
// case it falls back to the flat wordlist (spec §0 contract G).
func (c *Coordinator) pushCandidates(dir string, ds *dirState) {
	if c.corpusTemplate != nil {
		c.pushCorpusCandidates(dir, ds)
		return
	}
	c.pushWordlistCandidates(dir, ds)
}

// pushWordlistCandidates pushes the full wordlist for dir once its baseline
// exists (spec §9, Phase 1 fallback path per spec §0 contract G). Phase 1
// Score = BasePrio. Root additionally gets any nmap-seeded paths (spec §7),
// scored above BasePrio's range so they're tried first; they're TypeFullPath
// so isDirectory() never recurses into them (Phase 2a doesn't attempt to map
// discovered nmap paths as dirs).
func (c *Coordinator) pushWordlistCandidates(dir string, ds *dirState) {
	pending := ds.pendingSeeds
	ds.pendingSeeds = nil

	provenance := "wordlist"
	if dir != "" {
		provenance = "recursion:" + dir
	}
	ds.knownPaths = make(map[string]bool, len(c.wordlist)+len(pending))
	for _, entry := range c.wordlist {
		typ := TypeDir
		if entry.Type == wordlist.EntryFile {
			typ = TypeFile
		}
		cand := Candidate{
			Path:       entry.Word,
			Type:       typ,
			BasePrio:   entry.BasePrio,
			Tags:       []string{"generic"},
			Depth:      ds.depth + 1,
			ParentDir:  dir,
			Provenance: provenance,
		}
		mergeSeedCandidate(pending, &cand) // spec §3: a seed at this path upgrades it in place (max prio, unioned provenance)
		cand.Score = c.scoreCandidate(cand)
		ds.knownPaths[entry.Word] = true
		c.frontier.Push(cand)
	}
	// Whatever's left in pending after the loop above didn't match any
	// template entry — a genuinely new path the seed named (the deep-seed
	// case, spec §4).
	leftoverSeeds := len(pending)
	for path, cand := range pending {
		cand.Score = c.scoreCandidate(cand)
		ds.knownPaths[path] = true
		c.frontier.Push(cand)
	}

	ds.candidatesTotal = len(c.wordlist) + leftoverSeeds
	if dir == "" {
		ds.candidatesTotal += len(c.nmapSeeds)
	}
	if ds.candidatesTotal == 0 {
		ds.state = dirDone
		return
	}
	ds.budget = ds.candidatesTotal // PER_DIR_BUDGET default = wordlist size (+seeds, +nmap at root)
	if c.config.PerDirBudget > 0 {
		ds.budget = c.config.PerDirBudget + leftoverSeeds
		if dir == "" {
			ds.budget += len(c.nmapSeeds)
		}
	}

	if dir == "" {
		for _, ns := range c.nmapSeeds {
			ds.knownPaths[ns.Path] = true
			c.frontier.Push(Candidate{
				Path:       ns.Path,
				Type:       TypeFullPath,
				BasePrio:   1.0,
				Score:      nmapSeedScore,
				Depth:      ds.depth + 1,
				ParentDir:  dir,
				Provenance: ns.Provenance,
			})
		}
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

	c.learnFromHit(res, cls, dir) // spec §5: dirCtx + markov + assoc, confidence-gated (dynamic.go)

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

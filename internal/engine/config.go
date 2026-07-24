package engine

import (
	"time"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/scope"
)

// Config controls one scan (spec §13).
type Config struct {
	Targets     []string
	Wordlist    string
	Concurrency int           // default 20
	Rate        float64       // req/s; 0 = unbounded; default 0
	Jitter      float64       // default 0.30
	MaxDepth    int           // default 4
	RequestTO   time.Duration // default 10s
	Seed        int64         // default: time-based, recorded to audit
	Scope       scope.Config
	DryRun      bool
	OutDir      string

	PerDirBudget  int           // 0 = default to wordlist size (spec §13 PER_DIR_BUDGET)
	TimePerBranch time.Duration // 0 = disabled (spec §13 default)

	// Phase 6a modes, timing & request shape (spec §2, §8). Mode selects a
	// Preset (fast|normal|quiet|stealth; "" defaults to "normal"); Rate/
	// Jitter/Concurrency/Epsilon above still override individual preset
	// fields when explicitly set (see ResolvePreset) — Mode doesn't replace
	// them, it just supplies their defaults. Budget (--budget) is
	// time-budget pacing: 0 disables it, otherwise the scan's target rate
	// is recomputed periodically as remainingFrontier/remainingTime so it
	// spreads over Budget regardless of frontier size.
	Mode          string        // --mode; default "normal"
	Budget        time.Duration // --budget; 0 = off (time-budget pacing)
	JitterKind    string        // override preset's JitterSpec.Kind
	HeaderProfile string        // override preset's header profile

	// Phase 6b TLS/HTTP-2 fingerprint mimicry + proxies (spec §7).
	// Fingerprint is its own axis, orthogonal to Mode (spec §6): ""
	// overrides nothing (stealth still defaults to "chrome", every other
	// preset stays off); a non-empty value selects a BrowserProfile and
	// switches the scan's whole HTTPDoer to the tls-client implementation
	// (see ResolvePreset, newHTTPDoer). Proxy is a single opt-in upstream
	// (http/https/socks5) passed to the fingerprint client at construction
	// (spec §5); "" = direct connection. Both are resolved once at
	// Coordinator construction and fixed for the scan's lifetime — never
	// re-read on a live mode switch (spec §5: "one browser identity per
	// session").
	Fingerprint string
	Proxy       string

	// Phase 2a target profiling (spec §8).
	RulesetDir   string   // system ruleset dir; "" = embedded defaults only
	UserRulesDir string   // user overlay dir; "" = none
	RulesOff     []string // categories to drop; "" (nil) = profile.DefaultRulesOff
	NmapFile     string   // --nmap: path to an nmap -oX XML file to ingest
	RunNmap      bool     // --run-nmap: opt-in, shells out to nmap
	ActiveProbes bool     // default false: passive-only unless asked
	FaviconProbe bool     // default true: callers must set this explicitly

	// Phase 2b corpus & selection (spec §8). Wordlist != "" bypasses the
	// corpus entirely (spec §0 contract G): -w stays Phase 1 behavior.
	CorpusDB     string  // path to prebuilt DB; "" = embedded minimal corpus
	SecListsPath string  // for `corpus build`
	SourceMap    string  // sourcemap.yaml; "" = embedded default
	CorpusMax    int     // Select LIMIT; 0 = unbounded (medium corpus default)
	TechBoostW   float64 // default 2.0
	CorpusStream bool    // default false (Phase 7 perf)

	// Phase 3 dynamic scoring (spec §7). Weights.WSem/WAssoc/WConv and
	// Epsilon are used exactly as given (0 is a real, meaningful value —
	// "this signal off" / "pure greedy" — not "apply the default"), matching
	// how Rate/Jitter/TimePerBranch already work above; every other field
	// here follows the rest of this Config's "<=0 means apply the default"
	// convention. Weights.WTech is set from TechBoostW for reporting
	// parity (spec §10 handoff) — corpus.Score already applies TechBoostW
	// on its own, so DynamicScorer.Boost never reads WTech itself.
	Weights          ScoreWeights
	MarkovOrder      int           // default 3
	MarkovMinSamples int           // default 8
	LearnMinConf     float64       // default 0.8; gate for feeding the Phase 3 learners (spec §5)
	SubtreeBurst     int           // default 200; consecutive reqs per dir before round-robin (spec §4)
	Epsilon          float64       // default 0.05; ε-greedy explore probability; 0 = pure greedy (spec §4)
	ReprioHits       int           // default 25; reprioritize after this many qualifying hits (spec §6)
	ReprioInterval   time.Duration // default 500ms; or after this much elapsed time, whichever first (spec §6)

	// Phase 4a passive seeding (spec §6). Robots/Sitemap default true
	// (cheap, on-target, high-signal); Wayback defaults false (an
	// off-target network call that pulls thousands of rows) — like
	// FaviconProbe, these are bools so callers must set them explicitly;
	// there's no "<=0 means default" for a bool.
	Robots     bool
	Sitemap    bool
	Wayback    bool
	WaybackMax int    // 0 = seed.WaybackMaxDefault
	SeedAssets bool   // --seed-assets: keep static-asset noise from Wayback seeds
	WaybackURL string // CDX endpoint override; "" = seed.CDXBaseURL (real archive.org). Lets tests point at a stub.

	// Phase 4b crawl + JS harvesting + SPA pivot (spec §7). Crawl/JSHarvest
	// default true (near-free / bounded) and Headless defaults false
	// (opt-in, heavy) — like Robots/Sitemap/Wayback above, these are bools
	// callers must set explicitly; there's no "<=0 means default" for one.
	Crawl      bool
	JSHarvest  bool
	Headless   bool
	CrawlDepth int // 0 = MaxDepth

	// Phase 5a session save/resume (spec §7). SavePath/Autosave are CLI-only
	// (the daemon's own save path is POST .../save's request body, spec
	// §4) — a periodic autosave writes SessionState to SavePath every
	// Autosave interval while the scan runs; 0 disables it.
	SavePath string
	Autosave time.Duration
}

// ScoreWeights are the Phase 3 dynamic-signal weights (spec §7): Boost
// multiplies (1 + Wx*signal) per signal. WTech mirrors the static prior's
// TECH_BOOST_W for reporting; it is not itself read by Boost.
type ScoreWeights struct {
	WTech, WSem, WAssoc, WConv float64
}

const (
	DefaultConcurrency = 20
	DefaultMaxDepth    = 4
	DefaultRequestTO   = 10 * time.Second

	RecurseMinConf  = 0.7 // min confidence to recurse into a directory
	WildcardHitRate = 0.9 // branch hit-rate that flags a wildcard trap

	// Phase 3 defaults (spec §7).
	DefaultWSem   = 1.5
	DefaultWAssoc = 1.0
	DefaultWConv  = 0.8

	DefaultMarkovOrder      = 3
	DefaultMarkovMinSamples = 8
	DefaultLearnMinConf     = 0.8
	DefaultSubtreeBurst     = 200
	DefaultEpsilon          = 0.05
	DefaultReprioHits       = 25
	DefaultReprioInterval   = 500 * time.Millisecond

	GenSiblingBound = 5 // spec §7: bound on generated sibling/sequence candidates per hit

	// SPABruteForceScoreDown is the SPA pivot's deprioritization factor
	// (spec §4: "scale corpus-candidate scores down, don't purge" —
	// reorder-not-exclude). Brute-force candidates stay dispatchable, just
	// far behind harvested ones once root calibrates as an SPA.
	SPABruteForceScoreDown = 0.1

	// Phase 5a telemetry cadence (spec §7).
	StatsInterval    = 400 * time.Millisecond
	SnapshotInterval = 1 * time.Second
	SnapshotTopK     = 25

	// AIMDTickInterval is how often dispatchLoop polls the AIMD recovery
	// controller and time-budget pacing (Phase 6a spec §3, §4) — coarse on
	// purpose, since neither needs per-request precision.
	AIMDTickInterval = 1 * time.Second

	// PinScore is the manual pin override's forced-boost factor (spec §4.1:
	// "force-try + top priority"): scoreCandidate multiplies by this, well
	// above every other scoring tier (corpus priors are ~0-5, nmapSeedScore
	// is 2.0), so a pinned candidate always sorts first.
	PinScore = 1000.0
)

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
}

const (
	DefaultConcurrency = 20
	DefaultMaxDepth    = 4
	DefaultRequestTO   = 10 * time.Second

	RecurseMinConf  = 0.7 // min confidence to recurse into a directory
	WildcardHitRate = 0.9 // branch hit-rate that flags a wildcard trap

	BackoffFactor = 4.0
	BackoffWindow = 30 * time.Second
)

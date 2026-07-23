package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/AlbertoCastagnaro/SmartBuster/internal/audit"
	"github.com/AlbertoCastagnaro/SmartBuster/internal/engine"
)

// openAuditSink mirrors the CLI's own audit-log wiring (cmd/smartbuster/
// main.go's scanOne): when Config.OutDir is set, a daemon-started scan gets
// the same lossless per-request JSONL record the CLI does. Returns nil (no
// error, no sink — engine.NewCoordinator's own noop default applies) when
// OutDir is empty, so a caller that doesn't want a log on disk doesn't get
// one forced on it.
func openAuditSink(cfg engine.Config) (engine.AuditSink, error) {
	if cfg.OutDir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return nil, fmt.Errorf("output dir: %w", err)
	}
	w, err := audit.New(filepath.Join(cfg.OutDir, "audit.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	return w, nil
}

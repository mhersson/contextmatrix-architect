package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/mhersson/contextmatrix-vendorkit/claudecode"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

const claudeCheckTimeout = 10 * time.Second

// checkClaudeCode logs the operator's claude version and returns an error
// only when no runnable binary was found; a version mismatch only warns.
func checkClaudeCode(ctx context.Context, binary string, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, claudeCheckTimeout)
	defer cancel()

	found, tested, err := claudecode.CheckVersion(ctx, binary)
	if err != nil {
		logger.Warn("claude_code sessions are refused", "error", err)

		return err
	}

	logger.Info("claude_code available", "version", found)

	if !tested {
		logger.Warn("claude_code version differs from the tested version", "version", found, "tested", claudecode.Version)
	}

	return nil
}

// warnClaudeEffort warns once when claude_code runs drop the configured
// effort: claude takes only its own levels and runs at its default.
func warnClaudeEffort(effort string, logger *slog.Logger) {
	if effort != "" && claudecode.NormalizeEffort(effort) == "" {
		logger.Warn("claude_code runs drop a reasoning effort claude does not take and use its default", "reasoning_effort", effort)
	}
}

// sweepClaudeHomes removes the run homes a stopped process left in dir. A
// failure only warns: each run removes its own home when it ends.
func sweepClaudeHomes(dir string, logger *slog.Logger) {
	removed, err := stage.SweepClaudeHomes(dir)
	if err != nil {
		logger.Warn("claude home sweep failed", "error", err)
	}

	if removed > 0 {
		logger.Info("claude home sweep removed stale homes", "removed", removed)
	}
}

package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
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

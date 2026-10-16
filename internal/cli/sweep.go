package cli

import (
	"context"
	"log/slog"
	"time"

	"github.com/mhersson/contextmatrix-architect/internal/code"
)

const sweepInterval = time.Hour

// sweepWorkspaces removes trees untouched for maxAge, once at once and then
// every interval, until ctx is cancelled. Mirrors are never removed.
func sweepWorkspaces(ctx context.Context, ws *code.Workspace, maxAge, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		removed, err := ws.Sweep(ctx, maxAge)
		if err != nil && ctx.Err() == nil {
			logger.Warn("workspace sweep failed", "error", err)
		}

		if removed > 0 {
			logger.Info("workspace sweep removed stale trees", "removed", removed)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

package blueprint

import (
	"errors"
	"fmt"
	"time"

	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
)

// claudeStop reports the two Claude Code errors that stop a stage until
// something outside the architect changes: a subscription usage limit and a
// login that failed. ok is false for any other error.
func claudeStop(err error) (reason string, payload map[string]any, ok bool) {
	var limit *claudecode.UsageLimitError

	switch {
	case errors.As(err, &limit):
		payload = map[string]any{"reason": "usage_limit", "window": limit.Window}
		message := fmt.Sprintf("Claude Code stopped on its %s usage limit", limit.Window)

		if !limit.ResetsAt.IsZero() {
			resets := limit.ResetsAt.UTC()
			payload["resets_at"] = resets.Format(time.RFC3339)
			message += ", which resets at " + resets.Format(time.RFC3339)
		}

		payload["message"] = message + ". Reply `continue` after the reset to run the stage again."

		return "usage_limit", payload, true
	case errors.Is(err, claudecode.ErrNotLoggedIn):
		return "not_logged_in", map[string]any{
			"reason":  "not_logged_in",
			"message": "Claude Code did not authenticate with the configured token. Fix the token in ContextMatrix, then reply `continue` to run the stage again.",
		}, true
	}

	return "", nil, false
}

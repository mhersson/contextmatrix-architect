package blueprint

import (
	"errors"
	"time"

	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
)

// usageWindows words the limit windows Claude Code names; any other window
// is left out of the message.
var usageWindows = map[string]string{
	"five_hour":        "5-hour",
	"seven_day":        "weekly",
	"seven_day_opus":   "weekly Opus",
	"seven_day_sonnet": "weekly Sonnet",
}

// claudeStop reports the two Claude Code errors that stop a stage until
// something outside the architect changes: a subscription usage limit and a
// login that failed. ok is false for any other error.
func claudeStop(err error) (reason string, payload map[string]any, ok bool) {
	var limit *claudecode.UsageLimitError

	switch {
	case errors.As(err, &limit):
		payload = map[string]any{"reason": "usage_limit", "window": limit.Window}
		message := "Claude Code stopped on a usage limit"

		if name, known := usageWindows[limit.Window]; known {
			message = "Claude Code stopped on its " + name + " usage limit"
		}

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
			"message": "Claude Code did not authenticate with the configured token. Fix the token in ContextMatrix, end this session and reopen the blueprint (a session keeps the token it started with), then reply `continue` to run the stage again.",
		}, true
	}

	return "", nil, false
}

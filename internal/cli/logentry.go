package cli

import (
	"fmt"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

// logEntryFor renders a controller event as a panel entry. Until CM renders
// typed entries, questions and artifacts are plain text and stage boundaries
// are system lines. Answers are echoed by CM itself and harness events reach
// the hub through the bridge, so both are skipped here.
func logEntryFor(e blueprint.Event) (protocol.LogEntry, bool) {
	entry := protocol.LogEntry{Timestamp: e.Time, SessionID: e.BlueprintID}
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}

	p := e.Payload
	if p == nil {
		p = map[string]any{}
	}

	switch e.Kind {
	case blueprint.EventCreated:
		entry.Type = "system"
		entry.Content = fmt.Sprintf("Blueprint created for %s in %s mode. State the need to begin.",
			strings.Join(stringsOf(p["projects"]), ", "), str(p["mode"]))
	case blueprint.EventReopened:
		entry.Type = "system"
		entry.Content = fmt.Sprintf("Blueprint reopened (state %s).", str(p["state"]))
	case blueprint.EventStageStarted:
		entry.Type = "system"
		entry.Content = fmt.Sprintf("Stage %s started as %s.", str(p["stage"]), strings.ReplaceAll(str(p["persona"]), "_", " "))
	case blueprint.EventQuestion:
		entry.Type = "text"

		var b strings.Builder

		b.WriteString("**Question:** " + str(p["text"]) + "\n")

		if ctxText := str(p["context"]); ctxText != "" {
			b.WriteString("\n" + ctxText + "\n")
		}

		if opts := stringsOf(p["options"]); len(opts) > 0 {
			b.WriteString("\nOptions:\n")

			for i, o := range opts {
				fmt.Fprintf(&b, "%d. %s\n", i+1, o)
			}
		}

		b.WriteString("\nReply with your answer.")
		entry.Content = b.String()
	case blueprint.EventArtifact:
		entry.Type = "text"
		entry.Content = str(p["body"])
	case blueprint.EventStageEnded:
		entry.Type = "system"
		// The harness cannot tell a zero cost from an endpoint that reports
		// none, so the figure is labelled as reported, not measured.
		entry.Content = fmt.Sprintf("Stage %s %s (reported cost %.2f USD).", str(p["stage"]), str(p["status"]), num(p["cost_usd"]))
	case blueprint.EventNote:
		entry.Type = "system"
		entry.Content = "Message recorded in the audit log. It is not delivered to a stage and starts none."
	case blueprint.EventAborted:
		entry.Type = "system"
		entry.Content = "Abort requested. Any running stage is canceled; the blueprint is kept and can be reopened."
	case blueprint.EventError:
		entry.Type = "stderr"
		entry.Content = str(p["message"])
	default:
		return protocol.LogEntry{}, false
	}

	return entry, true
}

func str(v any) string {
	s, _ := v.(string)

	return s
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	default:
		return 0
	}
}

func stringsOf(v any) []string {
	switch s := v.(type) {
	case []string:
		return s
	case []any:
		out := make([]string, 0, len(s))

		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}

		return out
	default:
		return nil
	}
}

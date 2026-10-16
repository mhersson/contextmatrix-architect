package cli

import (
	"fmt"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
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
		// CM already shows the message a human-sourced artifact came from.
		if str(p["source"]) == pack.SourceHuman {
			entry.Type = "system"
			entry.Content = fmt.Sprintf("Your message is stored as the %s (version %.0f).", str(p["stage"]), num(p["version"]))

			break
		}

		entry.Type = "text"
		entry.Content = str(p["body"])
	case blueprint.EventStageEnded:
		entry.Type = "system"

		label := "reported cost"
		if priced, _ := p["priced"].(bool); priced {
			label = "cost"
		}

		entry.Content = fmt.Sprintf("Stage %s %s (%s %.2f USD).", str(p["stage"]), str(p["status"]), label, num(p["cost_usd"]))
	case blueprint.EventNote:
		entry.Type = "system"

		if stage := str(p["retries"]); stage != "" {
			entry.Content = fmt.Sprintf("Message recorded in the audit log. It is not delivered to a stage; stage %s runs again.", stage)

			break
		}

		entry.Content = "Message recorded in the audit log. It is not delivered to a stage and starts none."
	case blueprint.EventAborted:
		entry.Type = "system"
		entry.Content = "Abort requested. Any running stage is canceled; the blueprint is kept and can be reopened."
	case blueprint.EventError:
		entry.Type = "stderr"
		entry.Content = str(p["message"])
	case blueprint.EventAwaitInput:
		entry.Type = "text"
		entry.Content = str(p["prompt"])

		if strings.TrimSpace(entry.Content) == "" {
			entry.Content = fmt.Sprintf("Stage %s takes your next message.", str(p["stage"]))
		}
	case blueprint.EventGrounded:
		entry.Type = "system"

		var b strings.Builder

		b.WriteString("Checked out ")

		for i, src := range anyMaps(p["sources"]) {
			if i > 0 {
				b.WriteString(", ")
			}

			fmt.Fprintf(&b, "%s at %s [%s]", str(src["project"]), shortSHA(str(src["sha"])), str(src["tag"]))
		}

		entry.Content = b.String() + "."
	case blueprint.EventReport:
		// The verdict only: the artifact entry before it showed the body.
		entry.Type = "text"

		if ready, _ := p["ready"].(bool); ready {
			entry.Content = fmt.Sprintf("**Ready** (round %.0f).", num(p["rounds"]))

			break
		}

		var b strings.Builder

		fmt.Fprintf(&b, "**Not ready** after round %.0f.", num(p["rounds"]))

		for _, f := range stringsOf(p["findings"]) {
			b.WriteString("\n- " + f)
		}

		entry.Content = b.String()
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

// anyMaps reads a list of objects, typed or JSON-decoded.
func anyMaps(v any) []map[string]any {
	switch s := v.(type) {
	case []map[string]any:
		return s
	case []any:
		out := make([]map[string]any, 0, len(s))

		for _, item := range s {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}

		return out
	default:
		return nil
	}
}

// shortSHA is a commit's first 12 characters.
func shortSHA(s string) string {
	const n = 12

	if len(s) > n {
		return s[:n]
	}

	return s
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

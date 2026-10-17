package cli

import (
	"fmt"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
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
		if str(p["responder"]) == store.ResponderAuto {
			entry.Type = "system"
			entry.Content = fmt.Sprintf("Stage %s asked: %s Its policy is auto, so it decides this itself; the question stays in the blueprint's event log.", str(p["stage"]), str(p["text"]))

			break
		}

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
		} else {
			entry.Content = "Message recorded in the audit log. It is not delivered to a stage and starts none."
		}

		if hint := str(p["hint"]); hint != "" {
			entry.Content += " " + hint
		}
	case blueprint.EventSignOff:
		entry.Type = "text"
		entry.Content = signoffText(p)
	case blueprint.EventGate:
		entry.Type = "text"
		entry.Content = gateText(p)
	case blueprint.EventDecided:
		entry.Type = "system"
		entry.Content = decidedText(p)
	case blueprint.EventAborted:
		entry.Type = "system"
		entry.Content = "Abort requested. Any running stage is canceled; the blueprint is kept and can be reopened."
	case blueprint.EventExported:
		files, _ := p["files"].(int)
		entry.Type = "system"
		entry.Content = fmt.Sprintf("Export written on the server: %s (%d files).", str(p["path"]), files)
	case blueprint.EventError:
		entry.Type = "stderr"
		entry.Content = str(p["message"])
	case blueprint.EventAwaitInput:
		entry.Type = "text"
		entry.Content = str(p["prompt"])

		if strings.TrimSpace(entry.Content) == "" {
			entry.Content = fmt.Sprintf("Stage %s takes your next message.", str(p["stage"]))
		}
	case blueprint.EventSetupGaps:
		entry.Type = "text"
		entry.Content = setupGapsText(p)
	case blueprint.EventPolicy:
		entry.Type = "system"
		entry.Content = policyText(p)
	case blueprint.EventGatePlan:
		entry.Type = "text"
		entry.Content = gatePlanText(p)
	case blueprint.EventGrounded:
		entry.Type = "system"

		var b strings.Builder

		b.WriteString("Checked out ")

		for i, src := range anyMaps(p["sources"]) {
			if i > 0 {
				b.WriteString(", ")
			}

			if str(src["sha"]) == "" {
				fmt.Fprintf(&b, "%s, an empty repository [%s]", str(src["project"]), str(src["tag"]))

				continue
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
	case blueprint.EventCardCreated:
		entry.Type = "system"

		var b strings.Builder

		fmt.Fprintf(&b, "Created %s (%s) in %s: %s.", str(p["card"]), str(p["draft"]), str(p["project"]), str(p["title"]))

		for _, w := range stringsOf(p["warnings"]) {
			b.WriteString("\nContextMatrix warned: " + w)
		}

		entry.Content = b.String()
	case blueprint.EventPlaybookCreated:
		entry.Type = "system"
		entry.Content = fmt.Sprintf("Created playbook %s with %.0f entries.", str(p["playbook"]), num(p["entries"]))
	case blueprint.EventRunStarted:
		entry.Type = "system"

		// A run ContextMatrix already had was recorded, not started.
		if existing, _ := p["existing"].(bool); existing {
			if str(p["kind"]) == "card" {
				entry.Content = fmt.Sprintf("%s is already running in ContextMatrix; the architect recorded it.", str(p["target"]))
			} else {
				entry.Content = fmt.Sprintf("Playbook %s already runs in ContextMatrix; the architect recorded it.", str(p["target"]))
			}

			break
		}

		if str(p["kind"]) == "card" {
			entry.Content = fmt.Sprintf("Started %s in ContextMatrix; it opens one pull request to the default branch.", str(p["target"]))

			break
		}

		entry.Content = fmt.Sprintf("Started playbook %s in ContextMatrix. Each card's pull request merges into the playbook branch once CI passes.", str(p["target"]))
	case blueprint.EventWatch:
		entry.Type = "system"
		entry.Content = str(p["line"])
	case blueprint.EventWatchEnded:
		entry.Type = "text"

		var b strings.Builder

		if pb := str(p["playbook"]); pb != "" {
			fmt.Fprintf(&b, "**Done.** Playbook %s completed.", pb)

			if repos := anyMaps(p["repos"]); len(repos) > 0 {
				b.WriteString("\n\nOpen the final pull request for each repository:")

				for _, r := range repos {
					fmt.Fprintf(&b, "\n- %s: %s", str(r["project"]), str(r["compare_url"]))
				}
			}
		} else {
			b.WriteString("**Done.**")
		}

		cards := anyMaps(p["cards"])
		if len(cards) > 0 {
			b.WriteString("\n\nCards:")
		}

		for _, card := range cards {
			fmt.Fprintf(&b, "\n- %s is %s", str(card["card"]), str(card["state"]))

			if pr := str(card["pr"]); pr != "" {
				b.WriteString(": " + pr)
			}
		}

		if manual := stringsOf(p["manual"]); len(manual) > 0 {
			b.WriteString("\n\nChecks a human runs now:")

			for _, m := range manual {
				b.WriteString("\n- " + m)
			}
		}

		if cost, ok := p["cost_usd"]; ok {
			if priced, _ := p["priced"].(bool); priced {
				fmt.Fprintf(&b, "\n\nThe architect's runs cost %.2f USD.", num(cost))
			} else {
				fmt.Fprintf(&b, "\n\nThe architect's runs cost %.2f USD, as the endpoint reported it.", num(cost))
			}
		}

		b.WriteString("\n\nReply export for the bundle, or end the session.")

		entry.Content = b.String()
	case blueprint.EventEmitted:
		entry.Type = "text"

		var ids []string
		for _, card := range anyMaps(p["cards"]) {
			ids = append(ids, str(card["card"]))
		}

		var b strings.Builder

		fmt.Fprintf(&b, "**Emitted** %d card(s) into ContextMatrix: %s.", len(ids), strings.Join(ids, ", "))

		if pb := str(p["playbook"]); pb != "" {
			fmt.Fprintf(&b, " Playbook: %s.", pb)
		}

		if manual := stringsOf(p["manual"]); len(manual) > 0 {
			b.WriteString("\n\nChecks a human runs once the card is merged:")

			for _, m := range manual {
				b.WriteString("\n- " + m)
			}
		}

		if steps := stringsOf(p["steps"]); len(steps) > 0 {
			b.WriteString("\n\nManual steps for you before the cards run:")

			for _, s := range steps {
				b.WriteString("\n- " + s)
			}
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

// signoffText renders the sign-off: the entries in playbook order, the
// playbook, the preflight result and how to decide.
func signoffText(p map[string]any) string {
	var b strings.Builder

	if auto, _ := p["auto_stopped"].(bool); auto {
		b.WriteString("**Sign-off needed.** This mode emits on its own, but preflight found something for a human.\n\n")
	} else {
		b.WriteString("**Sign-off.** Readiness passed the cards.\n\n")
	}

	cards := map[string]map[string]any{}
	for _, card := range anyMaps(p["cards"]) {
		cards[str(card["id"])] = card
	}

	manual := map[string]string{}
	for _, m := range anyMaps(p["manual"]) {
		manual[str(m["id"])] = str(m["text"])
	}

	b.WriteString("In playbook order:\n")

	for i, id := range stringsOf(p["order"]) {
		if card, ok := cards[id]; ok {
			fmt.Fprintf(&b, "%d. %s %s: %s (%s, %s", i+1, id, str(card["project"]), str(card["title"]), str(card["type"]), str(card["priority"]))

			if deps := stringsOf(card["depends_on"]); len(deps) > 0 {
				b.WriteString("; depends on " + strings.Join(deps, ", "))
			}

			if writes := str(card["writes"]); writes != "" {
				b.WriteString("; also writes " + writes)
			}

			b.WriteString(")\n")

			continue
		}

		fmt.Fprintf(&b, "%d. %s manual: %s\n", i+1, id, manual[id])
	}

	if title := str(p["title"]); title != "" && len(cards) > 1 {
		fmt.Fprintf(&b, "\nPlaybook: %q.\n", title)
	} else {
		b.WriteString("\nOne card, so no playbook.\n")
	}

	if a := str(p["assumptions"]); a != "" {
		b.WriteString("\nAssumptions:\n" + a + "\n")
	} else {
		b.WriteString("\nAssumptions: none recorded.\n")
	}

	blocking, warnings, steps, pfErr := stringsOf(p["blocking"]), stringsOf(p["warnings"]), stringsOf(p["steps"]), str(p["preflight_error"])

	if len(blocking) > 0 {
		b.WriteString("\nBlocking (fix these in ContextMatrix first):\n")

		for _, s := range blocking {
			b.WriteString("- " + s + "\n")
		}
	}

	if len(warnings) > 0 {
		b.WriteString("\nWarnings:\n")

		for _, s := range warnings {
			b.WriteString("- " + s + "\n")
		}
	}

	if len(steps) > 0 {
		b.WriteString("\nManual steps for you before the cards run (they do not hold up emission):\n")

		for _, s := range steps {
			b.WriteString("- " + s + "\n")
		}
	}

	if pfErr != "" {
		b.WriteString("\nPreflight could not run: " + pfErr + "\n")
	}

	if len(blocking) == 0 && len(warnings) == 0 && len(steps) == 0 && pfErr == "" {
		b.WriteString("\nPreflight found nothing for a human.\n")
	}

	b.WriteString("\nReply `approve` to emit the cards")

	if len(warnings) > 0 {
		b.WriteString(" (this also overrides the warnings)")
	}

	b.WriteString(", or `reject <feedback>` to re-run decomposition with your feedback.")

	return b.String()
}

// policyText renders a gate reply: the change it made and what it leaves
// alone, or why it changed nothing.
func policyText(p map[string]any) string {
	if refused := str(p["refused"]); refused != "" {
		return "Nothing changed. " + refused + " Reply `gate <stage> human` or `gate <stage> auto`."
	}

	text := fmt.Sprintf("Stage %s is now %s for this blueprint, from its next question or gate.", str(p["stage"]), str(p["policy"]))

	if str(p["open"]) == "question" {
		text += " Its open question stays with whoever it was put to."
	}

	if str(p["open"]) == "gate" {
		text += " Its open gate stays as it is; decide it with approve or reject."
	}

	return text
}

// gatePlanText renders the gate plan: who answers each stage's questions
// and decides its gate, and the replies that change it.
func gatePlanText(p map[string]any) string {
	var b strings.Builder

	b.WriteString("**Gate plan.** Who answers each stage's questions and decides its gate:")

	for _, s := range anyMaps(p["stages"]) {
		fmt.Fprintf(&b, "\n- %s: %s", str(s["stage"]), str(s["policy"]))

		if gate, _ := s["gate"].(bool); gate {
			b.WriteString(", with a gate")
		}
	}

	switch str(p["signoff"]) {
	case "auto":
		b.WriteString("\n- sign-off: auto when preflight finds nothing for a human")
	case "human":
		b.WriteString("\n- sign-off: human")
	}

	b.WriteString("\n\nReply `gate <stage> human` or `gate <stage> auto` to change a stage for this blueprint; it applies from the stage's next question or gate. The sign-off follows the mode.")

	return b.String()
}

// gateText renders a stage's gate: which artifact waits and the replies
// that decide it.
func gateText(p map[string]any) string {
	var replies []string

	for _, a := range anyMaps(p["actions"]) {
		reply := "`" + str(a["reply"])
		if feedback, _ := a["feedback"].(bool); feedback {
			reply += " <feedback>"
		}

		replies = append(replies, reply+"`")
	}

	return fmt.Sprintf("**Gate: %s, version %.0f.** The artifact above waits for your decision. Reply %s.",
		str(p["stage"]), num(p["version"]), strings.Join(replies, " or "))
}

// decidedText renders a decision at a stage's gate or at the sign-off; a
// decision without a stage is the sign-off's.
func decidedText(p map[string]any) string {
	reject, auto := str(p["decision"]) == "reject", str(p["responder"]) == "auto"

	if stage := str(p["stage"]); stage != "" && stage != "signoff" {
		switch {
		case reject:
			return fmt.Sprintf("Rejected %s; it runs again with your feedback.", stage)
		case auto:
			return fmt.Sprintf("%s passed its gate under the auto policy.", stage)
		default:
			return fmt.Sprintf("Approved %s.", stage)
		}
	}

	switch {
	case reject:
		return "Rejected. Decomposition runs again with your feedback."
	case auto:
		return "Preflight found nothing that needs a human first, so the mode approved the cards. Emission starts."
	default:
		return "Approved. Emission starts."
	}
}

// setupGapsText renders what grounding found the target projects lack for
// an agent run, and what happens to each gap.
func setupGapsText(p map[string]any) string {
	var b strings.Builder

	b.WriteString("**Setup.** Grounding found what the target projects lack for an agent run. None of it blocks; the cards plan it as work:")

	for _, g := range anyMaps(p["projects"]) {
		name := str(g["project"])

		if verify, _ := g["has_verify"].(bool); !verify {
			fmt.Fprintf(&b, "\n- %s declares no verify command in ContextMatrix. Decomposition proposes one, and the manual steps shown before the cards run include setting it; setting it now works too.", name)
		}

		if file, _ := g["has_instruction_file"].(bool); !file {
			fmt.Fprintf(&b, "\n- %s has no AGENTS.md or CLAUDE.md at the root. Its first card also writes AGENTS.md.", name)
		}

		if empty, _ := g["empty"].(bool); empty {
			fmt.Fprintf(&b, "\n- %s is an empty repository. Its default branch needs an initial commit before the cards run, because the agent needs a base branch; the manual steps shown before the cards run include it.", name)
		}
	}

	return b.String()
}

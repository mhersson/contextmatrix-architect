package cli

import (
	"fmt"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// logEntryFor renders a controller event as a panel entry. A question or gate
// a human answers carries a prompt, whose actions the panel shows as buttons
// and whose replies come back as ordinary messages; the markdown stays the
// readable fallback. A human's answer is echoed by CM itself and harness
// events reach the hub through the bridge, so both are skipped here; the
// responder's answer is shown.
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
		entry.Content = fmt.Sprintf("%s started as %s.", runLabel(p), strings.ReplaceAll(str(p["persona"]), "_", " "))
	case blueprint.EventQuestion:
		if str(p["responder"]) == store.ResponderAuto {
			entry.Type = "system"
			entry.Content = fmt.Sprintf("Stage %s asked Q%.0f: %s Its policy is auto.", str(p["stage"]), num(p["number"]), str(p["text"]))

			break
		}

		entry.Type = "text"

		var b strings.Builder

		heading := "**Question:**"
		if n := num(p["number"]); n > 0 {
			heading = fmt.Sprintf("**Question Q%.0f:**", n)
		}

		b.WriteString(heading + " " + str(p["text"]) + "\n")

		if ctxText := str(p["context"]); ctxText != "" {
			b.WriteString("\n" + ctxText + "\n")
		}

		if opts := stringsOf(p["options"]); len(opts) > 0 {
			b.WriteString("\nOptions:\n")

			for i, o := range opts {
				if i+1 == int(num(p["recommended"])) {
					o += " (recommended)"
				}

				fmt.Fprintf(&b, "%d. %s\n", i+1, o)
			}
		}

		b.WriteString("\nReply with your answer.")
		entry.Content = b.String()
		entry.Prompt = questionPrompt(p)
	case blueprint.EventArtifact:
		entry.Type = "text"
		entry.Content = str(p["body"])
	case blueprint.EventStageEnded:
		entry.Type = "system"

		label := "reported cost"
		if priced, _ := p["priced"].(bool); priced {
			label = "cost"
		}

		entry.Content = fmt.Sprintf("%s %s (%s %.2f USD).", runLabel(p), str(p["status"]), label, num(p["cost_usd"]))
	case blueprint.EventNote:
		entry.Type = "system"

		duringRun, _ := p["during_run"].(bool)
		ended, _ := p["ended"].(bool)
		critique, _ := p["critique"].(bool)

		switch stage := str(p["retries"]); {
		case stage != "" && critique:
			entry.Content = fmt.Sprintf("Noted. The review of %s, or its next builder round, runs again, and it and every run after it get the note.", stage)
		case stage != "":
			entry.Content = fmt.Sprintf("Noted. Stage %s runs again, and it and every stage after it get the note.", stage)
		case ended:
			entry.Content = "Noted. The pipeline has ended, so no stage starts to get it."
		case duringRun:
			entry.Content = "Noted. Every stage that starts after the running one gets it; the running stage does not."
		default:
			entry.Content = "Noted. Every stage that starts from now on gets it."
		}

		if hint := str(p["hint"]); hint != "" {
			entry.Content += " " + hint
		}
	case blueprint.EventSignOff:
		entry.Type = "text"
		entry.Content = signoffText(p)
		entry.Prompt = actionsPrompt(fmt.Sprintf("gate:signoff:v%.0f", num(p["version"])), p["actions"])
	case blueprint.EventGate:
		entry.Type = "text"
		entry.Content = gateText(p)
		entry.Prompt = actionsPrompt(fmt.Sprintf("gate:%s:v%.0f", str(p["stage"]), num(p["version"])), p["actions"])
	case blueprint.EventReview:
		entry.Type = "system"
		entry.Content = reviewText(p)
	case blueprint.EventReshow:
		// The open question again, rendered as it was first shown.
		inner, _ := p["payload"].(map[string]any)

		return logEntryFor(blueprint.Event{Kind: str(p["kind"]), BlueprintID: e.BlueprintID, Time: e.Time, Payload: inner})
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

		// A prompt rides a text frame, so a stop that offers approve is one.
		if prompt := resumePrompt(p); prompt != nil {
			entry.Type = "text"
			entry.Prompt = prompt
		}
	case blueprint.EventAwaitInput:
		entry.Type = "text"
		entry.Content = str(p["prompt"])

		if strings.TrimSpace(entry.Content) == "" {
			entry.Content = fmt.Sprintf("Stage %s takes your next message.", str(p["stage"]))
		}

		entry.Prompt = resumePrompt(p)
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
	case blueprint.EventAnswer:
		// CM shows a human's answer as the message it was.
		if str(p["responder"]) != store.ResponderAuto {
			return protocol.LogEntry{}, false
		}

		grounded := "unverified, an assumption for the sign-off"
		if g, _ := p["grounded"].(bool); g {
			grounded = "grounded"
		}

		entry.Type = "text"
		entry.Content = fmt.Sprintf("**Q%.0f, answered by the responder** (%s): %s\n\n%s", num(p["number"]), grounded, str(p["text"]), str(p["reasoning"]))
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

	switch list := anyMaps(p["assumptions"]); {
	case len(list) > 0:
		b.WriteString("\nAssumptions, unverified first:\n")

		for _, a := range list {
			status := str(a["status"])
			if evidence := str(a["evidence"]); evidence != "" {
				status += ": " + evidence
			}

			fmt.Fprintf(&b, "- %s (%s) %s\n", str(a["id"]), status, str(a["text"]))
		}
	case str(p["assumptions"]) != "":
		// Intake's section text: a sign-off stored before assumptions were
		// data, or one whose intake predates them.
		b.WriteString("\nAssumptions:\n" + str(p["assumptions"]) + "\n")
	default:
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

// gateText renders a stage's gate: the artifact above waits, with the
// reviewer's score, the artifacts folded into it shown below, the problems
// its checks accepted them with, and the replies that decide it.
func gateText(p map[string]any) string {
	var replies []string

	for _, a := range anyMaps(p["actions"]) {
		reply := "`" + str(a["reply"])
		if feedback, _ := a["feedback"].(bool); feedback {
			reply += " <feedback>"
		}

		replies = append(replies, reply+"`")
	}

	stage, version := str(p["stage"]), num(p["version"])
	folded := anyMaps(p["folded"])

	text := fmt.Sprintf("**Gate: %s, version %.0f.** The artifact above waits for your decision.", stage, version)

	if len(folded) > 0 {
		names := make([]string, 0, len(folded))
		for _, f := range folded {
			names = append(names, fmt.Sprintf("%s version %.0f", str(f["stage"]), num(f["version"])))
		}

		text += fmt.Sprintf(" It also judges %s, whose gate folded into this one, shown below.", strings.Join(names, " and "))
	}

	// A blank line ends a list or an artifact, so the next line is not read
	// as part of it.
	sep := " "

	// The findings are in the review entry posted just above the gate, so the
	// gate refers to them rather than listing them a second time.
	if v, ok := p["review"].(map[string]any); ok {
		if passed, _ := v["passed"].(bool); passed {
			text += fmt.Sprintf(" The reviewer scored it %.0f of 5 (passes at %.0f)", num(v["score"]), num(v["threshold"]))
		} else {
			text += fmt.Sprintf(" The reviewer did not pass it after %.0f round(s): %.0f of 5 (passes at %.0f)", num(v["round"]), num(v["score"]), num(v["threshold"]))
		}

		if len(stringsOf(v["findings"])) > 0 {
			text += "; its findings are in the review above"
		}

		text += "."
	}

	if list := problemsList(p["problems"]); list != "" {
		lead := " "
		if sep != " " {
			lead = sep
		}

		text += lead + list
		sep = "\n\n"
	}

	if len(folded) > 0 {
		for _, f := range folded {
			text += fmt.Sprintf("\n\n**%s, version %.0f**\n\n%s", str(f["stage"]), num(f["version"]), str(f["body"]))

			if list := problemsList(f["problems"]); list != "" {
				text += "\n\n" + list
			}
		}

		sep = "\n\n"
	}

	return text + sep + fmt.Sprintf("Reply %s.", strings.Join(replies, " or "))
}

// problemsList renders the problems an artifact's checks accepted it with;
// "" when there are none.
func problemsList(v any) string {
	problems := stringsOf(v)
	if len(problems) == 0 {
		return ""
	}

	text := "Its checks still found these problems when they accepted its third failing submit:"

	for _, p := range problems {
		text += "\n- " + p
	}

	return text
}

// decidedText renders a decision at a stage's gate, a fold into a later
// gate, or a decision at the sign-off; a decision without a stage is the
// sign-off's.
func decidedText(p map[string]any) string {
	reject, auto := str(p["decision"]) == "reject", str(p["responder"]) == "auto"

	if stage := str(p["stage"]); stage != "" && stage != "signoff" {
		switch {
		case str(p["decision"]) == "fold":
			return fmt.Sprintf("%s folds into %s's gate, which judges both.", stage, str(p["feedback"]))
		case str(p["decision"]) == "superseded":
			return fmt.Sprintf("%s runs again after %s.", stage, str(p["feedback"]))
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

// runLabel names a run in the panel: a stage, a stage's review, or the
// responder answering for a stage.
func runLabel(p map[string]any) string {
	switch str(p["kind"]) {
	case store.RunKindReviewer:
		return "Review of " + str(p["stage"])
	case store.RunKindResponder:
		return "The responder for " + str(p["stage"])
	default:
		return "Stage " + str(p["stage"])
	}
}

// reviewText renders a reviewer's verdict.
func reviewText(p map[string]any) string {
	text := fmt.Sprintf("The reviewer scored %s version %.0f %.0f of 5 (passes at %.0f), round %.0f.",
		str(p["stage"]), num(p["version"]), num(p["score"]), num(p["threshold"]), num(p["round"]))

	for _, f := range stringsOf(p["findings"]) {
		text += "\n- " + f
	}

	return text
}

// labelMax caps the option text of a button's label in runes; the reply keeps
// the full text and " (recommended)" follows the cap.
const labelMax = 60

// questionPrompt makes a human's question answerable by a click: one action
// per option, its reply the full option, which reaches the stage verbatim.
// Any typed reply answers it too.
func questionPrompt(p map[string]any) *protocol.LogPrompt {
	recommended := int(num(p["recommended"]))

	var actions []protocol.LogAction

	for i, o := range stringsOf(p["options"]) {
		label := capLabel(o)
		if i+1 == recommended {
			label += " (recommended)"
		}

		actions = append(actions, protocol.LogAction{Label: label, Reply: o})
	}

	return &protocol.LogPrompt{ID: fmt.Sprintf("Q%.0f", num(p["number"])), FreeText: true, Actions: actions}
}

// actionsPrompt makes a gate decidable by a click from the replies its
// event carries; nil when it carries none, as an event stored before the
// replies travelled with it.
func actionsPrompt(id string, v any) *protocol.LogPrompt {
	var actions []protocol.LogAction

	for _, a := range anyMaps(v) {
		feedback, _ := a["feedback"].(bool)
		actions = append(actions, protocol.LogAction{Label: str(a["label"]), Reply: str(a["reply"]), Feedback: feedback})
	}

	if len(actions) == 0 {
		return nil
	}

	return &protocol.LogPrompt{ID: id, Actions: actions}
}

// resumePrompt offers approve on a stopped emission or run start; nil on
// any other error or wait.
func resumePrompt(p map[string]any) *protocol.LogPrompt {
	var label string

	switch str(p["resume"]) {
	case "emission":
		label = "Resume emission"
	case "run":
		label = "Retry the run start"
	case "held":
		label = "Start the run"
	default:
		return nil
	}

	return &protocol.LogPrompt{
		ID:      fmt.Sprintf("emission:v%.0f", num(p["version"])),
		Actions: []protocol.LogAction{{Label: label, Reply: "approve"}},
	}
}

// capLabel cuts a label to labelMax runes, ending in an ellipsis when cut.
func capLabel(s string) string {
	r := []rune(s)
	if len(r) <= labelMax {
		return s
	}

	return string(r[:labelMax-1]) + "…"
}

// setupGapsText renders what the checkout found the target projects lack for
// an agent run, and what happens to each gap.
func setupGapsText(p map[string]any) string {
	var b strings.Builder

	b.WriteString("**Setup.** The code checkout found what the target projects lack for an agent run. None of it blocks; the cards plan it as work:")

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

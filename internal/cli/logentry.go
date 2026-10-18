package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// logEntryFor renders a controller event as a panel entry. A gate a human
// decides carries a prompt, whose actions the panel shows as buttons and
// whose replies come back as ordinary messages; a round's prompt carries the
// round, whose answers come back structured. The markdown stays the readable
// fallback. A gate's prompt also carries a kind and its own text, the entry
// without its reply instruction. A human's answer is echoed
// by CM itself and harness events reach the hub through the bridge, so both
// are skipped here; the responder's answer is shown.
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

		// A run that goes on from a round is no new start, so no divider.
		switch str(p["segment"]) {
		case blueprint.SegmentAnswers:
			entry.Content = fmt.Sprintf("%s resumes with the answers to round %.0f.", runLabel(p), num(p["question_round"]))
		case blueprint.SegmentChat:
			entry.Content = fmt.Sprintf("%s answers the chat about round %.0f.", runLabel(p), num(p["question_round"]))
		default:
			entry.Content = fmt.Sprintf("%s started as %s.", runLabel(p), strings.ReplaceAll(str(p["persona"]), "_", " "))

			// A stage run's first round is a divider; its builder rounds, its
			// reviews and its responders are not.
			if kind := str(p["kind"]); (kind == "" || kind == store.RunKindStage) && num(p["round"]) <= 1 {
				entry.Marker = &protocol.LogMarker{Kind: "stage", Stage: str(p["stage"]), Persona: str(p["persona"])}
			}
		}
	case blueprint.EventRound:
		n, frame, ok := roundFrame(p)
		if !ok {
			return protocol.LogEntry{}, false
		}

		left := str(p["change"]) == blueprint.ChangeLeft
		entry.Type = "text"
		entry.Content = roundText(n, frame, left)
		entry.Prompt = &protocol.LogPrompt{ID: fmt.Sprintf("round:%d", n), Kind: "round", Text: roundHeading(n, frame, left), Round: &frame}
	case blueprint.EventArtifact:
		// A sheet reference: the body is on the sheet, never transcript text.
		note := artifactNote(p)
		entry.Type = "text"
		entry.Content = fmt.Sprintf("%s rev %.0f: %s.", pack.StageName(str(p["stage"])), num(p["version"]), note)
		entry.Marker = &protocol.LogMarker{
			Kind: "artifact", Stage: str(p["stage"]), Version: int(num(p["version"])), Persona: str(p["persona"]), Note: note,
		}
	case blueprint.EventStageEnded:
		entry.Type = "system"

		label := "reported cost"
		if priced, _ := p["priced"].(bool); priced {
			label = "cost"
		}

		if str(p["status"]) == store.RunAsked {
			what := fmt.Sprintf("%s waits for the answers to round %.0f", runLabel(p), num(p["question_round"]))

			switch chat, _ := p["chat"].(bool); {
			case chat:
				what = fmt.Sprintf("%s replied in round %.0f's chat", runLabel(p), num(p["question_round"]))
			case p["responder"] == true:
				what = fmt.Sprintf("%s had the responder answer round %.0f", runLabel(p), num(p["question_round"]))
			}

			entry.Content = fmt.Sprintf("%s (%s %.2f USD).", what, label, num(p["cost_usd"]))

			break
		}

		entry.Content = fmt.Sprintf("%s %s (%s %.2f USD).", runLabel(p), str(p["status"]), label, num(p["cost_usd"]))
	case blueprint.EventNote:
		entry.Type = "system"

		duringRun, _ := p["during_run"].(bool)
		ended, _ := p["ended"].(bool)
		critique, _ := p["critique"].(bool)

		switch stage := str(p["retries"]); {
		case num(p["round"]) > 0 && stage == "":
			entry.Content = fmt.Sprintf("Noted. Round %.0f still waits for its answers in the question block; the stage's next run gets the note.", num(p["round"]))
		case stage != "" && critique:
			entry.Content = fmt.Sprintf("Noted. The review of %s, or its next builder round, runs again, and it and every run after it get the note.", pack.StageName(stage))
		case stage != "":
			entry.Content = fmt.Sprintf("Noted. Stage %s runs again, and it and every stage after it get the note.", pack.StageName(stage))
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
		body, reply := signoffText(p)
		entry.Content = body + reply
		entry.Prompt = actionsPrompt(fmt.Sprintf("gate:signoff:v%.0f", num(p["version"])), body, p["actions"])
	case blueprint.EventGate:
		entry.Type = "text"
		body, reply := gateText(p)
		entry.Content = body + reply
		entry.Prompt = actionsPrompt(fmt.Sprintf("gate:%s:v%.0f", str(p["stage"]), num(p["version"])), body, p["actions"])
	case blueprint.EventReview:
		entry.Type = "system"
		entry.Content = reviewText(p)
	case blueprint.EventDecided:
		entry.Type = "system"
		entry.Content = decidedText(p)
	case blueprint.EventAborted:
		entry.Type = "system"
		entry.Content = abortedText(p)
	case blueprint.EventExported:
		files, _ := p["files"].(int)
		entry.Type = "system"
		entry.Content = fmt.Sprintf("Export written on the server: %s (%d files).", str(p["path"]), files)
	case blueprint.EventError:
		entry.Type = "stderr"
		entry.Content = str(p["message"])

		if stopsStage(p) {
			entry.Content = strings.TrimSuffix(entry.Content, ".") + ". Reply `continue` to run it again."
		}

		// A prompt rides a text frame, so a stop that offers approve is one.
		if prompt := resumePrompt(p); prompt != nil {
			entry.Type = "text"
			entry.Prompt = prompt
		}
	case blueprint.EventAwaitInput:
		entry.Type = "text"
		entry.Content = str(p["prompt"])

		if strings.TrimSpace(entry.Content) == "" {
			entry.Content = fmt.Sprintf("Stage %s takes your next message.", pack.StageName(str(p["stage"])))
		}

		entry.Prompt = resumePrompt(p)
	case blueprint.EventSetupGaps:
		entry.Type = "text"
		entry.Content = setupGapsText(p)
	case blueprint.EventPolicy:
		entry.Type = "system"
		entry.Content = policyText(p)
	case blueprint.EventCap:
		entry.Type = "system"
		entry.Content = capText(p)
	case blueprint.EventResume:
		entry.Type = "system"
		entry.Content = resumeText(p)
	case blueprint.EventGatePlan:
		entry.Type = "text"
		entry.Content = gatePlanText(p)
	case blueprint.EventResearchOff:
		entry.Type = "system"
		entry.Content = fmt.Sprintf("Research is off for this session (%s): stages work from the model's knowledge and the code, and question options cite no web sources.", str(p["reason"]))
	case blueprint.EventResearch:
		// A page read is a source, shown as a checkout is; a search or a
		// refusal is already the tool call's own entry.
		tag := str(p["tag"])
		if tag == "" {
			return protocol.LogEntry{}, false
		}

		name := str(p["final_url"])
		if title := str(p["title"]); title != "" {
			name = fmt.Sprintf("%q (%s)", title, name)
		}

		entry.Type = "system"
		entry.Content = fmt.Sprintf("Read %s as [%s].", name, tag)
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
		// The verdict only: the artifact is on the sheet.
		entry.Type = "text"

		// No round is named when none applies to the end.
		rounds := num(p["rounds"])

		if ready, _ := p["ready"].(bool); ready {
			entry.Content = "**Ready**."
			if rounds > 0 {
				entry.Content = fmt.Sprintf("**Ready** (round %.0f).", rounds)
			}

			break
		}

		var b strings.Builder

		b.WriteString("**Not ready**")

		if rounds > 0 {
			fmt.Fprintf(&b, " after round %.0f", rounds)
		}

		b.WriteString(".")

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

		grounded := "assumed by the responder, for the sign-off"
		if g, _ := p["grounded"].(bool); g {
			grounded = "verified"
			if evidence := str(p["evidence"]); evidence != "" {
				grounded += ": " + evidence
			}
		}

		entry.Type = "text"
		entry.Content = fmt.Sprintf("**Q%.0f, answered by the responder** (%s): %s\n\n%s", num(p["number"]), grounded, str(p["text"]), str(p["reasoning"]))
	default:
		return protocol.LogEntry{}, false
	}

	return entry, true
}

// roundFrame reads a round snapshot's number and frame; ok is false for a
// payload that does not decode, and for a close in progress, which only
// reserves numbers: the closed snapshot follows it.
func roundFrame(p map[string]any) (int, protocol.LogRound, bool) {
	var f protocol.LogRound

	if str(p["change"]) == blueprint.ChangeClosing {
		return 0, f, false
	}

	raw, err := json.Marshal(p["frame"])
	if err != nil || json.Unmarshal(raw, &f) != nil {
		return 0, f, false
	}

	n := int(num(p["round"]))

	return n, f, n > 0
}

// roundHeading names round n; left marks a round the pipeline went past
// before it was answered, which is closed but holds no full answer.
func roundHeading(n int, f protocol.LogRound, left bool) string {
	switch {
	case left:
		return fmt.Sprintf("Round %d, left unanswered", n)
	case f.Closed:
		return fmt.Sprintf("Round %d, answered", n)
	case f.Responder:
		return fmt.Sprintf("Round %d: %s from the %s, answered by the responder", n, roundCount(f), f.Persona)
	default:
		return fmt.Sprintf("Round %d: %s from the %s", n, roundCount(f), f.Persona)
	}
}

// roundCount is what round f puts to the user: its questions, the
// assumptions on its tab, or both.
func roundCount(f protocol.LogRound) string {
	questions := fmt.Sprintf("%d question(s)", len(f.Questions))
	assumptions := fmt.Sprintf("%d assumption(s)", len(f.Assumptions))

	switch {
	case len(f.Assumptions) == 0:
		return questions
	case len(f.Questions) == 0:
		return assumptions
	default:
		return questions + " and " + assumptions
	}
}

// roundText is the round as plain markdown, for a panel without the block:
// each question with its answer, or its options while it has none, then the
// assumptions tab with each decision made.
func roundText(n int, f protocol.LogRound, left bool) string {
	var b strings.Builder

	b.WriteString("**" + roundHeading(n, f, left) + ".**")

	answers := map[int]protocol.RoundAnswer{}
	for _, a := range f.Answers {
		answers[a.Number] = a
	}

	for _, q := range f.Questions {
		fmt.Fprintf(&b, "\n\n**Q%d. %s**", q.Number, q.Text)

		if a, ok := answers[q.Number]; ok {
			b.WriteString("\nAnswer: " + blueprint.AnswerText(q, a))

			continue
		}

		if q.Why != "" {
			b.WriteString("\n" + q.Why)
		}

		for i, o := range q.Options {
			line := fmt.Sprintf("\n%d. %s", i+1, o.Title)
			if i+1 == q.Recommended {
				line += " (recommended)"
			}

			if o.Summary != "" {
				line += ": " + o.Summary
			}

			b.WriteString(line)
		}
	}

	decisions := map[string]bool{}
	for _, d := range f.Decisions {
		decisions[d.ID] = d.Accept
	}

	if len(f.Assumptions) > 0 {
		b.WriteString("\n\n**Assumptions**")
	}

	for _, a := range f.Assumptions {
		line := fmt.Sprintf("\n%s. %s", a.ID, a.Text)

		switch accept, ok := decisions[a.ID]; {
		case ok && accept:
			line += " (accepted)"
		case ok:
			line += " (to be asked)"
		}

		b.WriteString(line)
	}

	if !f.Closed && !f.Responder {
		b.WriteString("\n\nAnswer in the question block.")
	}

	return b.String()
}

// artifactNote is a stored artifact's sheet reference note, finished prose
// of what is known at store time: cards accepted with contract findings, a
// loop stage's round, a revision for its reviewer's findings, or the
// persona that drew it.
func artifactNote(p map[string]any) string {
	revisedFor := int(num(p["revised_for"]))

	switch {
	case num(p["accepted_round"]) > 0:
		return fmt.Sprintf("Refused by checks, accepted on round %.0f", num(p["accepted_round"]))
	case num(p["max_rounds"]) > 0:
		return fmt.Sprintf("Round %.0f of %.0f", num(p["loop_round"]), num(p["max_rounds"]))
	case revisedFor == 1:
		return "Revised for the 1 finding"
	case revisedFor > 1:
		return fmt.Sprintf("Revised for the %d findings", revisedFor)
	default:
		return "Drawn by " + strings.ReplaceAll(str(p["persona"]), "_", " ")
	}
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

// signoffText renders the sign-off: the body (the entries in playbook order,
// the playbook and the preflight result) and the reply line that decides it,
// which starts with its separator.
func signoffText(p map[string]any) (body, reply string) {
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
		b.WriteString("\nAssumptions, riskiest first:\n")

		for _, a := range list {
			fmt.Fprintf(&b, "- %s (%s) %s\n", str(a["id"]), assumptionStatus(a), str(a["text"]))
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

	body = strings.TrimSuffix(b.String(), "\n")
	reply = "\n\nReply `approve` to emit the cards"

	if len(warnings) > 0 {
		reply += " (this also overrides the warnings)"
	}

	reply += ", or `reject <feedback>` to re-run decomposition with your feedback."

	return body, reply
}

// assumptionStatus names an assumption's status with the source it rests on:
// who answered the question, which one unless it is the entry's own id, and
// the answer; any other status with its evidence and the answer it rests on.
func assumptionStatus(a map[string]any) string {
	status, basis, answer := str(a["status"]), str(a["basis"]), str(a["answer"])

	var who string

	switch status {
	case "assumed":
		who = "assumed by the responder"
	case "confirmed":
		who = "confirmed by you"

		if at := str(a["accepted"]); at != "" {
			return who + ", accepted at " + pack.StageName(at)
		}
	default:
		if evidence := str(a["evidence"]); evidence != "" {
			status += ": " + evidence
		}

		if answer != "" {
			status += "; rests on " + basis + ": " + answer
		}

		return status
	}

	if basis != "" && basis != str(a["id"]) {
		who += ", " + basis
	}

	if answer != "" {
		who += ": " + answer
	}

	return who
}

// policyText renders a gate reply: the change it made and what it leaves
// alone, or why it changed nothing.
func policyText(p map[string]any) string {
	if refused := str(p["refused"]); refused != "" {
		return "Nothing changed. " + refused + " Reply `gate <stage> human` or `gate <stage> auto`."
	}

	text := fmt.Sprintf("Stage %s is now %s for this blueprint, from its next question or gate.", pack.StageName(str(p["stage"])), str(p["policy"]))

	if str(p["open"]) == "round" {
		text += " Its open round of questions stays with whoever it was put to."
	}

	if str(p["open"]) == "gate" {
		text += " Its open gate stays as it is; decide it with approve or reject."
	}

	return text
}

// capText renders a cap reply: the change it made and the stage it
// resumes, or why it changed nothing.
func capText(p map[string]any) string {
	if refused := str(p["refused"]); refused != "" {
		return "Nothing changed: " + refused + ", or reply `cap off`."
	}

	from, to := num(p["from"]), num(p["to"])

	var text string

	switch {
	case to == 0 && from == 0:
		text = "The blueprint has no cost cap"
	case to == 0:
		text = fmt.Sprintf("Cost cap removed (it was $%.2f)", from)
	case from == 0 || from == to:
		text = fmt.Sprintf("Cost cap set to $%.2f", to)
	case to > from:
		text = fmt.Sprintf("Cost cap raised from $%.2f to $%.2f", from, to)
	default:
		text = fmt.Sprintf("Cost cap lowered from $%.2f to $%.2f", from, to)
	}

	if resumes := str(p["resumes"]); resumes != "" {
		return text + "; resuming " + pack.StageName(resumes) + "."
	}

	return text + "."
}

// abortedText renders an abort: a stage whose run it cut runs again on
// continue, while an abort that cut nothing, or the sign-off's emission or
// run start, which approve resumes, leaves continue nothing to run.
func abortedText(p map[string]any) string {
	if stage := str(p["stage"]); stage != "" && stage != "signoff" {
		return "Abort requested. " + pack.StageName(stage) + " is canceled; reply `continue` to run it again. The blueprint is kept and can be reopened."
	}

	return "Abort requested. Any running stage is canceled; the blueprint is kept and can be reopened."
}

// stopsStage reports an error that is a stage run's own stop, which
// continue runs again: it names the stage and why the run ended, and is
// neither a cap stop, whose message says to raise the cap, nor a responder
// refused inside a run that goes on.
func stopsStage(p map[string]any) bool {
	reason := str(p["reason"])

	return str(p["stage"]) != "" && reason != "" && reason != "responder_refused" && str(p["cap"]) == ""
}

// resumeText renders a resume reply: the stage it runs again, or that
// nothing waits to resume and what runs.
func resumeText(p map[string]any) string {
	if nothing, _ := p["nothing"].(bool); !nothing {
		return "Resuming " + pack.StageName(str(p["stage"])) + "."
	}

	if running := str(p["running"]); running != "" {
		return "Nothing is waiting to resume; " + pack.StageName(running) + " is running."
	}

	return "Nothing is waiting to resume."
}

// gatePlanText renders the gate plan: who answers each stage's questions
// and decides its gate, and the replies that change it.
func gatePlanText(p map[string]any) string {
	var b strings.Builder

	b.WriteString("**Gate plan.** Who answers each stage's questions and decides its gate:")

	for _, s := range anyMaps(p["stages"]) {
		policy := str(s["policy"])

		fmt.Fprintf(&b, "\n- %s: %s", pack.StageName(str(s["stage"])), policy)

		if gate, _ := s["gate"].(bool); gate {
			if gp := str(s["gate_policy"]); gp != "" && gp != policy {
				fmt.Fprintf(&b, "; its gate is %s", gp)
			} else {
				b.WriteString(", with a gate")
			}

			if into := str(s["folds_into"]); into != "" {
				limit, unit := num(s["fold_max"]), "entries"
				if limit == 1 {
					unit = "entry"
				}

				fmt.Fprintf(&b, " that folds into %s's gate when both stages have the same gate policy and its %s has at most %.0f %s", pack.StageName(into), str(s["fold_field"]), limit, unit)
			}
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

// gateText renders a stage's gate: the body (the version on the sheet waits,
// the reviewer's score and the critical findings it left open, the
// artifacts folded into it shown below, the problems its checks accepted
// them with) and the reply line that decides it, which starts with its
// separator.
func gateText(p map[string]any) (body, reply string) {
	var replies []string

	for _, a := range anyMaps(p["actions"]) {
		quoted := "`" + str(a["reply"])
		if feedback, _ := a["feedback"].(bool); feedback {
			quoted += " <feedback>"
		}

		replies = append(replies, quoted+"`")
	}

	stage, version := str(p["stage"]), num(p["version"])
	folded := anyMaps(p["folded"])

	text := fmt.Sprintf("**Gate: %s, version %.0f.** Version %.0f is on the sheet and waits for your decision.", pack.StageName(stage), version, version)

	if len(folded) > 0 {
		names := make([]string, 0, len(folded))
		for _, f := range folded {
			names = append(names, fmt.Sprintf("%s version %.0f", pack.StageName(str(f["stage"])), num(f["version"])))
		}

		text += fmt.Sprintf(" It also judges %s, whose gate folded into this one, shown below.", strings.Join(names, " and "))
	}

	// A blank line ends a list or an artifact, so the next line is not read
	// as part of it.
	sep := " "

	// The findings are in the review entry posted just above the gate, so the
	// gate refers to them rather than listing them a second time.
	if v, ok := p["review"].(map[string]any); ok {
		findings := ""
		if len(anyMaps(v["findings"])) > 0 {
			findings = "; its findings are in the review above"
		}

		passed, _ := v["passed"].(bool)

		switch {
		case passed:
			text += fmt.Sprintf(" The reviewer scored it %.0f of 5 (passes at %.0f)%s.", num(v["score"]), num(v["threshold"]), findings)
		default:
			text += fmt.Sprintf(" The reviewer did not pass it after %.0f round(s): %.0f of 5 (passes at %.0f)%s.", num(v["round"]), num(v["score"]), num(v["threshold"]), findings)
		}

		text += openCriticalText(stringsOf(v["open_critical"]))
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
			text += fmt.Sprintf("\n\n**%s, version %.0f**\n\n%s", pack.StageName(str(f["stage"])), num(f["version"]), str(f["body"]))

			if list := problemsList(f["problems"]); list != "" {
				text += "\n\n" + list
			}
		}

		sep = "\n\n"
	}

	return text, sep + fmt.Sprintf("Reply %s.", strings.Join(replies, " or "))
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
			return fmt.Sprintf("%s folds into %s's gate, which judges both.", pack.StageName(stage), pack.StageName(str(p["feedback"])))
		case str(p["decision"]) == "superseded":
			return fmt.Sprintf("%s runs again after %s.", pack.StageName(stage), pack.StageName(str(p["feedback"])))
		case reject:
			return fmt.Sprintf("Rejected %s; it runs again with your feedback.", pack.StageName(stage))
		case auto:
			return fmt.Sprintf("%s passed its gate under the auto policy.", pack.StageName(stage))
		default:
			return fmt.Sprintf("Approved %s.", pack.StageName(stage))
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
		return "Review of " + pack.StageName(str(p["stage"]))
	case store.RunKindResponder:
		return "The responder for " + pack.StageName(str(p["stage"]))
	default:
		return "Stage " + pack.StageName(str(p["stage"]))
	}
}

// reviewText renders a reviewer's verdict and its ledger: each finding's
// id, severity and status, with the builder's claim beside a re-checked one.
func reviewText(p map[string]any) string {
	text := fmt.Sprintf("The reviewer scored %s version %.0f %.0f of 5 (passes at %.0f), round %.0f.",
		pack.StageName(str(p["stage"])), num(p["version"]), num(p["score"]), num(p["threshold"]), num(p["round"]))

	text += openCriticalText(stringsOf(p["open_critical"]))

	for _, f := range anyMaps(p["findings"]) {
		text += "\n- " + findingLine(f)
	}

	return text
}

// openCriticalText names the critical findings a verdict left open, which
// fail it whatever its score; "" when there are none.
func openCriticalText(ids []string) string {
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf(" Open critical finding %s fails it whatever the score.", ids[0])
	default:
		return fmt.Sprintf(" Open critical findings %s fail it whatever the score.", joinIDs(ids))
	}
}

// findingLine is one ledger entry: id, severity, the reviewer's status and
// the text, then the builder's claim, which is never the verdict.
func findingLine(f map[string]any) string {
	status := "open"

	switch str(f["status"]) {
	case "fixed":
		status = fmt.Sprintf("fixed in version %.0f", num(f["verified_in"]))
	case "still_open":
		status = fmt.Sprintf("still open in version %.0f", num(f["verified_in"]))
	}

	line := fmt.Sprintf("%s (%s, %s): %s", str(f["id"]), str(f["severity"]), status, str(f["text"]))

	if note := str(f["note"]); note != "" {
		line += " Reviewer: " + note
	}

	if claim, ok := f["claim"].(string); ok {
		if claim == "" {
			line += " The builder lists it as addressed."
		} else {
			line += " The builder's claim: " + claim
		}
	}

	return line
}

// joinIDs lists ids as a sentence does: R2; R2 and R5; or R2, R4 and R5.
func joinIDs(ids []string) string {
	if len(ids) < 2 {
		return strings.Join(ids, "")
	}

	return strings.Join(ids[:len(ids)-1], ", ") + " and " + ids[len(ids)-1]
}

// actionsPrompt makes a gate decidable by a click from the replies its
// event carries; nil when it carries none, as an event stored before the
// replies travelled with it. The text is the gate's body without its reply
// line.
func actionsPrompt(id, text string, v any) *protocol.LogPrompt {
	var actions []protocol.LogAction

	for _, a := range anyMaps(v) {
		feedback, _ := a["feedback"].(bool)
		actions = append(actions, protocol.LogAction{Label: str(a["label"]), Reply: str(a["reply"]), Feedback: feedback})
	}

	if len(actions) == 0 {
		return nil
	}

	return &protocol.LogPrompt{ID: id, Text: text, Kind: "gate", Actions: actions}
}

// resumePrompt offers approve on a stopped emission or run start; nil on
// any other error or wait. The text is the stop without its reply
// instruction, empty on an event stored before resume events carried one.
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
		Text:    str(p["text"]),
		Kind:    "gate",
		Actions: []protocol.LogAction{{Label: label, Reply: "approve"}},
	}
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

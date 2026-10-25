package cli

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/retro"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// offerPromptID is the mockup offer's prompt id; the view's open prompt for
// the offer carries the same id, and consumers match the two by equality.
const offerPromptID = "mockup:offer"

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
			entry.Content = fmt.Sprintf("%s resumes with the answers to round %.0f.", runLabel(p, e.Sliced), num(p["question_round"]))
		case blueprint.SegmentChat:
			entry.Content = fmt.Sprintf("%s answers the chat about round %.0f.", runLabel(p, e.Sliced), num(p["question_round"]))
			if p["mockup"] == true {
				entry.Content = runLabel(p, e.Sliced) + " answers the chat about the mockup."
			}
		default:
			entry.Content = fmt.Sprintf("%s started as %s.", runLabel(p, e.Sliced), strings.ReplaceAll(str(p["persona"]), "_", " "))

			// A stage run's first round is a divider; its builder rounds, its
			// reviews and its responders are not.
			if kind := str(p["kind"]); (kind == "" || kind == store.RunKindStage) && num(p["round"]) <= 1 {
				stage, slice := pack.SplitStep(str(p["stage"]))
				entry.Marker = &protocol.LogMarker{Kind: "stage", Stage: stage, Slice: slice, Persona: str(p["persona"])}
			}
		}
	case blueprint.EventRound:
		if _, isMockup := p["mockup"]; isMockup {
			return mockupRoundEntry(entry, p)
		}

		n, frame, ok := roundFrame(p)
		if !ok {
			return protocol.LogEntry{}, false
		}

		// Inside the architect a round's stage is its step's key; the wire splits it.
		frame.Stage, frame.Slice = pack.SplitStep(frame.Stage)

		left := str(p["change"]) == blueprint.ChangeLeft
		entry.Type = "text"
		entry.Content = roundText(n, frame, left)
		entry.Prompt = &protocol.LogPrompt{ID: fmt.Sprintf("round:%d", n), Kind: "round", Text: roundHeading(n, frame, left), Round: &frame}
	case blueprint.EventArtifact:
		// A sheet reference: the body is on the sheet, never transcript text.
		note := artifactNote(p)
		stage, slice := pack.SplitStep(str(p["stage"]))
		entry.Type = "text"
		entry.Content = fmt.Sprintf("%s rev %.0f: %s.", pack.StageName(str(p["stage"]), e.Sliced), num(p["version"]), note)
		entry.Marker = &protocol.LogMarker{
			Kind: "artifact", Stage: stage, Slice: slice, Version: int(num(p["version"])), Persona: str(p["persona"]), Note: note,
		}
	case blueprint.EventStageEnded:
		entry.Type = "system"

		label := "reported cost"
		if priced, _ := p["priced"].(bool); priced {
			label = "cost"
		}

		if str(p["status"]) == store.RunAsked {
			what := fmt.Sprintf("%s waits for the answers to round %.0f", runLabel(p, e.Sliced), num(p["question_round"]))

			// A mockup's round never reaches the panel, so its end names none.
			switch chat, _ := p["chat"].(bool); {
			case p["mockup"] == true && num(p["revision"]) > 0:
				what = fmt.Sprintf("%s shows revision %.0f", runLabel(p, e.Sliced), num(p["revision"]))
			case p["mockup"] == true:
				what = runLabel(p, e.Sliced) + " replied about the mockup"
			case chat:
				what = fmt.Sprintf("%s replied in round %.0f's chat", runLabel(p, e.Sliced), num(p["question_round"]))
			case p["responder"] == true:
				what = fmt.Sprintf("%s had the responder answer round %.0f", runLabel(p, e.Sliced), num(p["question_round"]))
			}

			entry.Content = fmt.Sprintf("%s (%s %.2f USD).", what, label, num(p["cost_usd"]))

			break
		}

		entry.Content = fmt.Sprintf("%s %s (%s %.2f USD).", runLabel(p, e.Sliced), str(p["status"]), label, num(p["cost_usd"]))
	case blueprint.EventNote:
		entry.Type = "system"

		duringRun, _ := p["during_run"].(bool)
		ended, _ := p["ended"].(bool)
		critique, _ := p["critique"].(bool)

		switch stage := str(p["retries"]); {
		case num(p["round"]) > 0 && stage == "":
			entry.Content = fmt.Sprintf("Noted. Round %.0f still waits for its answers in the question block; the stage's next run gets the note.", num(p["round"]))
		case stage != "" && critique:
			entry.Content = fmt.Sprintf("Noted. The review of %s, or its next builder round, runs again, and it and every run after it get the note.", pack.StageName(stage, e.Sliced))
		case stage != "":
			entry.Content = fmt.Sprintf("Noted. Stage %s runs again, and it and every stage after it get the note.", pack.StageName(stage, e.Sliced))
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
		body, reply := signoffText(p, e.Sliced)
		entry.Content = body + reply
		entry.Prompt = actionsPrompt(fmt.Sprintf("gate:signoff:v%.0f", num(p["version"])), body, p["actions"], e.Plain, plainSignoffText(p), true)
	case blueprint.EventGate:
		entry.Type = "text"
		body, reply := gateText(p, e.Sliced)
		entry.Content = body + reply
		entry.Prompt = actionsPrompt(blueprint.GatePromptID(str(p["stage"]), int(num(p["version"]))), body, p["actions"], e.Plain, plainGateText(p, e.Sliced), false)

		// A gate the review settled asks for a look, so approving it is no
		// "anyway".
		if e.Plain && entry.Prompt != nil && gateSettled(p) {
			for i, a := range entry.Prompt.Actions {
				if a.Reply == "approve" {
					entry.Prompt.Actions[i].Label = "Continue"
				}
			}
		}
	case blueprint.EventReview:
		// A review card: the view's review of this version renders it; the
		// verdict's text is the fallback.
		stage, slice := pack.SplitStep(str(p["stage"]))
		entry.Type = "text"
		entry.Content = reviewText(p, e.Sliced)
		entry.Marker = &protocol.LogMarker{
			Kind: "review", Stage: stage, Slice: slice, Version: int(num(p["version"])), Persona: str(p["reviewer"]),
		}
	case blueprint.EventDecided:
		entry.Type = "system"
		entry.Content = decidedText(p, e.Sliced)

		// The mode's approval of a sign-off or gate already shown closes that
		// prompt in the panel, as a reply would.
		if shown, _ := p["shown"].(bool); shown {
			stage, slice := pack.SplitStep(str(p["stage"]))
			entry.Marker = &protocol.LogMarker{Kind: "decision", Stage: stage, Slice: slice, Version: int(num(p["version"]))}
		}
	case blueprint.EventMockupOffer:
		switch str(p["state"]) {
		case "shown":
			// The same words with plain language on or off.
			text := "Your need has a screen people will use. Want a first look at it, a mockup you can adjust, before the architect designs the rest?"
			entry.Type = "text"
			entry.Content = text + " Reply `show mockup` or `skip mockup`."
			entry.Prompt = actionsPrompt(offerPromptID, text, p["actions"], false, "", false)
		case "accepted":
			entry.Type = "system"
			entry.Content = "The designer draws a first look."

			// The mode took the offer, which closes its prompt as a reply would.
			if str(p["by"]) == "mode" {
				entry.Content = "The new mode takes the offer: the designer draws a first look."
				entry.Marker = &protocol.LogMarker{Kind: "decision", Stage: str(p["stage"])}
			}
		case "declined":
			entry.Type = "system"
			entry.Content = "No mockup: the architect goes on without one."
		default:
			return protocol.LogEntry{}, false
		}
	case blueprint.EventAborted:
		entry.Type = "system"
		entry.Content = abortedText(p, e.Sliced)
	case blueprint.EventExported:
		files, _ := p["files"].(int)
		entry.Type = "system"
		entry.Content = fmt.Sprintf("Export written on the server: %s (%d files).", str(p["path"]), files)
	case blueprint.EventRetroReply:
		entry.Type = "system"
		entry.Content = str(p["message"])
	case blueprint.EventLessonList:
		entry.Type = "text"
		entry.Content = lessonListText(p)
	case blueprint.EventLessonRetire:
		entry.Type = "system"
		entry.Content = lessonRetireText(p)
	case blueprint.EventRetroReport:
		entry.Type = "text"
		entry.Content = retroReportText(p)
	case blueprint.EventLessonDecided:
		entry.Type = "system"
		entry.Content = lessonDecidedText(p)
	case blueprint.EventError:
		entry.Type = "stderr"
		entry.Content = str(p["message"])

		if stopsStage(p) {
			entry.Content = strings.TrimSuffix(entry.Content, ".") + ". Reply `continue` to run it again."
		}

		// A prompt rides a text frame, so a stop that offers approve is one.
		if prompt := resumePrompt(p, e.Plain); prompt != nil {
			entry.Type = "text"
			entry.Prompt = prompt
		}
	case blueprint.EventAwaitInput:
		entry.Type = "text"
		entry.Content = str(p["prompt"])

		if strings.TrimSpace(entry.Content) == "" {
			entry.Content = fmt.Sprintf("Stage %s takes your next message.", pack.StageName(str(p["stage"]), e.Sliced))
		}

		entry.Prompt = resumePrompt(p, e.Plain)
	case blueprint.EventSetupGaps:
		entry.Type = "text"
		entry.Content = setupGapsText(p)

		if e.Plain {
			lead := plainSetupSettledLead
			if setupNeedsAPerson(p) {
				lead = plainSetupLead
			}

			entry.Content = lead + "\n\n" + entry.Content
		}
	case blueprint.EventPolicy:
		entry.Type = "system"
		entry.Content = policyText(p)
	case blueprint.EventCap:
		entry.Type = "system"
		entry.Content = capText(p, e.Sliced)
	case blueprint.EventMode:
		entry.Type = "system"
		entry.Content = modeText(p, e.Sliced)
	case blueprint.EventResume:
		entry.Type = "system"
		entry.Content = resumeText(p, e.Sliced)
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
	case blueprint.EventRetroStopOffered:
		entry.Type = "system"
		entry.Content = str(p["line"])
	case blueprint.EventWatchEnded:
		entry.Type = "text"

		var b strings.Builder

		cards := anyMaps(p["cards"])
		pb := str(p["playbook"])

		switch {
		case pb != "" && flag(p["playbook_deleted"]):
			fmt.Fprintf(&b, "**Ended.** Playbook %s was deleted in ContextMatrix.", pb)
		case pb != "":
			fmt.Fprintf(&b, "**Done.** Playbook %s completed.", pb)
		case len(cards) == 1 && flag(cards[0]["deleted"]):
			b.WriteString("**Ended.**")
		default:
			b.WriteString("**Done.**")
		}

		if repos := anyMaps(p["repos"]); pb != "" && len(repos) > 0 {
			b.WriteString("\n\nOpen the final pull request for each repository:")

			for _, r := range repos {
				fmt.Fprintf(&b, "\n- %s: %s", str(r["project"]), str(r["compare_url"]))
			}
		}

		if len(cards) > 0 {
			b.WriteString("\n\nCards:")
		}

		for _, card := range cards {
			if flag(card["deleted"]) {
				fmt.Fprintf(&b, "\n- %s was deleted in ContextMatrix", str(card["card"]))
			} else {
				fmt.Fprintf(&b, "\n- %s is %s", str(card["card"]), str(card["state"]))
			}

			if pr := str(card["pr"]); pr != "" {
				b.WriteString(": " + pr)
			}
		}

		switch n := int(num(p["followups"])); {
		case n == 1:
			fmt.Fprintf(&b, "\n\nPlaybook %s keeps 1 follow-up for you to check after the run; open it in ContextMatrix.", str(p["playbook"]))
		case n > 1:
			fmt.Fprintf(&b, "\n\nPlaybook %s keeps %d follow-ups for you to check after the run; open them in ContextMatrix.", str(p["playbook"]), n)
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

		b.WriteString("\n\nReply export for the bundle, retrospective to learn from the cards that struggled, or end the session.")

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

		if notices := stringsOf(p["notices"]); len(notices) > 0 {
			b.WriteString("\n\nNotices (no step needed before the cards run):")

			for _, s := range notices {
				b.WriteString("\n- " + s)
			}
		}

		entry.Content = b.String()

		if e.Plain {
			entry.Content = plainEmittedText(p) + "\n\n" + entry.Content
		}
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

// mockupRoundEntry is a mockup round's snapshot as the transcript shows it:
// a mockup is a round inside the architect, never a round prompt. A revision
// or a reply adds the designer's chat entry, the one the run behind it saw
// last, which is a text row. The auto responder's change request and its
// use of a revision are system rows that name it. Any other change adds no
// row: CM shows the person's own messages and replies, and a failed
// responder's error row says what became of the revision.
func mockupRoundEntry(entry protocol.LogEntry, p map[string]any) (protocol.LogEntry, bool) {
	_, frame, ok := roundFrame(p)
	if !ok || len(frame.Chat) == 0 {
		return protocol.LogEntry{}, false
	}

	var m struct {
		Revision    int                      `json:"revision"`
		Suggestions []stage.MockupSuggestion `json:"suggestions"`
		Chosen      map[string]string        `json:"chosen"`
		By          string                   `json:"by"`
		Failed      bool                     `json:"failed"`
	}

	if raw, err := json.Marshal(p["mockup"]); err != nil || json.Unmarshal(raw, &m) != nil {
		return protocol.LogEntry{}, false
	}

	switch str(p["change"]) {
	case "posted", "revision", "reply":
		seen := int(num(p["seen"]))
		if seen < 1 || seen > len(frame.Chat) {
			seen = len(frame.Chat)
		}

		entry.Type = "text"
		entry.Content = frame.Chat[seen-1].Text
	case "chat":
		last := frame.Chat[len(frame.Chat)-1]
		if last.From != "responder" {
			return protocol.LogEntry{}, false
		}

		entry.Type = "system"
		entry.Content = fmt.Sprintf("The auto responder asks for a change to revision %d of the mockup: %s", m.Revision, last.Text)
	case "chosen":
		// A failed responder made no pick: its error row says the revision
		// is used as set.
		if m.By != store.ResponderAuto || m.Failed {
			return protocol.LogEntry{}, false
		}

		set := make([]string, 0, len(m.Suggestions))
		for _, sug := range m.Suggestions {
			set = append(set, fmt.Sprintf("%s: %s", sug.Title, sug.Label(m.Chosen[sug.ID])))
		}

		entry.Type = "system"
		entry.Content = fmt.Sprintf("The auto responder uses revision %d of the mockup.", m.Revision)

		if len(set) > 0 {
			entry.Content = fmt.Sprintf("The auto responder uses revision %d of the mockup, set to %s.", m.Revision, strings.Join(set, "; "))
		}
	default:
		return protocol.LogEntry{}, false
	}

	return entry, true
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
// of what is known at store time: a mockup's revision, cards accepted with
// contract findings, a loop stage's round, a revision for its reviewer's
// findings, or the persona that drew it.
func artifactNote(p map[string]any) string {
	revisedFor := int(num(p["revised_for"]))

	switch {
	case num(p["revision"]) > 0:
		return fmt.Sprintf("Revision %.0f", num(p["revision"]))
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

func flag(v any) bool {
	b, _ := v.(bool)

	return b
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
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

// unattendedText is an unattended sign-off's line on where the run stops
// for a person: nowhere, or at each stop in run order, with its note as the
// reason (its text, what the person does, when it has none) and readiness's
// finding when readiness judged the stop needless.
func unattendedText(stops []map[string]any) string {
	if len(stops) == 0 {
		return "Unattended: this run will not stop for a person."
	}

	at := func(s map[string]any) string {
		out := "at " + str(s["id"]) + ", "

		if note := strings.TrimSpace(str(s["note"])); note != "" {
			out += "because " + sentence(note)
		} else {
			out += "for this step: " + sentence(str(s["text"]))
		}

		if needless := strings.TrimSpace(str(s["needless"])); needless != "" {
			out += " Readiness judged this stop needless: " + sentence(needless)
		}

		return out
	}

	if len(stops) == 1 {
		return "Unattended: this run will stop once, " + at(stops[0])
	}

	var b strings.Builder

	times := "twice"
	if len(stops) > 2 {
		times = fmt.Sprintf("%d times", len(stops))
	}

	b.WriteString("Unattended: this run will stop " + times + ":")

	for _, s := range stops {
		b.WriteString("\n- " + at(s))
	}

	return b.String()
}

// sentence is s trimmed, ending in a full stop unless it ends a sentence
// already.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s[len(s)-1:], ".!?") {
		return s
	}

	return s + "."
}

// signoffText renders the sign-off: the body (readiness's findings across
// the blueprint, the entries in playbook order, the playbook and the
// preflight result) and the reply line that decides it, which starts with
// its separator. sliced names a step's slice, and lists the typed
// rejections. Cards readiness still found not ready (not_ready, absent on a
// sign-off stored before it existed) change the header in every mode and
// are listed ahead of the cross findings; a critical finding keeps its own
// reply.
func signoffText(p map[string]any, sliced bool) (body, reply string) {
	var b strings.Builder

	cross := anyMaps(p["cross"])
	critical := slices.ContainsFunc(cross, func(f map[string]any) bool { return str(f["severity"]) == "critical" })

	open := stringsOf(p["not_ready"])

	switch auto, _ := p["auto_stopped"].(bool); {
	case len(open) > 0:
		b.WriteString("**Sign-off needed.** Readiness still found cards not ready after its last round, so a human decides in every mode.\n\n")
	case auto && critical:
		b.WriteString("**Sign-off needed.** This mode emits on its own, but readiness found a critical problem across the blueprint.\n\n")
	case auto:
		b.WriteString("**Sign-off needed.** This mode emits on its own, but preflight found something for a human.\n\n")
	default:
		b.WriteString("**Sign-off.** Readiness passed the cards.\n\n")
	}

	if len(open) > 0 {
		b.WriteString("Readiness still finds these cards not ready:\n")

		for _, s := range open {
			b.WriteString("- " + s + "\n")
		}

		b.WriteString("\n")
	}

	if len(cross) > 0 {
		b.WriteString("Across the blueprint, readiness found:\n")

		for _, f := range cross {
			fmt.Fprintf(&b, "- %s (%s) %s", str(f["id"]), str(f["severity"]), str(f["text"]))

			if refs := stringsOf(f["refs"]); len(refs) > 0 {
				b.WriteString(" [" + strings.Join(refs, ", ") + "]")
			}

			b.WriteString("\n")
		}

		b.WriteString("\n")
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

	if ids := stringsOf(p["followups"]); len(ids) > 0 {
		b.WriteString("\nAfter the run (follow-ups):\n")

		for _, id := range ids {
			fmt.Fprintf(&b, "- %s: %s\n", id, manual[id])
		}
	}

	// Only an unattended blueprint's sign-off carries stops.
	if stops, ok := p["stops"]; ok {
		b.WriteString("\n" + unattendedText(anyMaps(stops)) + "\n")
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
			fmt.Fprintf(&b, "- %s (%s) %s\n", str(a["id"]), assumptionStatus(a, sliced), str(a["text"]))
		}
	case str(p["assumptions"]) != "":
		// Intake's section text: a sign-off stored before assumptions were
		// data, or one whose intake predates them.
		b.WriteString("\nAssumptions:\n" + str(p["assumptions"]) + "\n")
	default:
		b.WriteString("\nAssumptions: none recorded.\n")
	}

	blocking, warnings, steps, notices, pfErr := stringsOf(p["blocking"]), stringsOf(p["warnings"]), stringsOf(p["steps"]), stringsOf(p["notices"]), str(p["preflight_error"])

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

	if len(notices) > 0 {
		b.WriteString("\nNotices (no step needed before the cards run):\n")

		for _, s := range notices {
			b.WriteString("- " + s + "\n")
		}
	}

	if pfErr != "" {
		b.WriteString("\nPreflight could not run: " + pfErr + "\n")
	}

	if len(blocking) == 0 && len(warnings) == 0 && len(steps) == 0 && len(notices) == 0 && pfErr == "" {
		b.WriteString("\nPreflight found nothing for a human.\n")
	}

	body = strings.TrimSuffix(b.String(), "\n")
	reply = "\n\nReply `approve` to emit the cards"

	switch {
	case critical:
		reply = "\n\nReply `approve <reason>` to emit the cards over the critical findings"
	case len(open) > 0:
		reply = "\n\nReply `approve <reason>` to emit the cards over the open findings"
	}

	if len(warnings) > 0 {
		reply += " (this also overrides the warnings)"
	}

	reply += ", or `reject <feedback>` to re-run the blueprint with your feedback."

	// Below two slices only the typed replies that name no slice are
	// listed: nothing slice-related shows.
	typed := stringsOf(p["typed"])
	if !sliced {
		typed = slices.DeleteFunc(slices.Clone(typed), blueprint.NamesASlice)
	}

	if len(typed) > 0 {
		quoted := make([]string, 0, len(typed))
		for _, t := range typed {
			quoted = append(quoted, "`"+t+" <feedback>`")
		}

		reply += " Or reply " + strings.Join(quoted, ", ") + " to run that step and everything after it again."
	}

	return body, reply
}

// assumptionStatus names an assumption's status with the source it rests on:
// who answered the question, which one unless it is the entry's own id, and
// the answer; any other status with its evidence and the answer it rests on.
// sliced names the slice of the step it was accepted at.
func assumptionStatus(a map[string]any, sliced bool) string {
	status, basis, answer := str(a["status"]), str(a["basis"]), str(a["answer"])

	var who string

	switch status {
	case "assumed":
		who = "assumed by the responder"
	case "confirmed":
		who = "confirmed by you"

		if at := str(a["accepted"]); at != "" {
			return who + ", accepted at " + pack.StageName(at, sliced)
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

	text := fmt.Sprintf("Stage %s is now %s for this blueprint, from its next question or gate.", pack.StageName(str(p["stage"]), false), str(p["policy"]))

	if str(p["open"]) == "round" {
		text += " Its open round of questions stays with whoever it was put to."
	}

	// A mockup is never a round to the person, so its line names none.
	if str(p["open"]) == "mockup" {
		text += " Its open mockup stays with whoever steers it until its next revision."
	}

	if str(p["open"]) == "gate" {
		text += " Its open gate stays as it is; decide it with approve or reject."
	}

	if str(p["open"]) == "offer" {
		text += " Its open offer of a first look stays as it is; reply show mockup or skip mockup."
	}

	return text
}

// capText renders a cap reply: the change it made and the stage it
// resumes, or why it changed nothing.
func capText(p map[string]any, sliced bool) string {
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
		return text + "; resuming " + pack.StageName(resumes, sliced) + "."
	}

	return text + "."
}

// modeText renders a mode reply: the switch and what it hands on at once, or
// why it changed nothing.
func modeText(p map[string]any, sliced bool) string {
	if refused := str(p["refused"]); refused != "" {
		return "Nothing changed. " + refused
	}

	text := fmt.Sprintf("Mode changed from %s to %s.", modeName(str(p["from"])), modeName(str(p["to"])))

	// A mockup is never a round to the person, so its hand-over names none.
	switch n := num(p["hands_over"]); {
	case n > 0 && p["mockup"] == true:
		text += " The mockup goes to the auto responder, which steers it on from the revision shown."
	case n > 0:
		text += fmt.Sprintf(" Round %.0f goes to the auto responder, which answers what is still open; the answers already sent stay.", n)
	}

	if signs, _ := p["signoff"].(bool); signs {
		text += " The sign-off is decided again under the new mode."
	}

	if gate := str(p["gate"]); gate != "" {
		text += fmt.Sprintf(" %s's gate is decided under the new mode.", pack.StageName(gate, sliced))
	}

	return text
}

// modeName is a mode as CM's new-blueprint dialog labels it.
func modeName(m string) string {
	switch m {
	case protocol.ArchitectModeDefault:
		return "Default"
	case protocol.ArchitectModeAuto:
		return "Auto"
	case protocol.ArchitectModeAutoEmit:
		return "Auto + emit"
	case protocol.ArchitectModeYolo:
		return "Yolo"
	default:
		return m
	}
}

// abortedText renders an abort: a stage whose run it cut runs again on
// continue, while an abort that cut nothing, or the sign-off's emission or
// run start, which approve resumes, leaves continue nothing to run, and a
// retrospective runs again on its own reply.
func abortedText(p map[string]any, sliced bool) string {
	if str(p["stage"]) == blueprint.RetroStage {
		return "Abort requested. The retrospective is canceled; reply `retrospective` to run it again. The blueprint is kept and can be reopened."
	}

	if stage := str(p["stage"]); stage != "" && stage != "signoff" {
		return "Abort requested. " + pack.StageName(stage, sliced) + " is canceled; reply `continue` to run it again. The blueprint is kept and can be reopened."
	}

	return "Abort requested. Any running stage is canceled; the blueprint is kept and can be reopened."
}

// stopsStage reports an error that is a stage run's own stop, which
// continue runs again: it names the stage and why the run ended, and is
// neither a cap stop, whose message says to raise the cap, nor a responder
// refused inside a run that goes on, nor a mockup responder that failed,
// whose revision is used.
func stopsStage(p map[string]any) bool {
	reason := str(p["reason"])

	return str(p["stage"]) != "" && reason != "" && !goesOn(reason) && str(p["cap"]) == ""
}

// goesOn reports an error reason after which the pipeline goes on by
// itself: a responder a cap refused inside its asking run, and a mockup
// responder that failed, whose revision is then used.
func goesOn(reason string) bool {
	return reason == "responder_refused" || reason == "responder_failed"
}

// resumeText renders a resume reply: the stage it runs again, or that
// nothing waits to resume and what runs.
func resumeText(p map[string]any, sliced bool) string {
	if nothing, _ := p["nothing"].(bool); !nothing {
		return "Resuming " + pack.StageName(str(p["stage"]), sliced) + "."
	}

	if running := str(p["running"]); running != "" {
		return "Nothing is waiting to resume; " + pack.StageName(running, sliced) + " is running."
	}

	return "Nothing is waiting to resume."
}

// retroReportText renders a retrospective's findings message: the counts,
// then each struggled card with its signals, why it struggled, its finding's
// rule and evidence, and its proposed lesson or that the struggle was not
// the card's fault.
func retroReportText(p map[string]any) string {
	var b strings.Builder

	fmt.Fprintf(&b, "**Retrospective**: %.0f of %.0f emitted card(s) struggled.", num(p["struggled"]), num(p["read"]))

	for _, card := range anyMaps(p["cards"]) {
		fmt.Fprintf(&b, "\n\n**%s** (%s): %s", str(card["card"]), str(card["project"]), str(card["title"]))
		fmt.Fprintf(&b, "\n- State %s", str(card["state"]))

		if worker := str(card["worker"]); worker != "" {
			fmt.Fprintf(&b, ", worker %s", worker)
		}

		fmt.Fprintf(&b, "; %.0f review attempt(s), %.0f subtask(s), $%.2f.", num(card["review_attempts"]), num(card["subtasks"]), num(card["cost_usd"]))
		fmt.Fprintf(&b, "\n- Struggled: %s.", strings.Join(whyWords(card), "; "))
		fmt.Fprintf(&b, "\n- Rule: %s", str(card["rule"]))
		fmt.Fprintf(&b, "\n- Evidence: \"%s\"", strings.Join(strings.Fields(str(card["evidence"])), " "))

		if lesson, ok := card["lesson"].(map[string]any); ok {
			scope := "for project " + str(lesson["project"])
			if str(lesson["scope"]) == store.ScopePack {
				scope = "for the whole pack"
			}

			fmt.Fprintf(&b, "\n- Proposed lesson L%.0f, %s: %s", num(lesson["id"]), scope, str(lesson["text"]))

			continue
		}

		b.WriteString("\n- Not the card's fault, so no lesson.")
	}

	return b.String()
}

// lessonDecidedText renders a lesson's decision: approved for its project
// or the whole pack, rejected, or rejected because the pack's lists merged
// with its additions failed, with the parser's error.
func lessonDecidedText(p map[string]any) string {
	tag := fmt.Sprintf("L%.0f", num(p["lesson"]))

	switch {
	case str(p["error"]) != "":
		return fmt.Sprintf("Lesson %s is rejected: the pack's lists with its additions do not load: %s", tag, str(p["error"]))
	case str(p["state"]) == store.LessonRejected:
		return "Lesson " + tag + " is rejected."
	case str(p["scope"]) == store.ScopePack:
		return fmt.Sprintf("Lesson %s is approved for the whole pack: %s", tag, str(p["text"]))
	default:
		return fmt.Sprintf("Lesson %s is approved for project %s: %s", tag, str(p["project"]), str(p["text"]))
	}
}

// lessonRetireText renders a retire reply: the lesson retired with its text,
// or the refusal.
func lessonRetireText(p map[string]any) string {
	if refused := str(p["refused"]); refused != "" {
		return refused
	}

	return fmt.Sprintf("Retired L%.0f: %s.", num(p["lesson"]), strings.TrimSuffix(str(p["text"]), "."))
}

// lessonListText renders a lessons reply: one line per lesson with its state,
// scope, text, rationale and evidence card ids, or that there are none yet.
func lessonListText(p map[string]any) string {
	lessons := anyMaps(p["lessons"])
	if len(lessons) == 0 {
		return "There are no lessons yet."
	}

	var b strings.Builder

	b.WriteString("**Lessons**")

	for _, l := range lessons {
		scope := "whole pack"
		if str(l["scope"]) == store.ScopeProject {
			scope = "project " + str(l["project"])
		}

		fmt.Fprintf(&b, "\n- L%.0f (%s, %s): %s", num(l["lesson"]), str(l["state"]), scope, str(l["text"]))

		if why := str(l["rationale"]); why != "" {
			fmt.Fprintf(&b, " Why: %s", why)
		}

		if ev := stringsOf(l["evidence"]); len(ev) > 0 {
			fmt.Fprintf(&b, " Evidence: %s.", strings.Join(ev, ", "))
		}
	}

	return b.String()
}

// whyWords says in words why a struggled card of a findings message
// struggled.
func whyWords(card map[string]any) []string {
	var out []string

	for _, why := range stringsOf(card["why"]) {
		switch why {
		case retro.WhyReviews:
			out = append(out, fmt.Sprintf("%.0f review attempts", num(card["review_attempts"])))
		case retro.WhyParked:
			parked := "parked"
			if reason := str(card["reason"]); reason != "" {
				parked += " (" + reason + ")"
			}

			out = append(out, parked)
		case retro.WhyStalled:
			out = append(out, "stalled")
		case retro.WhyBlocked:
			out = append(out, "blocked")
		case retro.WhyNotPlanned:
			out = append(out, "not planned")
		case retro.WhyCost:
			cost, median := num(card["cost_usd"]), num(card["median_usd"])
			if median > 0 {
				out = append(out, fmt.Sprintf("cost $%.2f, %.1f times the costed cards' median of $%.2f", cost, cost/median, median))

				break
			}

			out = append(out, fmt.Sprintf("cost $%.2f", cost))
		default:
			out = append(out, why)
		}
	}

	return out
}

// gatePlanText renders the gate plan: who answers each stage's questions
// and decides its gate, and the replies that change it.
func gatePlanText(p map[string]any) string {
	var b strings.Builder

	b.WriteString("**Gate plan.** Who answers each stage's questions and decides its gate:")

	for _, s := range anyMaps(p["stages"]) {
		policy := str(s["policy"])

		fmt.Fprintf(&b, "\n- %s: %s", pack.StageName(str(s["stage"]), false), policy)

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

				fmt.Fprintf(&b, " that folds into %s's gate when both stages have the same gate policy and its %s has at most %.0f %s", pack.StageName(into, false), str(s["fold_field"]), limit, unit)
			}
		}

		if perSlice, _ := s["per_slice"].(bool); perSlice {
			b.WriteString(", once per slice")
		}

		if shared, _ := s["shared"].(bool); shared {
			b.WriteString(", skipped with one slice, whose design then settles it")
		}
	}

	switch str(p["signoff"]) {
	case "auto":
		b.WriteString("\n- sign-off: auto unless something needs a person (preflight, a critical finding or cards not ready)")
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
// separator. sliced names a step's slice.
func gateText(p map[string]any, sliced bool) (body, reply string) {
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

	text := fmt.Sprintf("**Gate: %s, version %.0f.** Version %.0f is on the sheet and waits for your decision.", pack.StageName(stage, sliced), version, version)

	if len(folded) > 0 {
		names := make([]string, 0, len(folded))
		for _, f := range folded {
			names = append(names, fmt.Sprintf("%s version %.0f", pack.StageName(str(f["stage"]), sliced), num(f["version"])))
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
			text += fmt.Sprintf("\n\n**%s, version %.0f**\n\n%s", pack.StageName(str(f["stage"]), sliced), num(f["version"]), str(f["body"]))

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
// gate, a mockup's use or skip, or a decision at the sign-off; a decision
// without a stage is the sign-off's. sliced names a step's slice.
func decidedText(p map[string]any, sliced bool) string {
	reject, auto := str(p["decision"]) == "reject", str(p["responder"]) == "auto"

	// A mockup's use or skip, which settles its step; no gate decided it. A
	// use a failed responder left to the architect is no pick of its own.
	if p["mockup"] == true {
		switch {
		case p["failed"] == true:
			return fmt.Sprintf("The architect uses revision %.0f of the mockup as it is set and designs the rest from it.", num(p["version"]))
		case reject:
			return fmt.Sprintf("Mockup skipped at revision %.0f: the architect goes on without one.", num(p["version"]))
		case auto:
			return fmt.Sprintf("The auto responder chose revision %.0f of the mockup; the architect designs the rest from it.", num(p["version"]))
		default:
			return fmt.Sprintf("You chose revision %.0f of the mockup; the architect designs the rest from it.", num(p["version"]))
		}
	}

	if stage := str(p["stage"]); stage != "" && stage != "signoff" {
		switch {
		case str(p["decision"]) == "fold" && num(p["version"]) == 0:
			// A stage collapses only below two slices, so no name shows a slice.
			return fmt.Sprintf("%s is skipped: %s settles what every slice shares.", pack.StageName(stage, false), pack.StageName(str(p["feedback"]), false))
		case str(p["decision"]) == "fold":
			return fmt.Sprintf("%s folds into %s's gate, which judges both.", pack.StageName(stage, sliced), pack.StageName(str(p["feedback"]), sliced))
		case str(p["decision"]) == "superseded":
			return fmt.Sprintf("%s runs again after %s.", pack.StageName(stage, sliced), pack.StageName(str(p["feedback"]), sliced))
		case reject:
			return fmt.Sprintf("Rejected %s; it runs again with your feedback.", pack.StageName(stage, sliced))
		case auto:
			return fmt.Sprintf("%s passed its gate under the auto policy.", pack.StageName(stage, sliced))
		default:
			return fmt.Sprintf("Approved %s.", pack.StageName(stage, sliced))
		}
	}

	switch {
	case str(p["override_reason"]) != "":
		return "Approved over open findings: " + str(p["override_reason"]) + ". Emission starts."
	case reject && str(p["reruns"]) != "":
		return fmt.Sprintf("Rejected. %s runs again with your feedback, and everything after it.", pack.StageName(str(p["reruns"]), sliced))
	case reject:
		return "Rejected. Decomposition runs again with your feedback."
	case auto:
		return "Preflight found nothing that needs a human first, so the mode approved the cards. Emission starts."
	default:
		return "Approved. Emission starts."
	}
}

// runLabel names a run in the panel: a stage, a stage's review, the
// responder answering for a stage, or the retrospective. sliced names a
// step's slice.
func runLabel(p map[string]any, sliced bool) string {
	switch str(p["kind"]) {
	case store.RunKindReviewer:
		return "Review of " + pack.StageName(str(p["stage"]), sliced)
	case store.RunKindResponder:
		return "The responder for " + pack.StageName(str(p["stage"]), sliced)
	case store.RunKindRetro:
		return "The retrospective"
	default:
		return "Stage " + pack.StageName(str(p["stage"]), sliced)
	}
}

// reviewText renders a reviewer's verdict and its ledger: each finding's
// id, severity and status, with the builder's claim beside a re-checked one.
// sliced names a step's slice.
func reviewText(p map[string]any, sliced bool) string {
	text := fmt.Sprintf("The reviewer scored %s version %.0f %.0f of 5 (passes at %.0f), round %.0f.",
		pack.StageName(str(p["stage"]), sliced), num(p["version"]), num(p["score"]), num(p["threshold"]), num(p["round"]))

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
// line. On a plain blueprint the prompt shows plainText and the choices in
// plain words, and the technical text is folded into Detail; the replies are
// those of the event.
func actionsPrompt(id, text string, v any, plain bool, plainText string, signoff bool) *protocol.LogPrompt {
	var actions []protocol.LogAction

	for _, a := range anyMaps(v) {
		feedback, _ := a["feedback"].(bool)
		recommended, _ := a["recommended"].(bool)
		action := protocol.LogAction{Label: str(a["label"]), Reply: str(a["reply"]), Feedback: feedback, Ask: str(a["ask"]), Recommended: recommended}

		if plain {
			action = plainLabel(action, signoff)
		}

		actions = append(actions, action)
	}

	if len(actions) == 0 {
		return nil
	}

	prompt := &protocol.LogPrompt{ID: id, Text: text, Kind: "gate", Actions: actions}
	if plain {
		prompt.Text, prompt.Detail = plainText, text
	}

	return prompt
}

// plainLabel words a gate's choice plainly. It changes the label and the
// question asked for feedback only, never the reply or whether feedback is
// taken. A sign-off's approve is worded so only when it takes a reason, the
// override over a critical finding or over open card findings; a reject that
// is not the plain one (a folded gate's, a typed step) keeps its label.
func plainLabel(a protocol.LogAction, signoff bool) protocol.LogAction {
	switch {
	case a.Reply == "approve" && (!signoff || a.Feedback):
		a.Label = "Continue anyway"
		if a.Feedback {
			a.Ask = "Why continue anyway?"
		}
	case a.Reply == "reject":
		a.Label = "Try again"
		if a.Feedback {
			a.Ask = "What should change?"
		}
	}

	return a
}

// gateSettled reports whether a gate's review passed: the gate then asks for
// a look, not for a decision over a failure.
func gateSettled(p map[string]any) bool {
	v, ok := p["review"].(map[string]any)
	if !ok {
		return false
	}

	passed, _ := v["passed"].(bool)

	return passed
}

// plainGateText is what a person reads of a stage gate: whether the review
// settled it decides the wording.
func plainGateText(p map[string]any, sliced bool) string {
	name := pack.StageName(str(p["stage"]), sliced)

	if gateSettled(p) {
		return name + " is ready for your look. Continue, or ask the architect to try again with a note."
	}

	return "The architect could not settle " + name + " on its own. A technical helper can read why below. Continue anyway, or ask it to try again with a note."
}

// plainSignoffText is what a person reads of the sign-off: cards readiness
// did not pass, or a critical finding across the blueprint, are the moments
// the architect is stuck (the open cards win), setup steps are named
// whenever the sign-off carries any, notices say the work settles some
// setup itself, and an unattended run's stops are counted.
func plainSignoffText(p map[string]any) string {
	var text string

	switch {
	case len(stringsOf(p["not_ready"])) > 0:
		text = "The architect could not settle every piece of the work on its own. A technical helper can read what is open below. Continue anyway and say why, or ask it to try again."
	case slices.ContainsFunc(anyMaps(p["cross"]), func(f map[string]any) bool { return str(f["severity"]) == "critical" }):
		text = "The architect found a problem across the plan that it could not settle on its own. A technical helper can read it below. Continue anyway and say why, or ask it to try again."
	default:
		text = "Your plan is ready: the summary on the sheet says what will be built and what you will be able to do with it. Approve to create the work, or choose Try again to change it."
	}

	if len(stringsOf(p["blocking"])) > 0 || len(stringsOf(p["warnings"])) > 0 || len(stringsOf(p["steps"])) > 0 {
		text += plainStepsSentence
	}

	if len(stringsOf(p["notices"])) > 0 {
		text += plainNoticesSentence
	}

	return text + plainStopsSentence(len(anyMaps(p["stops"])))
}

const (
	plainStepsSentence   = " Some setup needs someone technical before the work can run; the steps are below."
	plainNoticesSentence = " The work will settle some technical setup on its own; a technical helper can read how below."
)

// plainStopsSentence counts the steps an unattended run waits for a person
// at; the technical text below says where and why. With none it is empty.
func plainStopsSentence(n int) string {
	switch {
	case n == 1:
		return " When the work runs, it will wait for you at 1 step only a person can take; where and why are below."
	case n > 1:
		return fmt.Sprintf(" When the work runs, it will wait for you at %d steps only a person can take; where and why are below.", n)
	default:
		return ""
	}
}

// plainEmittedText is what a person reads above the announcement of the
// emitted cards, which stays technical below it.
func plainEmittedText(p map[string]any) string {
	text := "The work is created in ContextMatrix."

	if len(stringsOf(p["manual"])) > 0 {
		text += " A few checks for a person to run once the work is built are listed below."
	}

	if len(stringsOf(p["steps"])) > 0 {
		text += plainStepsSentence
	}

	if len(stringsOf(p["notices"])) > 0 {
		text += plainNoticesSentence
	}

	return text
}

// plainSetupLead is what a person reads above the setup gaps the code
// checkout found when one needs a person, which stay technical for the
// helper who sets them up.
const plainSetupLead = "Someone technical needs to set up a few things in the projects before the work can run; the details for them follow."

// plainSetupSettledLead is the same note when no gap needs a person before
// the cards run.
const plainSetupSettledLead = "The work will settle a few things in the projects on its own; the details for a technical helper follow."

// setupNeedsAPerson is whether a gap in the setup note needs a person before
// the cards run: a repository with no commits always does, a missing verify
// command does unless the blueprint runs in yolo, where it is a notice, and
// a missing instruction file never does, a card writes it. An event without
// a mode reads as not yolo.
func setupNeedsAPerson(p map[string]any) bool {
	yolo := str(p["mode"]) == protocol.ArchitectModeYolo

	for _, g := range anyMaps(p["projects"]) {
		if empty, _ := g["empty"].(bool); empty {
			return true
		}

		if verify, _ := g["has_verify"].(bool); !verify && !yolo {
			return true
		}
	}

	return false
}

// plainAwaitText is what a person reads of a stopped emission or run start.
func plainAwaitText() string {
	return "Someone technical needs to finish the setup before the work can start; the steps are below. When it is done, reply approve."
}

// resumePrompt offers approve on a stopped emission or run start; nil on
// any other error or wait. The text is the stop without its reply
// instruction, empty on an event stored before resume events carried one.
// On a plain blueprint it is worded plainly, with the stop folded.
func resumePrompt(p map[string]any, plain bool) *protocol.LogPrompt {
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

	prompt := &protocol.LogPrompt{
		ID:   fmt.Sprintf("emission:v%.0f", num(p["version"])),
		Text: str(p["text"]),
		Kind: "gate",
	}

	if plain {
		label = "Try again"
		if str(p["resume"]) == "held" {
			label = "Done, continue"
		}

		prompt.Text, prompt.Detail = plainAwaitText(), str(p["text"])
	}

	prompt.Actions = []protocol.LogAction{{Label: label, Reply: "approve"}}

	return prompt
}

// setupGapsText renders what the checkout found the target projects lack for
// an agent run, and what happens to each gap.
func setupGapsText(p map[string]any) string {
	var b strings.Builder

	b.WriteString("**Setup.** The code checkout found what the target projects lack for an agent run. None of it blocks; the cards plan it as work:")

	for _, g := range anyMaps(p["projects"]) {
		name := str(g["project"])

		if verify, _ := g["has_verify"].(bool); !verify {
			fmt.Fprintf(&b, "\n- %s declares no verify command in ContextMatrix. Decomposition proposes one, and the sign-off names it while it is still unset; setting it before the cards run pins it.", name)
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

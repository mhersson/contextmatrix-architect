package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// signoffStage names the sign-off in the gates table. Its decisions judge
// the artifact the pipeline ended ready on, by version.
const signoffStage = "signoff"

const signoffHint = "Reply approve to emit the cards, or reject <feedback> to re-run the cards with your feedback."

// signoffCriticalHint is signoffHint over a critical finding, where approve
// takes a reason.
const signoffCriticalHint = "Reply approve <reason> to emit the cards over the critical findings, or reject <feedback> to re-run the cards with your feedback."

// emitsOnItsOwn reports whether a mode approves the sign-off itself.
func emitsOnItsOwn(mode string) bool {
	return mode == protocol.ArchitectModeAutoEmit || mode == protocol.ArchitectModeYolo
}

// rejection reads the sign-off's rejections against a loop stage's verdict
// at version: base is the version of the latest rejection before it, from
// which the loop's rounds count again, and rejected reports a rejection of
// version itself.
func (c *Controller) rejection(ctx context.Context, id string, version int) (int, bool, error) {
	gates, err := c.opts.Store.ListGates(ctx, id, signoffStage)
	if err != nil {
		return 0, false, fmt.Errorf("list gates: %w", err)
	}

	base, rejected := 0, false

	for _, g := range gates {
		if g.Decision != store.GateReject {
			continue
		}

		switch {
		case g.Version == version:
			rejected = true
		case g.Version < version && g.Version > base:
			base = g.Version
		}
	}

	return base, rejected, nil
}

// launchEnd runs the sign-off of a ready end on the run slot r, which the
// caller holds, and releases it.
func (c *Controller) launchEnd(ctx context.Context, r *run, sess *session, bp store.Blueprint, out outcome) {
	c.holdForSignoff(r)

	go func() {
		defer c.release(bp.ID, r)

		c.signoff(ctx, sess, bp, out)
	}()
}

// holdForSignoff marks the run slot r as held by the sign-off and the
// emission that follows it, so a note that comes meanwhile records the
// sign-off rather than the stage before it.
func (c *Controller) holdForSignoff(r *run) {
	c.mu.Lock()
	r.stage = signoffStage
	c.mu.Unlock()
}

// signoff acts on cards the pipeline ended ready with. Approved: emission,
// resumed when it stopped partway. Undecided in an emitting mode: preflight,
// then an automatic approval when nothing needs a human. Otherwise the
// sign-off is shown and the blueprint waits for approve or reject. An
// emitting mode never approves over a critical finding across the blueprint.
func (c *Controller) signoff(ctx context.Context, sess *session, bp store.Blueprint, out outcome) {
	bg := context.WithoutCancel(ctx)
	au := sess.audit

	decision, err := c.gateDecision(bg, bp.ID, signoffStage, out.art.Version)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, "sign-off: "+err.Error())

		return
	}

	switch decision {
	case store.GateApprove:
		c.emit(ctx, sess, bp)

		return
	case store.GateReject:
		// after turns a rejected verdict into the re-run.
		return
	}

	draft, err := c.draft(bg, bp.ID)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, "sign-off: "+err.Error())

		return
	}

	var (
		pf    preflightResult
		pfErr error
	)

	client, err := c.connectCM(ctx, sess)
	if err != nil {
		pfErr = err
	} else {
		pf, pfErr = c.preflight(ctx, sess, bp, draft, client)
		_ = client.Close()
	}

	auto := emitsOnItsOwn(bp.Mode)
	critical := criticalCross(crossOf(out.art))
	payload := c.signoffPayload(bg, au, bp, out, draft, pf, pfErr)

	// Steps do not stop it: creating the cards does not need them, and they
	// are shown again with the emitted cards. The approval goes first and
	// the record of what it approved after it, so a crash between the two
	// leaves an approved sign-off the restart emits, never an unapproved
	// record.
	if auto && !critical && ctx.Err() == nil && pfErr == nil && len(pf.Blocking) == 0 && len(pf.Warnings) == 0 {
		if !c.decide(bg, au, bp.ID, signoffStage, out.art.Version, store.GateApprove, "", store.ResponderAuto, "") {
			return
		}

		au.event(bg, Event{Kind: EventSignOffAuto, BlueprintID: bp.ID, Payload: payload})
		c.emit(ctx, sess, bp)

		return
	}

	// A sign-off Shutdown cut short is not shown: the restart runs it again.
	if c.closingDown() {
		return
	}

	payload["auto_stopped"] = auto
	payload["actions"] = actionsPayload(signoffActions(len(pf.Warnings) > 0, critical))

	// The typed replies are never buttons; the transcript lists them.
	if steps, _, err := c.steps(bg, bp.ID); err != nil {
		c.opts.Logger.Warn("blueprint: sign-off typed rejections", "blueprint", bp.ID, "error", au.text(err.Error()))
	} else {
		payload["typed"] = typedReplies(typedRejects(steps))
	}

	c.setState(bg, bp.ID, StateWaiting)
	au.event(bg, Event{Kind: EventSignOff, BlueprintID: bp.ID, Payload: payload})
}

// signoffPayload is what a sign-off carries: the cards in playbook order,
// the manual checks, the playbook title, what preflight found, readiness's
// findings across the blueprint, critical first, and the assumptions,
// riskiest first. Assumptions that cannot be read show as none, and the log
// says why; an intake from before assumptions were data gives its section's
// text, which the panel shows as it shows an older sign-off.
func (c *Controller) signoffPayload(ctx context.Context, au audit, bp store.Blueprint, out outcome, draft contract.Draft, pf preflightResult, pfErr error) map[string]any {
	var assumptions any = []any{}

	sum, err := c.summary(ctx, bp.ID)

	switch {
	case err != nil:
		c.opts.Logger.Warn("blueprint: sign-off assumptions", "blueprint", bp.ID, "error", au.text(err.Error()))
	case sum.prose != "":
		assumptions = sum.prose
	default:
		assumptions = assumptionsPayload(signoffOrder(sum.assumptions))
	}

	cards := make([]any, 0, len(draft.Cards))

	for _, card := range draft.Cards {
		entry := map[string]any{
			"id": card.ID, "project": card.Project, "title": card.Title, "type": card.Type,
			"priority": card.Priority, "depends_on": card.DependsOn,
		}

		if draft.Setup[card.Project].InstructionCard == card.ID {
			entry["writes"] = "AGENTS.md"
		}

		cards = append(cards, entry)
	}

	manual := make([]any, 0, len(draft.Manual))
	for _, m := range draft.Manual {
		manual = append(manual, map[string]any{"id": m.ID, "text": m.Text})
	}

	payload := map[string]any{
		"version": out.art.Version, "cards": cards, "order": draft.Order, "manual": manual, "title": draft.Title,
		"blocking": pf.Blocking, "warnings": pf.Warnings, "steps": pf.Steps, "assumptions": assumptions,
		"cross": crossPayload(crossOf(out.art)),
	}

	if pfErr != nil {
		payload["preflight_error"] = pfErr.Error()
	}

	return payload
}

// typedRejects are the sign-off's typed replies that reject one step and run
// it, and everything after it, again: reject slicing <feedback>, reject
// SL<n> <feedback> for slice n's design, its first gated step, and reject
// <shared stage> <feedback> for a shared stage that ran. They are matched as
// replies are but never offered as buttons, and come in pipeline order, so
// a reopen that finds two picks the earliest.
func typedRejects(steps []pipelineStep) []gateAction {
	var out []gateAction

	for _, s := range steps {
		switch {
		case slices.Contains(s.Checks, pack.CheckSlices), s.Shared && !s.Collapsed:
			out = append(out, gateAction{Reply: store.GateReject + " " + s.ID, Feedback: true, Decision: store.GateReject, Target: s.ID})
		case firstGated(steps, s):
			out = append(out, gateAction{Reply: store.GateReject + " " + s.Slice, Feedback: true, Decision: store.GateReject, Target: s.ID})
		}
	}

	return out
}

// typedReplies lists the replies of actions, for the sign-off's payload.
func typedReplies(actions []gateAction) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, a.Reply)
	}

	return out
}

// quotedReplies quotes each typed reply with its feedback placeholder.
func quotedReplies(typed []gateAction) []string {
	out := make([]string, 0, len(typed))
	for _, a := range typed {
		out = append(out, "`"+a.Reply+" <feedback>`")
	}

	return out
}

// stepWord matches the word a typed sign-off rejection names a slice by.
var stepWord = regexp.MustCompile(`(?i)^SL[0-9]+$`)

// NamesASlice reports a typed rejection whose step word names a slice.
func NamesASlice(reply string) bool {
	word, _ := typedWord(reply)

	return stepWord.MatchString(word)
}

// typedWord is the word after reject in text, cut at the first `:`, `,`, `;`
// or `.` a person may type after it, with a space before the feedback or
// not, and whether it was cut; "" when text is not reject followed by a
// word.
func typedWord(text string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) < 2 || !strings.EqualFold(fields[0], store.GateReject) {
		return "", false
	}

	i := strings.IndexAny(fields[1], ":,;.")
	if i < 0 {
		return fields[1], false
	}

	return fields[1][:i], true
}

// namesAStep reports a reply that is reject followed by a slice's, the
// slicing stage's or a shared stage's name, punctuation after it or not: a
// typed rejection, never read as a plain one.
func (c *Controller) namesAStep(text string) bool {
	word, _ := typedWord(text)
	if word == "" {
		return false
	}

	return stepWord.MatchString(word) || slices.ContainsFunc(c.opts.Pack.Stages, func(s pack.Stage) bool {
		return (s.Shared || slices.Contains(s.Checks, pack.CheckSlices)) && strings.EqualFold(s.ID, word)
	})
}

// typedNoteHint is the hint for a reply namesAStep reads as a typed
// rejection that no typed reply took: one of typed with punctuation after
// its step word or without feedback, or one naming no step of the
// blueprint. listed are the typed replies text may name.
func typedNoteHint(text string, typed, listed []gateAction) string {
	word, glued := typedWord(text)

	i := slices.IndexFunc(typed, func(a gateAction) bool { return strings.EqualFold(a.Reply, store.GateReject+" "+word) })

	switch {
	case i >= 0 && glued:
		return "Reply `" + store.GateReject + " " + word + " <feedback>`, with a space after " + word + ", to run that step and everything after it again."
	case i >= 0:
		return "Say what to change: reply " + typed[i].Reply + " <feedback>."
	default:
		return typedHint(listed)
	}
}

// typedHint is the hint for a typed rejection that names no step this
// blueprint can run again; typed are the replies text may name.
func typedHint(typed []gateAction) string {
	if len(typed) == 0 {
		return "Reply reject <feedback> to re-run the cards."
	}

	return "That names no step of this blueprint. Reply reject <feedback> to re-run the cards, or " +
		strings.Join(quotedReplies(typed), ", ") + " to run that step and everything after it again."
}

// signoffTarget is the step a typed sign-off rejection runs again: the first
// typed target whose latest version's latest decision is a rejection, which
// only that reply records while the sign-off waits; "" for a plain
// rejection. Each target stores a new version before the next sign-off, so
// one stands at a time; should two, the earliest in the pipeline wins, and
// its re-run runs the later one again too.
func (c *Controller) signoffTarget(ctx context.Context, id string, steps []pipelineStep) (string, error) {
	return c.rejectedTarget(ctx, id, typedRejects(steps))
}

// rejectStep rejects the sign-off for a typed target: the sign-off's
// rejection first, naming what runs again, then a rejection of the
// target's latest version, which gives its re-run the feedback and tells a
// reopen what runs, then the later gated steps' versions superseded. A
// crash between the two rejections leaves a plain one, which re-runs every
// slice's cards with the feedback. The target runs again, with everything
// after it. The caller holds the run slot.
func (c *Controller) rejectStep(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, out outcome, target, feedback, msgID string) error {
	au := sess.audit

	// Read before anything is recorded, so a failed read records nothing.
	art, err := c.opts.Store.LatestArtifact(ctx, bp.ID, target)
	if err != nil {
		c.release(bp.ID, r)

		return fmt.Errorf("blueprint: sign-off: read %s: %w", c.stepName(ctx, bp.ID, target), err)
	}

	if !c.decideWith(ctx, au, bp.ID, signoffStage, out.art.Version, store.GateReject, feedback, store.ResponderHuman, msgID, map[string]any{"reruns": target}) ||
		!c.decide(ctx, au, bp.ID, target, art.Version, store.GateReject, feedback, store.ResponderHuman, msgID) ||
		!c.supersedeAfter(ctx, au, bp.ID, target, msgID) {
		c.release(bp.ID, r)

		return nil
	}

	c.launch(runCtx, r, sess, bp, target)

	return nil
}

// atReadyEnd handles a message after the pipeline ended ready, matching it to
// the sign-off's replies as a stage's gate does. Undecided:
// approve emits, reject with feedback re-runs the loop's target stage, from
// its first slice's step. reject slicing <feedback>, reject SL<n> <feedback>
// and reject <shared stage> <feedback> run that step and everything after it
// again; one that names no step of the blueprint is a note with a hint.
// Approved with emission unfinished: approve resumes it, and other text is
// a note with that hint, or without one when the record cannot be read.
// Over a critical finding across the blueprint approve takes a reason, and a
// bare approve is a note asking for it. Anything else is a note. The caller
// holds the run slot.
func (c *Controller) atReadyEnd(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, out outcome, text, msgID string) error {
	decision, err := c.gateDecision(ctx, bp.ID, signoffStage, out.art.Version)
	if err != nil {
		c.release(bp.ID, r)

		return err
	}

	steps, pieces, err := c.steps(ctx, bp.ID)
	if err != nil {
		c.release(bp.ID, r)

		return err
	}

	typed := typedRejects(steps)

	// Below two slices, as sliced reads them, the typed replies still match,
	// but only those naming no slice are listed: nothing slice-related shows.
	listed := typed
	if len(shownPieces(steps, pieces)) < 2 {
		listed = slices.DeleteFunc(slices.Clone(typed), func(a gateAction) bool { return NamesASlice(a.Reply) })
	}

	// Only an undecided sign-off wants the reason: once approved, a bare
	// approve resumes a stopped emission or a held run.
	critical := decision == "" && criticalCross(crossOf(out.art))

	action, feedback, matched := matchAction(text, append(signoffActions(false, critical), typed...))
	approve := matched && action.Decision == store.GateApprove
	retarget := matched && action.Decision == store.GateReject && action.Target != ""
	reject := matched && action.Decision == store.GateReject && action.Target == ""
	note := noteEvent(bp.ID, text, msgID, signoffStage, false)

	// The end artifact is a step's: a pack whose last stage runs per slice
	// ends on a step key, which the pack's own lookup misses.
	end, _ := c.stepAt(out.art.Stage)

	switch decision {
	case "":
		switch {
		case approve:
			// Over a critical finding the reply's text is the reason, which
			// the decision and the sign-off's data keep.
			var extra map[string]any
			if critical {
				extra = map[string]any{"override_reason": feedback}
			}

			if c.decideWith(ctx, sess.audit, bp.ID, signoffStage, out.art.Version, store.GateApprove, feedback, store.ResponderHuman, msgID, extra) {
				c.launchEnd(runCtx, r, sess, bp, out)

				return nil
			}

			c.release(bp.ID, r)

			return nil
		case (reject || retarget) && end.LoopsTo == "":
			// A reopen reads a typed rejection's target from the loop stage's
			// verdict too, so without a loop neither kind can re-run.
			note.Payload["hint"] = "This pack's last stage loops to no stage, so a rejection cannot re-run it; start a new blueprint with a better design."
		case retarget:
			return c.rejectStep(ctx, runCtx, r, sess, bp, out, action.Target, feedback, msgID)
		case reject && c.namesAStep(text):
			note.Payload["hint"] = typedNoteHint(text, typed, listed)
		case reject:
			if c.decide(ctx, sess.audit, bp.ID, signoffStage, out.art.Version, store.GateReject, feedback, store.ResponderHuman, msgID) {
				c.launch(runCtx, r, sess, bp, loopTargets(steps, end.LoopsTo)[0].ID)

				return nil
			}

			c.release(bp.ID, r)

			return nil
		case rejectWithoutFeedback(text):
			note.Payload["hint"] = "Say what to change: reply reject <feedback>."
		case critical && strings.EqualFold(strings.TrimSpace(text), store.GateApprove):
			note.Payload["hint"] = "Readiness found a critical problem across the blueprint. Say why you approve over it: reply approve <reason>, or reject <feedback>."
		default:
			hint := signoffHint
			if critical {
				hint = signoffCriticalHint
			}

			note.Payload["hint"] = hint

			if len(listed) > 0 {
				note.Payload["hint"] = hint + " Or reply " + strings.Join(quotedReplies(listed), ", ") + " to run that step and everything after it again."
			}
		}
	case store.GateApprove:
		cards, complete, err := c.emitted(ctx, bp)

		switch {
		case err == nil && complete:
			// Nothing is left to resume.
		case approve:
			// An error resumes too: emit reports the draft or store error.
			c.launchEnd(runCtx, r, sess, bp, out)

			return nil
		case err != nil:
			// A plain note: without the record, a hint could point the wrong way.
			c.opts.Logger.Warn("blueprint: read the emission for a note", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))
		case cards:
			note.Payload["hint"] = "The cards are approved; reply approve to start the run."
		default:
			note.Payload["hint"] = "The cards are approved; reply approve to resume emission."
		}
	}

	sess.audit.event(ctx, note)
	c.release(bp.ID, r)

	return nil
}

// resumeEnd picks up a ready end on reopen. An undecided sign-off that was
// shown is shown again as it was, without preflight or an automatic
// approval, and one never shown runs. An approved emission still emitting
// was cut short by a restart and resumes; one a stop left idle or emitted
// waits for approve, and the user is reminded. An emitted blueprint's watch
// restarts, and a blueprint whose run ended stays done. The caller holds the
// run slot; unless a sign-off or emission resumes, it is free when this
// returns, so a message CM re-sends right after the start is the decision.
func (c *Controller) resumeEnd(ctx context.Context, r *run, sess *session, bp store.Blueprint, out outcome) {
	bg := context.WithoutCancel(ctx)
	au := sess.audit

	// Read again on the slot: Start loaded bp before it claimed the slot,
	// which the ended session's last run may have released after a stop.
	cur, err := c.opts.Store.GetBlueprint(bg, bp.ID)
	if err != nil {
		c.unreadable(bg, au, bp.ID, fmt.Errorf("blueprint: resume: %w", err))
		c.release(bp.ID, r)

		return
	}

	if cur.State == string(StateDone) {
		c.release(bp.ID, r)

		return
	}

	decision, err := c.gateDecision(bg, bp.ID, signoffStage, out.art.Version)
	if err != nil {
		c.unreadable(bg, au, bp.ID, err)
		c.release(bp.ID, r)

		return
	}

	switch decision {
	case "":
		shown, err := c.shownSignoff(bg, bp.ID, out.art.Version)
		if err != nil {
			c.unreadable(bg, au, bp.ID, err)
			c.release(bp.ID, r)

			return
		}

		if shown != nil {
			c.setState(bg, bp.ID, StateWaiting)
			au.event(bg, Event{Kind: EventSignOff, BlueprintID: bp.ID, Payload: shown})
			c.release(bp.ID, r)

			return
		}
	case store.GateApprove:
		cards, complete, err := c.emitted(bg, bp)

		// Still emitting means a restart cut emission short, maybe after its
		// last record: emit finishes the rest, the announcement and the state
		// included, creating and starting nothing twice.
		switch {
		case err != nil:
			// Resumed: emit reports the draft or store error.
		case complete && cur.State != string(StateEmitting):
			c.release(bp.ID, r)
			c.startWatch(sess, bp.ID)

			return
		case cur.State != string(StateEmitting):
			prompt, text, resume := "Emission is stopped; reply approve to resume it.", "Emission is stopped.", "emission"
			if cards {
				prompt, text, resume = "The run has not started; reply approve to start it.", "The run has not started.", "held"
			}

			au.event(bg, Event{Kind: EventAwaitInput, BlueprintID: bp.ID, Payload: map[string]any{
				"stage": signoffStage, "prompt": prompt, "text": text, "resume": resume, "version": out.art.Version,
			}})
			c.release(bp.ID, r)

			return
		}
	}

	c.launchEnd(ctx, r, sess, bp, out)
}

// shownSignoff returns the sign-off payload last published for version, or
// nil when none was: a crash or Shutdown fell between the ready end and it.
func (c *Controller) shownSignoff(ctx context.Context, id string, version int) (map[string]any, error) {
	ev, err := c.opts.Store.LatestEvent(ctx, id, EventSignOff)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("blueprint: read the sign-off: %w", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return nil, fmt.Errorf("blueprint: decode the sign-off: %w", err)
	}

	if v, _ := payload["version"].(float64); int(v) != version {
		return nil, nil
	}

	return payload, nil
}

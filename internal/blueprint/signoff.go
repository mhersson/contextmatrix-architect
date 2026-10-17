package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// signoffStage names the sign-off in the gates table. Its decisions judge
// the artifact the pipeline ended ready on, by version.
const signoffStage = "signoff"

const signoffHint = "Reply approve to emit the cards, or reject <feedback> to re-run decomposition with your feedback."

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
// sign-off is shown and the blueprint waits for approve or reject.
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

	// Steps do not stop it: creating the cards does not need them, and they
	// are shown again with the emitted cards.
	if auto && ctx.Err() == nil && pfErr == nil && len(pf.Blocking) == 0 && len(pf.Warnings) == 0 {
		if !c.decide(bg, au, bp.ID, signoffStage, out.art.Version, store.GateApprove, "", store.ResponderAuto, "") {
			return
		}

		c.emit(ctx, sess, bp)

		return
	}

	// A sign-off Shutdown cut short is not shown: the restart runs it again.
	if c.closingDown() {
		return
	}

	// Assumptions that cannot be read show as none, and the log says why. An
	// intake from before assumptions were data gives its section's text,
	// which the panel shows as it shows an older sign-off.
	var assumptions any = []any{}

	sum, err := c.summary(bg, bp.ID)

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
		"blocking": pf.Blocking, "warnings": pf.Warnings, "steps": pf.Steps, "auto_stopped": auto, "assumptions": assumptions,
		"actions": actionsPayload(signoffActions(len(pf.Warnings) > 0)),
	}

	if pfErr != nil {
		payload["preflight_error"] = pfErr.Error()
	}

	c.setState(bg, bp.ID, StateWaiting)
	au.event(bg, Event{Kind: EventSignOff, BlueprintID: bp.ID, Payload: payload})
}

// atReadyEnd handles a message after the pipeline ended ready, matching it to
// the sign-off's replies as a stage's gate does. Undecided:
// approve emits, reject with feedback re-runs the loop's target stage.
// Approved with emission unfinished: approve resumes it, and other text is
// a note with that hint, or without one when the record cannot be read.
// Anything else is a note. The caller holds the run slot.
func (c *Controller) atReadyEnd(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, out outcome, text, msgID string) error {
	decision, err := c.gateDecision(ctx, bp.ID, signoffStage, out.art.Version)
	if err != nil {
		c.release(bp.ID, r)

		return err
	}

	action, feedback, matched := matchAction(text, signoffActions(false))
	approve := matched && action.Decision == store.GateApprove
	reject := matched && action.Decision == store.GateReject
	note := noteEvent(bp.ID, text, msgID, signoffStage, false)

	switch decision {
	case "":
		switch {
		case approve:
			if c.decide(ctx, sess.audit, bp.ID, signoffStage, out.art.Version, store.GateApprove, "", store.ResponderHuman, msgID) {
				c.launchEnd(runCtx, r, sess, bp, out)

				return nil
			}

			c.release(bp.ID, r)

			return nil
		case reject:
			st, _ := c.opts.Pack.Stage(out.art.Stage)
			if st.LoopsTo == "" {
				note.Payload["hint"] = "This pack's last stage loops to no stage, so a rejection cannot re-run it; start a new blueprint with a better design."

				break
			}

			if c.decide(ctx, sess.audit, bp.ID, signoffStage, out.art.Version, store.GateReject, feedback, store.ResponderHuman, msgID) {
				c.launch(runCtx, r, sess, bp, st.LoopsTo)

				return nil
			}

			c.release(bp.ID, r)

			return nil
		case rejectWithoutFeedback(text):
			note.Payload["hint"] = "Say what to change: reply reject <feedback>."
		default:
			note.Payload["hint"] = signoffHint
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

package blueprint

import (
	"context"
	"fmt"
	"strings"
	"unicode"

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

// decision returns the sign-off decision on the artifact version: approve,
// reject, or "" when none was made.
func (c *Controller) decision(ctx context.Context, id string, version int) (string, error) {
	gates, err := c.opts.Store.ListGates(ctx, id, signoffStage)
	if err != nil {
		return "", fmt.Errorf("blueprint: list gates: %w", err)
	}

	d := ""

	for _, g := range gates {
		if g.Version == version {
			d = g.Decision
		}
	}

	return d, nil
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

// rejectFeedback is every rejection's feedback, oldest first.
func (c *Controller) rejectFeedback(ctx context.Context, id string) ([]string, error) {
	gates, err := c.opts.Store.ListGates(ctx, id, signoffStage)
	if err != nil {
		return nil, fmt.Errorf("list gates: %w", err)
	}

	var out []string

	for _, g := range gates {
		if g.Decision == store.GateReject {
			out = append(out, g.Feedback)
		}
	}

	return out, nil
}

// launchEnd runs the sign-off of a ready end on the run slot r, which the
// caller holds, and releases it.
func (c *Controller) launchEnd(ctx context.Context, r *run, sess *session, bp store.Blueprint, out outcome) {
	go func() {
		defer c.release(bp.ID, r)

		c.signoff(ctx, sess, bp, out)
	}()
}

// signoff acts on cards the pipeline ended ready with. Approved: emission,
// resumed when it stopped partway. Undecided in an emitting mode: preflight,
// then an automatic approval when nothing needs a human. Otherwise the
// sign-off is shown and the blueprint waits for approve or reject.
func (c *Controller) signoff(ctx context.Context, sess *session, bp store.Blueprint, out outcome) {
	bg := context.WithoutCancel(ctx)
	au := sess.audit

	decision, err := c.decision(bg, bp.ID, out.art.Version)
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

	if auto && ctx.Err() == nil && pfErr == nil && len(pf.Blocking) == 0 && len(pf.Warnings) == 0 {
		if !c.decide(bg, au, bp.ID, out.art.Version, store.GateApprove, "", store.ResponderAuto, "") {
			return
		}

		c.emit(ctx, sess, bp)

		return
	}

	// A need that cannot be read shows as no assumptions, and the log says why.
	assumptions := ""

	if sum, err := c.summary(bg, bp.ID); err != nil {
		c.opts.Logger.Warn("blueprint: sign-off assumptions", "blueprint", bp.ID, "error", au.text(err.Error()))
	} else {
		assumptions = sum.assumptions
	}

	cards := make([]any, 0, len(draft.Cards))

	for _, card := range draft.Cards {
		cards = append(cards, map[string]any{
			"id": card.ID, "project": card.Project, "title": card.Title, "type": card.Type,
			"priority": card.Priority, "depends_on": card.DependsOn,
		})
	}

	manual := make([]any, 0, len(draft.Manual))
	for _, m := range draft.Manual {
		manual = append(manual, map[string]any{"id": m.ID, "text": m.Text})
	}

	payload := map[string]any{
		"version": out.art.Version, "cards": cards, "order": draft.Order, "manual": manual, "title": draft.Title,
		"blocking": pf.Blocking, "warnings": pf.Warnings, "auto_stopped": auto, "assumptions": assumptions,
	}

	if pfErr != nil {
		payload["preflight_error"] = pfErr.Error()
	}

	c.setState(bg, bp.ID, StateWaiting)
	au.event(bg, Event{Kind: EventSignOff, BlueprintID: bp.ID, Payload: payload})
}

// decide records a sign-off decision and announces it; false when it could
// not be stored, which fails the blueprint.
func (c *Controller) decide(ctx context.Context, au audit, id string, version int, decision, feedback, responder, msgID string) bool {
	if _, err := c.opts.Store.AddGate(ctx, store.Gate{
		BlueprintID: id, Stage: signoffStage, Version: version, Decision: decision,
		Feedback: au.text(feedback), Responder: responder, DecidedAt: c.opts.Now().UTC(),
	}); err != nil {
		c.fail(ctx, au, id, 0, "sign-off: record the decision: "+err.Error())

		return false
	}

	au.event(ctx, Event{Kind: EventDecided, BlueprintID: id, Payload: map[string]any{
		"decision": decision, "feedback": feedback, "responder": responder, "version": version, "message_id": msgID,
	}})

	return true
}

// atReadyEnd handles a message after the pipeline ended ready. Undecided:
// approve emits, reject with feedback re-runs the loop's target stage.
// Approved with emission unfinished: approve resumes it. Anything else is a
// note. The caller holds the run slot.
func (c *Controller) atReadyEnd(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, out outcome, text, msgID string) error {
	decision, err := c.decision(ctx, bp.ID, out.art.Version)
	if err != nil {
		c.release(bp.ID, r)

		return err
	}

	approve := strings.EqualFold(strings.TrimSpace(text), "approve")
	feedback, reject := parseReject(text)
	note := noteEvent(bp.ID, text, msgID, false)

	switch decision {
	case "":
		switch {
		case approve:
			if c.decide(ctx, sess.audit, bp.ID, out.art.Version, store.GateApprove, "", store.ResponderHuman, msgID) {
				c.launchEnd(runCtx, r, sess, bp, out)

				return nil
			}

			c.release(bp.ID, r)

			return nil
		case reject && feedback != "":
			st, _ := c.opts.Pack.Stage(out.art.Stage)
			if st.LoopsTo == "" {
				note.Payload["hint"] = "This pack's last stage loops to no stage, so a rejection cannot re-run it; start a new blueprint with a better design."

				break
			}

			if c.decide(ctx, sess.audit, bp.ID, out.art.Version, store.GateReject, feedback, store.ResponderHuman, msgID) {
				c.launch(runCtx, r, sess, bp, st.LoopsTo)

				return nil
			}

			c.release(bp.ID, r)

			return nil
		case reject:
			note.Payload["hint"] = "Say what to change: reply reject <feedback>."
		default:
			note.Payload["hint"] = signoffHint
		}
	case store.GateApprove:
		if approve {
			// An error resumes too: emit reports the draft or store error.
			complete, err := c.emitted(ctx, bp)
			if err != nil || !complete {
				c.launchEnd(runCtx, r, sess, bp, out)

				return nil
			}
		}
	}

	sess.audit.event(ctx, note)
	c.release(bp.ID, r)

	return nil
}

// parseReject reads "reject <feedback>": ok when the first word, split at any
// whitespace, is reject; feedback is the rest, trimmed.
func parseReject(text string) (string, bool) {
	s := strings.TrimSpace(text)

	word, rest := s, ""
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		word, rest = s[:i], s[i:]
	}

	if !strings.EqualFold(word, "reject") {
		return "", false
	}

	return strings.TrimSpace(rest), true
}

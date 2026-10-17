package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// gateAction is one reply a gate takes. Label and Reply are what the panel
// offers, and a Feedback action needs text after its reply. Decision is what
// the reply records; a stage gate's reject runs Target again, and the
// sign-off's reject leaves the stage to re-run to the pack's loop.
type gateAction struct {
	Label    string
	Reply    string
	Feedback bool
	Decision string
	Target   string
}

// stageGateActions are the replies a stage's gate takes: approve, and
// reject with feedback, which runs the stage again.
func stageGateActions(stageID string) []gateAction {
	return []gateAction{
		{Label: "Approve", Reply: store.GateApprove, Decision: store.GateApprove},
		{Label: "Reject", Reply: store.GateReject, Feedback: true, Decision: store.GateReject, Target: stageID},
	}
}

// signoffActions are the replies the sign-off takes: approve, which also
// overrides the warnings it was shown with, and reject with feedback, which
// re-runs the loop's target stage.
func signoffActions(warnings bool) []gateAction {
	approve := "Approve"
	if warnings {
		approve = "Approve and override warnings"
	}

	return []gateAction{
		{Label: approve, Reply: store.GateApprove, Decision: store.GateApprove},
		{Label: "Reject", Reply: store.GateReject, Feedback: true, Decision: store.GateReject},
	}
}

// matchAction finds the action a reply takes, the way CM's panel matches a
// reply to a prompt: case is ignored, a feedback action needs whitespace and
// non-empty text after its reply, and when several actions match the
// longest reply wins. It returns the action and its feedback.
func matchAction(text string, actions []gateAction) (gateAction, string, bool) {
	s := strings.TrimSpace(text)

	var (
		best     gateAction
		feedback string
		found    bool
	)

	for _, a := range actions {
		fb, ok := replyMatches(s, a)
		if ok && (!found || len(a.Reply) > len(best.Reply)) {
			best, feedback, found = a, fb, true
		}
	}

	return best, feedback, found
}

// replyMatches reports whether s, trimmed, takes action a, and the feedback
// after its reply.
func replyMatches(s string, a gateAction) (string, bool) {
	if !a.Feedback {
		return "", strings.EqualFold(s, a.Reply)
	}

	if len(s) <= len(a.Reply) || !strings.EqualFold(s[:len(a.Reply)], a.Reply) {
		return "", false
	}

	rest := s[len(a.Reply):]
	if r, _ := utf8.DecodeRuneInString(rest); !unicode.IsSpace(r) {
		return "", false
	}

	feedback := strings.TrimSpace(rest)

	return feedback, feedback != ""
}

// rejectWithoutFeedback reports a reply that is reject and nothing more.
func rejectWithoutFeedback(text string) bool {
	return strings.EqualFold(strings.TrimSpace(text), store.GateReject)
}

// actionsPayload carries the actions in an event, for the panel's prompt.
func actionsPayload(actions []gateAction) []any {
	out := make([]any, 0, len(actions))

	for _, a := range actions {
		out = append(out, map[string]any{"label": a.Label, "reply": a.Reply, "feedback": a.Feedback})
	}

	return out
}

// gateDecision is the latest decision on stageID's artifact version:
// approve, reject, or "" when none was made.
func (c *Controller) gateDecision(ctx context.Context, id, stageID string, version int) (string, error) {
	gates, err := c.opts.Store.ListGates(ctx, id, stageID)
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

// gateFeedback is the feedback of every rejection at stageID's gate, oldest
// first.
func (c *Controller) gateFeedback(ctx context.Context, id, stageID string) ([]string, error) {
	gates, err := c.opts.Store.ListGates(ctx, id, stageID)
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

// decide records a decision at stageID's gate and announces it; false when
// it could not be stored, which fails the blueprint. An approved sign-off
// makes the blueprint emitting, so a crash before emission ends leaves it
// for the restart.
func (c *Controller) decide(ctx context.Context, au audit, id, stageID string, version int, decision, feedback, responder, msgID string) bool {
	if _, err := c.opts.Store.AddGate(ctx, store.Gate{
		BlueprintID: id, Stage: stageID, Version: version, Decision: decision,
		Feedback: au.text(feedback), Responder: responder, DecidedAt: c.opts.Now().UTC(),
	}); err != nil {
		c.fail(ctx, au, id, 0, fmt.Sprintf("gate %s: record the decision: %v", stageID, err))

		return false
	}

	if stageID == signoffStage && decision == store.GateApprove {
		c.setState(ctx, id, StateEmitting)
	}

	au.event(ctx, Event{Kind: EventDecided, BlueprintID: id, Payload: map[string]any{
		"stage": stageID, "decision": decision, "feedback": feedback, "responder": responder, "version": version, "message_id": msgID,
	}})

	return true
}

// openGate acts on a gate whose artifact has its verdict: under the auto
// policy a passing score approves and what follows the stage is returned as
// advance returns it; otherwise, a human policy or a score still below the
// threshold after the attempt's rounds, the gate is shown and the chain
// stops.
func (c *Controller) openGate(ctx context.Context, sess *session, bp store.Blueprint, out outcome) (string, outcome) {
	st, _ := c.opts.Pack.Stage(out.stage)

	policy, err := c.policy(ctx, bp, out.stage)
	if err != nil {
		c.fail(ctx, sess.audit, bp.ID, 0, err.Error())

		return "", outcome{}
	}

	v, found, err := c.latestReview(ctx, bp.ID, out.stage, out.art.Version)
	if err != nil {
		c.fail(ctx, sess.audit, bp.ID, 0, err.Error())

		return "", outcome{}
	}

	if policy == pack.PolicyAuto && found && st.Reviewer != nil && v.Score >= st.Reviewer.Threshold {
		if !c.decide(ctx, sess.audit, bp.ID, out.stage, out.art.Version, store.GateApprove, "", store.ResponderAuto, "") {
			return "", outcome{}
		}

		return c.advance(ctx, sess, bp, out.stage)
	}

	c.showGate(ctx, sess.audit, bp, out)

	return "", outcome{}
}

// showGate shows a stage's gate with its reviewer's verdict and leaves the
// blueprint waiting for the decision. The replies it takes travel with it,
// for the panel's prompt.
func (c *Controller) showGate(ctx context.Context, au audit, bp store.Blueprint, out outcome) {
	payload := map[string]any{
		"stage": out.stage, "version": out.art.Version, "actions": actionsPayload(stageGateActions(out.stage)),
	}

	st, _ := c.opts.Pack.Stage(out.stage)

	v, found, err := c.latestReview(ctx, bp.ID, out.stage, out.art.Version)

	switch {
	case err != nil:
		c.opts.Logger.Warn("blueprint: gate shown without its verdict", "blueprint", bp.ID, "stage", out.stage, "error", au.text(err.Error()))
	case found && st.Reviewer != nil:
		findings := v.Findings
		if findings == nil {
			findings = []string{}
		}

		payload["review"] = map[string]any{
			"score": v.Score, "threshold": st.Reviewer.Threshold, "findings": findings, "round": v.Round,
			"passed": v.Score >= st.Reviewer.Threshold,
		}
	}

	c.setState(ctx, bp.ID, StateWaiting)
	au.event(ctx, Event{Kind: EventGate, BlueprintID: bp.ID, Payload: payload})
}

// gateShown reports whether stageID's gate for version was shown. A shown
// gate stays with the human it was put to, whatever the policy says now.
func (c *Controller) gateShown(ctx context.Context, id, stageID string, version int) (bool, error) {
	events, err := c.opts.Store.ListEventsOfKind(ctx, id, EventGate)
	if err != nil {
		return false, fmt.Errorf("blueprint: read the gates shown: %w", err)
	}

	for _, e := range events {
		var p struct {
			Stage   string `json:"stage"`
			Version int    `json:"version"`
		}

		// The controller wrote the payload itself, so a decode failure is a
		// fault: read as never shown, the gate would leave its human.
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return false, fmt.Errorf("blueprint: read gate event %d: %w", e.ID, err)
		}

		if p.Stage == stageID && p.Version == version {
			return true, nil
		}
	}

	return false, nil
}

// resumeGate picks up an undecided gate on reopen: one that was shown is
// shown again, and one never shown is opened under the policy now in
// effect. A gate never shown whose artifact still needs its critique runs it
// first; a shown gate stays with its human even without a verdict. The
// caller holds the run slot.
func (c *Controller) resumeGate(ctx context.Context, r *run, sess *session, bp store.Blueprint, out outcome) {
	bg := context.WithoutCancel(ctx)

	shown, err := c.gateShown(bg, bp.ID, out.stage, out.art.Version)
	if err != nil {
		c.unreadable(bg, sess.audit, bp.ID, err)
		c.release(bp.ID, r)

		return
	}

	if shown {
		c.showGate(bg, sess.audit, bp, out)
		c.release(bp.ID, r)

		return
	}

	if out.critique {
		c.launchCritique(ctx, r, sess, bp, out.stage)

		return
	}

	next, end := c.openGate(bg, sess, bp, out)
	c.continueWith(ctx, r, sess, bp, next, end)
}

// atGate handles a message while a stage's gate is open: approve records
// the approval and the pipeline goes on, reject <feedback> records the
// rejection and runs the stage again, and any other text is a note with a
// hint. The caller holds the run slot.
func (c *Controller) atGate(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, out outcome, text, msgID string) error {
	action, feedback, ok := matchAction(text, stageGateActions(out.stage))
	if !ok {
		note := noteEvent(bp.ID, text, msgID, out.stage, false)
		note.Payload["hint"] = fmt.Sprintf("Reply approve to go on, or reject <feedback> to run %s again with your feedback.", out.stage)

		if rejectWithoutFeedback(text) {
			note.Payload["hint"] = "Say what to change: reply reject <feedback>."
		}

		sess.audit.event(ctx, note)
		c.release(bp.ID, r)

		return nil
	}

	if !c.decide(ctx, sess.audit, bp.ID, out.stage, out.art.Version, action.Decision, feedback, store.ResponderHuman, msgID) {
		c.release(bp.ID, r)

		return nil
	}

	if action.Decision == store.GateReject {
		c.launch(runCtx, r, sess, bp, action.Target)

		return nil
	}

	c.proceed(ctx, runCtx, r, sess, bp, out.stage)

	return nil
}

// proceed runs or awaits what follows completed on the run slot r, which
// the caller holds.
func (c *Controller) proceed(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, completed string) {
	next, end := c.advance(ctx, sess, bp, completed)
	c.continueWith(runCtx, r, sess, bp, next, end)
}

// continueWith hands the run slot r to what advance decided: the sign-off
// of a ready end, the critique of a gated stage, the next stage, or nothing,
// which releases it.
func (c *Controller) continueWith(ctx context.Context, r *run, sess *session, bp store.Blueprint, next string, end outcome) {
	switch {
	case end.ready:
		c.launchEnd(ctx, r, sess, bp, end)
	case end.critique:
		c.launchCritique(ctx, r, sess, bp, next)
	case next != "":
		c.launch(ctx, r, sess, bp, next)
	default:
		c.release(bp.ID, r)
	}
}

package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
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

// GatePromptID is the prompt id of key's gate for version: gate:<stage>:v<n>,
// or gate:<stage>:<slice>:v<n> for a per-slice step. The view's open prompt
// and the transcript's gate prompt carry it; a consumer matches the two by
// equality and never parses it.
func GatePromptID(key string, version int) string {
	stageID, slice := pack.SplitStep(key)
	if slice == "" {
		return fmt.Sprintf("gate:%s:v%d", stageID, version)
	}

	return fmt.Sprintf("gate:%s:%s:v%d", stageID, slice, version)
}

// gateDecision is the latest decision on stageID's artifact version:
// approve, reject, fold, or "" when none was made. A superseded row is
// audit only and never read as one.
func (c *Controller) gateDecision(ctx context.Context, id, stageID string, version int) (string, error) {
	gates, err := c.opts.Store.ListGates(ctx, id, stageID)
	if err != nil {
		return "", fmt.Errorf("blueprint: list gates: %w", err)
	}

	d := ""

	for _, g := range gates {
		if g.Version == version && g.Decision != store.GateSuperseded {
			d = g.Decision
		}
	}

	return d, nil
}

// supersededAt reports whether a gates row records stageID's version
// superseded, as the view reads it.
func (c *Controller) supersededAt(ctx context.Context, id, stageID string, version int) (bool, error) {
	gates, err := c.opts.Store.ListGates(ctx, id, stageID)
	if err != nil {
		return false, fmt.Errorf("blueprint: list gates: %w", err)
	}

	return slices.ContainsFunc(gates, func(g store.Gate) bool {
		return g.Version == version && g.Decision == store.GateSuperseded
	}), nil
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
	return c.decideWith(ctx, au, id, stageID, version, decision, feedback, responder, msgID, nil)
}

// decideWith is decide with extra keys on the announcement.
func (c *Controller) decideWith(ctx context.Context, au audit, id, stageID string, version int, decision, feedback, responder, msgID string, extra map[string]any) bool {
	if _, err := c.opts.Store.AddGate(ctx, store.Gate{
		BlueprintID: id, Stage: stageID, Version: version, Decision: decision,
		Feedback: au.text(feedback), Responder: responder, DecidedAt: c.opts.Now().UTC(),
	}); err != nil {
		c.fail(ctx, au, id, 0, fmt.Sprintf("gate %s: record the decision: %v", c.stepName(ctx, id, stageID), err))

		return false
	}

	if stageID == signoffStage && decision == store.GateApprove {
		c.setState(ctx, id, StateEmitting)
	}

	payload := map[string]any{
		"stage": stageID, "decision": decision, "feedback": feedback, "responder": responder, "version": version, "message_id": msgID,
	}
	maps.Copy(payload, extra)

	au.event(ctx, Event{Kind: EventDecided, BlueprintID: id, Payload: payload})

	return true
}

// openGate acts on a gate whose artifact has its verdict: when the stage and
// every stage folded into its gate are under the auto policy now, a passing
// verdict approves them all and what follows the stage is returned as
// advance returns it; otherwise, a human policy on any of them or a verdict
// that still does not pass after the attempt's rounds, the gate is shown and
// the chain stops.
func (c *Controller) openGate(ctx context.Context, sess *session, bp store.Blueprint, out outcome) (string, outcome) {
	st, _ := c.stepAt(out.stage)

	v, found, err := c.gateReview(ctx, bp.ID, st, out.art)
	if err != nil {
		c.fail(ctx, sess.audit, bp.ID, 0, err.Error())

		return "", outcome{}
	}

	folded, err := c.folded(ctx, bp.ID, out.stage)
	if err != nil {
		c.fail(ctx, sess.audit, bp.ID, 0, err.Error())

		return "", outcome{}
	}

	auto, err := c.allAuto(ctx, bp, out.stage, folded)
	if err != nil {
		c.fail(ctx, sess.audit, bp.ID, 0, err.Error())

		return "", outcome{}
	}

	if auto && found && st.Reviewer != nil && v.passes(st.Reviewer.Threshold) {
		for _, f := range folded {
			if !c.decide(ctx, sess.audit, bp.ID, f.Stage, f.Version, store.GateApprove, "", store.ResponderAuto, "") {
				return "", outcome{}
			}
		}

		if !c.decide(ctx, sess.audit, bp.ID, out.stage, out.art.Version, store.GateApprove, "", store.ResponderAuto, "") {
			return "", outcome{}
		}

		return c.advance(ctx, sess, bp, out.stage)
	}

	c.showGate(ctx, sess.audit, bp, out)

	return "", outcome{}
}

// allAuto reports whether stageID and every stage folded into its gate are
// under the auto gate policy now: a folded gate stays a human's while any
// stage it judges has a human gate policy.
func (c *Controller) allAuto(ctx context.Context, bp store.Blueprint, stageID string, folded []store.Artifact) (bool, error) {
	stages := []string{stageID}
	for _, f := range folded {
		stages = append(stages, f.Stage)
	}

	for _, id := range stages {
		policy, err := c.gatePolicy(ctx, bp, id)
		if err != nil {
			return false, err
		}

		if policy != pack.PolicyAuto {
			return false, nil
		}
	}

	return true, nil
}

// showGate shows a stage's gate with its reviewer's verdict, the version
// that verdict judged, and leaves the blueprint waiting for the decision; a
// folded gate carries the folded artifacts, which the panel has not shown
// near it. The replies it takes travel with it, for the panel's prompt; when
// they cannot be read, the gate is not shown and the blueprint goes idle with
// the error reported.
func (c *Controller) showGate(ctx context.Context, au audit, bp store.Blueprint, out outcome) {
	actions, folded, err := c.gateActions(ctx, bp.ID, out.stage)
	if err != nil {
		c.opts.Logger.Error("blueprint: read the gate's replies", "blueprint", bp.ID, "stage", out.stage, "error", au.text(err.Error()))
		c.setState(ctx, bp.ID, StateIdle)
		au.event(ctx, Event{Kind: EventError, BlueprintID: bp.ID, Payload: map[string]any{
			"stage": out.stage, "message": fmt.Sprintf("The replies %s's gate takes cannot be read from the record, so the gate is not shown: %v", c.stepName(ctx, bp.ID, out.stage), err),
		}})

		return
	}

	foldedPayload := make([]any, 0, len(folded))

	for _, f := range folded {
		entry := map[string]any{"stage": f.Stage, "version": f.Version, "body": f.Body}
		if problems := problemsOf(f); len(problems) > 0 {
			entry["problems"] = problems
		}

		foldedPayload = append(foldedPayload, entry)
	}

	payload := map[string]any{
		"stage": out.stage, "version": out.art.Version, "actions": actionsPayload(actions), "folded": foldedPayload,
	}

	if problems := problemsOf(out.art); len(problems) > 0 {
		payload["problems"] = problems
	}

	st, _ := c.stepAt(out.stage)

	v, found, err := c.gateReview(ctx, bp.ID, st, out.art)

	switch {
	case err != nil:
		c.opts.Logger.Warn("blueprint: gate shown without its verdict", "blueprint", bp.ID, "stage", out.stage, "error", au.text(err.Error()))
	case found && st.Reviewer != nil:
		threshold := st.Reviewer.Threshold

		payload["review"] = map[string]any{
			"score": v.Score, "threshold": threshold, "round": v.Round, "passed": v.passes(threshold),
			"judged":   v.Version,
			"findings": v.Findings.payload(addressedClaims(out.art)), "open_critical": v.openCritical(),
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
// hint. Approving a gate approves every stage folded into it; reject <stage>
// <feedback> rejects a folded stage, which runs again with everything after
// it. At a slice's design gate, reject slicing <feedback> and reject <shared
// stage> <feedback> reject that earlier step, which runs again with every
// slice, and the versions they replace are recorded superseded. The caller
// holds the run slot.
func (c *Controller) atGate(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, out outcome, text, msgID string) error {
	actions, folded, err := c.gateActions(ctx, bp.ID, out.stage)
	if err != nil {
		c.release(bp.ID, r)

		return err
	}

	action, feedback, ok := matchAction(text, actions)
	if !ok {
		hint := fmt.Sprintf("Reply approve to go on, or reject <feedback> to run %s again with your feedback.", c.stepName(ctx, bp.ID, out.stage))

		for _, a := range actions {
			if a.Target != "" && a.Target != out.stage {
				hint += fmt.Sprintf(" Reply %s <feedback> to run %s and everything after it again.", a.Reply, c.stepName(ctx, bp.ID, a.Target))
			}
		}

		note := noteEvent(bp.ID, text, msgID, out.stage, false)
		note.Payload["hint"] = hint

		if rejectWithoutFeedback(text) {
			note.Payload["hint"] = "Say what to change: reply reject <feedback>."
		}

		sess.audit.event(ctx, note)
		c.release(bp.ID, r)

		return nil
	}

	if action.Decision == store.GateReject {
		// The rejection lands on the step that runs again: the gate's own, a
		// stage folded into it, or an earlier step the gate offers to reject
		// (slicing, a shared stage). A shared stage's own gate rejects it as
		// any gate rejects its own stage.
		version, earlier := out.art.Version, false
		folds := slices.ContainsFunc(folded, func(f store.Artifact) bool { return f.Stage == action.Target })

		for _, f := range folded {
			if f.Stage == action.Target {
				version = f.Version
			}
		}

		if action.Target != out.stage && !folds {
			art, err := c.opts.Store.LatestArtifact(ctx, bp.ID, action.Target)
			if err != nil {
				c.release(bp.ID, r)

				return fmt.Errorf("blueprint: gate: read %s: %w", c.stepName(ctx, bp.ID, action.Target), err)
			}

			version, earlier = art.Version, true
		}

		if !c.decide(ctx, sess.audit, bp.ID, action.Target, version, store.GateReject, feedback, store.ResponderHuman, msgID) {
			c.release(bp.ID, r)

			return nil
		}

		// Recorded after the reject, which alone drives the re-run, so a
		// crash between them loses only audit rows.
		recorded := true

		switch {
		case earlier:
			recorded = c.supersedeAfter(ctx, sess.audit, bp.ID, action.Target, msgID)
		case action.Target != out.stage:
			recorded = c.decide(ctx, sess.audit, bp.ID, out.stage, out.art.Version, store.GateSuperseded, action.Target, store.ResponderHuman, msgID)
		}

		if !recorded {
			c.release(bp.ID, r)

			return nil
		}

		c.launch(runCtx, r, sess, bp, action.Target)

		return nil
	}

	for _, f := range folded {
		if !c.decide(ctx, sess.audit, bp.ID, f.Stage, f.Version, store.GateApprove, "", store.ResponderHuman, msgID) {
			c.release(bp.ID, r)

			return nil
		}
	}

	if !c.decide(ctx, sess.audit, bp.ID, out.stage, out.art.Version, store.GateApprove, "", store.ResponderHuman, msgID) {
		c.release(bp.ID, r)

		return nil
	}

	c.proceed(ctx, runCtx, r, sess, bp, out.stage)

	return nil
}

// supersedeAfter records superseded, naming target, on the latest version
// of every gated step after target whose latest decision is no rejection
// and that no row records superseded yet: the versions a re-run from target
// replaces, each recorded once. Audit rows only, so the view shows them
// replaced before they are; false when one could not be stored, which fails
// the blueprint.
func (c *Controller) supersedeAfter(ctx context.Context, au audit, id, target, msgID string) bool {
	failed := func(err error) bool {
		c.fail(ctx, au, id, 0, fmt.Sprintf("record what %s replaces: %v", c.stepName(ctx, id, target), err))

		return false
	}

	steps, _, err := c.steps(ctx, id)
	if err != nil {
		return failed(err)
	}

	i := stepIndex(steps, target)
	if i < 0 {
		return true
	}

	for _, s := range steps[i+1:] {
		if !s.Gate {
			continue
		}

		art, err := c.opts.Store.LatestArtifact(ctx, id, s.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return failed(fmt.Errorf("read %s: %w", c.stepName(ctx, id, s.ID), err))
		}

		d, err := c.gateDecision(ctx, id, s.ID, art.Version)
		if err != nil {
			return failed(err)
		}

		marked, err := c.supersededAt(ctx, id, s.ID, art.Version)
		if err != nil {
			return failed(err)
		}

		if d == store.GateReject || marked {
			continue
		}

		if !c.decide(ctx, au, id, s.ID, art.Version, store.GateSuperseded, target, store.ResponderHuman, msgID) {
			return false
		}
	}

	return true
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

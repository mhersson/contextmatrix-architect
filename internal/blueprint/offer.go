package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// offerPromptID is the open prompt, and the transcript's gate prompt, of the
// offer; a reopen posts the offer again under the same id.
const offerPromptID = "mockup:offer"

// The offer's states, as its events record them.
const (
	offerShown    = "shown"
	offerAccepted = "accepted"
	offerDeclined = "declined"
)

// offerByMode marks an offer a mode switch took.
const offerByMode = "mode"

// The hints of a note at the offer, and of one that runs the mockup step
// again while no revision was shown since the offer was accepted.
const (
	offerHint     = "Reply show mockup for a first look, or skip mockup to go on without one."
	firstLookHint = "Reply skip mockup to go on without a mockup."
)

// offerActions are the replies the offer takes: show mockup, recommended,
// runs the mockup step; skip mockup goes on without it.
func offerActions() []gateAction {
	return []gateAction{
		{Label: "Show mockup", Reply: "show mockup", Decision: offerAccepted, Recommended: true},
		{Label: "Skip mockup", Reply: "skip mockup", Decision: offerDeclined},
	}
}

// offerState is the state of blueprint id's newest offer; "" when none was
// posted.
func (c *Controller) offerState(ctx context.Context, id string) (string, error) {
	e, err := c.opts.Store.LatestEvent(ctx, id, EventMockupOffer)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("blueprint: read the mockup offer: %w", err)
	}

	var p struct {
		State string `json:"state"`
	}

	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return "", fmt.Errorf("blueprint: read offer event %d: %w", e.ID, err)
	}

	return p.State, nil
}

// offerEvent is an offer event in state.
func (c *Controller) offerEvent(id, state string, extra map[string]any) Event {
	payload := map[string]any{"stage": c.opts.Pack.MockupStage(), "state": state}
	maps.Copy(payload, extra)

	return Event{Kind: EventMockupOffer, BlueprintID: id, Payload: payload}
}

// mockupGo reports whether the mockup step runs now: the newest offer was
// accepted, or none was shown and the policy is auto. Otherwise it posts the
// offer, again on a reopen, and the caller leaves the blueprint waiting with
// the slot free. A shown offer stays with its person whatever the policy says
// now, as a shown gate does; only a mode switch's take decides it unasked.
func (c *Controller) mockupGo(ctx context.Context, sess *session, bp store.Blueprint) (bool, error) {
	state, err := c.offerState(ctx, bp.ID)
	if err != nil {
		return false, err
	}

	switch state {
	case offerAccepted:
		return true, nil
	case offerShown:
		// Posted again: it stays with its person.
	default:
		policy, err := c.policy(ctx, bp, c.opts.Pack.MockupStage())
		if err != nil {
			return false, err
		}

		if policy == pack.PolicyAuto {
			return true, nil
		}
	}

	sess.audit.event(ctx, c.offerEvent(bp.ID, offerShown, map[string]any{"actions": actionsPayload(offerActions())}))

	return false, nil
}

// awaitsOffer reports whether step, a fresh run of the mockup step, waits
// for the offer instead, which ends the chain and so frees the slot: the
// blueprint waits, or fails when the offer cannot be read. A run that goes
// on from one of the step's rounds never asks.
func (c *Controller) awaitsOffer(ctx context.Context, sess *session, bp store.Blueprint, step segmentStep) bool {
	if step.seg != nil || step.stage != c.opts.Pack.MockupStage() {
		return false
	}

	bg := context.WithoutCancel(ctx)

	goOn, err := c.mockupGo(bg, sess, bp)

	switch {
	case err != nil:
		c.fail(bg, sess.audit, bp.ID, 0, err.Error())

		return true
	case goOn:
		return false
	}

	c.setState(bg, bp.ID, StateWaiting)

	return true
}

// offerOpen reports whether the offer waits for a person: the newest offer
// is shown. It stays open until a reply, or a mode switch's take, decides
// it; a gate reply that changes the policy meanwhile leaves it open. The
// caller checks that the pipeline stands at the mockup step.
func (c *Controller) offerOpen(ctx context.Context, id string) (bool, error) {
	state, err := c.offerState(ctx, id)

	return state == offerShown, err
}

// offerOpenFor reports whether stageID is the mockup stage and its offer
// waits at the step the pipeline stands at. A record that cannot be read
// reports no open offer and is logged, as for an open gate: it only drops a
// hint.
func (c *Controller) offerOpenFor(ctx context.Context, id, stageID string) bool {
	if stageID != c.opts.Pack.MockupStage() {
		return false
	}

	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the blueprint for an open offer", "blueprint", id, "error", err)

		return false
	}

	out, _, err := c.position(ctx, bp)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the position for an open offer", "blueprint", id, "error", err)

		return false
	}

	open, err := c.offerAt(ctx, id, out)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the open offer", "blueprint", id, "error", err)

		return false
	}

	return open
}

// offerAt reports whether the offer waits at out, where blueprint id's
// pipeline stands.
func (c *Controller) offerAt(ctx context.Context, id string, out outcome) (bool, error) {
	if mockup := c.opts.Pack.MockupStage(); mockup == "" || out.stage != mockup || out.gate {
		return false, nil
	}

	return c.offerOpen(ctx, id)
}

// firstLookPending reports whether out stands at the mockup step after its
// offer was accepted and before any revision was shown, as when the
// designer's first run failed or a cap refused it: skip mockup still goes on
// without a mockup there, and a step that showed nothing reads skipped. Any
// other text is a note that runs the step again.
func (c *Controller) firstLookPending(ctx context.Context, id string, out outcome) (bool, error) {
	mockup := c.opts.Pack.MockupStage()
	if mockup == "" || out.stage != mockup || out.gate {
		return false, nil
	}

	state, err := c.offerState(ctx, id)
	if err != nil || state != offerAccepted {
		return false, err
	}

	_, err = c.opts.Store.LatestArtifact(ctx, id, mockup)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("blueprint: read the mockup: %w", err)
	}

	return false, nil
}

// skipsMockup reports whether text is the offer's skip mockup reply.
func skipsMockup(text string) bool {
	action, _, ok := matchAction(text, offerActions())

	return ok && action.Decision == offerDeclined
}

// atOffer handles a message while the offer is open, or a skip while the
// first look is pending (firstLookPending): show mockup records the
// acceptance and runs the mockup step, skip mockup records the decline,
// which collapses the step, and runs what follows it, and any other text is
// a note that leaves the offer open. The caller holds the run slot.
func (c *Controller) atOffer(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, text, msgID string) error {
	mockup := c.opts.Pack.MockupStage()

	action, _, ok := matchAction(text, offerActions())
	if !ok {
		note := noteEvent(bp.ID, text, msgID, mockup, false)
		note.Payload["hint"] = offerHint
		sess.audit.event(ctx, note)
		c.release(bp.ID, r)

		return nil
	}

	sess.audit.event(ctx, c.offerEvent(bp.ID, action.Decision, map[string]any{"message_id": msgID}))

	if action.Decision == offerAccepted {
		c.launch(runCtx, r, sess, bp, mockup)

		return nil
	}

	out, _, err := c.position(ctx, bp)
	if err != nil {
		c.unreadable(ctx, sess.audit, bp.ID, err)
		c.release(bp.ID, r)

		return nil
	}

	if out.stage == "" || out.gate {
		c.release(bp.ID, r)

		return nil
	}

	c.launch(runCtx, r, sess, bp, out.stage)

	return nil
}

// claimWaitingOffer takes the run slot while the offer is shown and
// undecided and the mockup stage's policy is now auto, records that the mode
// took it and returns the slot for the caller to run the step on; ok is
// false, and the slot free, anywhere else. A held slot is left alone, as for
// a waiting gate.
func (c *Controller) claimWaitingOffer(ctx context.Context, sess *session, bp store.Blueprint) (*run, context.Context, bool) {
	r, runCtx, claimed, _ := c.claimAttached(bp.ID, sess)
	if !claimed {
		return nil, nil, false
	}

	waiting, err := c.waitingOffer(ctx, bp)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the offer a mode switch takes", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))
	}

	if !waiting {
		c.release(bp.ID, r)

		return nil, nil, false
	}

	sess.audit.event(ctx, c.offerEvent(bp.ID, offerAccepted, map[string]any{"by": offerByMode}))

	return r, runCtx, true
}

// waitingOffer reports a shown, undecided offer at the mockup step the
// pipeline stands at, under a policy that is now auto.
func (c *Controller) waitingOffer(ctx context.Context, bp store.Blueprint) (bool, error) {
	mockup := c.opts.Pack.MockupStage()
	if mockup == "" {
		return false, nil
	}

	out, _, err := c.position(ctx, bp)
	if err != nil || out.stage != mockup || out.gate {
		return false, err
	}

	state, err := c.offerState(ctx, bp.ID)
	if err != nil || state != offerShown {
		return false, err
	}

	policy, err := c.policy(ctx, bp, mockup)

	return policy == pack.PolicyAuto, err
}

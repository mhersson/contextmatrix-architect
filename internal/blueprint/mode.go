package blueprint

import (
	"context"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Why a mode reply changed nothing, as its event's reason.
const (
	modeUnchanged       = "unchanged"
	modeEmissionStarted = "emission_started"
	modeEnded           = "ended"
)

// SetMode switches the blueprint's mode, which every later question, gate
// and sign-off reads from the store; the start of a reopen never changes
// it. A switch to the mode in effect, once emission has started or after
// the pipeline ended, is refused and changes nothing. Like a gate reply it
// works while a stage runs. An applied switch posts the gate plan again.
func (c *Controller) SetMode(ctx context.Context, id, mode, msgID string) error {
	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	bp, applied, payload, err := c.switchMode(ctx, id, mode, msgID)
	if err != nil {
		return err
	}

	payload["during_run"] = c.running(id)
	sess.audit.event(ctx, Event{Kind: EventMode, BlueprintID: id, Payload: payload})

	if applied {
		c.repostGatePlan(ctx, sess, bp)
	}

	return nil
}

// switchMode refuses, or stores, a switch of blueprint id to mode under its
// mode lock. It returns the blueprint as stored, whether the switch applied,
// and the event payload so far.
func (c *Controller) switchMode(ctx context.Context, id, mode, msgID string) (store.Blueprint, bool, map[string]any, error) {
	lock := c.modeLock(id)
	lock.Lock()
	defer lock.Unlock()

	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		return store.Blueprint{}, false, nil, fmt.Errorf("blueprint: mode: %w", err)
	}

	payload := map[string]any{"from": bp.Mode, "to": mode, "message_id": msgID}

	reason, refused, err := c.modeRefusal(ctx, bp, mode)
	if err != nil {
		return store.Blueprint{}, false, nil, fmt.Errorf("blueprint: mode: %w", err)
	}

	if reason != "" {
		payload["reason"], payload["refused"] = reason, refused

		return bp, false, payload, nil
	}

	if err := c.opts.Store.SetBlueprintMode(ctx, id, mode); err != nil {
		return store.Blueprint{}, false, nil, fmt.Errorf("blueprint: mode: %w", err)
	}

	bp.Mode = mode

	return bp, true, payload, nil
}

// modeRefusal says why bp cannot switch to mode, as a reason code and a
// sentence for the panel; "" when it can. Emission has started once the
// sign-off of the version the pipeline ended ready on is approved: the state
// is emitting from that approval, and a stop that leaves emission idle keeps
// it approved. A record the pack cannot place skips the position checks, as
// the view tolerates it.
func (c *Controller) modeRefusal(ctx context.Context, bp store.Blueprint, mode string) (string, string, error) {
	if mode == bp.Mode {
		return modeUnchanged, fmt.Sprintf("The blueprint is already in %s mode.", mode), nil
	}

	switch State(bp.State) {
	case StateEmitting, StateEmitted, StateDone:
		return modeEmissionStarted, fmt.Sprintf("Emission has started, so the mode stays %s.", bp.Mode), nil
	}

	out, _, err := c.position(ctx, bp)
	if err != nil {
		c.opts.Logger.Warn("blueprint: mode position unknown", "blueprint", bp.ID, "error", err)

		return "", "", nil
	}

	switch {
	case out.end && !out.ready:
		return modeEnded, "The pipeline has ended; nothing more runs under any mode.", nil
	case out.end && c.cardsStage() != "":
		decision, err := c.gateDecision(ctx, bp.ID, signoffStage, out.art.Version)
		if err != nil {
			return "", "", err
		}

		if decision == store.GateApprove {
			return modeEmissionStarted, fmt.Sprintf("The sign-off is approved and emission has started, so the mode stays %s.", bp.Mode), nil
		}
	}

	return "", "", nil
}

// repostGatePlan posts the gate plan again after a mode switch, once the
// first stage has posted one, so who answers what matches the mode.
func (c *Controller) repostGatePlan(ctx context.Context, sess *session, bp store.Blueprint) {
	_, err := c.opts.Store.LatestEvent(ctx, bp.ID, EventGatePlan)

	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		c.opts.Logger.Warn("blueprint: gate plan not posted", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))
	default:
		c.postGatePlan(ctx, sess, bp)
	}
}

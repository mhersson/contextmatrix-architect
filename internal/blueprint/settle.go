package blueprint

import (
	"context"
	"fmt"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// segmentStep is what runs next on a run slot: a stage run, a critique, or
// a run that goes on from a round (seg). The zero step runs nothing.
type segmentStep struct {
	stage    string
	critique bool
	seg      *round
}

// Reply stores a round's answers and assumption decisions as far as they fit
// the open round, and once every question has an answer closes the round
// and continues its stage. A reply for a round that takes no answers stores
// nothing and says so. A reply carries every answer CM holds, so entries
// replace per question and a missing one deletes nothing.
func (c *Controller) Reply(ctx context.Context, id string, rr protocol.RoundReply, msgID string) error {
	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	stored, err := c.storeReply(ctx, sess, id, rr, msgID)
	if err != nil || !stored {
		return err
	}

	c.poke(id)

	return nil
}

func (c *Controller) storeReply(ctx context.Context, sess *session, id string, rr protocol.RoundReply, msgID string) (bool, error) {
	lock := c.roundLock(id)
	lock.Lock()
	defer lock.Unlock()

	place, found, err := c.roundAt(ctx, id)
	if err != nil {
		return false, err
	}

	s := place.state
	if !found || s.Frame.Closed || s.Frame.Responder || rr.PromptID != roundPromptID(s.Number) {
		sess.audit.event(ctx, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
			"reason": "stale_reply", "prompt_id": rr.PromptID, "message_id": msgID,
			"message": fmt.Sprintf("That reply answers %s, which takes no answers now; nothing was stored.", rr.PromptID),
		}})

		return false, nil
	}

	next, dropped := mergeReply(s, rr.Answers, rr.Decisions)
	next.Change, next.MessageID, next.Dropped = changeAnswers, msgID, dropped
	c.putRound(ctx, sess.audit, id, 0, next)

	return true, nil
}

// poke acts on a changed round: on a free slot it claims it and runs what
// follows the round; on a held one it marks the holder, whose release acts
// on the round again, so a change that lands while a run winds down is
// never lost.
func (c *Controller) poke(id string) {
	c.mu.Lock()

	if c.closing || c.sessions[id] == nil {
		c.mu.Unlock()

		return
	}

	if r := c.runs[id]; r != nil {
		r.recheck = true
		c.mu.Unlock()

		return
	}

	r, ctx, _ := c.claimLocked(id)
	c.mu.Unlock()

	c.settleOn(ctx, r, id)
}

// settleOn runs what follows the blueprint's round on slot r, which the
// caller claimed, or releases the slot.
func (c *Controller) settleOn(ctx context.Context, r *run, id string) {
	bg := context.Background()

	c.mu.Lock()
	sess := c.sessions[id]
	c.mu.Unlock()

	if sess == nil {
		c.release(id, r)

		return
	}

	bp, err := c.opts.Store.GetBlueprint(bg, id)
	if err != nil {
		c.opts.Logger.Error("blueprint: read the blueprint for its round", "blueprint", id, "error", err)
		c.release(id, r)

		return
	}

	c.follow(bg, ctx, r, sess, bp)
}

// follow runs what follows the round the pipeline stands at on slot r,
// which the caller holds, waking a stalled round: the continuation once the
// round is answered, else nothing and the slot is released. A round that
// cannot be read or closed fails the blueprint.
func (c *Controller) follow(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint) {
	step, err := c.nextSegment(ctx, sess, bp, true)
	if err != nil {
		c.fail(ctx, sess.audit, bp.ID, 0, err.Error())
		c.release(bp.ID, r)

		return
	}

	if step.stage == "" {
		c.release(bp.ID, r)

		return
	}

	c.launchStep(runCtx, r, sess, bp, step)
}

// nextSegment is what follows the round the pipeline stands at, for the
// holder of the run slot with nothing in flight: the continuation once every
// question has an answer (the round is closed first), or nothing while the
// round waits for answers, which leaves the blueprint waiting. A stalled
// round, whose run after it failed or was aborted, runs again only when
// wake is set: a message arrived.
func (c *Controller) nextSegment(ctx context.Context, sess *session, bp store.Blueprint, wake bool) (segmentStep, error) {
	lock := c.roundLock(bp.ID)
	lock.Lock()
	defer lock.Unlock()

	place, found, err := c.roundAt(ctx, bp.ID)
	if err != nil || !found || (place.stalled && !wake) {
		return segmentStep{}, err
	}

	s := place.state

	if place.next == roundClose {
		if s, err = c.closeRound(ctx, sess.audit, bp, s); err != nil {
			return segmentStep{}, err
		}

		place.next = roundContinue
	}

	if place.next != roundContinue {
		c.setState(ctx, bp.ID, StateWaiting)

		return segmentStep{}, nil
	}

	rd, err := c.continuation(ctx, bp, s)
	if err != nil {
		return segmentStep{}, err
	}

	if rd == nil {
		return segmentStep{stage: s.Frame.Stage, critique: true}, nil
	}

	return segmentStep{stage: s.Frame.Stage, seg: rd}, nil
}

// atRound handles a message while the pipeline stands at a round and
// nothing runs: a note the stage's later runs get. When the run after a
// closed round failed or was aborted, the note runs it again.
func (c *Controller) atRound(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, place roundPlace, text, msgID string) error {
	s := place.state

	note := noteEvent(bp.ID, text, msgID, s.Frame.Stage, false)
	note.Payload["round"] = s.Number

	if s.Frame.Closed {
		note.Payload["retries"] = s.Frame.Stage
	}

	sess.audit.event(ctx, note)
	c.follow(context.WithoutCancel(ctx), runCtx, r, sess, bp)

	return nil
}

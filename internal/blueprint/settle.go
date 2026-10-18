package blueprint

import (
	"context"
	"fmt"
	"strings"

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

	lock := c.roundLock(id)
	lock.Lock()

	r, runCtx, err := c.storeReply(ctx, sess, id, rr, msgID)

	lock.Unlock()

	if r != nil {
		c.settleOn(runCtx, r, id)
	}

	return err
}

// storeReply stores rr in the open round and pokes, or reports a stale
// reply. The caller holds id's round lock and settles the slot poke claimed,
// if any, once it has released the lock.
func (c *Controller) storeReply(ctx context.Context, sess *session, id string, rr protocol.RoundReply, msgID string) (*run, context.Context, error) {
	place, found, err := c.roundAt(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	s := place.state
	if !found || s.Frame.Closed || s.Frame.Responder || rr.PromptID != roundPromptID(s.Number) {
		c.mu.Lock()
		running := c.runs[id] != nil
		c.mu.Unlock()

		// during_run tells the panel whether a run goes on working after it.
		sess.audit.event(ctx, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
			"reason": "stale_reply", "prompt_id": rr.PromptID, "message_id": msgID, "during_run": running,
			"message": fmt.Sprintf("That reply answers %s, which takes no answers now; nothing was stored.", rr.PromptID),
		}})

		return nil, nil, nil
	}

	next, dropped := mergeReply(s, rr.Answers, rr.Decisions)
	next.Change, next.MessageID, next.Dropped = changeAnswers, msgID, dropped
	c.putRound(ctx, sess.audit, id, 0, next)

	r, runCtx := c.poke(id)

	return r, runCtx, nil
}

// poke acts on a round change the caller stored and still holds id's round
// lock over. A held slot's holder is marked: its release acts on the round
// again, unless its own read of the round, which takes the same lock, came
// after the change and spent the mark. A free slot is claimed and returned
// for the caller to settle once it has released the round lock. Marking
// under that lock means a mark never outlives a read that saw its change.
func (c *Controller) poke(id string) (*run, context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closing || c.sessions[id] == nil {
		return nil, nil
	}

	if r := c.runs[id]; r != nil {
		r.recheck = true

		return nil, nil
	}

	r, ctx, _ := c.claimLocked(id)

	return r, ctx
}

// spendRecheck clears the mark on id's slot holder, which is reading the
// round under the round lock and so sees every change stored before.
func (c *Controller) spendRecheck(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if r := c.runs[id]; r != nil {
		r.recheck = false
	}
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

	c.follow(bg, ctx, r, sess, bp, true)
}

// follow runs what follows the round the pipeline stands at on slot r,
// which the caller holds: the continuation once the round is answered, a
// chat run for chat its stage has not seen, else nothing and the slot is
// released. wake runs a stalled round again: a
// message arrived. A round that cannot be read or closed fails the
// blueprint.
func (c *Controller) follow(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, wake bool) {
	step, err := c.nextSegment(ctx, sess, bp, wake)
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
// question has an answer (the round is closed first), a chat run while chat
// its stage has not seen waits, or nothing while the round waits for
// answers, which leaves the blueprint waiting. A stalled
// round, whose run after it failed or was aborted, runs again only when
// wake is set: a message arrived. The caller holds the run slot.
func (c *Controller) nextSegment(ctx context.Context, sess *session, bp store.Blueprint, wake bool) (segmentStep, error) {
	lock := c.roundLock(bp.ID)
	lock.Lock()
	defer lock.Unlock()

	place, found, err := c.roundAt(ctx, bp.ID)

	// This read sees every change stored so far, so it spends the holder's
	// mark; a stalled round it leaves asleep keeps the mark, for the release
	// to act on a change that may wake it.
	if !place.stalled || wake {
		c.spendRecheck(bp.ID)
	}

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

	switch place.next {
	case roundContinue:
		rd, err := c.continuation(ctx, bp, s)
		if err != nil {
			return segmentStep{}, err
		}

		if rd == nil {
			return segmentStep{stage: s.Frame.Stage, critique: true}, nil
		}

		return segmentStep{stage: s.Frame.Stage, seg: rd}, nil
	case roundChat:
		rd, err := c.chatSegment(ctx, bp, s)
		if err != nil {
			return segmentStep{}, err
		}

		return segmentStep{stage: s.Frame.Stage, seg: rd}, nil
	default:
		c.setState(ctx, bp.ID, StateWaiting)

		return segmentStep{}, nil
	}
}

// chatAbout stores a message as chat about the round a human answers while
// it waits, and has it answered: a free run slot is claimed and settled
// once the round lock is released, and a held one answers it when its
// holder reads the round or releases the slot. false when no such round
// waits.
func (c *Controller) chatAbout(ctx context.Context, sess *session, id, text, msgID string) (bool, error) {
	lock := c.roundLock(id)
	lock.Lock()

	chatted, err := c.storeChat(ctx, sess, id, text, msgID)

	var (
		r      *run
		runCtx context.Context
	)

	if chatted {
		r, runCtx = c.poke(id)
	}

	lock.Unlock()

	if r != nil {
		c.settleOn(runCtx, r, id)
	}

	return chatted, err
}

// storeChat adds text to the chat of the round a human answers while it
// waits; false when no such round waits or text is empty. The caller holds
// id's round lock.
func (c *Controller) storeChat(ctx context.Context, sess *session, id, text, msgID string) (bool, error) {
	if text == "" {
		return false, nil
	}

	place, found, err := c.roundAt(ctx, id)
	if err != nil || !found {
		return false, err
	}

	s := place.state
	if s.Frame.Closed || s.Frame.Responder {
		return false, nil
	}

	next := s.clone()
	next.Frame.Chat = append(next.Frame.Chat, protocol.RoundChat{From: "user", Text: text})
	next.Change, next.MessageID, next.Dropped = changeChat, msgID, 0
	c.putRound(ctx, sess.audit, id, 0, next)

	return true, nil
}

// chatSegment is the run that answers the chat about open round s: its
// attempt and builder round, the conversation of the run that last touched
// it, and the user's entries it has not seen as the next message. Without
// that conversation it is given the round itself.
func (c *Controller) chatSegment(ctx context.Context, bp store.Blueprint, s roundState) (*round, error) {
	rd := &round{attempt: s.Attempt, number: s.BuilderRound, segment: SegmentChat, qround: s.Number, seen: len(s.Frame.Chat)}
	rd.history = c.runHistory(ctx, bp.ID, s.RunID)

	var b strings.Builder

	if rd.history == nil {
		writeOpenRound(&b, s)
	}

	fmt.Fprintf(&b, "## The user writes about round %d\n\n", s.Number)

	for _, m := range s.Frame.Chat[s.Seen:] {
		fmt.Fprintf(&b, "> %s\n\n", m.Text)
	}

	b.WriteString("Answer with answer_chat. Read the code first when the answer needs it. Revise a question, giving its number, or add one when the point changes the round; a question whose options you change loses its answer.\n\n")

	if err := c.writeNewNotes(ctx, &b, bp.ID, s.RunID); err != nil {
		return nil, err
	}

	rd.task = b.String()

	return rd, nil
}

// atRound handles a message the round the pipeline stands at takes no chat
// from, while nothing runs: a note the stage's later runs get. When the run
// after a closed round failed or was aborted, the note runs it again.
func (c *Controller) atRound(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, place roundPlace, text, msgID string) error {
	s := place.state

	note := noteEvent(bp.ID, text, msgID, s.Frame.Stage, false)
	note.Payload["round"] = s.Number

	if s.Frame.Closed {
		note.Payload["retries"] = s.Frame.Stage
	}

	sess.audit.event(ctx, note)
	c.follow(context.WithoutCancel(ctx), runCtx, r, sess, bp, true)

	return nil
}

// resumeRound acts on the round a reopened blueprint stands at, on slot r,
// which the caller holds: an open round is shown again with what is stored
// and waits, or has a chat run answer chat its stage has not seen, an
// answered one closes and its continuation runs, and a stalled one waits
// for the next message.
func (c *Controller) resumeRound(ctx context.Context, r *run, sess *session, bp store.Blueprint, place roundPlace) {
	bg := context.WithoutCancel(ctx)

	c.showRound(bg, sess.audit, bp.ID, place.state.Number)
	c.follow(bg, ctx, r, sess, bp, false)
}

// showRound publishes round n again as stored while it is the newest round
// and waits for answers. A complete round is not shown: it closes at once,
// and its close keeps the answering message for the question rows it
// writes. The round is read under the round lock, so a reply stored since
// the caller placed it is shown, never overwritten.
func (c *Controller) showRound(ctx context.Context, au audit, id string, n int) {
	lock := c.roundLock(id)
	lock.Lock()
	defer lock.Unlock()

	s, ok := c.newestRound(ctx, id)
	if !ok || s.Number != n || s.Frame.Closed || s.complete() {
		return
	}

	shown := s.clone()
	shown.Change, shown.MessageID, shown.Dropped = changeShown, "", 0
	c.putRound(ctx, au, id, 0, shown)
}

// collapseLeftRound closes the newest round in the panel when it is still
// open but the pipeline no longer stands at it, as when the run that posted
// it never recorded its end: nothing will answer it, and the panel would
// keep it docked. It never goes through closeRound, so no answer is
// recorded for it. The caller has found no round the pipeline stands at.
func (c *Controller) collapseLeftRound(ctx context.Context, au audit, id string) {
	lock := c.roundLock(id)
	lock.Lock()
	defer lock.Unlock()

	s, ok := c.newestRound(ctx, id)
	if !ok || s.Frame.Closed {
		return
	}

	left := s.clone()
	left.Change, left.MessageID, left.Dropped, left.Frame.Closed = ChangeLeft, "", 0, true
	c.putRound(ctx, au, id, 0, left)
}

// newestRound is the blueprint's newest round snapshot; ok is false when it
// has none, or when the rounds cannot be read, which is logged. The caller
// holds id's round lock.
func (c *Controller) newestRound(ctx context.Context, id string) (roundState, bool) {
	rounds, err := c.rounds(ctx, id)
	if err != nil {
		c.opts.Logger.Error("blueprint: read the newest round", "blueprint", id, "error", err)

		return roundState{}, false
	}

	if len(rounds) == 0 {
		return roundState{}, false
	}

	return rounds[len(rounds)-1], true
}

package blueprint

import (
	"context"
	"errors"
	"fmt"
	"slices"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
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
// the pipeline ended, is refused and changes nothing. Into a mode other than
// default, the round waiting for a human goes to the auto responder at once
// when its stage's policy is now auto (see handOver). Into auto+emit or
// yolo, a sign-off that waits for a human runs again, preflight included,
// and approves itself under the rule the sign-off always follows. Like a
// gate reply it works while a stage runs. An applied switch posts the gate
// plan again.
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

	var (
		r      *run
		runCtx context.Context
		end    outcome
		handed int
		signs  bool
	)

	if applied && mode != protocol.ArchitectModeDefault {
		if handed, r, runCtx, err = c.handOver(ctx, sess, bp, msgID); err != nil {
			// The mode is stored; the round stays with its human.
			c.opts.Logger.Warn("blueprint: hand the waiting round to the responder", "blueprint", id, "error", sess.audit.text(err.Error()))
		}
	}

	if applied && handed == 0 && emitsOnItsOwn(mode) {
		r, runCtx, end, signs = c.claimWaitingSignoff(ctx, sess, bp)
	}

	if handed > 0 {
		payload["hands_over"] = handed
	}

	if signs {
		payload["signoff"] = true
	}

	payload["during_run"] = r == nil && c.running(id)
	sess.audit.event(ctx, Event{Kind: EventMode, BlueprintID: id, Payload: payload})

	if applied {
		c.repostGatePlan(ctx, sess, bp)
	}

	switch {
	case signs:
		c.launchEnd(runCtx, r, sess, bp, end)
	case r != nil:
		c.follow(context.WithoutCancel(ctx), runCtx, r, sess, bp, true)
	}

	return nil
}

// handOver hands the round waiting for a human to the auto responder when
// its stage's policy is now auto (a gate reply still wins) and the pack has
// responder instructions. The round turns read-only, the answers and
// decisions stored so far stay and are recorded as the human's, and each
// assumption left undecided is accepted. The run slot is poked: a free one
// is claimed and returned for the caller to settle, a held one acts on the
// round when it is released. It returns the round's number, 0 when nothing
// was handed over. The caller has stored the new mode.
func (c *Controller) handOver(ctx context.Context, sess *session, bp store.Blueprint, msgID string) (int, *run, context.Context, error) {
	if c.opts.Pack.ResponderInstructions() == "" {
		return 0, nil, nil, nil
	}

	lock := c.roundLock(bp.ID)
	lock.Lock()
	defer lock.Unlock()

	place, found, err := c.roundAt(ctx, bp.ID)
	if err != nil || !found {
		return 0, nil, nil, err
	}

	s := place.state
	if s.Frame.Closed || s.Frame.Responder || s.complete() {
		return 0, nil, nil, nil
	}

	policy, err := c.policy(ctx, bp, s.Frame.Stage)
	if err != nil || policy != pack.PolicyAuto {
		return 0, nil, nil, err
	}

	h := &handover{}
	for _, a := range s.Frame.Answers {
		h.Answered = append(h.Answered, a.Number)
	}

	for _, d := range s.Frame.Decisions {
		h.Decided = append(h.Decided, d.ID)
	}

	next := acceptUndecided(s)
	next.Frame.Responder, next.HandedOver = true, h
	next.Change, next.MessageID, next.Dropped = changeHandedOver, msgID, 0
	c.putRound(ctx, sess.audit, bp.ID, 0, next)

	r, runCtx := c.poke(bp.ID)

	return s.Number, r, runCtx, nil
}

// respondHandedOver has the auto responder answer each question of the
// handed-over round the pipeline stands at that has no answer yet, outside
// any stage run: with the asking stage's tools, each responder run capped
// as a run of its own. Each answer is stored on the round as it comes, so a
// cancel or a restart answers only what is left, and a question a row
// already answers (a store cut short before its snapshot) takes that answer.
// stageAsked hands the round to nextSegment, which closes it and continues
// the stage; tools that cannot be prepared fail the blueprint, and a cancel
// leaves it idle.
func (c *Controller) respondHandedOver(ctx context.Context, r *run, sess *session, bp store.Blueprint) stageEnd {
	bg := context.WithoutCancel(ctx)
	au := sess.audit
	lock := c.roundLock(bp.ID)

	lock.Lock()
	place, found, err := c.roundAt(bg, bp.ID)
	lock.Unlock()

	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return stageStopped
	}

	s := place.state
	if !found || s.HandedOver == nil || s.Frame.Closed {
		return stageAsked
	}

	st, ok := c.stepAt(s.Frame.Stage)
	if !ok {
		c.fail(bg, au, bp.ID, 0, fmt.Sprintf("unknown stage %q", s.Frame.Stage))

		return stageStopped
	}

	rows, err := c.opts.Store.ListQuestions(bg, bp.ID)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, fmt.Sprintf("blueprint: read the answered questions: %v", err))

		return stageStopped
	}

	recorded := map[int]store.Question{}
	for _, q := range rows {
		recorded[q.Number] = q
	}

	answered := map[int]bool{}
	for _, a := range s.Frame.Answers {
		answered[a.Number] = true
	}

	// The asking stage's tools are prepared once, at the first question the
	// responder has to answer, and closed when the step ends.
	var iv *interviewer

	closeTools := func() {}
	defer func() { closeTools() }()

	for _, q := range s.Frame.Questions {
		if answered[q.Number] {
			continue
		}

		var ans protocol.RoundAnswer

		if row, ok := recorded[q.Number]; ok {
			ans = rowAnswer(q, row)
		} else {
			if iv == nil {
				in, checks, closer, err := c.stageInput(ctx, sess, bp, s.PostedBy, st)
				if err != nil {
					switch {
					case c.shuttingDown(r):
					case ctx.Err() != nil:
						c.setState(bg, bp.ID, StateIdle)
					default:
						c.opts.Logger.Error("blueprint: prepare the responder", "blueprint", bp.ID, "stage", s.Frame.Stage, "error", au.text(err.Error()))
						c.setState(bg, bp.ID, StateFailed)
						au.event(bg, Event{Kind: EventError, BlueprintID: bp.ID, Payload: map[string]any{
							"stage": s.Frame.Stage, "reason": "prepare", "message": err.Error(),
						}})
					}

					return stageStopped
				}

				closeTools = closer
				iv = &interviewer{
					c: c, bp: bp, st: st, sess: sess, r: r, runID: s.PostedBy, attempt: s.Attempt,
					tools: in.Tools, poc: checks.poc,
				}
			}

			ans, err = iv.respond(ctx, q)

			switch {
			case ctx.Err() != nil:
				// A run Shutdown cut short writes nothing, as a stage run's.
				if !c.shuttingDown(r) {
					c.setState(bg, bp.ID, StateIdle)
				}

				return stageStopped
			case err != nil:
				ans = protocol.RoundAnswer{Number: q.Number, Text: err.Error()}
			}
		}

		lock.Lock()

		if cur, ok := c.newestRound(bg, bp.ID); ok && cur.Number == s.Number && !cur.Frame.Closed && cur.HandedOver != nil {
			next, _ := mergeReply(cur, []protocol.RoundAnswer{ans}, nil)
			// A question a chat run revised since the handover lost the
			// human's answer; this one is the responder's.
			next.HandedOver.Answered = slices.DeleteFunc(next.HandedOver.Answered, func(n int) bool { return n == q.Number })
			next.Change, next.MessageID, next.Dropped = changeResponded, "", 0
			c.putRound(bg, au, bp.ID, 0, next)
		}

		lock.Unlock()
	}

	lock.Lock()
	defer lock.Unlock()

	// A chat run that held the slot at the handover may have put new
	// assumptions on the tab since; the responder accepts them too, so the
	// round is complete and closes.
	if cur, ok := c.newestRound(bg, bp.ID); ok && cur.Number == s.Number && !cur.Frame.Closed && !cur.complete() {
		next := acceptUndecided(cur)
		next.Change, next.MessageID, next.Dropped = changeResponded, "", 0
		c.putRound(bg, au, bp.ID, 0, next)
	}

	return stageAsked
}

// acceptUndecided is s with each assumption on its tab that has no decision
// accepted.
func acceptUndecided(s roundState) roundState {
	out := s.clone()

	decided := map[string]bool{}
	for _, d := range s.Frame.Decisions {
		decided[d.ID] = true
	}

	for _, a := range s.Frame.Assumptions {
		if !decided[a.ID] {
			out.Frame.Decisions = append(out.Frame.Decisions, protocol.AssumptionDecision{ID: a.ID, Accept: true})
		}
	}

	return out
}

// rowAnswer is the answer a questions row records for q: the option whose
// title it holds, else its text.
func rowAnswer(q protocol.RoundQuestion, row store.Question) protocol.RoundAnswer {
	return optionAnswer(q, row.Answer)
}

// claimWaitingSignoff takes the run slot while the blueprint waits at a
// shown, undecided sign-off, for the caller to run it again; ok is false,
// and the slot free, anywhere else. A held slot is left alone: its holder
// reads the stored mode when it reaches a sign-off.
func (c *Controller) claimWaitingSignoff(ctx context.Context, sess *session, bp store.Blueprint) (*run, context.Context, outcome, bool) {
	r, runCtx, claimed, _ := c.claimAttached(bp.ID, sess)
	if !claimed {
		return nil, nil, outcome{}, false
	}

	out, waiting, err := c.waitingSignoff(ctx, bp)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the sign-off a mode switch decides", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))
	}

	if !waiting {
		c.release(bp.ID, r)

		return nil, nil, outcome{}, false
	}

	return r, runCtx, out, true
}

// waitingSignoff is the ready end whose sign-off was shown and waits for a
// human's decision; false anywhere else.
func (c *Controller) waitingSignoff(ctx context.Context, bp store.Blueprint) (outcome, bool, error) {
	if c.cardsStage() == "" {
		return outcome{}, false, nil
	}

	out, _, err := c.position(ctx, bp)
	if err != nil || !out.end || !out.ready {
		return outcome{}, false, err
	}

	decision, err := c.gateDecision(ctx, bp.ID, signoffStage, out.art.Version)
	if err != nil || decision != "" {
		return outcome{}, false, err
	}

	shown, err := c.shownSignoff(ctx, bp.ID, out.art.Version)
	if err != nil || shown == nil {
		return outcome{}, false, err
	}

	return out, true, nil
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

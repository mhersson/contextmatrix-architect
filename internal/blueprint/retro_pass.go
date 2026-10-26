package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/retro"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// RetroStage is the stage id of a retrospective's run; no pack stage has it.
const RetroStage = "retrospective"

// Retro reply reasons: why a retrospective reply started no pass, why a
// pass did not complete, or why a pass that stored its lessons posted no
// lesson round (round_too_big, or stopped when the post failed).
const (
	retroRunning        = "running"
	retroNotEmitted     = "not_emitted"
	retroNoInstructions = "no_instructions"
	retroNoStruggle     = "no_struggle"
	retroDeleted        = "deleted"
	retroUnread         = "unread"
	retroStopped        = "stopped"
	retroCapped         = "cap"
	retroRoundOpen      = "round_open"
	retroTooBig         = "round_too_big"
)

const (
	// retroCanceled is the reply to a pass an abort or the session's end cut short.
	retroCanceled = "The retrospective was canceled before it finished; nothing is stored. Reply `retrospective` to run it again."
	// retroRestarted is the reply, after the restart, to a pass a restart cut short.
	retroRestarted = "A restart cut the retrospective short; nothing is stored. Reply `retrospective` to run it again."
)

// Retrospective starts a pass over the blueprint's emitted cards on its run
// slot (see retroPass). It refuses with a retro_reply, starting nothing,
// while a run holds the slot, before the cards are emitted, on a pack
// without the retrospective's instructions, and while a lesson round is
// open. The pass's outcome arrives as events only.
func (c *Controller) Retrospective(ctx context.Context, id, msgID string) error {
	c.mu.Lock()

	sess, ok := c.sessions[id]
	if !ok {
		c.mu.Unlock()

		return ErrNotFound
	}

	if r := c.runs[id]; r != nil {
		running := r.stage
		c.mu.Unlock()

		c.retroReply(ctx, sess.audit, id, retroRunning, msgID, c.retroBusy(ctx, id, running), true)

		return nil
	}

	if c.closing {
		c.mu.Unlock()

		return ErrShuttingDown
	}

	r, runCtx, _ := c.claimLocked(id)
	// Named with the claim, so an abort or a reply before the pass starts
	// names the retrospective.
	r.stage = RetroStage
	c.mu.Unlock()

	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		c.release(id, r)

		return fmt.Errorf("blueprint: retrospective: %w", err)
	}

	// The emitted event, not the state: a crash during a pass leaves the
	// state running, which must not refuse every later pass.
	_, err = c.opts.Store.LatestEvent(ctx, id, EventEmitted)

	switch {
	case errors.Is(err, store.ErrNotFound):
		c.retroReply(ctx, sess.audit, id, retroNotEmitted, msgID,
			"The retrospective judges emitted cards, and this blueprint has emitted none yet.", false)
		c.release(id, r)

		return nil
	case err != nil:
		c.release(id, r)

		return fmt.Errorf("blueprint: retrospective: read the emission: %w", err)
	}

	if c.opts.Pack.RetroInstructions() == "" {
		c.retroReply(ctx, sess.audit, id, retroNoInstructions, msgID,
			"The pack has no retrospective instructions (stages/retrospective.md), so it runs no retrospective.", false)
		c.release(id, r)

		return nil
	}

	// No round-lock holder waits for the run slot, so taking the lock while
	// holding the slot cannot deadlock.
	lock := c.roundLock(id)
	lock.Lock()
	open, isOpen, err := c.lessonRoundAt(ctx, id)
	lock.Unlock()

	switch {
	case err != nil:
		c.release(id, r)

		return fmt.Errorf("blueprint: retrospective: %w", err)
	case isOpen:
		c.retroReply(ctx, sess.audit, id, retroRoundOpen, msgID,
			fmt.Sprintf("Round %d still asks about the last retrospective's lessons; answer it, then reply `retrospective` again.", open.Number), false)
		c.release(id, r)

		return nil
	}

	go c.retroPass(runCtx, r, sess, bp, msgID)

	return nil
}

// retroPass judges the emitted cards that struggled, on the run slot r its
// caller claimed: it reads every emitted card still in ContextMatrix once,
// has one retro run write a finding per struggled card, stores each at-fault
// finding's lesson as proposed and posts the findings message, then the
// lesson round when there are lessons. A pass that does not complete stores
// nothing and says why.
// Either way the state is then settled (see settleAfterPass), unless
// Shutdown cut the pass short, which writes nothing, as the store may be
// closing; the restart says so (see retroInterrupted).
func (c *Controller) retroPass(runCtx context.Context, r *run, sess *session, bp store.Blueprint, msgID string) {
	defer c.release(bp.ID, r)

	bg := context.WithoutCancel(runCtx)
	au := sess.audit

	stop := func(reason, message string) {
		if c.shuttingDown(r) {
			return
		}

		c.retroReply(bg, au, bp.ID, reason, msgID, message, false)
		c.settleAfterPass(bg, bp.ID)
	}

	signals, deleted, err := c.readEmitted(runCtx, sess, bp.ID)

	switch {
	case err != nil && runCtx.Err() != nil:
		stop(retroStopped, retroCanceled)

		return
	case err != nil:
		stop(retroUnread, fmt.Sprintf("The retrospective could not read the emitted cards: %s. Nothing is stored; reply `retrospective` to try again.", err))

		return
	case len(signals) == 0 && len(deleted) > 0:
		stop(retroDeleted, fmt.Sprintf("Every emitted card was deleted in ContextMatrix (%s), so the retrospective has nothing to judge.", strings.Join(deleted, ", ")))

		return
	}

	struggled := retro.Struggled(signals)
	if len(struggled) == 0 {
		read := fmt.Sprintf("%d read", len(signals))
		if len(deleted) > 0 {
			read += fmt.Sprintf(", %d deleted in ContextMatrix", len(deleted))
		}

		stop(retroNoStruggle, fmt.Sprintf("No emitted card struggled (%s), so the retrospective has nothing to judge.", read))

		return
	}

	runs, err := c.opts.Store.ListRuns(bg, bp.ID)
	if err != nil {
		stop(retroStopped, stoppedText(fmt.Errorf("blueprint: list runs: %w", err)))

		return
	}

	attempt := 1

	for _, x := range runs {
		if x.Kind == store.RunKindRetro {
			attempt++
		}
	}

	lists, err := c.listsFor(bg, bp)
	if err != nil {
		stop(retroStopped, stoppedText(err))

		return
	}

	var runID int64

	out, err := c.runSide(runCtx, r, sess, bp, sideRun{
		kind: store.RunKindRetro, stage: RetroStage, persona: pack.RetroPersona, attempt: attempt, round: 1,
		started: func(id int64) { runID = id },
		in: stage.Input{
			Stage: RetroStage, Persona: c.opts.Pack.Persona(pack.RetroPersona), Instructions: c.opts.Pack.RetroInstructions(),
			Task: retro.TaskFor(struggled, c.opts.Pack.CardTemplate(), lists, c.opts.Pack.Instructions(c.readinessStage())),
			Validate: func(_ context.Context, a stage.Artifact) error {
				_, err := retro.ParseFindings(a.Data, struggled)

				return err
			},
		},
	})

	var capped *capStop

	switch {
	case errors.As(err, &capped):
		stop(retroCapped, retroCapText(capped))

		return
	case err != nil && runCtx.Err() != nil:
		stop(retroStopped, retroCanceled)

		return
	case err != nil:
		stop(retroStopped, stoppedText(err))

		return
	}

	findings, err := retro.ParseFindings(out.Artifact.Data, struggled)
	if err != nil {
		stop(retroStopped, stoppedText(err))

		return
	}

	proposed, err := c.proposals(au, bp.ID, runID, struggled, findings)
	if err == nil {
		proposed, err = c.opts.Store.AddLessons(bg, proposed)
	}

	if err != nil {
		stop(retroStopped, stoppedText(err))

		return
	}

	au.event(bg, Event{
		Kind: EventRetroReport, BlueprintID: bp.ID, RunID: runID,
		Payload: retroReport(runID, len(signals), deleted, struggled, findings, proposed),
	})

	if len(proposed) > 0 {
		c.postLessons(bg, au, bp.ID, msgID, runID, attempt, proposed, findings)
	}

	c.settleAfterPass(bg, bp.ID)
}

// postLessons posts the lesson round, or says why it is not posted; the
// lessons then stay proposed.
func (c *Controller) postLessons(ctx context.Context, au audit, id, msgID string, runID int64, attempt int, lessons []store.Lesson, findings []retro.Finding) {
	err := c.postLessonRound(ctx, au, id, runID, attempt, lessons, findings)

	switch {
	case err == nil:
	case errors.Is(err, errLessonRoundTooBig):
		c.retroReply(ctx, au, id, retroTooBig, msgID, fmt.Sprintf(
			"The lessons are too long to ask in one round, which holds at most %d KiB, so no lesson round is posted and they stay proposed.", maxRoundBytes>>10), false)
	default:
		c.retroReply(ctx, au, id, retroStopped, msgID, fmt.Sprintf(
			"The lesson round could not be posted: %s. The lessons stay proposed.", err), false)
	}
}

// readEmitted reads every emitted card once, as its run left it, in emission
// order. A card ContextMatrix answers is not found was deleted there: it is
// skipped and returned in deleted. Any other error names the card whose read
// failed.
func (c *Controller) readEmitted(ctx context.Context, sess *session, id string) ([]retro.Signals, []string, error) {
	client, err := c.connectCM(ctx, sess)
	if err != nil {
		return nil, nil, err
	}

	defer func() { _ = client.Close() }()

	recs, err := c.opts.Store.ListEmissions(ctx, id)
	if err != nil {
		return nil, nil, fmt.Errorf("blueprint: list emissions: %w", err)
	}

	var (
		signals []retro.Signals
		deleted []string
	)

	for _, e := range recs {
		if e.Kind != store.EmitCard {
			continue
		}

		card, err := client.GetEndedCard(ctx, e.Project, e.CMID)

		switch {
		case cm.CardNotFound(err):
			deleted = append(deleted, e.CMID)
		case err != nil:
			return nil, nil, fmt.Errorf("blueprint: read %s: %w", e.CMID, err)
		default:
			signals = append(signals, retro.SignalsOf(card, e.Project))
		}
	}

	return signals, deleted, nil
}

// proposals are the lessons of the at-fault findings, in finding order,
// masked as the record is. findings are in struggled's order.
func (c *Controller) proposals(au audit, id string, runID int64, struggled []retro.Struggle, findings []retro.Finding) ([]store.Lesson, error) {
	var out []store.Lesson

	for i, f := range findings {
		if !f.AtFault {
			continue
		}

		raw, err := json.Marshal(f.Lesson.Overlay)
		if err != nil {
			return nil, fmt.Errorf("blueprint: encode %s's overlay: %w", f.Card, err)
		}

		overlay, err := au.maskJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("blueprint: mask %s's overlay: %w", f.Card, err)
		}

		out = append(out, store.Lesson{
			BlueprintID: id, RunID: runID, Scope: f.Lesson.Scope, Project: struggled[i].Project,
			Text: au.text(f.Lesson.Text), Rationale: au.text(f.Lesson.Rationale),
			Evidence: []string{f.Card}, Overlay: overlay, CreatedAt: c.opts.Now().UTC(),
		})
	}

	return out, nil
}

// retroReport is the findings message's payload: the cards read and those
// skipped as deleted, each struggled card in order with its signals and
// finding, and its stored lesson when it was at fault. lessons are the
// at-fault findings' lessons, in finding order.
func retroReport(runID int64, read int, deleted []string, struggled []retro.Struggle, findings []retro.Finding, lessons []store.Lesson) map[string]any {
	cards := make([]any, 0, len(struggled))
	next := 0

	for i, s := range struggled {
		f := findings[i]
		card := map[string]any{
			"card": s.Card, "project": s.Project, "title": s.Title, "state": s.State, "worker": s.Worker,
			"review_attempts": s.ReviewAttempts, "cost_usd": s.CostUSD, "subtasks": s.Subtasks, "reason": s.Reason,
			"why": s.Why, "rule": f.Rule, "evidence": f.Evidence, "at_fault": f.AtFault,
		}

		if slices.Contains(s.Why, retro.WhyCost) {
			card["median_usd"] = s.MedianUSD
		}

		if f.AtFault {
			l := lessons[next]
			next++

			card["lesson"] = map[string]any{"id": l.ID, "text": l.Text, "scope": l.Scope, "project": l.Project}
		}

		cards = append(cards, card)
	}

	return map[string]any{"run": runID, "read": read, "deleted": nonNil(deleted), "struggled": len(struggled), "cards": cards}
}

// retroReply posts a retro_reply. duringRun tells the panel a run goes on
// working after it.
func (c *Controller) retroReply(ctx context.Context, au audit, id, reason, msgID, message string, duringRun bool) {
	au.event(ctx, Event{Kind: EventRetroReply, BlueprintID: id, Payload: map[string]any{
		"reason": reason, "message": message, "message_id": msgID, "during_run": duringRun,
	}})
}

// retroBusy is the reply to a retrospective reply that found the slot held
// by running, the stage the slot names.
func (c *Controller) retroBusy(ctx context.Context, id, running string) string {
	switch running {
	case RetroStage:
		return "A retrospective is running already."
	case "":
		return "A run holds the blueprint; reply `retrospective` once it ends."
	default:
		return c.stepName(ctx, id, running) + " is running; reply `retrospective` once it ends."
	}
}

// retroInterrupted tells, on the restart that marks its retro run
// interrupted, that a restart cut a pass short, and settles the state the
// pass left running.
func (c *Controller) retroInterrupted(ctx context.Context, au audit, id string) {
	c.retroReply(ctx, au, id, retroStopped, "", retroRestarted, false)
	c.settleAfterPass(ctx, id)
}

// settleAfterPass sets the state a pass leaves: done when the watch has
// ended, else emitted. It settles a state a crash during a pass left
// running too.
func (c *Controller) settleAfterPass(ctx context.Context, id string) {
	_, err := c.opts.Store.LatestEvent(ctx, id, EventWatchEnded)

	switch {
	case err == nil:
		c.setState(ctx, id, StateDone)
	case errors.Is(err, store.ErrNotFound):
		c.setState(ctx, id, StateEmitted)
	default:
		// Emitted heals: the next watch finds the closing summary stored and
		// sets done.
		c.opts.Logger.Error("blueprint: read the closing summary after a retrospective", "blueprint", id, "error", err)
		c.setState(ctx, id, StateEmitted)
	}
}

// retroCapText is the reply to a pass the blueprint cap refused or cut
// short. cap and continue resume no pass, so it says to reply retrospective
// again.
func retroCapText(stop *capStop) string {
	if stop.unpriced != "" {
		return strings.TrimSuffix(stop.unpriced, ".") + ". Then reply `retrospective` again."
	}

	text := "The " + stop.shortfall() + "; the retrospective stopped."

	switch {
	case stop.lifted && stop.nowUSD > 0:
		return text + fmt.Sprintf(" The cap is now $%.2f: reply `retrospective` again.", stop.nowUSD)
	case stop.lifted:
		return text + " The cap is now off: reply `retrospective` again."
	default:
		return text + " Raise the cap by clicking the cost on the scale bar (or reply `cap <usd>`), then reply `retrospective` again."
	}
}

// stoppedText is the reply to a pass err stopped.
func stoppedText(err error) string {
	return fmt.Sprintf("The retrospective stopped: %s. Nothing is stored; reply `retrospective` to run it again.", err)
}

// readinessStage is the stage that checks readiness; "" when the pack has
// none. Its instructions are the checks a retrospective's finding may name.
func (c *Controller) readinessStage() string {
	for _, st := range c.opts.Pack.Stages {
		if slices.Contains(st.Checks, pack.CheckReadiness) {
			return st.ID
		}
	}

	return ""
}

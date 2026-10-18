package blueprint

import (
	"context"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// sideRun is a run beside a stage's own: a reviewer judging its artifact or
// a responder answering one of its questions. It produces no artifact of
// the stage; the caller stores what it returns.
type sideRun struct {
	kind    string // store.RunKindReviewer or store.RunKindResponder
	stage   string // the stage it serves
	persona string // the pack persona that plays it
	attempt int
	round   int
	in      stage.Input
	// parent is the sink of the in-flight run this one serves; it bounds this
	// run's cap and is charged this run's cost.
	parent *harnessSink
}

// refusal is why the cost cap refused a side run; nothing was written.
type refusal struct{ reason string }

func (r refusal) Error() string { return r.reason }

// runSide runs s on the run slot and returns its output. It is recorded as
// a stage run is: a row of its kind, stage_started and stage_ended events
// that name the kind, its transcript and its cost; meanwhile the blueprint
// is running and a note names s's stage. The cost cap is checked
// first; a refusal reports nothing. A run Shutdown cut short writes no
// results, as a stage run's, so the row stays running for the restart.
func (c *Controller) runSide(ctx context.Context, r *run, sess *session, bp store.Blueprint, s sideRun) (stage.Output, error) {
	bg := context.Background()
	au := sess.audit
	m := c.modelFor(sess, s.persona)

	runCap, reason, err := c.runCap(bg, bp, s.stage, m)
	if err != nil {
		return stage.Output{}, err
	}

	if left, capped := s.parent.left(); capped {
		if left <= 0 {
			reason = fmt.Sprintf("the asking run's cost cap of %.2f USD is spent", s.parent.capUSD)
		} else if runCap <= 0 || left < runCap {
			runCap = left
		}
	}

	if reason != "" {
		return stage.Output{}, refusal{reason: reason}
	}

	runID, err := c.opts.Store.StartRun(bg, store.Run{
		BlueprintID: bp.ID, Stage: s.stage, Kind: s.kind, Attempt: s.attempt, Round: s.round,
		Status: store.RunRunning, Model: m.name, StartedAt: c.opts.Now().UTC(),
	})
	if err != nil {
		return stage.Output{}, fmt.Errorf("blueprint: start the %s run of %s: %w", s.kind, pack.StageName(s.stage), err)
	}

	f := &flight{
		runID: runID, stage: s.stage, kind: s.kind, persona: s.persona, model: m.name,
		round: s.round, startedAt: c.opts.Now().UTC(), price: m.price, capUSD: runCap,
	}
	defer c.fly(bp.ID, f)()

	c.setState(bg, bp.ID, StateRunning)

	c.mu.Lock()
	r.stage = s.stage
	c.mu.Unlock()

	au.event(bg, Event{Kind: EventStageStarted, BlueprintID: bp.ID, RunID: runID, Payload: map[string]any{
		"stage": s.stage, "kind": s.kind, "persona": s.persona, "attempt": s.attempt, "round": s.round,
		"model": m.name, "pack": c.opts.Pack.Name, "pack_version": c.opts.Pack.Version,
	}})

	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	in := s.in
	in.Model, in.Endpoint, in.MaxCostUSD, in.MaxTurns = m.name, sess.endpoint, runCap, c.opts.MaxTurns
	in.Secrets = sess.secrets()
	in.Events = &harnessSink{
		audit: au, id: bp.ID, runID: runID, canceled: runCtx.Done(),
		price: m.price, capUSD: runCap, stop: stop, flight: f,
	}

	out, runErr := c.opts.Runner.Run(withRun(runCtx, runID), in)

	if runErr != nil && c.shuttingDown(r) {
		return out, runErr
	}

	if len(out.Result.Messages) > 0 {
		if err := au.transcript(bg, runID, out.Result.Messages); err != nil {
			c.opts.Logger.Error("blueprint: store transcript", "run", runID, "error", err)
		}
	}

	usage := usageOf(m.price, out.Result)
	status, endReason := store.RunDone, out.Result.Reason

	switch {
	case runErr == nil:
	case errors.Is(context.Cause(runCtx), errCostCap) || out.Result.Reason == "max_cost":
		status, endReason = store.RunFailed, "max_cost"
		runErr = fmt.Errorf("%w after %.2f USD: %w", errCostCap, usage.CostUSD, runErr)
	case ctx.Err() != nil:
		status, endReason = store.RunCanceled, cancelReason(ctx)
	default:
		status = store.RunFailed

		if endReason == "" {
			endReason = "error"
		}
	}

	c.endRun(bg, runID, status, endReason, usage)
	// Charged after the row ends: charged before it, a view in between
	// would count the cost in both the parent and the flight.
	s.parent.charge(usage.CostUSD)
	au.event(bg, Event{Kind: EventStageEnded, BlueprintID: bp.ID, RunID: runID, Payload: map[string]any{
		"stage": s.stage, "kind": s.kind, "status": status, "cost_usd": usage.CostUSD, "priced": m.price != nil,
	}})

	if runErr != nil {
		return out, fmt.Errorf("blueprint: the %s run of %s: %w", s.kind, pack.StageName(s.stage), runErr)
	}

	return out, nil
}

// sideFailed reports a reviewer run that did not complete, as a stage run's
// end is reported: a refusal or a run cap reached leaves the blueprint idle
// with an error, a cancel leaves it idle, and anything else fails it. A run
// Shutdown cut short reports nothing.
func (c *Controller) sideFailed(ctx context.Context, r *run, au audit, id, stageID string, err error) {
	bg := context.Background()

	var refused refusal

	switch {
	case c.shuttingDown(r):
	case errors.As(err, &refused), errors.Is(err, errCostCap):
		au.event(bg, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{"stage": stageID, "message": err.Error()}})
		c.setState(bg, id, StateIdle)
	case ctx.Err() != nil:
		c.setState(bg, id, StateIdle)
	default:
		c.fail(bg, au, id, 0, err.Error())
	}
}

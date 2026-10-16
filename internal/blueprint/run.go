package blueprint

import (
	"bytes"
	"context"
	"fmt"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// launch runs a stage in the background on the run slot r, which the caller
// holds; the slot is released when the run has recorded its end.
func (c *Controller) launch(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID, task string) {
	go func() {
		defer c.release(bp.ID, r)

		c.runStage(ctx, r, sess, bp, stageID, task)
	}()
}

func (c *Controller) runStage(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID, task string) {
	bg := context.Background()
	id := bp.ID
	au := sess.audit

	st, ok := c.opts.Pack.Stage(stageID)
	if !ok {
		c.fail(bg, au, id, 0, fmt.Sprintf("unknown stage %q", stageID))

		return
	}

	// The blueprint cap bounds the whole blueprint; the run cap is what is
	// left of it, never more than the configured per-run cap.
	runCap := c.opts.MaxRunCostUSD

	if bp.CostCapUSD > 0 {
		spent, err := c.opts.Store.BlueprintCost(bg, id)
		if err != nil {
			c.fail(bg, au, id, 0, "cost lookup failed: "+err.Error())

			return
		}

		remaining := bp.CostCapUSD - spent
		if remaining < runCap {
			runCap = remaining
		}

		if runCap <= 0 {
			au.event(bg, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
				"stage":   stageID,
				"message": fmt.Sprintf("cost cap %.2f USD would be breached (spent %.2f)", bp.CostCapUSD, spent),
			}})
			c.setState(bg, id, StateIdle)

			return
		}
	}

	attempt, err := c.nextAttempt(bg, id, stageID)
	if err != nil {
		c.fail(bg, au, id, 0, err.Error())

		return
	}

	model := sess.model
	if override, ok := c.opts.Models[st.Persona]; ok && override != "" {
		model = override
	}

	runID, err := c.opts.Store.StartRun(bg, store.Run{
		BlueprintID: id, Stage: stageID, Attempt: attempt, Round: 1,
		Status: store.RunRunning, Model: model, StartedAt: c.opts.Now().UTC(),
	})
	if err != nil {
		c.fail(bg, au, id, 0, "start run failed: "+err.Error())

		return
	}

	c.setState(bg, id, StateRunning)
	au.event(bg, Event{Kind: EventStageStarted, BlueprintID: id, RunID: runID, Payload: map[string]any{
		"stage": stageID, "persona": st.Persona, "attempt": attempt, "model": model,
		"pack": c.opts.Pack.Name, "pack_version": c.opts.Pack.Version,
	}})

	out, runErr := c.opts.Runner.Run(ctx, stage.Input{
		Stage:        stageID,
		Persona:      c.opts.Pack.Persona(st.Persona),
		Instructions: c.opts.Pack.Instructions(stageID),
		Task:         task,
		Model:        model,
		Endpoint:     sess.endpoint,
		MaxCostUSD:   runCap,
		MaxTurns:     c.opts.MaxTurns,
		Asker:        &asker{c: c, id: id, runID: runID, run: r, audit: au},
		Events:       &harnessSink{c: c, run: r, audit: au, id: id, runID: runID},
		Secrets:      []string{sess.endpoint.APIKey},
	})

	// A run Shutdown cut short is crash-equivalent: no writes, so recovery
	// re-runs the stage on the next Start. A completed run still writes;
	// Shutdown waits for it, so the store is open.
	if runErr != nil && c.shuttingDown(r) {
		return
	}

	if len(out.Result.Messages) > 0 {
		if err := au.transcript(bg, runID, out.Result.Messages); err != nil {
			c.opts.Logger.Error("blueprint: store transcript", "run", runID, "error", err)
		}
	}

	cost := out.Result.TotalCostUSD

	switch {
	case runErr == nil:
		// A submitted artifact is kept even when a cancel raced it.
		version, err := au.artifact(bg, id, stageID, out.Artifact.Body, out.Artifact.Data)
		if err != nil {
			c.endRun(bg, runID, store.RunFailed, "store artifact failed", cost)
			c.fail(bg, au, id, runID, err.Error())

			return
		}

		c.endRun(bg, runID, store.RunDone, out.Result.Reason, cost)
		c.setState(bg, id, StateIdle)
		au.event(bg, Event{Kind: EventArtifact, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "version": version, "body": out.Artifact.Body, "data": out.Artifact.Data,
		}})
		au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "status": store.RunDone, "cost_usd": cost,
		}})
	case ctx.Err() != nil:
		// The harness reports a cancelled run as an error; abort and End
		// cancel, so this is a canceled run, not a failed one.
		c.endRun(bg, runID, store.RunCanceled, "canceled", cost)
		c.setState(bg, id, StateIdle)
		au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "status": store.RunCanceled, "cost_usd": cost,
		}})
	default:
		reason := out.Result.Reason
		if reason == "" {
			reason = "error"
		}

		c.endRun(bg, runID, store.RunFailed, reason, cost)
		c.setState(bg, id, StateFailed)
		au.event(bg, Event{Kind: EventError, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "reason": reason, "message": runErr.Error(),
		}})
	}
}

// nextAttempt numbers a new run of stageID after every earlier one.
func (c *Controller) nextAttempt(ctx context.Context, id, stageID string) (int, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("list runs failed: %w", err)
	}

	attempt := 1

	for _, r := range runs {
		if r.Stage == stageID && r.Attempt >= attempt {
			attempt = r.Attempt + 1
		}
	}

	return attempt, nil
}

// fail records an error event and marks the blueprint failed.
func (c *Controller) fail(ctx context.Context, au audit, id string, runID int64, msg string) {
	c.opts.Logger.Error("blueprint: stage failed", "blueprint", id, "run", runID, "message", au.text(msg))
	c.setState(ctx, id, StateFailed)
	au.event(ctx, Event{Kind: EventError, BlueprintID: id, RunID: runID, Payload: map[string]any{"message": msg}})
}

func (c *Controller) setState(ctx context.Context, id string, s State) {
	if err := c.opts.Store.SetBlueprintState(ctx, id, string(s)); err != nil {
		c.opts.Logger.Error("blueprint: set state", "blueprint", id, "state", s, "error", err)
	}
}

func (c *Controller) endRun(ctx context.Context, runID int64, status, reason string, costUSD float64) {
	if err := c.opts.Store.EndRun(ctx, runID, status, reason, costUSD, c.opts.Now().UTC()); err != nil {
		c.opts.Logger.Error("blueprint: end run", "run", runID, "status", status, "error", err)
	}
}

// asker parks the run on its slot until a message answers the question.
type asker struct {
	c     *Controller
	id    string
	runID int64
	run   *run
	audit audit
}

func (a *asker) Ask(ctx context.Context, q stage.Question) (string, error) {
	ch := make(chan string, 1)

	// Checked under c.mu: abort and End cancel under it, so a cancelled run
	// never opens a question.
	a.c.mu.Lock()

	if err := ctx.Err(); err != nil {
		a.c.mu.Unlock()

		return "", err
	}

	// Registered before the question is published, so an answer to it is
	// never a lost wakeup.
	a.run.answer = ch
	a.c.mu.Unlock()

	bg := context.WithoutCancel(ctx)
	a.c.setState(bg, a.id, StateWaiting)
	a.audit.event(bg, Event{Kind: EventQuestion, BlueprintID: a.id, RunID: a.runID, Payload: map[string]any{
		"text": q.Text, "options": q.Options, "context": q.Context,
	}})

	select {
	case answer := <-ch:
		a.c.setState(bg, a.id, StateRunning)

		return answer, nil
	case <-ctx.Done():
		a.c.mu.Lock()

		if a.run.answer == ch {
			a.run.answer = nil
		}

		a.c.mu.Unlock()

		return "", ctx.Err()
	}
}

// harnessSink stores each harness JSONL line as an event and forwards it.
type harnessSink struct {
	c     *Controller
	run   *run
	audit audit
	id    string
	runID int64
}

func (s *harnessSink) Write(p []byte) (int, error) {
	line := bytes.TrimRight(bytes.Clone(p), "\n")
	if len(line) == 0 {
		return len(p), nil
	}

	if s.c.shuttingDown(s.run) {
		return len(p), nil
	}

	s.audit.harness(context.Background(), s.id, s.runID, line)

	return len(p), nil
}

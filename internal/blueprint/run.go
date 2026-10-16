package blueprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/mhersson/contextmatrix-harness/harness"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// errCostCap is the cancel cause when a run's priced usage passes its cap.
var errCostCap = errors.New("run cost cap reached")

// priced is the cost of u at p, in USD. CM resolves the cache rates before
// it sends the price, so this only multiplies. Zero without a price.
func priced(p *protocol.ModelPrice, u store.RunUsage) float64 {
	if p == nil {
		return 0
	}

	const perMTok = 1e6

	return (float64(u.PromptTokens)*p.PromptUSDPerMTok +
		float64(u.CompletionTokens)*p.CompletionUSDPerMTok +
		float64(u.CacheReadTokens)*p.CacheReadUSDPerMTok +
		float64(u.CacheWriteTokens)*p.CacheWriteUSDPerMTok) / perMTok
}

// usageOf is what the ledger records for a run: the harness's token buckets
// and the higher of the endpoint's reported cost and the priced tokens.
func usageOf(p *protocol.ModelPrice, res harness.Result) store.RunUsage {
	u := store.RunUsage{
		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		CacheReadTokens:  res.CacheReadTokens,
		CacheWriteTokens: res.CacheCreationTokens,
	}
	u.CostUSD = max(res.TotalCostUSD, priced(p, u))

	return u
}

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

	m := c.modelFor(sess, st.Persona)

	runCap, ok := c.capAllows(bg, au, bp, stageID, m, nil)
	if !ok {
		return
	}

	attempt, err := c.nextAttempt(bg, id, stageID)
	if err != nil {
		c.fail(bg, au, id, 0, err.Error())

		return
	}

	runID, err := c.opts.Store.StartRun(bg, store.Run{
		BlueprintID: id, Stage: stageID, Attempt: attempt, Round: 1,
		Status: store.RunRunning, Model: m.name, StartedAt: c.opts.Now().UTC(),
	})
	if err != nil {
		c.fail(bg, au, id, 0, "start run failed: "+err.Error())

		return
	}

	c.setState(bg, id, StateRunning)
	au.event(bg, Event{Kind: EventStageStarted, BlueprintID: id, RunID: runID, Payload: map[string]any{
		"stage": stageID, "persona": st.Persona, "attempt": attempt, "model": m.name,
		"pack": c.opts.Pack.Name, "pack_version": c.opts.Pack.Version,
	}})

	// runCtx is cancelled with errCostCap by the sink when the priced usage
	// passes the run cap; the harness's own cap only sees the wire cost.
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	sink := &harnessSink{
		audit: au, id: id, runID: runID, canceled: runCtx.Done(),
		price: m.price, capUSD: runCap, stop: stop,
	}

	out, runErr := c.opts.Runner.Run(runCtx, stage.Input{
		Stage:        stageID,
		Persona:      c.opts.Pack.Persona(st.Persona),
		Instructions: c.opts.Pack.Instructions(stageID),
		Task:         task,
		Model:        m.name,
		Endpoint:     sess.endpoint,
		MaxCostUSD:   runCap,
		MaxTurns:     c.opts.MaxTurns,
		Asker:        &asker{c: c, id: id, runID: runID, run: r, audit: au},
		Events:       sink,
		Secrets:      []string{sess.endpoint.APIKey},
	})

	// A run Shutdown cut short is crash-equivalent: no result writes, so
	// recovery re-runs the stage on the next Start. A completed run still
	// writes; Shutdown waits for it, so the store is open.
	if runErr != nil && c.shuttingDown(r) {
		return
	}

	if len(out.Result.Messages) > 0 {
		if err := au.transcript(bg, runID, out.Result.Messages); err != nil {
			c.opts.Logger.Error("blueprint: store transcript", "run", runID, "error", err)
		}
	}

	usage := usageOf(m.price, out.Result)
	isPriced := m.price != nil

	switch {
	case runErr == nil:
		// A submitted artifact is kept even when a cancel raced it.
		version, err := au.artifact(bg, id, stageID, out.Artifact.Body, out.Artifact.Data)
		if err != nil {
			c.endRun(bg, runID, store.RunFailed, "store artifact failed", usage)
			c.fail(bg, au, id, runID, err.Error())

			return
		}

		c.endRun(bg, runID, store.RunDone, out.Result.Reason, usage)
		c.setState(bg, id, StateIdle)
		au.event(bg, Event{Kind: EventArtifact, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "version": version, "body": out.Artifact.Body, "data": out.Artifact.Data,
		}})
		au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "status": store.RunDone, "cost_usd": usage.CostUSD, "priced": isPriced,
		}})
	case errors.Is(context.Cause(runCtx), errCostCap) || out.Result.Reason == "max_cost":
		// The sink's stop and the harness's own cap on the reported cost are
		// one condition: the run cap was reached.
		c.endRun(bg, runID, store.RunFailed, "max_cost", usage)
		c.setState(bg, id, StateIdle)
		au.event(bg, Event{Kind: EventError, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage":   stageID,
			"reason":  "max_cost",
			"message": fmt.Sprintf("run cost cap %.2f USD reached after %.2f USD; the stage stopped", runCap, usage.CostUSD),
		}})
	case ctx.Err() != nil:
		// The harness reports a cancelled run as an error; abort and End
		// cancel, so this is a canceled run, not a failed one.
		c.endRun(bg, runID, store.RunCanceled, "canceled", usage)
		c.setState(bg, id, StateIdle)
		au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "status": store.RunCanceled, "cost_usd": usage.CostUSD, "priced": isPriced,
		}})
	default:
		reason := out.Result.Reason
		if reason == "" {
			reason = "error"
		}

		c.endRun(bg, runID, store.RunFailed, reason, usage)
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

func (c *Controller) endRun(ctx context.Context, runID int64, status, reason string, usage store.RunUsage) {
	if err := c.opts.Store.EndRun(ctx, runID, status, reason, usage, c.opts.Now().UTC()); err != nil {
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
// Lines are stored even during Shutdown, which waits for the run, so the
// store is open.
type harnessSink struct {
	audit audit
	id    string
	runID int64
	// canceled is closed once abort, End, Shutdown or the cost cap cancels
	// the run.
	canceled <-chan struct{}
	// price, capUSD and stop bind the run cap on endpoints that report no
	// cost: every usage line is priced and the run is stopped once the sum
	// passes capUSD. Idle when price is nil or capUSD is zero.
	price  *protocol.ModelPrice
	capUSD float64
	stop   context.CancelCauseFunc

	mu    sync.Mutex
	spent float64
}

func (s *harnessSink) Write(p []byte) (int, error) {
	line := bytes.TrimRight(bytes.Clone(p), "\n")
	if len(line) == 0 {
		return len(p), nil
	}

	// A canceled run's harness reports the cancel as an error. It is
	// audited, but the panel shows the cancel as the stage's end instead.
	forward := !s.isCanceled() || !isErrorLine(line)

	s.audit.harness(context.Background(), s.id, s.runID, line, forward)
	s.account(line)

	return len(p), nil
}

// account prices a usage line and stops the run once the cap is passed.
func (s *harnessSink) account(line []byte) {
	if s.price == nil || s.capUSD <= 0 || s.stop == nil {
		return
	}

	var ev struct {
		Kind string `json:"kind"`
		Data struct {
			Prompt     int64 `json:"prompt_tokens"`
			Completion int64 `json:"completion_tokens"`
			CacheRead  int64 `json:"cache_read_tokens"`
			CacheWrite int64 `json:"cache_creation_tokens"`
		} `json:"data"`
	}

	if json.Unmarshal(line, &ev) != nil || ev.Kind != "usage" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.spent += priced(s.price, store.RunUsage{
		PromptTokens: ev.Data.Prompt, CompletionTokens: ev.Data.Completion,
		CacheReadTokens: ev.Data.CacheRead, CacheWriteTokens: ev.Data.CacheWrite,
	})

	if s.spent > s.capUSD {
		s.stop(errCostCap)
	}
}

func (s *harnessSink) isCanceled() bool {
	select {
	case <-s.canceled:
		return true
	default:
		return false
	}
}

func isErrorLine(line []byte) bool {
	var ev struct {
		Kind string `json:"kind"`
	}

	return json.Unmarshal(line, &ev) == nil && ev.Kind == "error"
}

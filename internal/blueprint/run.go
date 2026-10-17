package blueprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
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

// launch runs stageID and whatever follows it on the run slot r, which the
// caller holds. The chain stops at a run that did not complete, a gate that
// waits for the user, the pipeline's end (after the sign-off of ready cards,
// shown even when a cancel landed first), or a cancel between steps; the
// slot is released then.
func (c *Controller) launch(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string) {
	c.launchStep(ctx, r, sess, bp, stageID, false)
}

// launchCritique is launch starting with the critique of stageID's latest
// artifact, its review or its next builder round, instead of a run of it.
func (c *Controller) launchCritique(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string) {
	c.launchStep(ctx, r, sess, bp, stageID, true)
}

func (c *Controller) launchStep(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string, critique bool) {
	go func() {
		defer c.release(bp.ID, r)

		for stageID != "" && ctx.Err() == nil {
			if critique {
				if !c.critique(ctx, r, sess, bp, stageID) {
					return
				}
			} else {
				if !c.runStage(ctx, r, sess, bp, stageID, nil) {
					return
				}

				if stageID == c.opts.Pack.Stages[0].ID {
					c.postGatePlan(context.WithoutCancel(ctx), sess, bp)
				}

				if stageID == c.checkoutStage() {
					c.reportSetup(ctx, sess, bp)
				}
			}

			// What follows is decided and recorded even when a cancel
			// landed as the step completed; the loop then stops.
			var end outcome

			stageID, end = c.advance(context.WithoutCancel(ctx), sess, bp, stageID)
			critique = end.critique

			if end.ready {
				c.holdForSignoff(r)
				c.signoff(ctx, sess, bp, end)
			}
		}
	}()
}

// runStage runs one stage, or one builder round of it, and reports whether
// it completed with an artifact.
func (c *Controller) runStage(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string, rd *round) bool {
	bg := context.Background()
	id := bp.ID
	au := sess.audit

	st, ok := c.opts.Pack.Stage(stageID)
	if !ok {
		c.fail(bg, au, id, 0, fmt.Sprintf("unknown stage %q", stageID))

		return false
	}

	m := c.modelFor(sess, st.Persona)

	runCap, ok := c.capAllows(bg, au, bp, stageID, m, nil)
	if !ok {
		return false
	}

	attempt, number, asked := 0, 1, 0

	var err error

	if rd != nil {
		// The question cap is per attempt, so a round counts what the
		// attempt's earlier rounds asked.
		attempt, number = rd.attempt, rd.number
		asked, err = c.askedIn(bg, id, stageID, attempt)
	} else {
		attempt, err = c.nextAttempt(bg, id, stageID)
	}

	if err != nil {
		c.fail(bg, au, id, 0, err.Error())

		return false
	}

	runID, err := c.opts.Store.StartRun(bg, store.Run{
		BlueprintID: id, Stage: stageID, Kind: store.RunKindStage, Attempt: attempt, Round: number,
		Status: store.RunRunning, Model: m.name, StartedAt: c.opts.Now().UTC(),
	})
	if err != nil {
		c.fail(bg, au, id, 0, "start run failed: "+err.Error())

		return false
	}

	c.setState(bg, id, StateRunning)

	c.mu.Lock()
	r.stage = stageID
	c.mu.Unlock()

	au.event(bg, Event{Kind: EventStageStarted, BlueprintID: id, RunID: runID, Payload: map[string]any{
		"stage": stageID, "kind": store.RunKindStage, "persona": st.Persona, "attempt": attempt, "round": number, "model": m.name,
		"pack": c.opts.Pack.Name, "pack_version": c.opts.Pack.Version,
	}})

	// A question a cut-short run left open is answered before the task is
	// written, so the re-run is given the answer.
	answered, err := c.askLeftOpen(ctx, r, au, bp, stageID)

	// The input is built inside the run: a failed checkout is a failed run
	// the next message retries, and abort, End and Shutdown cancel a fetch.
	var (
		in         stage.Input
		checks     stageChecks
		closeTools func()
	)

	if err == nil {
		in, checks, closeTools, err = c.stageInput(ctx, sess, bp, st)
	}

	if err != nil {
		switch {
		case c.shuttingDown(r):
			// Crash-equivalent, as below: the next Start re-runs the stage.
		case ctx.Err() != nil:
			c.endRun(bg, runID, store.RunCanceled, cancelReason(ctx), store.RunUsage{})
			c.setState(bg, id, StateIdle)
			au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
				"stage": stageID, "kind": store.RunKindStage, "status": store.RunCanceled, "cost_usd": 0.0, "priced": m.price != nil,
			}})
		default:
			c.opts.Logger.Error("blueprint: prepare stage", "blueprint", id, "run", runID, "stage", stageID, "error", au.text(err.Error()))
			c.endRun(bg, runID, store.RunFailed, "prepare", store.RunUsage{})
			c.setState(bg, id, StateFailed)
			au.event(bg, Event{Kind: EventError, BlueprintID: id, RunID: runID, Payload: map[string]any{
				"stage": stageID, "reason": "prepare", "message": err.Error(),
			}})
		}

		return false
	}

	if rd != nil {
		if rd.history != nil {
			in.Task, in.History = rd.task, rd.history

			// The round's task was written before these were answered.
			if len(answered) > 0 {
				var b strings.Builder

				b.WriteString("## Questions this round left open\n\nThey are answered now; use the answers and do not ask them again:\n\n")
				writeQuestions(&b, answered)
				in.Task += b.String()
			}
		} else {
			in.Task += rd.task
		}
	}

	defer closeTools()

	// runCtx is cancelled with errCostCap by the sink when the priced usage
	// passes the run cap; the harness's own cap only sees the wire cost.
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	sink := &harnessSink{
		audit: au, id: id, runID: runID, canceled: runCtx.Done(),
		price: m.price, capUSD: runCap, stop: stop,
	}

	in.Model = m.name
	in.Endpoint = sess.endpoint
	in.MaxCostUSD = runCap
	in.MaxTurns = c.opts.MaxTurns
	in.Asker = &asker{
		c: c, bp: bp, stage: stageID, max: st.MaxQuestions, runID: runID, run: r, audit: au,
		sess: sess, attempt: attempt, tools: in.Tools, sink: sink, asked: asked,
	}
	in.Events = sink
	in.Secrets = sess.secrets()

	out, runErr := c.opts.Runner.Run(runCtx, in)

	// A run Shutdown cut short is crash-equivalent: no result writes, so
	// recovery re-runs the stage on the next Start. A completed run still
	// writes; Shutdown waits for it, so the store is open.
	if runErr != nil && c.shuttingDown(r) {
		return false
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
		// Cards accepted on the last contract round carry their findings, and
		// the stopped flag ends the pipeline at this stage. On a checked stage
		// both keys are the controller's, whatever the model put there.
		var findings []string

		if checks.cards != nil {
			findings = checks.cards.last()

			data := maps.Clone(out.Artifact.Data)
			if data == nil {
				data = map[string]any{}
			}

			data["stopped"] = len(findings) > 0
			data["findings"] = findings
			out.Artifact.Data = data
		}

		// The problems an accepted third failing submit still has stay with
		// it, for its gate and its reviewer.
		out.Artifact.Data = keepProblems(out.Artifact.Data, checks.unresolved())

		// A submitted artifact is kept even when a cancel raced it.
		version, err := au.artifact(bg, id, stageID, out.Artifact.Body, out.Artifact.Data)
		if err != nil {
			c.endRun(bg, runID, store.RunFailed, "store artifact failed", usage)
			c.fail(bg, au, id, runID, err.Error())

			return false
		}

		c.endRun(bg, runID, store.RunDone, out.Result.Reason, usage)
		c.setState(bg, id, StateIdle)
		au.event(bg, Event{Kind: EventArtifact, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "version": version, "body": out.Artifact.Body, "data": out.Artifact.Data, "findings": findings,
		}})
		au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "kind": store.RunKindStage, "status": store.RunDone, "cost_usd": usage.CostUSD, "priced": isPriced,
		}})

		return true
	case errors.Is(context.Cause(runCtx), errCostCap) || out.Result.Reason == "max_cost":
		// The sink's stop and the harness's own cap on the reported cost are
		// one condition: the run cap was reached.
		c.endRun(bg, runID, store.RunFailed, "max_cost", usage)
		c.setState(bg, id, StateIdle)

		spent, charged := sink.tally()
		spent = max(spent, usage.CostUSD)

		note := ""
		if charged > 0 {
			note = fmt.Sprintf(", %.2f USD of it for its responder runs", charged)
		}

		au.event(bg, Event{Kind: EventError, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage":     stageID,
			"reason":    "max_cost",
			"spent_usd": spent,
			"message":   fmt.Sprintf("run cost cap %.2f USD reached after %.2f USD%s; the stage stopped", runCap, spent, note),
		}})
	case ctx.Err() != nil:
		// The harness reports a cancelled run as an error; abort and End
		// cancel, so this is a canceled run, not a failed one.
		c.endRun(bg, runID, store.RunCanceled, cancelReason(ctx), usage)
		c.setState(bg, id, StateIdle)
		au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "kind": store.RunKindStage, "status": store.RunCanceled, "cost_usd": usage.CostUSD, "priced": isPriced,
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

	return false
}

// stageInput builds what a run of st is given besides its model and limits:
// the persona, the instructions, the task from the artifacts it consumes,
// the code tools over the blueprint's checkouts, the CM read tools, and the
// card contract on submit with the card template, and the states of its
// checks. The closer ends the run's CM session; it is a no-op without cm
// tools and never nil when err is nil.
func (c *Controller) stageInput(ctx context.Context, sess *session, bp store.Blueprint, st pack.Stage) (stage.Input, stageChecks, func(), error) {
	noop := func() {}

	task, err := c.taskFor(context.WithoutCancel(ctx), bp, st)
	if err != nil {
		return stage.Input{}, stageChecks{}, nil, err
	}

	in := stage.Input{
		Stage: st.ID, Persona: c.opts.Pack.Persona(st.Persona),
		Instructions: c.opts.Pack.Instructions(st.ID), Task: task,
	}

	// Checked before any checkout, so a server without the tool source fails
	// the stage without a fetch.
	useCM := slices.Contains(st.Tools, pack.ToolCM)
	if useCM && c.opts.CMTools == nil {
		return stage.Input{}, stageChecks{}, nil, errNoCMSupport
	}

	if slices.Contains(st.Tools, pack.ToolCode) {
		facts, err := c.prepareCode(ctx, sess, bp, st.ID)
		if err != nil {
			return stage.Input{}, stageChecks{}, nil, err
		}

		in.Tools = stage.CodeTools(facts.root, facts.repos)
		in.Task = checkoutLines(facts) + in.Task
	}

	closeCM := noop

	if useCM {
		src, err := c.cmSource(context.WithoutCancel(ctx), bp, st.ID)
		if err != nil {
			return stage.Input{}, stageChecks{}, nil, err
		}

		cmTools, closer, err := c.opts.CMTools(ctx, sess.mcpKey, src.Tag())
		if err != nil {
			return stage.Input{}, stageChecks{}, nil, fmt.Errorf("blueprint: open the ContextMatrix tools: %w", err)
		}

		if closer != nil {
			closeCM = closer
		}

		in.Tools = append(in.Tools, cmTools...)
		in.Task = fmt.Sprintf("ContextMatrix board reads (the %s tools) are source [%s]; cite it for anything read there.\n\n", cm.ToolPrefix+"*", src.Tag()) + in.Task
	}

	var (
		checks     stageChecks
		validators []func(context.Context, stage.Artifact) error
	)

	bounded := func(v func(context.Context, stage.Artifact) error, s *validatorState) {
		validators = append(validators, v)
		checks.bounded = append(checks.bounded, s)
	}

	if slices.Contains(st.Checks, pack.CheckAssumptions) {
		bounded(c.assumptionsValidator())
	}

	if slices.Contains(st.Checks, pack.CheckDecisions) {
		bounded(c.decisionsValidator(bp, st.ID))
	}

	if slices.Contains(st.Checks, pack.CheckSlices) {
		bounded(c.slicesValidator(bp))
	}

	if slices.Contains(st.Checks, pack.CheckCriteria) {
		bounded(c.criteriaValidator(bp, st.ID))
	}

	if slices.Contains(st.Checks, pack.CheckCards) {
		var cards func(context.Context, stage.Artifact) error

		cards, checks.cards = c.cardsValidator(sess, bp)
		validators = append(validators, cards)
		in.Instructions += "\n\n# Card template\n\n" + c.opts.Pack.CardTemplate()
	}

	in.Validate = allOf(validators)

	return in, checks, closeCM, nil
}

// allOf runs every validator on every submit and joins their refusals, nil
// only when all accept: each validator counts the submits it sees, so one
// that never ran would lose count of the third, the one that is accepted.
// nil when there are no validators.
func allOf(validators []func(context.Context, stage.Artifact) error) func(context.Context, stage.Artifact) error {
	if len(validators) == 0 {
		return nil
	}

	return func(ctx context.Context, a stage.Artifact) error {
		var errs []error

		for _, v := range validators {
			if err := v(ctx, a); err != nil {
				errs = append(errs, err)
			}
		}

		return errors.Join(errs...)
	}
}

// nextAttempt numbers a new run of stageID after every earlier stage run of
// it; reviewer and responder runs are not attempts.
func (c *Controller) nextAttempt(ctx context.Context, id, stageID string) (int, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("list runs failed: %w", err)
	}

	attempt := 1

	for _, r := range runs {
		if r.Kind == store.RunKindStage && r.Stage == stageID && r.Attempt >= attempt {
			attempt = r.Attempt + 1
		}
	}

	return attempt, nil
}

// askedIn counts the questions the runs of stageID's attempt asked,
// answered or not.
func (c *Controller) askedIn(ctx context.Context, id, stageID string, attempt int) (int, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("list runs failed: %w", err)
	}

	in := map[int64]bool{}

	for _, r := range runs {
		if r.Kind == store.RunKindStage && r.Stage == stageID && r.Attempt == attempt {
			in[r.ID] = true
		}
	}

	questions, err := c.opts.Store.ListEventsOfKind(ctx, id, EventQuestion)
	if err != nil {
		return 0, fmt.Errorf("blueprint: count the questions asked: %w", err)
	}

	n := 0

	for _, e := range questions {
		if in[e.RunID] {
			n++
		}
	}

	return n, nil
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

// cancelReason is how a canceled run is recorded: session_ended when End cut
// it short, so a reopen runs its stage again, else canceled.
func cancelReason(ctx context.Context) string {
	if errors.Is(context.Cause(ctx), errSessionEnded) {
		return reasonSessionEnded
	}

	return "canceled"
}

// asker puts a stage's questions to their responder, one at a time: it
// numbers each, refuses one past the stage's cap or with an option the
// webhook would take as a command, parks the run on its slot until a
// message answers it and records the answer. Under the stage's auto policy
// the responder answers it.
type asker struct {
	c     *Controller
	bp    store.Blueprint
	stage string
	// max caps the questions this run's attempt may ask; 0 is no cap.
	max   int
	runID int64
	run   *run
	audit audit
	sess  *session
	// attempt numbers the responder's runs with the asking run's attempt.
	attempt int
	// tools are the asking stage's tools, which its responder reads with.
	tools []tools.Tool
	// sink is the asking run's, which a responder's cost is charged to.
	sink *harnessSink

	// mu keeps one question open at a time, so two asks of one tool batch
	// never share a number or the run's answer channel.
	mu sync.Mutex
	// asked counts the attempt's questions, its earlier rounds' included.
	asked int
}

func (a *asker) Ask(ctx context.Context, q stage.Question) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Checked before anything is numbered or recorded: a cancelled run asks
	// nothing.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if err := commandOption(q.Options); err != nil {
		return "", err
	}

	if a.max > 0 && a.asked >= a.max {
		return "", fmt.Errorf("this stage may ask %d question(s) per attempt and has asked them; decide the rest yourself and, where this stage records assumptions, record each decision as one", a.max)
	}

	bg := context.WithoutCancel(ctx)

	// Read at every question, so a gate reply during the run applies from
	// the next question on.
	policy, err := a.c.policy(bg, a.bp, a.stage)
	if err != nil {
		return "", err
	}

	number, err := a.c.nextQuestion(bg, a.bp.ID)
	if err != nil {
		return "", err
	}

	a.asked++

	payload := map[string]any{
		"number": number, "stage": a.stage, "text": q.Text, "options": q.Options,
		"context": q.Context, "recommended": q.Recommended,
	}

	if policy == pack.PolicyAuto {
		payload["responder"] = store.ResponderAuto

		// As the human path: an abort or End never starts a responder run.
		a.c.mu.Lock()
		err := ctx.Err()
		a.c.mu.Unlock()

		if err != nil {
			return "", err
		}

		a.audit.event(bg, Event{Kind: EventQuestion, BlueprintID: a.bp.ID, RunID: a.runID, Payload: payload})

		return a.respond(ctx, q, number)
	}

	payload["responder"] = store.ResponderHuman

	ans, err := a.c.awaitAnswer(ctx, a.run, a.bp.ID, number, payload, func() {
		a.audit.event(bg, Event{Kind: EventQuestion, BlueprintID: a.bp.ID, RunID: a.runID, Payload: payload})
	})
	if err != nil {
		return "", err
	}

	a.record(bg, q, number, ans)

	return fmt.Sprintf("Q%d: %s", number, ans.text), nil
}

// awaitAnswer opens question number on the run slot r, has publish show it,
// and waits for the human's answer; ctx's error when abort, End or Shutdown
// cancels the run first.
func (c *Controller) awaitAnswer(ctx context.Context, r *run, id string, number int, payload map[string]any, publish func()) (reply, error) {
	bg := context.WithoutCancel(ctx)
	ch := make(chan reply, 1)

	// Checked under c.mu: abort and End cancel under it, so a cancelled run
	// never opens a question.
	c.mu.Lock()

	if err := ctx.Err(); err != nil {
		c.mu.Unlock()

		return reply{}, err
	}

	// Registered before the question is published, so an answer to it is
	// never a lost wakeup.
	r.answer, r.question, r.pending = ch, number, payload
	c.mu.Unlock()

	c.setState(bg, id, StateWaiting)
	publish()

	select {
	case ans := <-ch:
		c.setState(bg, id, StateRunning)

		return ans, nil
	case <-ctx.Done():
		c.mu.Lock()

		if r.answer == ch {
			r.answer = nil
		}

		c.mu.Unlock()

		return reply{}, ctx.Err()
	}
}

// record stores an answered question once. A failure is logged: the answer
// already reached the stage, and the events keep it.
func (a *asker) record(ctx context.Context, q stage.Question, number int, ans reply) {
	if err := a.c.opts.Store.AddQuestion(ctx, store.Question{
		BlueprintID: a.bp.ID, RunID: a.runID, Stage: a.stage, Number: number,
		Text: a.audit.text(q.Text), Options: a.audit.texts(q.Options), Context: a.audit.text(q.Context),
		Answer: a.audit.text(ans.text), Responder: store.ResponderHuman, MessageID: ans.msgID,
		AnsweredAt: a.c.opts.Now().UTC(),
	}); err != nil {
		a.c.opts.Logger.Error("blueprint: record the answer", "blueprint", a.bp.ID, "question", number, "error", err)
	}
}

// nextQuestion numbers a new question after every question the blueprint
// has asked, answered or not. The caller holds the blueprint's run slot, so
// no other question is numbered meanwhile.
func (c *Controller) nextQuestion(ctx context.Context, id string) (int, error) {
	n, err := c.opts.Store.CountEvents(ctx, id, EventQuestion)
	if err != nil {
		return 0, fmt.Errorf("blueprint: number the question: %w", err)
	}

	return n + 1, nil
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
	// charged is the part of spent that served runs cost.
	charged float64
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
	if !s.capped() {
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

// capped reports whether the sink enforces a cap: a priced model and a
// positive cap. A nil sink enforces none.
func (s *harnessSink) capped() bool {
	return s != nil && s.price != nil && s.capUSD > 0 && s.stop != nil
}

// left is what the run cap has not spent; capped is false when the sink
// enforces no cap.
func (s *harnessSink) left() (float64, bool) {
	if !s.capped() {
		return 0, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.capUSD - s.spent, true
}

// charge adds the cost of a run served on this run's behalf, and stops the
// run once the cap is passed. A nil or uncapped sink ignores it.
func (s *harnessSink) charge(usd float64) {
	if !s.capped() {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.spent += usd
	s.charged += usd

	if s.spent > s.capUSD {
		s.stop(errCostCap)
	}
}

// tally is what the sink has priced, and how much of it was charged for
// runs served on this run's behalf.
func (s *harnessSink) tally() (spent, charged float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.spent, s.charged
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

package blueprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/mhersson/contextmatrix-harness/harness"
	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/research"
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

// stageEnd is how a step of the pipeline ended.
type stageEnd int

const (
	// stageStopped: the run failed, was cancelled or refused; nothing follows
	// on its own.
	stageStopped stageEnd = iota
	// stageStored: the stage's artifact is stored, or the critique left it to
	// its gate.
	stageStored
	// stageAsked: the run posted a round or answered its chat.
	stageAsked
)

// launch runs stageID and whatever follows it on the run slot r, which the
// caller holds (see launchStep).
func (c *Controller) launch(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string) {
	c.launchStep(ctx, r, sess, bp, segmentStep{stage: stageID})
}

// launchCritique is launch starting with the critique of stageID's latest
// artifact, its review or its next builder round, instead of a run of it.
func (c *Controller) launchCritique(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string) {
	c.launchStep(ctx, r, sess, bp, segmentStep{stage: stageID, critique: true})
}

// launchStep runs step and whatever follows it on the run slot r, which the
// caller holds. A run that posts a round is followed by what the round needs
// next: its continuation at once when it is already answered, else nothing,
// and the blueprint waits. The chain also stops at a run that did not
// complete, a gate that waits for the user, the pipeline's end (after the
// sign-off of ready cards, shown even when a cancel landed first), or a
// cancel between steps; the slot is released then.
func (c *Controller) launchStep(ctx context.Context, r *run, sess *session, bp store.Blueprint, step segmentStep) {
	go func() {
		defer c.release(bp.ID, r)

		for step.stage != "" && ctx.Err() == nil {
			stageID := step.stage

			var end stageEnd

			if step.critique {
				end = c.critique(ctx, r, sess, bp, stageID)
			} else {
				end = c.runStage(ctx, r, sess, bp, stageID, step.seg)
			}

			switch end {
			case stageStopped:
				return
			case stageAsked:
				next, err := c.nextSegment(context.WithoutCancel(ctx), sess, bp, false)
				if err != nil {
					c.fail(context.WithoutCancel(ctx), sess.audit, bp.ID, 0, err.Error())

					return
				}

				step = next

				continue
			}

			// A first build's artifact: a builder round's, even one that went
			// on from a round, is not.
			if !step.critique && (step.seg == nil || step.seg.number <= 1) {
				if stageID == c.opts.Pack.Stages[0].ID {
					c.postGatePlan(context.WithoutCancel(ctx), sess, bp)
				}

				// Only the checkout stage's first step reports: a later
				// slice's step of it reads the same trees.
				checkout, slice := pack.SplitStep(stageID)
				if checkout == c.checkoutStage() && (slice == "" || slice == sliceID(1)) {
					c.reportSetup(ctx, sess, bp)
				}
			}

			// What follows is decided and recorded even when a cancel landed
			// as the step completed; the loop then stops.
			var adv outcome

			stageID, adv = c.advance(context.WithoutCancel(ctx), sess, bp, stageID)
			step = segmentStep{stage: stageID, critique: adv.critique}

			if adv.ready {
				c.holdForSignoff(r)
				c.signoff(ctx, sess, bp, adv)
			}
		}
	}()
}

// runStage runs one stage, one builder round of it, or a run that goes on
// from one of its rounds, and reports how it ended.
func (c *Controller) runStage(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string, rd *round) stageEnd {
	bg := context.Background()
	id := bp.ID
	au := sess.audit

	// Whatever way this run ends, its draft ends with it; a failure records
	// no stage_ended for observe to clear it on.
	defer func() {
		c.live.clearDraft(id)
		c.live.touch(id)
	}()

	st, ok := c.stepAt(stageID)
	if !ok {
		c.fail(bg, au, id, 0, fmt.Sprintf("unknown stage %q", stageID))

		return stageStopped
	}

	m := c.modelFor(sess, st.Persona)

	budget, ok := c.capAllows(bg, au, bp, stageID, m, nil)
	if !ok {
		return stageStopped
	}

	runCap := budget.capUSD

	var err error

	attempt, number, asked := 0, 1, 0

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

		return stageStopped
	}

	runID, err := c.opts.Store.StartRun(bg, store.Run{
		BlueprintID: id, Stage: stageID, Kind: store.RunKindStage, Attempt: attempt, Round: number,
		Status: store.RunRunning, Model: m.name, StartedAt: c.opts.Now().UTC(),
	})
	if err != nil {
		c.fail(bg, au, id, 0, "start run failed: "+err.Error())

		return stageStopped
	}

	f := &flight{
		runID: runID, stage: stageID, kind: store.RunKindStage, persona: st.Persona, model: m.name,
		round: number, startedAt: c.opts.Now().UTC(), price: m.price, capUSD: runCap,
	}
	defer c.fly(id, f)()

	c.setState(bg, id, StateRunning)

	c.mu.Lock()
	r.stage = stageID
	c.mu.Unlock()

	started := map[string]any{
		"stage": stageID, "kind": store.RunKindStage, "persona": st.Persona, "attempt": attempt, "round": number, "model": m.name,
		"pack": c.opts.Pack.Name, "pack_version": c.opts.Pack.Version,
	}

	if rd != nil && rd.segment != "" {
		started["segment"], started["question_round"] = rd.segment, rd.qround
	}

	au.event(bg, Event{Kind: EventStageStarted, BlueprintID: id, RunID: runID, Payload: started})

	// The input is built inside the run: a failed checkout is a failed run
	// the next message retries, and abort, End and Shutdown cancel a fetch.
	in, checks, closeTools, err := c.stageInput(ctx, sess, bp, runID, st)
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

		return stageStopped
	}

	if rd != nil {
		if rd.history != nil {
			in.Task, in.History = rd.task, rd.history
		} else {
			in.Task += rd.task
		}

		in.Chat = rd.segment == SegmentChat

		if len(rd.findings) > 0 {
			check, state := addressedValidator(rd.findings)
			in.Validate = allOf(append(validatorsOf(in.Validate), check))
			checks.bounded = append(checks.bounded, state)
		}
	}

	// The tab check needs the run's attempt, which stageInput does not know.
	if slices.Contains(st.Checks, pack.CheckAssumptions) {
		check, state := c.tabValidator(bp, st, attempt)
		in.Validate = allOf(append(validatorsOf(in.Validate), check))
		checks.bounded = append(checks.bounded, state)
	}

	defer closeTools()

	// runCtx is cancelled with errCostCap by the sink when the priced usage
	// passes the run cap; the harness's own cap only sees the wire cost.
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)

	sink := &harnessSink{
		audit: au, id: id, runID: runID, canceled: runCtx.Done(),
		price: m.price, capUSD: runCap, stop: stop, flight: f, chat: in.Chat,
	}

	in.Model = m.name
	in.Endpoint = sess.endpoint
	in.MaxCostUSD = runCap
	in.MaxTurns = c.opts.MaxTurns

	iv := &interviewer{
		c: c, bp: bp, st: st, sess: sess, r: r, runID: runID, attempt: attempt, builderRound: number,
		tools: in.Tools, sink: sink, max: st.MaxQuestions, asked: asked,
	}
	if rd != nil {
		iv.findings, iv.qround, iv.seen = rd.findings, rd.qround, rd.seen
	}

	in.Interviewer = iv

	in.Events = sink
	in.Secrets = sess.secrets()

	// The draft carries the version the artifact will be stored as; a read
	// that fails leaves version 1, and only the hint's match suffers.
	version := 1
	if latest, err := c.opts.Store.LatestArtifact(bg, id, stageID); err == nil {
		version = latest.Version + 1
	}

	in.Draft = func(f stage.DraftFrame) {
		// A void draft is none: the next snapshot leaves it out.
		if f.Void {
			c.live.clearDraft(id)
			c.live.touch(id)

			return
		}

		c.live.draft(id, protocol.Draft{Stage: stageID, Version: version, Offset: f.Offset, Text: f.Text, Refused: f.Refused, Reason: f.Reason})
	}

	out, runErr := c.opts.Runner.Run(withRun(runCtx, runID), in)

	// A run Shutdown cut short is crash-equivalent: no result writes, so
	// recovery re-runs the stage on the next Start. A completed run still
	// writes; Shutdown waits for it, so the store is open.
	if runErr != nil && c.shuttingDown(r) {
		return stageStopped
	}

	if len(out.Result.Messages) > 0 {
		if err := au.transcript(bg, runID, out.Result.Messages); err != nil {
			c.opts.Logger.Error("blueprint: store transcript", "run", runID, "error", err)
		}
	}

	usage := usageOf(m.price, out.Result)
	isPriced := m.price != nil

	switch {
	case runErr == nil && out.Asked:
		c.endRun(bg, runID, store.RunAsked, "round", usage)
		au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "kind": store.RunKindStage, "status": store.RunAsked, "cost_usd": usage.CostUSD, "priced": isPriced,
			"question_round": iv.posted, "chat": in.Chat, "responder": iv.responded,
		}})

		return stageAsked
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

			return stageStopped
		}

		c.endRun(bg, runID, store.RunDone, out.Result.Reason, usage)
		c.setState(bg, id, StateIdle)
		// What the transcript's sheet reference says of this version, all
		// known now: who drew it, the findings a revision was given, a loop
		// stage's round, the round cards with findings were accepted on.
		revisedFor := 0
		if rd != nil {
			revisedFor = len(rd.findings)
		}

		payload := map[string]any{
			"stage": stageID, "version": version, "body": out.Artifact.Body, "data": out.Artifact.Data, "findings": findings,
			"persona": st.Persona, "round": number, "revised_for": revisedFor,
		}

		if len(findings) > 0 {
			payload["accepted_round"] = checks.cards.round()
		}

		if st.LoopsTo != "" {
			if base, _, err := c.rejection(bg, id, version); err == nil {
				payload["loop_round"], payload["max_rounds"] = version-base, st.MaxRounds
			}
		}

		au.event(bg, Event{Kind: EventArtifact, BlueprintID: id, RunID: runID, Payload: payload})
		au.event(bg, Event{Kind: EventStageEnded, BlueprintID: id, RunID: runID, Payload: map[string]any{
			"stage": stageID, "kind": store.RunKindStage, "status": store.RunDone, "cost_usd": usage.CostUSD, "priced": isPriced,
		}})

		return stageStored
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

		payload := map[string]any{
			"stage":     stageID,
			"reason":    "max_cost",
			"spent_usd": spent,
			"message":   fmt.Sprintf("run cost cap %.2f USD reached after %.2f USD%s; the stage stopped", runCap, spent, note),
		}

		if budget.bound {
			if stop, err := c.capStopAfter(bg, id, budget.blueprintCapUSD, store.RunKindStage, stageID); err != nil {
				c.opts.Logger.Error("blueprint: read the cap stop", "blueprint", id, "run", runID, "error", err)
			} else {
				maps.Copy(payload, stop.payload())
			}
		}

		au.event(bg, Event{Kind: EventError, BlueprintID: id, RunID: runID, Payload: payload})
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

	return stageStopped
}

// stageInput builds what a run of st is given besides its model and limits:
// the persona, the instructions, the task from the artifacts it consumes,
// the code tools over the blueprint's checkouts, the CM read tools, the
// research tools when the session has research on, and the card contract on
// submit with the card template, and the states of its checks. The closer
// ends the run's CM session; it is a no-op without cm tools and never nil
// when err is nil.
func (c *Controller) stageInput(ctx context.Context, sess *session, bp store.Blueprint, runID int64, st pipelineStep) (stage.Input, stageChecks, func(), error) {
	noop := func() {}

	task, err := c.taskFor(context.WithoutCancel(ctx), bp, st)
	if err != nil {
		return stage.Input{}, stageChecks{}, nil, err
	}

	in := stage.Input{
		Stage: st.ID, Persona: c.opts.Pack.Persona(st.Persona),
		Instructions: c.opts.Pack.Instructions(st.Stage.ID), Task: task,
	}

	// Checked before any checkout, so a server without the tool source fails
	// the stage without a fetch.
	useCM := slices.Contains(st.Tools, pack.ToolCM)
	if useCM && c.opts.CMTools == nil {
		return stage.Input{}, stageChecks{}, nil, errNoCMSupport
	}

	var facts *codeFacts

	if slices.Contains(st.Tools, pack.ToolCode) {
		if facts, err = c.prepareCode(ctx, sess, bp, st.ID); err != nil {
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

	if slices.Contains(st.Tools, pack.ToolResearch) {
		if c.researchOn(sess, st.Stage) {
			guard := c.researchGuard(sess, bp, st.ID, facts)
			ledger := researchLedger{c: c, au: sess.audit, id: bp.ID, stage: st.ID}

			in.Tools = append(in.Tools, research.Tools(sess.searcher, c.opts.Fetch, guard, ledger)...)
			in.Task = researchOnTask + in.Task
		} else {
			c.noteResearchOff(ctx, sess, bp.ID, runID, st.ID)

			in.Task = researchOffTask + in.Task
		}
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
		bounded(c.assumptionsValidator(bp))
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

// validatorsOf is v as a list for allOf; empty when v is nil.
func validatorsOf(v func(context.Context, stage.Artifact) error) []func(context.Context, stage.Artifact) error {
	if v == nil {
		return nil
	}

	return []func(context.Context, stage.Artifact) error{v}
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

// askedIn counts the questions the runs of stageID's attempt asked: in the
// rounds they posted, and alone in a record from before rounds.
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

	rounds, err := c.rounds(ctx, id)
	if err != nil {
		return 0, err
	}

	n := 0

	for _, s := range rounds {
		if in[s.PostedBy] {
			n += len(s.Frame.Questions)
		}
	}

	alone, err := c.opts.Store.ListEventsOfKind(ctx, id, EventQuestion)
	if err != nil {
		return 0, fmt.Errorf("blueprint: count the questions asked: %w", err)
	}

	for _, e := range alone {
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

// setState records a blueprint's state and appends a state row: the view's
// sequence is its newest events row, so every change must write one. The
// row is stored, never sent to the panel, which learns of the state from
// the events around it.
func (c *Controller) setState(ctx context.Context, id string, s State) {
	if err := c.opts.Store.SetBlueprintState(ctx, id, string(s)); err != nil {
		c.opts.Logger.Error("blueprint: set state", "blueprint", id, "state", s, "error", err)

		return
	}

	payload, _ := json.Marshal(map[string]string{"state": string(s)})

	if err := c.opts.Store.AppendEvent(ctx, store.Event{
		Time: c.opts.Now().UTC(), BlueprintID: id, Kind: EventState, Payload: payload,
	}); err != nil {
		c.opts.Logger.Error("blueprint: store the state row", "blueprint", id, "state", s, "error", err)
	}

	c.live.touch(id)
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
	// flight counts the run's lines for the view; nil counts nothing.
	flight *flight
	// chat keeps a chat run's text and its answer_chat call off the panel:
	// the reply reaches it only in the round's chat.
	chat bool

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
	forward := (!s.isCanceled() || !isErrorLine(line)) && (!s.chat || !isChatEcho(line))

	s.flight.count(line)
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
	if s != nil {
		s.flight.charge(usd)
	}

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

// isChatEcho reports a line that would show a chat run's reply a second
// time beside the round: the model's text, or its answer_chat call.
func isChatEcho(line []byte) bool {
	var ev struct {
		Kind string `json:"kind"`
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}

	if json.Unmarshal(line, &ev) != nil {
		return false
	}

	return ev.Kind == "model_response" || (ev.Kind == "tool_call" && ev.Data.Name == stage.AnswerChatName)
}

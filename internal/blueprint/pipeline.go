package blueprint

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// maxContractRounds is how many submits of failing cards one decomposition
// run gets: the last is accepted with its findings and stops the pipeline.
const maxContractRounds = 3

// outcome is what follows a stage: a model stage to run, the open gate of
// the stage that completed (gate, with stage and art the artifact that
// waits), or the pipeline's end with the artifact it ended on. critique
// marks a gate whose artifact still needs its reviewer, or a builder round
// after a low verdict or the revision after a passing one with findings.
// fold, on an outcome that passes a stage, is the fold the caller records.
type outcome struct {
	stage    string
	gate     bool
	critique bool
	fold     *folding
	end      bool
	ready    bool
	findings []string
	art      store.Artifact
	// round is the round the pipeline ended on: a loop stage's, counted from
	// the sign-off's latest rejection, or the contract round cards stopped
	// on; 0 when no round applies.
	round int
	// qround is the question round a reopened blueprint stands at; nil when
	// none.
	qround *roundPlace
}

// after decides what follows completed from the stored artifacts; an empty
// completed means no stage has completed yet. A loop stage's verdict ends
// the pipeline or re-runs its target, cards accepted with findings end it,
// and otherwise the next stage in pack order follows.
func (c *Controller) after(ctx context.Context, bp store.Blueprint, completed string) (outcome, error) {
	from := 0

	var (
		last store.Artifact
		fold *folding
	)

	if completed != "" {
		st, ok := c.opts.Pack.Stage(completed)
		if !ok {
			return outcome{}, fmt.Errorf("blueprint: stage %q in the record is not in pack %s %s", completed, c.opts.Pack.Name, c.opts.Pack.Version)
		}

		art, err := c.opts.Store.LatestArtifact(ctx, bp.ID, completed)
		if err != nil {
			return outcome{}, fmt.Errorf("blueprint: after %s: %w", completed, err)
		}

		if st.LoopsTo != "" {
			// The loop stage judges its target's cards; its round is its
			// artifact version less the version the sign-off last rejected.
			target, err := c.opts.Store.LatestArtifact(ctx, bp.ID, st.LoopsTo)
			if err != nil {
				return outcome{}, fmt.Errorf("blueprint: after %s: the judged %s: %w", completed, st.LoopsTo, err)
			}

			ready, findings := verdictOf(art, cardIDs(target))

			base, rejected, err := c.rejection(ctx, bp.ID, art.Version)
			if err != nil {
				return outcome{}, fmt.Errorf("blueprint: after %s: %w", completed, err)
			}

			switch {
			case ready && rejected:
				return outcome{stage: st.LoopsTo}, nil
			case ready:
				return outcome{end: true, ready: true, art: art, round: art.Version - base}, nil
			case art.Version-base >= st.MaxRounds:
				return outcome{end: true, findings: findings, art: art, round: art.Version - base}, nil
			default:
				return outcome{stage: st.LoopsTo}, nil
			}
		}

		// Only a checked stage's stopped flag is the controller's own.
		if slices.Contains(st.Checks, pack.CheckCards) && artifactStopped(art) {
			// Cards are accepted with findings only on the last contract round.
			return outcome{end: true, findings: findingsOf(art), art: art, round: maxContractRounds}, nil
		}

		// A gated stage's artifact waits at its gate: approved, what follows
		// it runs; rejected, the stage runs again.
		if st.Gate {
			// A rejection at this gate of a stage folded into it runs that
			// stage again, and everything after it.
			again, err := c.foldedRejected(ctx, bp.ID, st)
			if err != nil {
				return outcome{}, fmt.Errorf("blueprint: after %s: %w", completed, err)
			}

			if again != "" {
				return outcome{stage: again}, nil
			}

			decision, err := c.gateDecision(ctx, bp.ID, st.ID, art.Version)
			if err != nil {
				return outcome{}, fmt.Errorf("blueprint: after %s: %w", completed, err)
			}

			switch decision {
			case "":
				into, err := c.foldsInto(ctx, bp, st, art)
				if err != nil {
					return outcome{}, fmt.Errorf("blueprint: after %s: %w", completed, err)
				}

				if into != "" {
					fold = &folding{stage: st.ID, version: art.Version, into: into}

					break
				}

				pending, err := c.critiquePending(ctx, bp.ID, st, art)
				if err != nil {
					return outcome{}, fmt.Errorf("blueprint: after %s: %w", completed, err)
				}

				return outcome{stage: st.ID, gate: true, critique: pending, art: art}, nil
			case store.GateReject:
				return outcome{stage: st.ID}, nil
			}
		}

		from = slices.IndexFunc(c.opts.Pack.Stages, func(s pack.Stage) bool { return s.ID == completed }) + 1
		last = art
	}

	if from < len(c.opts.Pack.Stages) {
		return outcome{stage: c.opts.Pack.Stages[from].ID, fold: fold}, nil
	}

	return outcome{end: true, ready: true, art: last}, nil
}

// position reads where a blueprint with no run in flight stands: what
// follows the stage whose run completed last, and, when that is a model
// stage, its newest run since then (zero when it has not started there).
// Only stage runs count; reviewer and responder runs place nothing. For a
// critique it is the newest stage or reviewer run of the gated stage.
func (c *Controller) position(ctx context.Context, bp store.Blueprint) (outcome, store.Run, error) {
	runs, err := c.opts.Store.ListRuns(ctx, bp.ID)
	if err != nil {
		return outcome{}, store.Run{}, fmt.Errorf("blueprint: list runs: %w", err)
	}

	completed, since := "", 0

	for i, r := range runs {
		if r.Kind == store.RunKindStage && r.Status == store.RunDone {
			completed, since = r.Stage, i+1
		}
	}

	out, err := c.after(ctx, bp, completed)
	if err != nil || out.stage == "" || (out.gate && !out.critique) {
		return out, store.Run{}, err
	}

	var last store.Run

	for _, r := range runs[since:] {
		if r.Stage == out.stage && (r.Kind == store.RunKindStage || (out.critique && r.Kind == store.RunKindReviewer)) {
			last = r
		}
	}

	return out, last, nil
}

// endedWithSession reports a run End cut short: a reopen runs its stage
// again.
func endedWithSession(r store.Run) bool {
	return r.Status == store.RunCanceled && r.Reason == reasonSessionEnded
}

// rerunsOnReopen reports a run a reopen runs again: one End cut short, or
// one a crash left running and the restart marked interrupted.
func rerunsOnReopen(r store.Run) bool {
	return endedWithSession(r) || r.Status == store.RunInterrupted
}

// resumePoint decides what a reopened blueprint does: resume the question
// round the pipeline stands at, re-run a stage a crash left running, run a
// model stage whose inputs exist but which never started (a crash between
// storing one artifact and starting the next run). An open round the
// pipeline went past is collapsed first. A failed or aborted stage waits for
// the user's next message and an ended pipeline for a new blueprint. A stage
// the session's end cut short runs again. An open gate is returned for the
// gate to resume, and so is one whose artifact still needs its critique,
// unless its latest review, builder round or revision failed or was aborted.
// A ready end is returned for the sign-off, emission or watch to resume. A
// position the record does not yield is reported, not returned: the session
// still attaches. The caller holds the run slot.
func (c *Controller) resumePoint(ctx context.Context, au audit, bp store.Blueprint) (outcome, error) {
	interrupted, err := c.interruptedStage(ctx, bp)
	if err != nil {
		return outcome{}, err
	}

	// A round decides first: the runs it left are its continuation's or its
	// chat's, whatever interruptedStage found.
	place, found, err := c.roundAt(ctx, bp.ID)
	if err != nil {
		return outcome{}, err
	}

	if found {
		return outcome{qround: &place}, nil
	}

	c.collapseLeftRound(ctx, au, bp.ID)

	if interrupted != "" {
		return outcome{stage: interrupted}, nil
	}

	out, last, err := c.position(ctx, bp)
	if err == nil && out.fold != nil && !c.recordFold(ctx, au, bp.ID, out.fold) {
		return outcome{}, nil
	}

	// A review that completed leaves the critique to go on: a crash came
	// before its verdict was stored or before the builder round started.
	if last.Kind == store.RunKindReviewer && last.Status == store.RunDone {
		last = store.Run{}
	}

	switch {
	case err != nil:
		c.unreadable(ctx, au, bp.ID, err)

		return outcome{}, nil
	case out.gate && !out.critique:
		return out, nil
	case out.end && out.ready && c.cardsStage() != "":
		return out, nil
	case out.stage == "":
		return outcome{}, nil
	case last.Status != "" && !rerunsOnReopen(last):
		// A failed, aborted or refused stage waits for the next message.
		return outcome{}, nil
	}

	if out.critique {
		return out, nil
	}

	// The first stage runs on the need; without one the next message is it.
	if out.stage == c.opts.Pack.Stages[0].ID {
		if _, err := c.opts.Store.LatestArtifact(ctx, bp.ID, needStage); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return outcome{}, nil
			}

			return outcome{}, fmt.Errorf("blueprint: resume: %w", err)
		}
	}

	return outcome{stage: out.stage}, nil
}

// unreadable reports a pipeline position the record does not yield, such
// as a stage the current pack lacks, and leaves the blueprint idle: the
// session stays usable and the user sees why nothing runs.
func (c *Controller) unreadable(ctx context.Context, au audit, id string, err error) {
	c.opts.Logger.Error("blueprint: read the pipeline position", "blueprint", id, "error", au.text(err.Error()))
	c.setState(ctx, id, StateIdle)
	au.event(ctx, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
		"message": "The pipeline's position cannot be read from the record, so nothing runs: " + err.Error(),
	}})
}

// input handles a message that arrives while no run is in flight and no
// human's round takes it as chat: a note at a round, the need while the
// first stage has no artifact, a decision at an open gate or the sign-off,
// else a note. The caller holds the run slot.
func (c *Controller) input(ctx, runCtx context.Context, r *run, sess *session, id, text, msgID string) error {
	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		c.release(id, r)

		return fmt.Errorf("blueprint: message: %w", err)
	}

	// A round takes the message first: the stage that asked it may have no
	// artifact yet, and the message must not read as the need.
	place, atRound, err := c.roundAt(ctx, id)
	if err != nil {
		c.release(id, r)

		return fmt.Errorf("blueprint: message: %w", err)
	}

	if atRound {
		return c.atRound(ctx, runCtx, r, sess, bp, place, text, msgID)
	}

	_, err = c.opts.Store.LatestArtifact(ctx, id, c.opts.Pack.Stages[0].ID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return c.need(ctx, runCtx, r, sess, bp, text, msgID)
	case err != nil:
		c.release(id, r)

		return fmt.Errorf("blueprint: message: %w", err)
	}

	out, _, err := c.position(ctx, bp)
	if err != nil {
		// The message is kept and the user told why it starts nothing.
		sess.audit.event(ctx, noteEvent(id, text, msgID, "", false))
		c.unreadable(ctx, sess.audit, id, err)
		c.release(id, r)

		return nil
	}

	if out.fold != nil && !c.recordFold(ctx, sess.audit, id, out.fold) {
		c.release(id, r)

		return nil
	}

	if out.gate && out.critique {
		// A review, builder round or revision that failed or was aborted:
		// the message is a note and runs it again.
		note := noteEvent(id, text, msgID, out.stage, false)
		note.Payload["retries"] = out.stage
		note.Payload["critique"] = true
		sess.audit.event(ctx, note)
		c.launchCritique(runCtx, r, sess, bp, out.stage)

		return nil
	}

	if out.gate {
		return c.atGate(ctx, runCtx, r, sess, bp, out, text, msgID)
	}

	if out.end && out.ready && c.cardsStage() != "" {
		return c.atReadyEnd(ctx, runCtx, r, sess, bp, out, text, msgID)
	}

	// A note. While a model stage is pending (it failed, was aborted or
	// refused, or a cancel fell between stages) the note also re-runs it,
	// so an abort is not a dead end; an ended pipeline is not re-run, and
	// the panel is told no stage starts.
	note := noteEvent(id, text, msgID, out.stage, false)
	if out.stage != "" {
		note.Payload["retries"] = out.stage
	} else {
		note.Payload["ended"] = true
	}

	sess.audit.event(ctx, note)

	if out.stage == "" {
		c.release(id, r)

		return nil
	}

	c.launch(runCtx, r, sess, bp, out.stage)

	return nil
}

// need stores the message as the next need version and starts the first
// stage, so a canceled or failed intake can be retried. The cap is checked
// before the need is stored, so a spent blueprint does not pile up refused
// need versions; the refusal's event keeps the need in the audit log
// instead.
func (c *Controller) need(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, text, msgID string) error {
	first := c.opts.Pack.Stages[0]
	need := sess.audit.text(text)
	m := c.modelFor(sess, first.Persona)

	if _, ok := c.capAllows(ctx, sess.audit, bp, first.ID, m, map[string]any{"message_id": msgID, "need": need}); !ok {
		c.release(bp.ID, r)

		return nil
	}

	if _, err := sess.audit.artifact(ctx, bp.ID, needStage, need, nil); err != nil {
		c.release(bp.ID, r)

		return err
	}

	c.launch(runCtx, r, sess, bp, first.ID)

	return nil
}

// stopPoint is where a pipeline with no run in flight stands stopped: a
// pending model stage, a gated stage whose critique (its review, builder
// round or revision) is pending, a round whose continuation or chat run is
// pending (round.stalled when that run failed or was aborted), or the first
// stage, before its first artifact, with a need stored. fold is the fold to
// record before the stage runs. byCap says the blueprint cap stopped it (see
// stoppedByCap), and capUSD is the cap it stopped under.
type stopPoint struct {
	stage    string
	critique bool
	round    *roundPlace
	need     bool
	fold     *folding
	byCap    bool
	capUSD   float64
}

// stoppedAt reads where bp stands stopped, as input reads it for a message;
// false when it stands at no stop: a round waits for answers, a gate or the
// sign-off is open, the pipeline has ended, or there is no need yet. The
// caller holds the run slot.
func (c *Controller) stoppedAt(ctx context.Context, bp store.Blueprint) (stopPoint, bool, error) {
	place, atRound, err := c.roundAt(ctx, bp.ID)
	if err != nil {
		return stopPoint{}, false, err
	}

	var p stopPoint

	if atRound {
		if place.next == roundWait {
			return stopPoint{}, false, nil
		}

		p = stopPoint{stage: place.state.Frame.Stage, round: &place}
	} else {
		first := c.opts.Pack.Stages[0].ID

		_, err := c.opts.Store.LatestArtifact(ctx, bp.ID, first)

		switch {
		case errors.Is(err, store.ErrNotFound):
			_, err := c.opts.Store.LatestArtifact(ctx, bp.ID, needStage)
			if errors.Is(err, store.ErrNotFound) {
				return stopPoint{}, false, nil
			}

			if err != nil {
				return stopPoint{}, false, fmt.Errorf("blueprint: read the need: %w", err)
			}

			p = stopPoint{stage: first, need: true}
		case err != nil:
			return stopPoint{}, false, fmt.Errorf("blueprint: read %s's artifact: %w", pack.StageName(first), err)
		default:
			out, _, err := c.position(ctx, bp)
			if err != nil {
				return stopPoint{}, false, err
			}

			if out.stage == "" || (out.gate && !out.critique) {
				return stopPoint{}, false, nil
			}

			p = stopPoint{stage: out.stage, critique: out.gate, fold: out.fold}
		}
	}

	if p.byCap, p.capUSD, err = c.stoppedByCap(ctx, bp.ID, p.stage); err != nil {
		return stopPoint{}, false, err
	}

	return p, true, nil
}

// stoppedByCap reports whether the newest start, end or refusal of a run is
// a cap stop of stageID, and the cap it stopped under. A run of stageID
// since says no, and so does a run of another stage, which places the
// pipeline past an older stop, or an error that names no stage, a failure.
// A stale reply's error stops nothing and is passed over.
func (c *Controller) stoppedByCap(ctx context.Context, id, stageID string) (bool, float64, error) {
	var events []store.Event

	for _, kind := range []string{EventStageStarted, EventStageEnded, EventError} {
		of, err := c.opts.Store.ListEventsOfKind(ctx, id, kind)
		if err != nil {
			return false, 0, fmt.Errorf("blueprint: read the %s events: %w", kind, err)
		}

		events = append(events, of...)
	}

	slices.SortFunc(events, func(a, b store.Event) int { return cmp.Compare(b.ID, a.ID) })

	for _, e := range events {
		var p struct {
			Stage  string  `json:"stage"`
			Reason string  `json:"reason"`
			CapUSD float64 `json:"cap_usd"`
		}

		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return false, 0, fmt.Errorf("blueprint: read event %d: %w", e.ID, err)
		}

		if e.Kind == EventError && p.Reason == reasonStaleReply {
			continue
		}

		if _, byCap := capStopOf(e); !byCap || p.Stage != stageID {
			return false, 0, nil
		}

		return true, p.CapUSD, nil
	}

	return false, 0, nil
}

// resume runs p again on slot r, which the caller holds, as a retry message
// would, but stores no note.
func (c *Controller) resume(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, p stopPoint) {
	switch {
	case p.round != nil:
		c.follow(context.WithoutCancel(ctx), runCtx, r, sess, bp, true)
	case p.critique:
		c.launchCritique(runCtx, r, sess, bp, p.stage)
	case p.fold != nil && !c.recordFold(ctx, sess.audit, bp.ID, p.fold):
		c.release(bp.ID, r)
	default:
		c.launch(runCtx, r, sess, bp, p.stage)
	}
}

// advance acts on what follows completed: it reports an end, and returns
// the stage to run next, or the ready end the caller hands to the sign-off
// when the pack has a stage that checks cards. A gate whose artifact still
// needs its critique is returned with critique set; any other gate is
// opened: shown, or decided under the auto policy and passed. A failure to
// decide fails the blueprint.
func (c *Controller) advance(ctx context.Context, sess *session, bp store.Blueprint, completed string) (string, outcome) {
	out, err := c.after(ctx, bp, completed)
	if err != nil {
		c.fail(ctx, sess.audit, bp.ID, 0, err.Error())

		return "", outcome{}
	}

	if out.fold != nil && !c.recordFold(ctx, sess.audit, bp.ID, out.fold) {
		return "", outcome{}
	}

	switch {
	case out.end:
		c.report(ctx, sess, bp, out)

		if out.ready && c.cardsStage() != "" {
			return "", out
		}

		return "", outcome{}
	case out.gate && out.critique:
		return out.stage, outcome{critique: true}
	case out.gate:
		return c.openGate(ctx, sess, bp, out)
	default:
		return out.stage, outcome{}
	}
}

// report ends the pipeline: the panel gets the artifact it ended on and the
// verdict, and the blueprint goes idle. rounds is the round the pipeline ended
// on, 0 when none applies. signoff tells the panel that the sign-off of ready
// cards follows on the held run slot.
func (c *Controller) report(ctx context.Context, sess *session, bp store.Blueprint, out outcome) {
	c.setState(ctx, bp.ID, StateIdle)

	sess.audit.event(ctx, Event{Kind: EventReport, BlueprintID: bp.ID, Payload: map[string]any{
		"stage": out.art.Stage, "body": out.art.Body, "ready": out.ready, "rounds": out.round, "findings": out.findings,
		"signoff": out.ready && c.cardsStage() != "",
	}})
}

// taskFor builds a stage's first user message: the need for a stage that
// consumes nothing, else the artifacts it consumes in pack order; for a
// stage run again after a rejection at its gate, the rejected artifact and
// every feedback; for a loop's target, its last submit and the loop's
// findings.
func (c *Controller) taskFor(ctx context.Context, bp store.Blueprint, st pack.Stage) (string, error) {
	var b strings.Builder

	if err := c.writeInputs(ctx, &b, bp, st, false, "task"); err != nil {
		return "", err
	}

	if err := c.writeAnswered(ctx, &b, bp, st); err != nil {
		return "", err
	}

	if err := c.writeAssumptions(ctx, &b, bp, st); err != nil {
		return "", err
	}

	if st.Gate {
		if err := c.writeGateFeedback(ctx, &b, bp, st, 0); err != nil {
			return "", err
		}
	}

	if err := c.writeLoopFindings(ctx, &b, bp, st); err != nil {
		return "", err
	}

	if err := c.writeNotes(ctx, &b, bp.ID); err != nil {
		return "", err
	}

	return b.String(), nil
}

// writeAssumptions gives a stage that consumes a stage with stated
// assumptions every assumption so far, each with its id and latest status:
// the ids a verdict names.
func (c *Controller) writeAssumptions(ctx context.Context, b *strings.Builder, bp store.Blueprint, st pack.Stage) error {
	states := slices.ContainsFunc(st.Consumes, func(id string) bool {
		consumed, _ := c.opts.Pack.Stage(id)

		return slices.Contains(consumed.Checks, pack.CheckAssumptions)
	})
	if !states {
		return nil
	}

	list, err := c.assumptions(ctx, bp.ID)
	if err != nil {
		return fmt.Errorf("blueprint: task for %s: %w", st.ID, err)
	}

	if len(list) == 0 {
		return nil
	}

	b.WriteString("## Assumptions so far\n\nEach with its id and latest status; a verdict names the id:\n\n")
	b.WriteString(assumptionLines(list) + "\n\n")

	return nil
}

// writeNotes gives a run every note the user left so far, in arrival order,
// each with the stage that was open when it came. A note never reaches a run
// already in flight; every run that starts after it gets it. A note that is
// only approve or reject is a misplaced decision, not guidance, and reaches
// no run.
func (c *Controller) writeNotes(ctx context.Context, b *strings.Builder, id string) error {
	return c.writeNotesAfter(ctx, b, id, 0)
}

// writeNotesAfter is writeNotes for the notes stored after event after.
func (c *Controller) writeNotesAfter(ctx context.Context, b *strings.Builder, id string, after int64) error {
	events, err := c.opts.Store.ListEventsOfKind(ctx, id, EventNote)
	if err != nil {
		return fmt.Errorf("blueprint: read the notes: %w", err)
	}

	var lines []string

	for _, e := range events {
		if e.ID <= after {
			continue
		}

		var p struct {
			Text  string `json:"text"`
			Stage string `json:"stage"`
		}

		// The controller wrote the payload itself, so a decode failure is a fault.
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("blueprint: read note %d: %w", e.ID, err)
		}

		if text := strings.TrimSpace(p.Text); text == "" || strings.EqualFold(text, store.GateApprove) || strings.EqualFold(text, store.GateReject) {
			continue
		}

		if p.Stage != "" {
			lines = append(lines, fmt.Sprintf("- (during %s) %s", p.Stage, p.Text))
		} else {
			lines = append(lines, "- "+p.Text)
		}
	}

	if len(lines) == 0 {
		return nil
	}

	b.WriteString("## Notes from the user\n\nLeft while the blueprint ran, oldest first; take them into account:\n\n")
	b.WriteString(strings.Join(lines, "\n") + "\n\n")

	return nil
}

// writeAnswered gives a run the questions earlier runs of its stage asked
// and got answered and the assumptions the user accepted on their tabs, so
// a stage run again after a crash, an End or a rejection neither asks them
// again nor drops an acceptance.
func (c *Controller) writeAnswered(ctx context.Context, b *strings.Builder, bp store.Blueprint, st pack.Stage) error {
	return c.writeAnsweredAfter(ctx, b, bp, st, 0)
}

// writeAnsweredAfter is writeAnswered for the questions and acceptances of
// the runs after run after.
func (c *Controller) writeAnsweredAfter(ctx context.Context, b *strings.Builder, bp store.Blueprint, st pack.Stage, after int64) error {
	questions, err := c.opts.Store.ListQuestions(ctx, bp.ID)
	if err != nil {
		return fmt.Errorf("blueprint: task for %s: %w", st.ID, err)
	}

	accepted, err := c.acceptedAssumptions(ctx, bp.ID)
	if err != nil {
		return fmt.Errorf("blueprint: task for %s: %w", st.ID, err)
	}

	var own []store.Question

	rows := map[string]bool{}

	for _, q := range questions {
		rows[q.Tag()] = true

		if q.Stage == st.ID && q.RunID > after {
			own = append(own, q)
		}
	}

	var ownAccepted []acceptedAssumption

	for _, a := range accepted {
		// A row an older close wrote is listed with the questions.
		if a.Stage == st.ID && a.RunID > after && !rows[a.Tag] {
			ownAccepted = append(ownAccepted, a)
		}
	}

	if len(own) > 0 {
		b.WriteString("## Questions this stage already asked\n\nThey are answered; use the answers and do not ask them again:\n\n")

		writeQuestions(b, own)
	}

	if len(ownAccepted) > 0 {
		b.WriteString("## Assumptions the user accepted at this stage\n\nState each with its tag as the basis:\n\n")

		for _, a := range ownAccepted {
			fmt.Fprintf(b, "- [%s] %s\n", a.Tag, a.Text)
		}

		b.WriteString("\n")
	}

	return nil
}

// Who answered a question, as every model that reads one is told.
const (
	answeredByUser      = "answered by the user"
	answeredByResponder = "answered by the responder for the user under the auto policy; an assumption, not the user's choice"
)

// answeredBy is the label for a question's recorded responder.
func answeredBy(q store.Question) string {
	if q.Responder == store.ResponderAuto {
		return answeredByResponder
	}

	return answeredByUser
}

// writeQuestions lists answered questions, one tagged line and its answer
// each, saying who gave the answer.
func writeQuestions(b *strings.Builder, questions []store.Question) {
	for _, q := range questions {
		fmt.Fprintf(b, "- [%s] %s\n  Answer (%s): %s\n", q.Tag(), q.Text, answeredBy(q), q.Answer)
	}

	b.WriteString("\n")
}

// writeInputs opens a task: the target projects, the need (always when
// needAlways, else only for a stage that consumes nothing) and the latest
// artifact of every stage st consumes. what names the task in an error.
func (c *Controller) writeInputs(ctx context.Context, b *strings.Builder, bp store.Blueprint, st pack.Stage, needAlways bool, what string) error {
	fmt.Fprintf(b, "Target projects: %s\n\n", strings.Join(bp.Projects, ", "))

	if needAlways || len(st.Consumes) == 0 {
		need, err := c.opts.Store.LatestArtifact(ctx, bp.ID, needStage)
		if err != nil {
			return fmt.Errorf("blueprint: %s for %s: %w", what, st.ID, err)
		}

		b.WriteString("Need:\n" + need.Body + "\n\n")
	}

	for _, id := range st.Consumes {
		art, err := c.opts.Store.LatestArtifact(ctx, bp.ID, id)
		if err != nil {
			return fmt.Errorf("blueprint: %s for %s: input %s: %w", what, st.ID, id, err)
		}

		writeArtifact(b, id, art)
	}

	return nil
}

// writeGateFeedback gives a run of st's attempt the artifact the attempt
// started from and the feedback of every rejection, when that artifact was
// rejected at the gate. Attempt 0 is a fresh one, which starts from the
// latest artifact.
func (c *Controller) writeGateFeedback(ctx context.Context, b *strings.Builder, bp store.Blueprint, st pack.Stage, attempt int) error {
	feedback, err := c.gateFeedback(ctx, bp.ID, st.ID)
	if err != nil {
		return fmt.Errorf("blueprint: task for %s: %w", st.ID, err)
	}

	if len(feedback) == 0 {
		return nil
	}

	rejected, found, err := c.startedFrom(ctx, bp.ID, st.ID, attempt)
	if err != nil {
		return fmt.Errorf("blueprint: task for %s: the rejected artifact: %w", st.ID, err)
	}

	if !found {
		return nil
	}

	// A later version approved at the gate was not rejected, and the
	// feedback on earlier versions is already addressed in it.
	decision, err := c.gateDecision(ctx, bp.ID, st.ID, rejected.Version)
	if err != nil {
		return fmt.Errorf("blueprint: task for %s: %w", st.ID, err)
	}

	if decision != store.GateReject {
		return nil
	}

	writeArtifact(b, fmt.Sprintf("%s version %d, which was rejected at its gate", st.ID, rejected.Version), rejected)
	b.WriteString("## Gate feedback\n\nEvery rejection at this stage's gate, oldest first; the new version must address all of it:\n\n")

	for _, f := range feedback {
		b.WriteString("- " + f + "\n")
	}

	b.WriteString("\n")

	return nil
}

// startedFrom is the artifact of stageID that attempt started from: the
// latest, less the version each completed round of the attempt stored.
// found is false when the attempt started from none.
func (c *Controller) startedFrom(ctx context.Context, id, stageID string, attempt int) (store.Artifact, bool, error) {
	latest, err := c.opts.Store.LatestArtifact(ctx, id, stageID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Artifact{}, false, nil
	case err != nil:
		return store.Artifact{}, false, err
	}

	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return store.Artifact{}, false, fmt.Errorf("list runs: %w", err)
	}

	version := latest.Version

	for _, r := range runs {
		if r.Kind == store.RunKindStage && r.Stage == stageID && r.Attempt == attempt && r.Status == store.RunDone {
			version--
		}
	}

	if version == latest.Version {
		return latest, true, nil
	}

	arts, err := c.opts.Store.ListArtifacts(ctx, id)
	if err != nil {
		return store.Artifact{}, false, fmt.Errorf("list artifacts: %w", err)
	}

	for _, a := range arts {
		if a.Stage == stageID && a.Version == version {
			return a, true, nil
		}
	}

	return store.Artifact{}, false, nil
}

// writeLoopFindings gives a stage a loop re-runs what it submitted last,
// the loop stage's verdict on it and the sign-off's feedback.
func (c *Controller) writeLoopFindings(ctx context.Context, b *strings.Builder, bp store.Blueprint, st pack.Stage) error {
	for _, other := range c.opts.Pack.Stages {
		if other.LoopsTo != st.ID {
			continue
		}

		verdict, err := c.opts.Store.LatestArtifact(ctx, bp.ID, other.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return fmt.Errorf("blueprint: task for %s: findings of %s: %w", st.ID, other.ID, err)
		}

		judged, err := c.opts.Store.LatestArtifact(ctx, bp.ID, st.ID)
		if err != nil {
			return fmt.Errorf("blueprint: task for %s: the judged %s: %w", st.ID, st.ID, err)
		}

		writeArtifact(b, fmt.Sprintf("%s version %d, which %s judged", st.ID, judged.Version, other.ID), judged)
		writeArtifact(b, fmt.Sprintf("%s findings (round %d)", other.ID, verdict.Version), verdict)

		feedback, err := c.gateFeedback(ctx, bp.ID, signoffStage)
		if err != nil {
			return fmt.Errorf("blueprint: task for %s: %w", st.ID, err)
		}

		if len(feedback) > 0 {
			b.WriteString("## Sign-off feedback\n\nA human rejected earlier cards with this feedback; address all of it:\n\n")

			for _, f := range feedback {
				b.WriteString("- " + f + "\n")
			}

			b.WriteString("\n")
		}
	}

	return nil
}

// writeArtifact renders an artifact for a task: its body, then its data,
// where structured output such as the cards lives, as indented JSON.
func writeArtifact(b *strings.Builder, heading string, art store.Artifact) {
	fmt.Fprintf(b, "## %s\n\n%s\n\n", heading, art.Body)

	if len(art.Data) == 0 {
		return
	}

	var data bytes.Buffer
	if err := json.Indent(&data, art.Data, "", "  "); err != nil {
		// Stored data is JSON; show it as stored rather than drop it.
		data.Reset()
		data.Write(art.Data)
	}

	fmt.Fprintf(b, "Data:\n\n```json\n%s\n```\n\n", data.String())
}

// validatorState is what a submit check saw on the latest submit: the round
// and the problems of what it accepted or refused.
type validatorState struct {
	mu       sync.Mutex
	rounds   int
	findings []string
}

// last returns the problems of the latest submit.
func (s *validatorState) last() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.findings)
}

// round is how many submits the check has judged; on an accepted submit,
// the round it was accepted on.
func (s *validatorState) round() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.rounds
}

// cardsValidator returns decomposition's submit validator: the draft's own
// problems and the contract run on every submit, and a failing submit goes
// back to the model. Every submit reads the project list from ContextMatrix
// again, so a fix made there during the run counts at the next submit; a
// read of ContextMatrix or the checkout that fails refuses the submit
// without spending a round. The third failing submit is accepted with its
// findings recorded, so the pipeline stops and reports instead of looping
// until the run's turns are spent. When the pack has acceptance, every
// criterion must be carried by exactly one card of its project and every
// manual check by a manual entry; acceptance data that does not decode is a
// finding of its own. A draft that does not decode is refused without
// spending a round.
func (c *Controller) cardsValidator(sess *session, bp store.Blueprint) (func(context.Context, stage.Artifact) error, *validatorState) {
	state := &validatorState{}
	lists := c.opts.Pack.Lists()

	return func(ctx context.Context, a stage.Artifact) error {
		draft, err := contract.ParseDraft(a.Data)
		if err != nil {
			return err
		}

		projects, err := c.freshView(ctx, sess, bp)
		if err != nil {
			return fmt.Errorf("the cards were not judged (%w); submit the same draft again", err)
		}

		problems := draftProblems(draft)
		for _, f := range contract.Check(draft, lists, projects) {
			problems = append(problems, f.String())
		}

		acc, acceptanceID, found, err := c.acceptanceOf(ctx, bp.ID)
		if err != nil {
			return fmt.Errorf("the cards were not judged (%w); submit the same draft again", err)
		}

		switch {
		case !found:
		case acc.unreadable != nil:
			problems = append(problems, problem(acceptanceID, "criteria_unreadable", acc.unreadable.Error()))
		case len(acc.criteria) == 0:
			problems = append(problems, problem(acceptanceID, "criteria_empty", "acceptance defines no criterion, so nothing maps to the cards"))
		default:
			for _, f := range contract.CheckCriteria(draft, acc.criteria, acc.manual) {
				problems = append(problems, f.String())
			}
		}

		state.mu.Lock()
		defer state.mu.Unlock()

		state.rounds++
		state.findings = problems

		if len(problems) == 0 || state.rounds >= maxContractRounds {
			return nil
		}

		return fmt.Errorf("%d problem(s) with the cards; fix them and submit again (submit %d of %d):\n%s",
			len(problems), state.rounds, maxContractRounds, strings.Join(problems, "\n"))
	}, state
}

// freshView reads the project list from ContextMatrix and builds the
// contract's view of the blueprint's target projects from it.
func (c *Controller) freshView(ctx context.Context, sess *session, bp store.Blueprint) (map[string]contract.Project, error) {
	projects, err := c.opts.Projects(ctx, sess.mcpKey)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	return contractView(ctx, projects, bp.Projects, c.opts.Workspace.TreeRoot(bp.ID))
}

// draftProblems are the faults of the draft as a whole: it holds no cards,
// several cards come without a playbook title, or its order does not list
// every card and manual entry exactly once and nothing else; emission walks
// the order as given.
func draftProblems(d contract.Draft) []string {
	if len(d.Cards) == 0 {
		return []string{"the draft has no cards"}
	}

	var problems []string

	if len(d.Cards) > 1 && strings.TrimSpace(d.Title) == "" {
		problems = append(problems, "the draft has several cards, so it needs a playbook title")
	}

	seen := map[string]int{}
	for _, id := range d.Order {
		seen[id]++
	}

	ids := make([]string, 0, len(d.Cards)+len(d.Manual))
	for _, card := range d.Cards {
		ids = append(ids, card.ID)
	}

	for _, m := range d.Manual {
		ids = append(ids, m.ID)
	}

	for _, id := range ids {
		switch seen[id] {
		case 0:
			problems = append(problems, id+" is missing from order")
		case 1:
		default:
			problems = append(problems, id+" appears more than once in order")
		}

		delete(seen, id)
	}

	for _, id := range d.Order {
		if _, stray := seen[id]; stray {
			problems = append(problems, id+" in order is not a card or a manual entry")

			delete(seen, id)
		}
	}

	// The playbook runner walks entries in order and a card waits for its
	// dependencies, so a dependency later in order would stall the run.
	at := map[string]int{}
	for i, id := range d.Order {
		if _, seen := at[id]; !seen {
			at[id] = i
		}
	}

	for _, card := range d.Cards {
		for _, dep := range card.DependsOn {
			cd, cardIn := at[card.ID]
			pd, depIn := at[dep]

			if cardIn && depIn && pd > cd {
				problems = append(problems, fmt.Sprintf("%s comes before its dependency %s in order", card.ID, dep))
			}
		}
	}

	return problems
}

// artifactStopped reports whether an artifact's data carries the stopped
// flag the controller sets when cards were accepted with findings.
func artifactStopped(a store.Artifact) bool {
	var data struct {
		Stopped bool `json:"stopped"`
	}

	return len(a.Data) > 0 && json.Unmarshal(a.Data, &data) == nil && data.Stopped
}

// findingsOf reads the findings stored with cards accepted on the last
// contract round.
func findingsOf(a store.Artifact) []string {
	var data struct {
		Findings []string `json:"findings"`
	}

	if len(a.Data) > 0 && json.Unmarshal(a.Data, &data) != nil {
		return []string{"the stored findings do not decode"}
	}

	return data.Findings
}

// cardIDs reads the card ids of a draft's data, in draft order; nil when
// the artifact holds no cards.
func cardIDs(a store.Artifact) []string {
	var data struct {
		Cards []struct {
			ID string `json:"id"`
		} `json:"cards"`
	}

	if len(a.Data) == 0 || json.Unmarshal(a.Data, &data) != nil {
		return nil
	}

	ids := make([]string, 0, len(data.Cards))
	for _, card := range data.Cards {
		ids = append(ids, card.ID)
	}

	return ids
}

// verdictOf reads a loop stage's per-card verdicts against the draft it
// judged: ready only when it judged at least one card, every draft card is
// among them, and every card it judged is ready and carries no finding.
func verdictOf(a store.Artifact, draft []string) (bool, []string) {
	var data struct {
		Cards []struct {
			ID       string   `json:"id"`
			Ready    bool     `json:"ready"`
			Findings []string `json:"findings"`
		} `json:"cards"`
	}

	if len(a.Data) == 0 || json.Unmarshal(a.Data, &data) != nil || len(data.Cards) == 0 {
		return false, []string{"the stage reported no cards"}
	}

	var findings []string

	judged := map[string]bool{}

	for _, card := range data.Cards {
		judged[card.ID] = true

		if card.Ready && len(card.Findings) == 0 {
			continue
		}

		for _, f := range card.Findings {
			findings = append(findings, card.ID+": "+f)
		}

		if len(card.Findings) == 0 {
			findings = append(findings, card.ID+": not ready")
		}
	}

	for _, id := range draft {
		if !judged[id] {
			findings = append(findings, id+": not judged")
		}
	}

	return len(findings) == 0, findings
}

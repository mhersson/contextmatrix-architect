package blueprint

import (
	"bytes"
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

// outcome is what follows a stage: a model stage to run, a human-sourced
// stage to await, or the pipeline's end with the artifact it ended on.
type outcome struct {
	stage    string
	human    bool
	end      bool
	ready    bool
	findings []string
	art      store.Artifact
}

// after decides what follows completed from the stored artifacts; an empty
// completed means no stage has completed yet. A loop stage's verdict ends
// the pipeline or re-runs its target, cards accepted with findings end it,
// and otherwise the next stage in pack order follows. A human-sourced stage
// is written once, so one that already holds its artifact is passed.
func (c *Controller) after(ctx context.Context, bp store.Blueprint, completed string) (outcome, error) {
	from := 0

	var last store.Artifact

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
			// The loop stage judges its target's cards; its artifact version
			// is its round.
			target, err := c.opts.Store.LatestArtifact(ctx, bp.ID, st.LoopsTo)
			if err != nil {
				return outcome{}, fmt.Errorf("blueprint: after %s: the judged %s: %w", completed, st.LoopsTo, err)
			}

			ready, findings := verdictOf(art, cardIDs(target))

			switch {
			case ready:
				return outcome{end: true, ready: true, art: art}, nil
			case art.Version >= st.MaxRounds:
				return outcome{end: true, findings: findings, art: art}, nil
			default:
				return outcome{stage: st.LoopsTo}, nil
			}
		}

		// Only a checked stage's stopped flag is the controller's own.
		if slices.Contains(st.Checks, pack.CheckCards) && artifactStopped(art) {
			return outcome{end: true, findings: findingsOf(art), art: art}, nil
		}

		from = slices.IndexFunc(c.opts.Pack.Stages, func(s pack.Stage) bool { return s.ID == completed }) + 1
		last = art
	}

	for _, st := range c.opts.Pack.Stages[from:] {
		if st.Source != pack.SourceHuman {
			return outcome{stage: st.ID}, nil
		}

		_, err := c.opts.Store.LatestArtifact(ctx, bp.ID, st.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			return outcome{stage: st.ID, human: true}, nil
		case err != nil:
			return outcome{}, fmt.Errorf("blueprint: after %s: %w", completed, err)
		}
	}

	if completed == "" {
		return outcome{}, nil
	}

	return outcome{end: true, ready: true, art: last}, nil
}

// position reads where a blueprint with no run in flight stands: what
// follows the stage whose run completed last, and, when that is a model
// stage, the status of its newest run since then ("" when it has not
// started there).
func (c *Controller) position(ctx context.Context, bp store.Blueprint) (outcome, string, error) {
	runs, err := c.opts.Store.ListRuns(ctx, bp.ID)
	if err != nil {
		return outcome{}, "", fmt.Errorf("blueprint: list runs: %w", err)
	}

	completed, since := "", 0

	for i, r := range runs {
		if r.Status == store.RunDone {
			completed, since = r.Stage, i+1
		}
	}

	out, err := c.after(ctx, bp, completed)
	if err != nil || out.stage == "" || out.human {
		return out, "", err
	}

	status := ""

	for _, r := range runs[since:] {
		if r.Stage == out.stage {
			status = r.Status
		}
	}

	return out, status, nil
}

// resumePoint decides what a reopened blueprint does: re-run a stage a
// crash left running, run a model stage whose inputs exist but which never
// started (a crash between storing one artifact and starting the next run),
// or await a human-sourced stage, returned with human set. A failed or
// aborted stage waits for the user's next message and an ended pipeline for
// a new blueprint. A position the record does not yield is reported, not
// returned: the session still attaches. The caller holds the run slot.
func (c *Controller) resumePoint(ctx context.Context, au audit, bp store.Blueprint) (outcome, error) {
	interrupted, err := c.interruptedStage(ctx, bp)
	if err != nil || interrupted != "" {
		return outcome{stage: interrupted}, err
	}

	out, status, err := c.position(ctx, bp)

	switch {
	case err != nil:
		c.unreadable(ctx, au, bp.ID, err)

		return outcome{}, nil
	case out.human:
		return out, nil
	case out.stage == "" || status != "":
		return outcome{}, nil
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

// await marks the blueprint waiting for a human-sourced stage and shows the
// stage's prompt.
func (c *Controller) await(ctx context.Context, au audit, id, stageID string) {
	c.setState(ctx, id, StateWaiting)
	au.event(ctx, Event{Kind: EventAwaitInput, BlueprintID: id, Payload: map[string]any{
		"stage": stageID, "prompt": c.opts.Pack.Instructions(stageID),
	}})
}

// input handles a message that arrives while no run is in flight: the need
// while the first stage has no artifact, the artifact of a human-sourced
// stage that waits for it, else a note. The caller holds the run slot.
func (c *Controller) input(ctx, runCtx context.Context, r *run, sess *session, id, text, msgID string) error {
	bp, err := c.opts.Store.GetBlueprint(ctx, id)
	if err != nil {
		c.release(id, r)

		return fmt.Errorf("blueprint: message: %w", err)
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
		sess.audit.event(ctx, noteEvent(id, text, msgID, false))
		c.unreadable(ctx, sess.audit, id, err)
		c.release(id, r)

		return nil
	}

	if out.human {
		return c.paste(ctx, runCtx, r, sess, bp, out.stage, text, msgID)
	}

	// A note. While a model stage is pending (it failed, was aborted or
	// refused, or a cancel fell between stages) the note also re-runs it,
	// so an abort is not a dead end; an ended pipeline is not re-run.
	note := noteEvent(id, text, msgID, false)
	if out.stage != "" {
		note.Payload["retries"] = out.stage
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

// paste stores the message as the artifact of the waiting human-sourced
// stage, then runs or awaits what follows it.
func (c *Controller) paste(ctx, runCtx context.Context, r *run, sess *session, bp store.Blueprint, stageID, text, msgID string) error {
	version, err := sess.audit.artifact(ctx, bp.ID, stageID, text, nil)
	if err != nil {
		c.release(bp.ID, r)

		return err
	}

	sess.audit.event(ctx, Event{Kind: EventArtifact, BlueprintID: bp.ID, Payload: map[string]any{
		"stage": stageID, "version": version, "body": text, "source": pack.SourceHuman, "message_id": msgID,
	}})

	next := c.advance(ctx, sess, bp, stageID)
	if next == "" {
		c.release(bp.ID, r)

		return nil
	}

	c.launch(runCtx, r, sess, bp, next)

	return nil
}

// advance acts on what follows completed: it awaits a human-sourced stage
// or reports the end and returns "", or returns the stage to run next. A
// failure to decide fails the blueprint.
func (c *Controller) advance(ctx context.Context, sess *session, bp store.Blueprint, completed string) string {
	out, err := c.after(ctx, bp, completed)
	if err != nil {
		c.fail(ctx, sess.audit, bp.ID, 0, err.Error())

		return ""
	}

	switch {
	case out.end:
		c.report(ctx, sess, bp, out)

		return ""
	case out.human:
		c.await(ctx, sess.audit, bp.ID, out.stage)

		return ""
	default:
		return out.stage
	}
}

// report ends the pipeline: the panel gets the artifact it ended on and the
// verdict, and the blueprint goes idle. rounds is that artifact's version,
// which for a loop stage is its round.
func (c *Controller) report(ctx context.Context, sess *session, bp store.Blueprint, out outcome) {
	c.setState(ctx, bp.ID, StateIdle)
	sess.audit.event(ctx, Event{Kind: EventReport, BlueprintID: bp.ID, Payload: map[string]any{
		"stage": out.art.Stage, "body": out.art.Body, "ready": out.ready, "rounds": out.art.Version, "findings": out.findings,
	}})
}

// taskFor builds a stage's first user message from the artifacts it
// consumes, in pack order, plus the loop findings when it is re-run.
func (c *Controller) taskFor(ctx context.Context, bp store.Blueprint, st pack.Stage) (string, error) {
	var b strings.Builder

	fmt.Fprintf(&b, "Target projects: %s\n\n", strings.Join(bp.Projects, ", "))

	if len(st.Consumes) == 0 {
		need, err := c.opts.Store.LatestArtifact(ctx, bp.ID, needStage)
		if err != nil {
			return "", fmt.Errorf("blueprint: task for %s: %w", st.ID, err)
		}

		b.WriteString("Need:\n" + need.Body)

		return b.String(), nil
	}

	for _, id := range st.Consumes {
		art, err := c.opts.Store.LatestArtifact(ctx, bp.ID, id)
		if err != nil {
			return "", fmt.Errorf("blueprint: task for %s: input %s: %w", st.ID, id, err)
		}

		writeArtifact(&b, id, art)
	}

	// A stage re-run by a loop sees what it submitted last and the loop
	// stage's verdict on it.
	for _, other := range c.opts.Pack.Stages {
		if other.LoopsTo != st.ID {
			continue
		}

		verdict, err := c.opts.Store.LatestArtifact(ctx, bp.ID, other.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return "", fmt.Errorf("blueprint: task for %s: findings of %s: %w", st.ID, other.ID, err)
		}

		judged, err := c.opts.Store.LatestArtifact(ctx, bp.ID, st.ID)
		if err != nil {
			return "", fmt.Errorf("blueprint: task for %s: the judged %s: %w", st.ID, st.ID, err)
		}

		writeArtifact(&b, fmt.Sprintf("%s version %d, which %s judged", st.ID, judged.Version, other.ID), judged)
		writeArtifact(&b, fmt.Sprintf("%s findings (round %d)", other.ID, verdict.Version), verdict)
	}

	return b.String(), nil
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

// validatorState is what decomposition's validator saw on the latest
// submit: the round and the problems of the cards it accepted or refused.
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

// cardsValidator returns decomposition's submit validator: the draft's own
// problems and the contract run on every submit, and a failing submit goes
// back to the model. The third failing submit is accepted with its findings
// recorded, so the pipeline stops and reports instead of looping until the
// run's turns are spent. A draft that does not decode is refused without
// spending a round.
func (c *Controller) cardsValidator(projects map[string]contract.Project) (func(stage.Artifact) error, *validatorState) {
	state := &validatorState{}
	lists := c.opts.Pack.Lists()

	return func(a stage.Artifact) error {
		draft, err := contract.ParseDraft(a.Data)
		if err != nil {
			return err
		}

		problems := draftProblems(draft)
		for _, f := range contract.Check(draft, lists, projects) {
			problems = append(problems, f.String())
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

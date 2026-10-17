package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// maxReviewRounds is how many reviewer rounds one attempt of a stage gets;
// a score still below the threshold then goes to a human.
const maxReviewRounds = 3

// review is a reviewer's verdict on one version of a stage's artifact,
// stored as an artifact of the pseudo-stage <stage>.review.
type review struct {
	Score    int      `json:"score"`
	Findings []string `json:"findings"`
	Version  int      `json:"version"`
	Attempt  int      `json:"attempt"`
	Round    int      `json:"round"`
}

// round is a builder round after a review, a low one's next round or a
// passing one's revision: the attempt it continues, its number, the
// conversation it resumes (nil starts over) and the next message.
type round struct {
	attempt, number int
	history         []llm.Message
	task            string
}

// reviewStage names the pseudo-stage a stage's verdicts are stored under.
func reviewStage(stageID string) string { return stageID + ".review" }

// parseReview reads a reviewer's submit: a whole score from 1 to 5 and a
// list of findings, each a sentence.
func parseReview(data map[string]any) (review, error) {
	score, ok := data["score"].(float64)
	if !ok || score != float64(int(score)) || score < 1 || score > 5 {
		return review{}, errors.New("score must be a whole number from 1 to 5")
	}

	v := review{Score: int(score)}

	if raw, ok := data["findings"]; ok && raw != nil {
		items, isList := raw.([]any)
		if !isList {
			return review{}, errors.New("findings must be a list of sentences")
		}

		for i, item := range items {
			s, isText := item.(string)
			if !isText || strings.TrimSpace(s) == "" {
				return review{}, fmt.Errorf("finding %d must be a sentence", i+1)
			}

			v.Findings = append(v.Findings, strings.TrimSpace(s))
		}
	}

	return v, nil
}

// newestReview is the newest verdict on any version of stageID's artifact;
// found is false when none exists.
func (c *Controller) newestReview(ctx context.Context, id, stageID string) (review, bool, error) {
	art, err := c.opts.Store.LatestArtifact(ctx, id, reviewStage(stageID))

	switch {
	case errors.Is(err, store.ErrNotFound):
		return review{}, false, nil
	case err != nil:
		return review{}, false, fmt.Errorf("blueprint: read the review of %s: %w", stageID, err)
	}

	var v review
	if err := json.Unmarshal(art.Data, &v); err != nil {
		return review{}, false, fmt.Errorf("blueprint: decode the review of %s: %w", stageID, err)
	}

	return v, true, nil
}

// owesRevision reports a verdict that passed with findings: the builder
// revises once more, and the revision goes to its gate unreviewed.
func owesRevision(st pack.Stage, v review) bool {
	return v.Score >= st.Reviewer.Threshold && len(v.Findings) > 0
}

// settled reports a verdict that leaves its artifact to the gate: a pass
// without findings, or a low score on the attempt's last round.
func settled(st pack.Stage, v review) bool {
	return !owesRevision(st, v) && (v.Score >= st.Reviewer.Threshold || v.Round >= maxReviewRounds)
}

// gateReview is the verdict that decides st's gate on art: the newest
// verdict when it judged art, or when art is the revision it owed (revised).
// found is false otherwise, and for a stage without a reviewer.
func (c *Controller) gateReview(ctx context.Context, id string, st pack.Stage, art store.Artifact) (v review, found, revised bool, err error) {
	if st.Reviewer == nil {
		return review{}, false, false, nil
	}

	v, found, err = c.newestReview(ctx, id, st.ID)

	switch {
	case err != nil || !found:
		return review{}, false, false, err
	case v.Version == art.Version:
		return v, true, false, nil
	case v.Version+1 != art.Version || !owesRevision(st, v):
		return review{}, false, false, nil
	}

	// Within an attempt the only version after a passing verdict is its
	// revision; a rejection at the gate starts a new attempt.
	built, err := c.latestStageRun(ctx, id, st.ID)
	if err != nil {
		return review{}, false, false, err
	}

	if built.Attempt != v.Attempt {
		return review{}, false, false, nil
	}

	return v, true, true, nil
}

// reviewRounds counts the verdicts on stageID in attempt.
func (c *Controller) reviewRounds(ctx context.Context, id, stageID string, attempt int) (int, error) {
	arts, err := c.opts.Store.ListArtifacts(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("blueprint: count the reviews of %s: %w", stageID, err)
	}

	n := 0

	for _, a := range arts {
		var v review
		if a.Stage == reviewStage(stageID) && json.Unmarshal(a.Data, &v) == nil && v.Attempt == attempt {
			n++
		}
	}

	return n, nil
}

// critiquePending reports whether a gated stage's artifact still needs its
// reviewer or its builder: no verdict on it yet, one below the threshold
// with a builder round left in the attempt, or one that passed with
// findings, which owes a revision. A revision needs neither. A gate already
// shown needs none: it stays with the human it was put to, even without a
// verdict or with a revision owed.
func (c *Controller) critiquePending(ctx context.Context, id string, st pack.Stage, art store.Artifact) (bool, error) {
	if st.Reviewer == nil {
		return false, nil
	}

	v, found, revised, err := c.gateReview(ctx, id, st, art)
	if err != nil || revised {
		return false, err
	}

	if found && settled(st, v) {
		return false, nil
	}

	shown, err := c.gateShown(ctx, id, st.ID, art.Version)
	if err != nil {
		return false, err
	}

	return !shown, nil
}

// latestStageRun is the newest completed stage run of stageID: the run
// that built its latest artifact.
func (c *Controller) latestStageRun(ctx context.Context, id, stageID string) (store.Run, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return store.Run{}, fmt.Errorf("blueprint: list runs: %w", err)
	}

	var built store.Run

	for _, r := range runs {
		if r.Kind == store.RunKindStage && r.Stage == stageID && r.Status == store.RunDone {
			built = r
		}
	}

	if built.ID == 0 {
		return store.Run{}, fmt.Errorf("blueprint: %s has no completed run", stageID)
	}

	return built, nil
}

// critique reviews a gated stage's latest artifact, unless a verdict on it
// exists, and runs the builder's next round with the findings when the
// score is below the threshold with a round left in the attempt, or its
// revision when the score passed with findings. A revision is never
// reviewed. false when a run did not complete, which has been reported.
func (c *Controller) critique(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string) bool {
	bg := context.Background()
	au := sess.audit

	st, ok := c.opts.Pack.Stage(stageID)
	if !ok || st.Reviewer == nil {
		c.fail(bg, au, bp.ID, 0, fmt.Sprintf("review %s: the pack has no reviewer for it", stageID))

		return false
	}

	art, err := c.opts.Store.LatestArtifact(bg, bp.ID, stageID)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, fmt.Sprintf("review %s: %v", stageID, err))

		return false
	}

	built, err := c.latestStageRun(bg, bp.ID, stageID)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return false
	}

	v, found, revised, err := c.gateReview(bg, bp.ID, st, art)

	switch {
	case err != nil:
		c.fail(bg, au, bp.ID, 0, err.Error())

		return false
	case revised:
		return true
	case !found:
		var ok bool
		if v, ok = c.runReviewer(ctx, r, sess, bp, st, art, built.Attempt); !ok {
			return false
		}
	}

	if settled(st, v) {
		return true
	}

	rd, err := c.builderRound(bg, bp, st, built, art, v)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return false
	}

	return c.runStage(ctx, r, sess, bp, stageID, rd)
}

// builderRound is the round after a low verdict, or the revision after a
// passing one with findings: the builder's conversation so far, from the
// stored transcript of the run that built the artifact, and as the next
// message the findings and what that conversation has not seen: the notes,
// and the answers to questions a round cut short asked. Without a transcript
// the round starts over, given its own artifact and the findings; its task
// carries every note and answer already.
func (c *Controller) builderRound(ctx context.Context, bp store.Blueprint, st pack.Stage, built store.Run, art store.Artifact, v review) (*round, error) {
	var b strings.Builder

	rd := &round{attempt: built.Attempt, number: built.Round + 1, history: c.builtHistory(ctx, bp.ID, built)}

	if len(rd.history) == 0 {
		rd.history = nil

		writeArtifact(&b, fmt.Sprintf("Your %s, version %d", st.ID, art.Version), art)
	}

	fmt.Fprintf(&b, "## Reviewer findings (round %d)\n\n", v.Round)

	if owesRevision(st, v) {
		fmt.Fprintf(&b, "The reviewer passed your artifact at %d of 5 with these findings. Revise it to address every finding, then submit it again; the revised version goes to its gate without another review:\n\n", v.Score)
	} else {
		fmt.Fprintf(&b, "The reviewer scored your artifact %d of 5; %d passes. Address every finding, then submit the artifact again:\n\n", v.Score, st.Reviewer.Threshold)
	}

	for _, f := range v.Findings {
		b.WriteString("- " + f + "\n")
	}

	b.WriteString("\n")

	if rd.history != nil {
		if err := c.writeNewNotes(ctx, &b, bp.ID, built.ID); err != nil {
			return nil, err
		}

		// Run ids rise, and the resumed conversation ends with built.
		if err := c.writeAnsweredAfter(ctx, &b, bp, st, built.ID); err != nil {
			return nil, err
		}
	}

	rd.task = b.String()

	return rd, nil
}

// builtHistory is the conversation of the run that built the artifact,
// without its system message; nil when the run stored no transcript. A
// transcript that cannot be read is logged, and the round starts over.
func (c *Controller) builtHistory(ctx context.Context, id string, built store.Run) []llm.Message {
	raw, err := c.opts.Store.GetTranscript(ctx, built.ID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		c.opts.Logger.Warn("blueprint: read the builder's transcript; the round starts over", "blueprint", id, "run", built.ID, "error", err)

		return nil
	}

	var history []llm.Message
	if err := json.Unmarshal(raw, &history); err != nil {
		c.opts.Logger.Warn("blueprint: decode the builder's transcript; the round starts over", "blueprint", id, "run", built.ID, "error", err)

		return nil
	}

	return withoutSystem(history)
}

// writeNewNotes gives a round that resumes runID's conversation the notes
// stored after that run started; its task carried the earlier ones. A run
// whose start was not recorded gets every note.
func (c *Controller) writeNewNotes(ctx context.Context, b *strings.Builder, id string, runID int64) error {
	started, err := c.opts.Store.ListEventsOfKind(ctx, id, EventStageStarted)
	if err != nil {
		return fmt.Errorf("blueprint: read the run starts: %w", err)
	}

	var after int64

	for _, e := range started {
		if e.RunID == runID {
			after = e.ID
		}
	}

	return c.writeNotesAfter(ctx, b, id, after)
}

// withoutSystem drops a conversation's system message: a run's own comes
// first, from its persona and instructions.
func withoutSystem(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))

	for _, m := range msgs {
		if m.Role != "system" {
			out = append(out, m)
		}
	}

	return out
}

// runReviewer judges a gated stage's artifact with the pack's reviewer: its
// persona, the review instructions and reviewTask, never the builder's
// transcript, and no tools. The verdict is stored as the next <stage>.review
// artifact and announced, and the blueprint is idle until the next step
// sets its state. false when the run did not complete, which is reported.
func (c *Controller) runReviewer(ctx context.Context, r *run, sess *session, bp store.Blueprint, st pack.Stage, art store.Artifact, attempt int) (review, bool) {
	bg := context.Background()
	au := sess.audit

	rounds, err := c.reviewRounds(bg, bp.ID, st.ID, attempt)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return review{}, false
	}

	task, err := c.reviewTask(bg, bp, st, art, attempt)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return review{}, false
	}

	out, err := c.runSide(ctx, r, sess, bp, sideRun{
		kind: store.RunKindReviewer, stage: st.ID, persona: st.Reviewer.Persona, attempt: attempt, round: rounds + 1,
		in: stage.Input{
			Stage: reviewStage(st.ID), Persona: c.opts.Pack.Persona(st.Reviewer.Persona),
			Instructions: c.opts.Pack.ReviewInstructions(), Task: task,
			Validate: func(_ context.Context, a stage.Artifact) error {
				_, err := parseReview(a.Data)

				return err
			},
		},
	})
	if err != nil {
		c.sideFailed(ctx, r, au, bp.ID, st.ID, err)

		return review{}, false
	}

	v, err := parseReview(out.Artifact.Data)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, fmt.Sprintf("review of %s: %v", st.ID, err))

		return review{}, false
	}

	v.Version, v.Attempt, v.Round = art.Version, attempt, rounds+1

	findings := v.Findings
	if findings == nil {
		findings = []string{}
	}

	if _, err := au.artifact(bg, bp.ID, reviewStage(st.ID), out.Artifact.Body, map[string]any{
		"score": v.Score, "findings": findings, "version": v.Version, "attempt": v.Attempt, "round": v.Round,
	}); err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return review{}, false
	}

	au.event(bg, Event{Kind: EventReview, BlueprintID: bp.ID, Payload: map[string]any{
		"stage": st.ID, "version": art.Version, "score": v.Score, "threshold": st.Reviewer.Threshold,
		"findings": findings, "round": v.Round,
	}})

	c.setState(bg, bp.ID, StateIdle)

	return v, true
}

// reviewTask is what a reviewer judges: the stage's inputs, the questions
// it got answered, the assumptions, the gate feedback the attempt addresses,
// the artifacts folded into its gate, each with the gate feedback its own
// attempt addresses, the artifact, whose data holds any problems it was
// accepted with, the user's notes and the score that passes.
func (c *Controller) reviewTask(ctx context.Context, bp store.Blueprint, st pack.Stage, art store.Artifact, attempt int) (string, error) {
	var b strings.Builder

	if err := c.writeInputs(ctx, &b, bp, st, false, "review task"); err != nil {
		return "", err
	}

	// The answers and the gate feedback are the user's directions; a
	// decision that rests on one is grounded.
	if err := c.writeAnswered(ctx, &b, bp, st); err != nil {
		return "", err
	}

	if err := c.writeAssumptions(ctx, &b, bp, st); err != nil {
		return "", err
	}

	if st.Gate {
		if err := c.writeGateFeedback(ctx, &b, bp, st, attempt); err != nil {
			return "", err
		}
	}

	folded, err := c.folded(ctx, bp.ID, st.ID)
	if err != nil {
		return "", err
	}

	for _, f := range folded {
		fst, _ := c.opts.Pack.Stage(f.Stage)

		built, err := c.latestStageRun(ctx, bp.ID, f.Stage)
		if err != nil {
			return "", err
		}

		if err := c.writeGateFeedback(ctx, &b, bp, fst, built.Attempt); err != nil {
			return "", err
		}

		writeArtifact(&b, fmt.Sprintf("%s version %d, whose gate folded into this one: judge it too", f.Stage, f.Version), f)
	}

	writeArtifact(&b, fmt.Sprintf("%s version %d, the artifact you judge", st.ID, art.Version), art)

	// The user's notes are part of what the artifact must satisfy.
	if err := c.writeNotes(ctx, &b, bp.ID); err != nil {
		return "", fmt.Errorf("blueprint: review task for %s: %w", st.ID, err)
	}

	fmt.Fprintf(&b, "Score it from 1 to 5; %d or more passes.\n", st.Reviewer.Threshold)

	return b.String(), nil
}

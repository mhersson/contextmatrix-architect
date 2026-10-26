package blueprint

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/research"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// maxReviewRounds is how many reviewer rounds one attempt of a stage gets;
// the last one goes to the gate whatever it says.
const maxReviewRounds = 3

// round is a builder round after a review: the attempt it continues, its
// number, the conversation it resumes (nil starts over), the next message,
// and the ids of the findings it was given, which its submit may say it
// addressed.
type round struct {
	attempt, number int
	history         []llm.Message
	// model ran history; empty when unknown.
	model    string
	task     string
	findings []string
	// segment marks a run that goes on from a question round: SegmentAnswers
	// resumes it with the answers, SegmentChat answers its chat; qround is the
	// round. Empty for a stage run or a builder round.
	segment string
	qround  int
	// seen is how many chat entries a chat run's task gave it.
	seen int
}

// reviewStage names the pseudo-stage a stage's verdicts are stored under.
func reviewStage(stageID string) string { return stageID + ".review" }

// Finding severities and statuses, as the view carries them.
const (
	severityCritical = "critical"
	severityMajor    = "major"
	severityMinor    = "minor"

	findingOpen      = "open"
	findingFixed     = "fixed"
	findingStillOpen = "still_open"
)

// finding is one entry of a review's ledger. ID is stable within the stage
// attempt; Status is the reviewer's verdict: open when this review raised
// it, fixed or still_open when it re-checked it against VerifiedIn. Note is
// the reviewer's reason for a re-check verdict, which a builder round gets
// with a finding still open.
type finding struct {
	ID         string `json:"id"`
	Severity   string `json:"severity"`
	Text       string `json:"text"`
	Status     string `json:"status"`
	VerifiedIn int    `json:"verified_in,omitempty"`
	Note       string `json:"note,omitempty"`
}

// findingList is a review's ledger. A verdict stored before the ledger
// lists sentences: each reads as an open major finding, numbered in order,
// the numbers its builder round named them by.
type findingList []finding

func (l *findingList) UnmarshalJSON(raw []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("decode the findings: %w", err)
	}

	out := make(findingList, 0, len(items))

	for i, item := range items {
		var text string
		if json.Unmarshal(item, &text) == nil {
			out = append(out, finding{ID: findingID(i + 1), Severity: severityMajor, Text: text, Status: findingOpen})

			continue
		}

		var f finding
		if err := json.Unmarshal(item, &f); err != nil {
			return fmt.Errorf("decode finding %d: %w", i+1, err)
		}

		out = append(out, f)
	}

	*l = out

	return nil
}

// payload is the ledger as plain data, so masking reaches every text;
// claims adds, by id, the note the judged version's addressed list gave to
// a re-checked finding. A finding the review raised was never given to the
// builder, so an entry that shares its id is no claim on it.
func (l findingList) payload(claims map[string]string) []any {
	out := make([]any, 0, len(l))

	for _, f := range l {
		m := map[string]any{"id": f.ID, "severity": f.Severity, "text": f.Text, "status": f.Status}

		if f.VerifiedIn > 0 {
			m["verified_in"] = f.VerifiedIn
		}

		if f.Note != "" {
			m["note"] = f.Note
		}

		if claim, ok := claims[f.ID]; ok && f.Status != findingOpen {
			m["claim"] = claim
		}

		out = append(out, m)
	}

	return out
}

// findingID names the nth finding of an attempt.
func findingID(n int) string { return fmt.Sprintf("R%d", n) }

// findingNumber is the n of an id R<n>; 0 for any other text.
func findingNumber(id string) int {
	digits, ok := strings.CutPrefix(id, "R")
	if !ok {
		return 0
	}

	n, err := strconv.Atoi(digits)
	if err != nil || n < 1 {
		return 0
	}

	return n
}

// review is a reviewer's verdict on one version of a stage's artifact,
// stored as an artifact of the pseudo-stage <stage>.review: the score and
// the ledger of every finding its round judged.
type review struct {
	Score    int         `json:"score"`
	Findings findingList `json:"findings"`
	Version  int         `json:"version"`
	Attempt  int         `json:"attempt"`
	Round    int         `json:"round"`
	// Summary is the reviewer's short account of what it read and checked
	// and its judgement; "" when it gave none.
	Summary string `json:"summary,omitempty"`
}

// unresolved is the findings the reviewer has not seen fixed.
func (v review) unresolved() []finding {
	var out []finding

	for _, f := range v.Findings {
		if f.Status != findingFixed {
			out = append(out, f)
		}
	}

	return out
}

// blocking is the unresolved findings of severity critical or major, the
// ones that keep a passing verdict from settling.
func (v review) blocking() []finding {
	var out []finding

	for _, f := range v.unresolved() {
		if f.Severity == severityCritical || f.Severity == severityMajor {
			out = append(out, f)
		}
	}

	return out
}

// openCritical names the critical findings not fixed; never nil.
func (v review) openCritical() []string {
	out := []string{}

	for _, f := range v.unresolved() {
		if f.Severity == severityCritical {
			out = append(out, f.ID)
		}
	}

	return out
}

// passes is the pass rule: a score at or above threshold, and no critical
// finding open or still open, whatever the score.
func (v review) passes(threshold int) bool {
	return v.Score >= threshold && len(v.openCritical()) == 0
}

// parseReview reads a reviewer's submit against prior, the ledger of the
// attempt's newest verdict: a whole score from 1 to 5, a verdict on every
// prior finding, fixed or still_open with an optional note, and the new
// findings, each a severity and a sentence. Re-checked findings were
// verified in version; new ones take the ids after the attempt's highest. A
// score below threshold sends the builder back, so it must leave a finding
// open or still open.
func parseReview(data map[string]any, prior findingList, threshold, version int) (review, error) {
	score, ok := data["score"].(float64)
	if !ok || score != float64(int(score)) || score < 1 || score > 5 {
		return review{}, errors.New("score must be a whole number from 1 to 5")
	}

	var summary string

	if raw, ok := data["summary"]; ok && raw != nil {
		s, isText := raw.(string)
		if !isText {
			return review{}, errors.New("summary must be text")
		}

		summary = strings.TrimSpace(s)
	}

	checks, err := parseChecks(data["verdicts"], prior)
	if err != nil {
		return review{}, err
	}

	raised, err := parseFindings(data["findings"])
	if err != nil {
		return review{}, err
	}

	v := review{Score: int(score), Findings: findingList{}, Summary: summary}
	next := 1

	for _, f := range prior {
		next = max(next, findingNumber(f.ID)+1)
		ch := checks[f.ID]
		f.Status, f.Note, f.VerifiedIn = ch.status, ch.note, version
		v.Findings = append(v.Findings, f)
	}

	for _, f := range raised {
		f.ID, f.Status = findingID(next), findingOpen
		v.Findings = append(v.Findings, f)
		next++
	}

	if v.Score < threshold && len(v.unresolved()) == 0 {
		return review{}, fmt.Errorf("a score below %d sends the artifact back, so it needs at least one finding open or still open for the builder to act on", threshold)
	}

	return v, nil
}

// findingCheck is a later round's verdict on one earlier finding.
type findingCheck struct{ status, note string }

// parseChecks reads the verdicts on prior: exactly one per prior finding.
func parseChecks(raw any, prior findingList) (map[string]findingCheck, error) {
	known := map[string]bool{}
	ids := make([]string, 0, len(prior))

	for _, f := range prior {
		known[f.ID] = true
		ids = append(ids, f.ID)
	}

	out := map[string]findingCheck{}

	if raw != nil {
		items, isList := raw.([]any)
		if !isList {
			return nil, errors.New(`verdicts must be a list of {"id", "status", "note"} entries`)
		}

		if len(items) > 0 && len(prior) == 0 {
			return nil, errors.New("a first round has no findings to re-check, so verdicts must be empty or left out")
		}

		for i, item := range items {
			e, _ := item.(map[string]any)
			id, _ := e["id"].(string)
			status, _ := e["status"].(string)
			note, noteIsText := e["note"].(string)
			_, dup := out[id]

			switch {
			case strings.TrimSpace(id) == "":
				return nil, fmt.Errorf("verdict %d needs the id of a finding to re-check", i+1)
			case !known[id]:
				return nil, fmt.Errorf("verdict %d names %s, which is not a finding to re-check; they are %s", i+1, id, strings.Join(ids, ", "))
			case dup:
				return nil, fmt.Errorf("%s has two verdicts", id)
			case status != findingFixed && status != findingStillOpen:
				return nil, fmt.Errorf("the verdict on %s must be fixed or still_open", id)
			case e["note"] != nil && !noteIsText:
				return nil, fmt.Errorf("the note on %s must be a sentence", id)
			}

			out[id] = findingCheck{status: status, note: strings.TrimSpace(note)}
		}
	}

	var missing []string

	for _, id := range ids {
		if _, ok := out[id]; !ok {
			missing = append(missing, id)
		}
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf(`give every finding to re-check an entry in verdicts, {"id", "status": "fixed" or "still_open", "note"}; missing: %s`, strings.Join(missing, ", "))
	}

	return out, nil
}

// parseFindings reads the new findings: each a severity and a sentence.
func parseFindings(raw any) ([]finding, error) {
	if raw == nil {
		return nil, nil
	}

	items, isList := raw.([]any)
	if !isList {
		return nil, errors.New(`findings must be a list of {"severity", "text"} entries`)
	}

	out := make([]finding, 0, len(items))

	for i, item := range items {
		e, isObject := item.(map[string]any)
		severity, _ := e["severity"].(string)
		text, _ := e["text"].(string)

		switch {
		case !isObject:
			return nil, fmt.Errorf(`finding %d must be {"severity", "text"}`, i+1)
		case severity != severityCritical && severity != severityMajor && severity != severityMinor:
			return nil, fmt.Errorf("finding %d needs a severity: critical, major or minor", i+1)
		case strings.TrimSpace(text) == "":
			return nil, fmt.Errorf("finding %d must be a sentence", i+1)
		}

		out = append(out, finding{Severity: severity, Text: strings.TrimSpace(text)})
	}

	return out, nil
}

// newestReview is the newest verdict on any version of stageID's artifact;
// found is false when none exists.
func (c *Controller) newestReview(ctx context.Context, id, stageID string) (review, bool, error) {
	art, err := c.opts.Store.LatestArtifact(ctx, id, reviewStage(stageID))

	switch {
	case errors.Is(err, store.ErrNotFound):
		return review{}, false, nil
	case err != nil:
		return review{}, false, fmt.Errorf("blueprint: read the review of %s: %w", c.stepName(ctx, id, stageID), err)
	}

	var v review
	if err := json.Unmarshal(art.Data, &v); err != nil {
		return review{}, false, fmt.Errorf("blueprint: decode the review of %s: %w", c.stepName(ctx, id, stageID), err)
	}

	return v, true, nil
}

// settled reports a verdict that leaves its artifact to the gate: one that
// passes and leaves no critical or major finding open, or the attempt's last
// round, whatever it says. Open minors go to the gate in the ledger, not
// back to the builder. Any other verdict sends the builder back, so every
// revision is reviewed again.
func settled(st pack.Stage, v review) bool {
	return v.Round >= maxReviewRounds || (v.passes(st.Reviewer.Threshold) && len(v.blocking()) == 0)
}

// gateReview is the verdict that decides st's gate on art: the newest
// verdict, when it judged art. found is false otherwise, and for a stage
// without a reviewer.
func (c *Controller) gateReview(ctx context.Context, id string, st pipelineStep, art store.Artifact) (review, bool, error) {
	if st.Reviewer == nil {
		return review{}, false, nil
	}

	v, found, err := c.newestReview(ctx, id, st.ID)
	if err != nil || !found || v.Version != art.Version {
		return review{}, false, err
	}

	return v, true, nil
}

// attemptReviews lists the verdicts on stageID in attempt, oldest first:
// their count is the rounds so far, and the newest holds the ledger a later
// round re-checks.
func (c *Controller) attemptReviews(ctx context.Context, id, stageID string, attempt int) ([]review, error) {
	arts, err := c.opts.Store.ListArtifacts(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: read the reviews of %s: %w", c.stepName(ctx, id, stageID), err)
	}

	var out []review

	for _, a := range arts {
		var v review
		if a.Stage == reviewStage(stageID) && json.Unmarshal(a.Data, &v) == nil && v.Attempt == attempt {
			out = append(out, v)
		}
	}

	return out, nil
}

// critiquePending reports whether a gated stage's artifact still needs its
// reviewer or its builder: no verdict on it yet, or one that does not settle
// it. A gate already shown needs none: it stays with the human it was put
// to, even without a verdict.
func (c *Controller) critiquePending(ctx context.Context, id string, st pipelineStep, art store.Artifact) (bool, error) {
	if st.Reviewer == nil {
		return false, nil
	}

	v, found, err := c.gateReview(ctx, id, st, art)
	if err != nil {
		return false, err
	}

	if found && settled(st.Stage, v) {
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
		return store.Run{}, fmt.Errorf("blueprint: %s has no completed run", c.stepName(ctx, id, stageID))
	}

	return built, nil
}

// critique reviews a gated stage's latest artifact, unless a verdict on it
// exists, and runs the builder's next round with the findings still open
// when the verdict does not settle it. A run that did not complete has been
// reported and stops.
func (c *Controller) critique(ctx context.Context, r *run, sess *session, bp store.Blueprint, stageID string) stageEnd {
	bg := context.Background()
	au := sess.audit

	st, ok := c.stepAt(stageID)
	if !ok || st.Reviewer == nil {
		c.fail(bg, au, bp.ID, 0, fmt.Sprintf("review %s: the pack has no reviewer for it", c.stepName(bg, bp.ID, stageID)))

		return stageStopped
	}

	art, err := c.opts.Store.LatestArtifact(bg, bp.ID, stageID)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, fmt.Sprintf("review %s: %v", c.stepName(bg, bp.ID, stageID), err))

		return stageStopped
	}

	built, err := c.latestStageRun(bg, bp.ID, stageID)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return stageStopped
	}

	v, found, err := c.gateReview(bg, bp.ID, st, art)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return stageStopped
	}

	if !found {
		var ok bool
		if v, ok = c.runReviewer(ctx, r, sess, bp, st, art, built.Attempt); !ok {
			return stageStopped
		}
	}

	if settled(st.Stage, v) {
		return stageStored
	}

	rd, err := c.builderRound(bg, bp, st, built, art, v)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return stageStopped
	}

	return c.runStage(ctx, r, sess, bp, stageID, rd)
}

// builderRound is the round after a verdict that does not settle its
// artifact: the builder's conversation so far, from the stored transcript of
// the run that built the artifact, and as the next message the findings and
// what that conversation has not seen: the notes, and the answers to
// questions a round cut short asked. Without a transcript the round starts
// over, given its own artifact and the findings; its task carries every note
// and answer already.
func (c *Controller) builderRound(ctx context.Context, bp store.Blueprint, st pipelineStep, built store.Run, art store.Artifact, v review) (*round, error) {
	var b strings.Builder

	open := v.unresolved()
	ids := make([]string, 0, len(open))

	for _, f := range open {
		ids = append(ids, f.ID)
	}

	rd := &round{attempt: built.Attempt, number: built.Round + 1, findings: ids}
	rd.history, rd.model = c.runHistory(ctx, bp.ID, built.ID)

	if len(rd.history) == 0 {
		rd.history = nil

		writeArtifact(&b, fmt.Sprintf("Your %s, version %d", st.ID, art.Version), art)
	}

	fmt.Fprintf(&b, "## Reviewer findings (round %d)\n\n", v.Round)

	fmt.Fprintf(&b, "The reviewer scored your artifact %d of 5; %d passes, and a critical finding left open fails it whatever the score. Act on the findings below as your instructions say, then submit the artifact again. Your revision is reviewed again: the reviewer re-checks each finding against it, whatever your addressed notes say:\n\n", v.Score, st.Reviewer.Threshold)

	// By id: the round's submit names the findings it addressed by id.
	for _, f := range open {
		writeFinding(&b, f)
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

// writeFinding lists a finding for a builder round: its id and severity
// and, for one still open, the reviewer's reason.
func writeFinding(b *strings.Builder, f finding) {
	fmt.Fprintf(b, "- %s (%s", f.ID, f.Severity)

	if f.Status == findingStillOpen {
		b.WriteString(", still open after your revision")

		if f.Note != "" {
			b.WriteString(": " + f.Note)
		}
	}

	fmt.Fprintf(b, "): %s\n", f.Text)
}

// statusWord is a finding's status as a task reads it.
func statusWord(status string) string {
	return strings.ReplaceAll(status, "_", " ")
}

// runHistory is the conversation of run runID without its system message
// and the model the run ran on; nil and "" when the run stored no
// transcript, "" for the model when it cannot be read. A transcript that
// cannot be read is logged, and the caller starts over.
func (c *Controller) runHistory(ctx context.Context, id string, runID int64) ([]llm.Message, string) {
	raw, err := c.opts.Store.GetTranscript(ctx, runID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, ""
	case err != nil:
		c.opts.Logger.Warn("blueprint: read the run's transcript; the caller starts over", "blueprint", id, "run", runID, "error", err)

		return nil, ""
	}

	var history []llm.Message
	if err := json.Unmarshal(raw, &history); err != nil {
		c.opts.Logger.Warn("blueprint: decode the run's transcript; the caller starts over", "blueprint", id, "run", runID, "error", err)

		return nil, ""
	}

	model, err := c.opts.Store.RunModel(ctx, runID)
	if err != nil {
		c.opts.Logger.Warn("blueprint: read the run's model; its reasoning is not sent again", "blueprint", id, "run", runID, "error", err)
	}

	return stage.WithoutSystem(history), model
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

// runReviewer judges a gated stage's artifact with the pack's reviewer: its
// persona, the review instructions, reviewerTools' tools and reviewTask,
// never the builder's transcript. The verdict, with its ledger,
// is stored as the next <stage>.review artifact and announced, and the
// blueprint is idle until the next step sets its state. false when the run
// did not complete, which is reported.
func (c *Controller) runReviewer(ctx context.Context, r *run, sess *session, bp store.Blueprint, st pipelineStep, art store.Artifact, attempt int) (review, bool) {
	bg := context.Background()
	au := sess.audit

	past, err := c.attemptReviews(bg, bp.ID, st.ID, attempt)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return review{}, false
	}

	var prior findingList
	if len(past) > 0 {
		prior = past[len(past)-1].Findings
	}

	// number, not round: round is the builder round type in this package.
	number, threshold := len(past)+1, st.Reviewer.Threshold

	poc, err := c.pocProjects(ctx, sess, bp, st)
	if err != nil {
		c.sideFailed(ctx, r, au, bp.ID, st.ID, err)

		return review{}, false
	}

	task, err := c.reviewTask(bg, bp, st, art, attempt, prior, poc)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return review{}, false
	}

	ts, lead, err := c.reviewerTools(ctx, sess, bp, st)
	if err != nil {
		c.sideFailed(ctx, r, au, bp.ID, st.ID, err)

		return review{}, false
	}

	out, err := c.runSide(ctx, r, sess, bp, sideRun{
		kind: store.RunKindReviewer, stage: st.ID, persona: st.Reviewer.Persona, attempt: attempt, round: number,
		in: stage.Input{
			Stage: reviewStage(st.ID), Persona: c.opts.Pack.Persona(st.Reviewer.Persona),
			Instructions: withPlain(bp, c.opts.Pack.ReviewInstructions(), c.opts.Pack.PlainInstructions()), Task: lead + task, Tools: ts,
			Validate: func(_ context.Context, a stage.Artifact) error {
				_, err := parseReview(a.Data, prior, threshold, art.Version)

				return err
			},
		},
	})
	if err != nil {
		c.sideFailed(ctx, r, au, bp.ID, st.ID, err)

		return review{}, false
	}

	v, err := parseReview(out.Artifact.Data, prior, threshold, art.Version)
	if err != nil {
		c.fail(bg, au, bp.ID, 0, fmt.Sprintf("review of %s: %v", c.stepName(bg, bp.ID, st.ID), err))

		return review{}, false
	}

	v.Version, v.Attempt, v.Round = art.Version, attempt, number
	passed := v.passes(threshold)

	stored := map[string]any{
		"score": v.Score, "passed": passed, "findings": v.Findings.payload(nil),
		"version": v.Version, "attempt": v.Attempt, "round": v.Round,
	}

	if v.Summary != "" {
		stored["summary"] = v.Summary
	}

	if _, err := au.artifact(bg, bp.ID, reviewStage(st.ID), out.Artifact.Body, stored); err != nil {
		c.fail(bg, au, bp.ID, 0, err.Error())

		return review{}, false
	}

	au.event(bg, Event{Kind: EventReview, BlueprintID: bp.ID, Payload: map[string]any{
		"stage": st.ID, "version": art.Version, "score": v.Score, "threshold": threshold, "passed": passed,
		"round": v.Round, "reviewer": st.Reviewer.Persona,
		"findings": v.Findings.payload(addressedClaims(art)), "open_critical": v.openCritical(),
	}})

	c.setState(bg, bp.ID, StateIdle)

	return v, true
}

// reviewTask is what a reviewer judges: the stage's inputs, the questions
// it got answered, the assumptions, on a plain-language blueprint the
// wording to judge, for a stage that checks criteria the edge cases, the
// target projects that are proofs of concept when the stage shapes cards,
// the gate feedback the attempt addresses, after a sign-off rejection its
// feedback the gate feedback did not already give, the artifacts folded into
// its gate, each with the gate feedback its own attempt addresses, the
// artifact, whose data holds any problems it was accepted with, the user's
// notes, and, from the attempt's second round, every earlier finding with
// the builder's claim beside it, and the score that passes.
func (c *Controller) reviewTask(ctx context.Context, bp store.Blueprint, st pipelineStep, art store.Artifact, attempt int, prior findingList, poc []string) (string, error) {
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

	if bp.PlainLanguage {
		if err := c.writePlainWording(ctx, &b, bp, st, art, attempt); err != nil {
			return "", err
		}
	}

	if err := c.writeEdgeCases(ctx, &b, bp, st); err != nil {
		return "", err
	}

	writePOC(&b, poc)

	given, err := c.writeGateFeedback(ctx, &b, bp, st, attempt)
	if err != nil {
		return "", err
	}

	if err := c.writeSignoffFeedback(ctx, &b, bp.ID, given); err != nil {
		return "", err
	}

	folded, err := c.folded(ctx, bp.ID, st.ID)
	if err != nil {
		return "", err
	}

	for _, f := range folded {
		fst, _ := c.stepAt(f.Stage)

		built, err := c.latestStageRun(ctx, bp.ID, f.Stage)
		if err != nil {
			return "", err
		}

		if _, err := c.writeGateFeedback(ctx, &b, bp, fst, built.Attempt); err != nil {
			return "", err
		}

		writeArtifact(&b, fmt.Sprintf("%s version %d, whose gate folded into this one: judge it too", f.Stage, f.Version), f)
	}

	writeArtifact(&b, fmt.Sprintf("%s version %d, the artifact you judge", st.ID, art.Version), art)

	// The user's notes are part of what the artifact must satisfy.
	if err := c.writeNotes(ctx, &b, bp.ID); err != nil {
		return "", fmt.Errorf("blueprint: review task for %s: %w", st.ID, err)
	}

	if len(prior) > 0 {
		claims := addressedClaims(art)

		fmt.Fprintf(&b, "## Findings to re-check\n\nEarlier rounds of this attempt raised these findings. Judge each against version %d itself, whatever the builder says it addressed, and give every one a verdict, fixed or still_open:\n\n", art.Version)

		for _, f := range prior {
			fmt.Fprintf(&b, "- %s (%s, %s): %s\n", f.ID, f.Severity, statusWord(f.Status), f.Text)

			if note, ok := claims[f.ID]; ok {
				fmt.Fprintf(&b, "  The builder's claim, to verify: %s\n", cmp.Or(note, "addressed, without a note"))
			}
		}

		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Score it from 1 to 5; %d or more passes, unless a critical finding is left open, which fails it whatever the score.\n", st.Reviewer.Threshold)

	return b.String(), nil
}

// reviewerTools gives a reviewer run the tool groups its pack entry names:
// the code tools over the blueprint's checkouts, and from the research
// group web_fetch alone while the session has research on, behind the same
// leak guard as the stage's research; and read_artifact whenever the stage
// is offered it (readsArtifacts). The text returned opens its task.
// It never posts the research-off notice: the session's first stage does.
func (c *Controller) reviewerTools(ctx context.Context, sess *session, bp store.Blueprint, st pipelineStep) ([]tools.Tool, string, error) {
	var (
		ts    []tools.Tool
		lead  string
		facts *codeFacts
	)

	if slices.Contains(st.Reviewer.Tools, pack.ToolCode) {
		var err error
		if facts, err = c.prepareCode(ctx, sess, bp, st.ID); err != nil {
			return nil, "", err
		}

		ts = stage.CodeTools(facts.root, facts.repos)
		lead = checkoutLines(facts)
	}

	steps, pieces, err := c.steps(ctx, bp.ID)
	if err != nil {
		return nil, "", err
	}

	reads, err := c.readsArtifacts(ctx, bp.ID, steps, len(pieces) > 1, st)
	if err != nil {
		return nil, "", err
	}

	if reads {
		ts = append(ts, artifactReader{c: c, id: bp.ID, st: st})
	}

	if slices.Contains(st.Reviewer.Tools, pack.ToolResearch) {
		if sess.searcher == nil || c.opts.Fetch == nil {
			return ts, reviewerResearchOffTask + lead, nil
		}

		guard := c.researchGuard(sess, bp, st.ID, facts)
		ledger := researchLedger{c: c, au: sess.audit, id: bp.ID, stage: st.ID}

		ts = append(ts, fetchOnly(research.Tools(sess.searcher, c.opts.Fetch, guard, ledger))...)
		lead = reviewerResearchOnTask + lead
	}

	return ts, lead, nil
}

// fetchOnly keeps web_fetch of the research tools: a reviewer opens what
// the artifact cites and searches nothing.
func fetchOnly(ts []tools.Tool) []tools.Tool {
	return slices.DeleteFunc(ts, func(t tools.Tool) bool { return t.Name() != research.FetchToolName })
}

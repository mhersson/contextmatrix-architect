package blueprint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// parkUntil blocks a scripted run until ch closes or the run is cancelled.
func parkUntil(ctx context.Context, ch <-chan struct{}) {
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// signalled waits for a scripted run to close ch.
func signalled(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("the scripted run never started")
	}
}

// reviews scripts a reviewer that gives scores in turn, the last one again
// once they run out; a score below 4 comes with one finding.
func reviews(scores ...int) func(context.Context, stage.Input) (stage.Output, error) {
	var n atomic.Int32

	return func(context.Context, stage.Input) (stage.Output, error) {
		i := int(n.Add(1)) - 1
		score := scores[min(i, len(scores)-1)]

		findings := []any{}
		if score < 4 {
			findings = []any{fmt.Sprintf("finding %d", i+1)}
		}

		return done("# Review", map[string]any{"score": float64(score), "findings": findings}), nil
	}
}

// scored is a reviewer's submit: a score and its findings.
func scored(score int, findings ...string) map[string]any {
	list := make([]any, 0, len(findings))
	for _, f := range findings {
		list = append(list, f)
	}

	return map[string]any{"score": float64(score), "findings": list}
}

// verdicts scripts a reviewer that submits vs in turn, the last one again
// once they run out.
func verdicts(vs ...map[string]any) func(context.Context, stage.Input) (stage.Output, error) {
	var n atomic.Int32

	return func(context.Context, stage.Input) (stage.Output, error) {
		i := int(n.Add(1)) - 1

		return done("# Review", vs[min(i, len(vs)-1)]), nil
	}
}

// shapeRounds lists shape's stage runs as "attempt A round R status".
func shapeRounds(t *testing.T, st *store.Store) []string {
	t.Helper()

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	var out []string

	for _, r := range runs {
		if r.Stage == "shape" && r.Kind == store.RunKindStage {
			out = append(out, fmt.Sprintf("attempt %d round %d %s", r.Attempt, r.Round, r.Status))
		}
	}

	return out
}

// reviewOf is the review map a gate event carries.
func reviewOf(t *testing.T, gate Event) map[string]any {
	t.Helper()

	v, ok := gate.Payload["review"].(map[string]any)
	require.True(t, ok, "the gate carries its verdict")

	return v
}

func TestParseReview(t *testing.T) {
	tests := []struct {
		name    string
		data    map[string]any
		want    review
		wantErr bool
	}{
		{name: "a score and findings", data: map[string]any{"score": float64(3), "findings": []any{"name the owner"}}, want: review{Score: 3, Findings: []string{"name the owner"}}},
		{name: "no findings", data: map[string]any{"score": float64(5)}, want: review{Score: 5}},
		{name: "no score", data: map[string]any{"findings": []any{}}, wantErr: true},
		{name: "a score above five", data: map[string]any{"score": float64(6)}, wantErr: true},
		{name: "a score of zero", data: map[string]any{"score": float64(0)}, wantErr: true},
		{name: "a fraction", data: map[string]any{"score": 3.5}, wantErr: true},
		{name: "a score as text", data: map[string]any{"score": "4"}, wantErr: true},
		{name: "a blank finding", data: map[string]any{"score": float64(2), "findings": []any{" "}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseReview(tt.data)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReview_AHumanGateShowsTheVerdict(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(4)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	gate, ok := rec.first(EventGate)
	require.True(t, ok)

	assert.Equal(t, 1, gate.Payload["version"])

	verdict := reviewOf(t, gate)
	assert.Equal(t, 4, verdict["score"])
	assert.Equal(t, true, verdict["passed"])
	assert.Equal(t, false, verdict["revised"], "a pass without findings is not revised")
	assert.Equal(t, 1, verdict["judged"])

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 1)
	assert.Contains(t, judged[0].Task, "# Shape", "the reviewer judges the artifact")
	assert.Contains(t, judged[0].Task, "# Need", "with the stage's inputs")
	assert.Nil(t, judged[0].History, "never the builder's transcript")
	assert.Nil(t, judged[0].Interviewer)
	assert.Equal(t, c.opts.Pack.Persona("reviewer"), judged[0].Persona)
	assert.Equal(t, c.opts.Pack.ReviewInstructions(), judged[0].Instructions)
	assert.Empty(t, judged[0].Tools, "a reviewer gets no tools")

	stored, err := st.LatestArtifact(ctx, "BP1", "shape.review")
	require.NoError(t, err)
	assert.JSONEq(t, `{"score":4,"findings":[],"version":1,"attempt":1,"round":1}`, string(stored.Data))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var kinds []string
	for _, r := range runs {
		kinds = append(kinds, r.Stage+":"+r.Kind)
	}

	assert.Equal(t, []string{"intake:stage", "shape:stage", "shape:reviewer"}, kinds)
}

func TestReview_ALowScoreResumesTheBuilderWithTheFindings(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(2, 5)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	shapes := inputsOf(runner, "shape")
	require.Len(t, shapes, 2)
	assert.NotEmpty(t, shapes[1].History, "the builder's conversation resumes")
	assert.Contains(t, shapes[1].Task, "finding 1", "with the findings")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var rounds []string

	for _, r := range runs {
		if r.Stage == "shape" && r.Kind == store.RunKindStage {
			rounds = append(rounds, fmt.Sprintf("attempt %d round %d", r.Attempt, r.Round))
		}
	}

	assert.Equal(t, []string{"attempt 1 round 1", "attempt 1 round 2"}, rounds)

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"], "the gate judges the second round's artifact")
	assert.Equal(t, 2, rec.count(EventReview))
}

func TestReview_ThreeLowRoundsGoToAHumanWhateverThePolicy(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(2)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeAuto)

	assert.Len(t, inputsOf(runner, "shape"), 3)
	assert.Len(t, inputsOf(runner, "shape.review"), 3)

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the auto policy does not decide a gate its reviewer did not pass")

	verdict, _ := gate.Payload["review"].(map[string]any)
	assert.Equal(t, false, verdict["passed"])
	assert.Equal(t, 3, verdict["round"])

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates)
	assert.Equal(t, []string{"intake", "shape", "shape", "shape"}, stagesRun(runner), "plan never runs")
}

func TestReview_AnAutoGateApprovesAtTheThreshold(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(4)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeAuto)

	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestReview_APassWithFindingsRevisesOnceBeforeTheHumanGate(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner"))

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	assert.Len(t, inputsOf(runner, "shape.review"), 1, "the revision is not reviewed")
	assert.Equal(t, 1, rec.count(EventReview))

	shapes := inputsOf(runner, "shape")
	require.Len(t, shapes, 2)
	assert.Equal(t, []llm.Message{{Role: "user", Content: "x"}}, shapes[1].History, "the builder's conversation resumes")
	assert.Contains(t, shapes[1].Task, "name the owner", "with the findings")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 done"}, shapeRounds(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 1, rec.count(EventGate))
	assert.Equal(t, 2, gate.Payload["version"], "the gate shows the revised version")

	verdict := reviewOf(t, gate)
	assert.Equal(t, true, verdict["revised"])
	assert.Equal(t, 1, verdict["judged"], "the version the reviewer scored")
	assert.Equal(t, 4, verdict["score"])
	assert.Equal(t, true, verdict["passed"])

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates, "the gate waits for its human")
}

func TestReview_AnAutoGateApprovesTheRevision(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner"))

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeAuto)

	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"intake", "shape", "shape", "plan"}, stagesRun(runner))
	assert.Len(t, inputsOf(runner, "shape.review"), 1)

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, 2, gates[0].Version, "the revised version is approved")
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestReview_ALowScoreThenAPassWithFindingsRevisesOnce(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(2, "split it"), scored(4, "name the owner"))

	c, st, runner, rec := newGated(t, script)

	startGated(t, c, protocol.ArchitectModeDefault)

	assert.Len(t, inputsOf(runner, "shape.review"), 2)

	shapes := inputsOf(runner, "shape")
	require.Len(t, shapes, 3)
	assert.Contains(t, shapes[1].Task, "split it", "the builder round")
	assert.Contains(t, shapes[2].Task, "name the owner", "the revision round")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 done", "attempt 1 round 3 done"}, shapeRounds(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 3, gate.Payload["version"])

	verdict := reviewOf(t, gate)
	assert.Equal(t, true, verdict["revised"])
	assert.Equal(t, 2, verdict["judged"])
	assert.Equal(t, 2, verdict["round"])
}

func TestReview_ARestartDuringTheRevisionRunsItAgain(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner"))
	script["shape"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Shape", nil), nil
	}

	c, st, runner, _ := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return shapes.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))
	require.Len(t, inputsOf(runner, "shape.review"), 1)

	c2, _, runner2, rec2 := newGatedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	again := inputsOf(runner2, "shape")
	require.Len(t, again, 1)
	assert.Contains(t, again[0].Task, "name the owner", "the revision runs again with its findings")
	assert.NotEmpty(t, again[0].History, "on the builder's conversation")
	assert.Empty(t, inputsOf(runner2, "shape.review"), "no new review")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 interrupted", "attempt 1 round 2 done"}, shapeRounds(t, st))

	gate, ok := rec2.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"])
	assert.Equal(t, true, reviewOf(t, gate)["revised"])
}

func TestReview_AFailedRevisionWaitsAndANoteRunsItAgain(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner"))
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			return stage.Output{}, errors.New("provider down")
		}

		return done("# Shape", nil), nil
	}

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.False(t, rec.has(EventGate), "the failed revision stops the pipeline")

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m2"))
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, "shape", note.Payload["retries"])
	assert.Equal(t, true, note.Payload["critique"])

	shapesRun := inputsOf(runner, "shape")
	require.Len(t, shapesRun, 3, "the revision runs again")
	assert.Contains(t, shapesRun[2].Task, "name the owner")
	assert.Len(t, inputsOf(runner, "shape.review"), 1, "its verdict is not asked for again")
	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 failed", "attempt 1 round 2 done"}, shapeRounds(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"])
	assert.Equal(t, true, reviewOf(t, gate)["revised"])
}

func TestReview_AGateShownOnAPassWithFindingsStaysWithItsHuman(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	// A gate shown on a verdict that passed with findings, before such a
	// verdict owed a revision.
	seedGated(t, st, protocol.ArchitectModeDefault)

	_, err := st.PutArtifact(ctx, store.Artifact{
		BlueprintID: "BP1", Stage: reviewStage("shape"), Body: "# Review",
		Data: []byte(`{"score":4,"findings":["name the owner"],"version":1,"attempt":1,"round":1}`),
	})
	require.NoError(t, err)
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventGate, Payload: []byte(`{"stage":"shape","version":1}`),
	}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the gate is shown again")
	assert.Equal(t, 1, gate.Payload["version"])
	assert.Equal(t, false, reviewOf(t, gate)["revised"])
	assert.Empty(t, stagesRun(runner), "no revision runs")

	// The rejection starts a new attempt, whose version is no revision.
	require.NoError(t, c.Message(ctx, "BP1", "reject make it smaller", "m2"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "shape.review"), 1, "the new attempt's version is reviewed")

	gates := rec.all(EventGate)
	require.Len(t, gates, 2)
	assert.Equal(t, 2, gates[1].Payload["version"])
	assert.Equal(t, false, reviewOf(t, gates[1])["revised"])
	assert.Equal(t, 2, reviewOf(t, gates[1])["judged"])
}

func TestReview_AFailedRoundWaitsAndANoteRunsItAgain(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape.review"] = reviews(2, 5)
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			return stage.Output{}, errors.New("provider down")
		}

		return done("# Shape", nil), nil
	}

	c, _, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.False(t, rec.has(EventGate), "the failed builder round stops the pipeline")

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m2"))
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, "shape", note.Payload["retries"])

	assert.Len(t, inputsOf(runner, "shape"), 3, "the round runs again")
	assert.Len(t, inputsOf(runner, "shape.review"), 2, "its earlier verdict is not asked for again")
	assert.True(t, rec.has(EventGate))
}

func TestReview_ARestartDuringTheReviewRunsItAgain(t *testing.T) {
	var calls atomic.Int32

	script := gatedScript()
	script["shape.review"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Review", map[string]any{"score": float64(5), "findings": []any{}}), nil
	}

	c, st, _, _ := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	c2, _, _, rec2 := newGatedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	assert.Equal(t, int32(2), calls.Load())
	assert.True(t, rec2.has(EventGate))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var reviewers []string

	for _, r := range runs {
		if r.Kind == store.RunKindReviewer {
			reviewers = append(reviewers, r.Status)
		}
	}

	assert.Equal(t, []string{store.RunInterrupted, store.RunDone}, reviewers)
}

func TestReview_ARestartDuringABuilderRoundRunsTheRoundAgain(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape.review"] = reviews(2, 5)
	script["shape"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Shape", nil), nil
	}

	c, st, _, _ := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return shapes.Load() == 2 }, 5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	c2, _, runner2, rec2 := newGatedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	again := inputsOf(runner2, "shape")
	require.Len(t, again, 1)
	assert.Contains(t, again[0].Task, "finding 1", "the round runs again with its findings")
	assert.NotEmpty(t, again[0].History, "on the builder's conversation")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var rounds []string

	for _, r := range runs {
		if r.Stage == "shape" && r.Kind == store.RunKindStage {
			rounds = append(rounds, fmt.Sprintf("attempt %d round %d %s", r.Attempt, r.Round, r.Status))
		}
	}

	assert.Equal(t, []string{"attempt 1 round 1 done", "attempt 1 round 2 interrupted", "attempt 1 round 2 done"}, rounds)
	assert.True(t, rec2.has(EventGate))
}

func TestReview_ARejectionWhoseRunNeverStartedRunsTheStageOnReopen(t *testing.T) {
	c, st, _, _ := newGated(t, gatedScript())
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	// A crash after the reject row, before the stage's run started.
	_, err := st.AddGate(ctx, store.Gate{
		BlueprintID: "BP1", Stage: "shape", Version: 1, Decision: store.GateReject,
		Feedback: "make it smaller", Responder: store.ResponderHuman, DecidedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, c.Shutdown(ctx))

	c2, _, runner2, rec2 := newGatedOn(t, st, gatedScript())
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	shapes := inputsOf(runner2, "shape")
	require.Len(t, shapes, 1, "the rejected stage runs again")
	assert.Contains(t, shapes[0].Task, "make it smaller")

	gate, ok := rec2.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, 2, gate.Payload["version"], "its new version waits at the gate")
}

func TestReview_AGateReplyWhileItsReviewFailedFindsNoOpenGate(t *testing.T) {
	var calls atomic.Int32

	script := gatedScript()
	script["shape.review"] = func(context.Context, stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			return stage.Output{}, errors.New("provider down")
		}

		return done("# Review", map[string]any{"score": float64(5), "findings": []any{}}), nil
	}

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.False(t, rec.has(EventGate), "the failed review stops the pipeline")

	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", "auto", "m2"))

	change, ok := rec.first(EventPolicy)
	require.True(t, ok)
	assert.NotContains(t, change.Payload, "open", "the gate was never shown")

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m3"))
	c.Wait("BP1")

	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder, "the gate opens under the new policy")
}

func TestReview_AShownGateWithoutAVerdictStaysWithItsHuman(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	// A gate shown before its stage had a reviewer run, so with no verdict.
	seedGated(t, st, protocol.ArchitectModeDefault)
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventGate, Payload: []byte(`{"stage":"shape","version":1}`),
	}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")
	require.True(t, rec.has(EventGate), "the gate is shown again")

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "shape.review"), "a shown gate is never reviewed")
	assert.Equal(t, []string{"plan"}, stagesRun(runner))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderHuman, gates[0].Responder, "the approve decides it")
}

func TestReview_ANoteReachesTheReviewer(t *testing.T) {
	var calls atomic.Int32

	script := gatedScript()
	script["shape.review"] = func(context.Context, stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			return stage.Output{}, errors.New("provider down")
		}

		return done("# Review", map[string]any{"score": float64(5), "findings": []any{}}), nil
	}

	c, _, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.NoError(t, c.Message(ctx, "BP1", "keep the owner named", "m2"))
	c.Wait("BP1")

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 2)
	assert.Contains(t, judged[1].Task, "keep the owner named")
	assert.True(t, rec.has(EventGate))
}

func TestReview_AReviewRunsAsItsStage(t *testing.T) {
	var calls atomic.Int32

	started, release := make(chan struct{}), make(chan struct{})

	script := gatedScript()
	script["shape.review"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			return stage.Output{}, errors.New("provider down")
		}

		close(started)
		parkUntil(ctx, release)

		return done("# Review", map[string]any{"score": float64(5), "findings": []any{}}), nil
	}

	c, st, _, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.Equal(t, string(StateFailed), stateOf(t, st, "BP1"))

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m2"))
	signalled(t, started)
	assert.Equal(t, string(StateRunning), stateOf(t, st, "BP1"), "the review runs")

	require.NoError(t, c.Message(ctx, "BP1", "keep it short", "m3"))
	close(release)
	c.Wait("BP1")

	var during Event

	for _, n := range rec.all(EventNote) {
		if n.Payload["message_id"] == "m3" {
			during = n
		}
	}

	assert.Equal(t, "shape", during.Payload["stage"], "a note during the review names its stage")
}

func TestReview_TheBuilderRoundsConversation(t *testing.T) {
	tests := []struct {
		name     string
		messages []llm.Message
	}{
		{name: "resumed without its system message", messages: []llm.Message{
			{Role: "system", Content: "persona"}, {Role: "user", Content: "task"}, {Role: "assistant", Content: "submitted"},
		}},
		{name: "started over without a transcript"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var shapes atomic.Int32

			script := gatedScript()
			script["shape.review"] = reviews(2, 5)
			script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
				return stage.Output{
					Artifact: stage.Artifact{Body: fmt.Sprintf("# Shape %d", shapes.Add(1))},
					Result:   harness.Result{Completed: true, Reason: "done", Messages: tt.messages},
				}, nil
			}

			c, _, runner, _ := newGated(t, script)

			startGated(t, c, protocol.ArchitectModeDefault)

			rounds := inputsOf(runner, "shape")
			require.Len(t, rounds, 2)

			if tt.messages == nil {
				assert.Nil(t, rounds[1].History)
				assert.Contains(t, rounds[1].Task, "# Shape 1", "its own artifact")
				assert.Contains(t, rounds[1].Task, "finding 1")

				return
			}

			require.NotEmpty(t, rounds[1].History)

			for _, m := range rounds[1].History {
				assert.NotEqual(t, "system", m.Role, "the round's own system message comes first")
			}
		})
	}
}

func TestReview_ABuilderRoundGetsEachNoteOnce(t *testing.T) {
	for _, resumes := range []bool{true, false} {
		t.Run(fmt.Sprintf("resumes %v", resumes), func(t *testing.T) {
			intakeRuns, intakeGoes := make(chan struct{}), make(chan struct{})
			shapeRuns, shapeGoes := make(chan struct{}), make(chan struct{})

			var shapes atomic.Int32

			script := gatedScript()
			script["shape.review"] = reviews(2, 5)
			script["intake"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
				close(intakeRuns)
				parkUntil(ctx, intakeGoes)

				return done("# Need", nil), nil
			}
			script["shape"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
				if shapes.Add(1) == 1 {
					close(shapeRuns)
					parkUntil(ctx, shapeGoes)
				}

				out := done("# Shape", nil)
				if !resumes {
					out.Result.Messages = nil
				}

				return out, nil
			}

			c, _, runner, _ := newGated(t, script)
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))

			signalled(t, intakeRuns)
			require.NoError(t, c.Message(ctx, "BP1", "an early note", "m2"))
			close(intakeGoes)

			signalled(t, shapeRuns)
			require.NoError(t, c.Message(ctx, "BP1", "a late note", "m3"))
			close(shapeGoes)
			c.Wait("BP1")

			rounds := inputsOf(runner, "shape")
			require.Len(t, rounds, 2)
			assert.Contains(t, rounds[0].Task, "an early note")

			if resumes {
				assert.Contains(t, rounds[1].Task, "a late note", "the note its conversation has not seen")
				assert.NotContains(t, rounds[1].Task, "an early note", "the conversation holds it")

				return
			}

			assert.Equal(t, 1, strings.Count(rounds[1].Task, "an early note"))
			assert.Equal(t, 1, strings.Count(rounds[1].Task, "a late note"))
		})
	}
}

func TestReview_AReopenAfterACompletedReviewGoesOn(t *testing.T) {
	tests := []struct {
		name    string
		verdict string // "" stores none
		shapes  int
	}{
		{name: "a low verdict runs the builder round", verdict: `{"score":2,"findings":["name the owner"],"version":1,"attempt":1,"round":1}`, shapes: 1},
		{name: "no verdict runs the review again"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := gatedScript()
			script["shape.review"] = reviews(5)

			c, st, runner, rec := newGated(t, script)
			ctx := context.Background()
			now := time.Now().UTC()

			// A crash after the review completed, before what follows it.
			seedGated(t, st, protocol.ArchitectModeDefault)

			id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "shape", Kind: store.RunKindReviewer, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
			require.NoError(t, err)
			require.NoError(t, st.EndRun(ctx, id, store.RunDone, "done", store.RunUsage{}, now))

			if tt.verdict != "" {
				_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: reviewStage("shape"), Body: "# Review", Data: []byte(tt.verdict)})
				require.NoError(t, err)
			}

			require.NoError(t, c.Start(ctx, startInput("BP1")))
			c.Wait("BP1")

			rounds := inputsOf(runner, "shape")
			require.Len(t, rounds, tt.shapes)

			if tt.shapes > 0 {
				assert.Contains(t, rounds[0].Task, "name the owner", "the stored verdict's findings")
			}

			assert.Len(t, inputsOf(runner, "shape.review"), 1)
			assert.True(t, rec.has(EventGate))
		})
	}
}

func TestReview_ACritiqueWithoutAReviewerFailsTheBlueprint(t *testing.T) {
	// Both stages have an artifact and a completed run in the record, so
	// only the missing reviewer stops the critique.
	for _, stageID := range []string{"nosuch", "intake"} {
		t.Run(stageID, func(t *testing.T) {
			c, st, runner, rec := newGated(t, gatedScript())
			ctx := context.Background()
			now := time.Now().UTC()

			seedGated(t, st, protocol.ArchitectModeDefault)

			_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "nosuch", Body: "# nosuch"})
			require.NoError(t, err)

			id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "nosuch", Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
			require.NoError(t, err)
			require.NoError(t, st.EndRun(ctx, id, store.RunDone, "", store.RunUsage{}, now))

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			assert.Equal(t, stageStopped, c.critique(ctx, &run{}, &session{audit: c.newAudit()}, bp, stageID))
			assert.True(t, rec.has(EventError))
			assert.Equal(t, string(StateFailed), stateOf(t, st, "BP1"))
			assert.Zero(t, runner.calls())
		})
	}
}

func TestReview_TheReviewerGetsTheAnswersAndTheGateFeedback(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 1 {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Store", "Which store?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		return done("# Shape", nil), nil
	}

	c, _, runner, rec := newGated(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "keep it in Postgres"))
	c.Wait("BP1")
	require.True(t, rec.has(EventGate))

	require.NoError(t, c.Message(ctx, "BP1", "reject name the owner", "m3"))
	c.Wait("BP1")

	judged := inputsOf(runner, "shape.review")
	require.Len(t, judged, 2)
	assert.Contains(t, judged[0].Task, "keep it in Postgres", "the answer the decision rests on")
	assert.Contains(t, judged[1].Task, "keep it in Postgres")
	assert.Contains(t, judged[1].Task, "name the owner", "the feedback the attempt addresses")
}

func TestReview_ACompletedReviewLeavesTheBlueprintIdle(t *testing.T) {
	c, st, _, _ := newGated(t, gatedScript())
	ctx := context.Background()

	seedGated(t, st, protocol.ArchitectModeDefault)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	require.Equal(t, stageStored, c.critique(ctx, &run{}, &session{audit: c.newAudit()}, bp, "shape"))
	assert.Equal(t, string(StateIdle), stateOf(t, st, "BP1"), "the step after the review sets its own state")
}

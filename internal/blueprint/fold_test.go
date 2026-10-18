package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func oneSlice() map[string]any {
	return map[string]any{"slices": []any{
		map[string]any{"id": "SL1", "title": "Ship the endpoint", "value": "callers see the version", "projects": []any{"alpha"}},
	}}
}

func twoSlices() map[string]any {
	d := oneSlice()
	d["slices"] = append(d["slices"].([]any),
		map[string]any{"id": "SL2", "title": "Show it in the UI", "value": "users see it", "projects": []any{"alpha"}})

	return d
}

// gateRows lists a blueprint's decisions as stage@version:decision, oldest
// first.
func gateRows(t *testing.T, st *store.Store) []string {
	t.Helper()

	gates, err := st.AllGates(context.Background(), "BP1")
	require.NoError(t, err)

	var out []string

	for _, g := range gates {
		out = append(out, fmt.Sprintf("%s@%d:%s", g.Stage, g.Version, g.Decision))
	}

	return out
}

// gateReplies lists the replies a gate event offers, in order.
func gateReplies(e Event) []string {
	var out []string

	for _, a := range anyList(e.Payload["actions"]) {
		out = append(out, a.(map[string]any)["reply"].(string))
	}

	return out
}

func TestFold_OneSliceFoldsIntoTheDesignGate(t *testing.T) {
	c, st, runner, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	drive(t, c, "BP1")

	assert.Empty(t, inputsOf(runner, "slicing.review"), "a folded gate's reviewer is skipped")
	assert.Equal(t, []string{"slicing@1:fold"}, gateRows(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, "solution_design", gate.Payload["stage"], "slicing's gate is not shown")
	assert.Equal(t, []any{map[string]any{"stage": "slicing", "version": 1, "body": "# Slices"}}, gate.Payload["folded"])

	assert.Equal(t, []string{"approve", "reject", "reject slicing"}, gateReplies(gate))

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{"slicing@1:fold", "slicing@1:approve", "solution_design@1:approve", "acceptance@1:approve"}, gateRows(t, st))
	assert.True(t, rec.has(EventSignOff))
}

func TestFold_TwoSlicesGetTheirOwnGate(t *testing.T) {
	script := dialogScript()
	script["slicing"] = returns("# Slices", twoSlices())

	c, st, runner, rec, _ := newDialog(t, script)

	drive(t, c, "BP1")

	assert.Len(t, inputsOf(runner, "slicing.review"), 1)

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, "slicing", gate.Payload["stage"])
	assert.Empty(t, gateRows(t, st))
}

func TestFold_NeedsTheSamePolicy(t *testing.T) {
	c, st, runner, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "solution_design", "auto", "m0"))

	drive(t, c, "BP1")

	assert.Len(t, inputsOf(runner, "slicing.review"), 1, "no fold, so slicing's reviewer runs")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, "slicing", gate.Payload["stage"])
	assert.Empty(t, gateRows(t, st))
}

func TestFold_AShownSlicingGateNeverFoldsLater(t *testing.T) {
	c, st, runner, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "solution_design", "auto", "m0"))

	drive(t, c, "BP1")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	require.Equal(t, "slicing", gate.Payload["stage"])

	// The policies are equal again, but the gate is the human's already.
	require.NoError(t, c.SetPolicy(ctx, "BP1", "solution_design", "human", "m2"))
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	rows := gateRows(t, st)
	require.NotEmpty(t, rows)
	assert.Equal(t, "slicing@1:approve", rows[0], "the approve decides slicing's own gate")
	assert.NotContains(t, rows, "slicing@1:fold")
	assert.Len(t, inputsOf(runner, "solution_design"), 1, "solution design runs")
}

func TestFold_RejectSlicingRunsSlicingAgain(t *testing.T) {
	c, st, runner, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "reject slicing merge them", "m2"))
	c.Wait("BP1")

	slicings := inputsOf(runner, "slicing")
	require.Len(t, slicings, 2)
	assert.Contains(t, slicings[1].Task, "merge them", "the feedback reaches slicing")
	assert.Contains(t, slicings[1].Task, "# Slices", "with its rejected artifact")
	assert.Len(t, inputsOf(runner, "solution_design"), 2, "solution design runs again after it")

	assert.Equal(t, []string{"slicing@1:fold", "slicing@1:reject", "solution_design@1:superseded", "slicing@2:fold"}, gateRows(t, st))
	assert.Equal(t, 2, rec.count(EventGate))

	designs, err := st.ListGates(ctx, "BP1", "solution_design")
	require.NoError(t, err)
	require.Len(t, designs, 1)
	assert.Equal(t, "slicing", designs[0].Feedback, "the row names the stage whose rejection replaced the design")
	assert.Equal(t, store.ResponderHuman, designs[0].Responder)

	decision, err := c.gateDecision(ctx, "BP1", "solution_design", 1)
	require.NoError(t, err)
	assert.Empty(t, decision, "a superseded row decides nothing")
}

func TestFold_TheDesignReviewerGetsTheFoldedStagesFeedback(t *testing.T) {
	c, _, runner, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "reject slicing merge them", "m2"))
	c.Wait("BP1")

	reviews := inputsOf(runner, "solution_design.review")
	require.Len(t, reviews, 2)
	assert.Contains(t, reviews[1].Task, "merge them", "the feedback the re-run slicing addresses")
}

func TestFold_TheFoldedGateCarriesSlicingNotTheDesign(t *testing.T) {
	const sliced, designed = "# Slices\n\nSL1 ships the endpoint.", "# Design\n\nPostgres, managed."

	script := dialogScript()
	script["slicing"] = returns(sliced, oneSlice())
	script["solution_design"] = returns(designed, goodDesign("assumption"))

	c, _, _, rec, _ := newDialog(t, script)

	drive(t, c, "BP1")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	require.Equal(t, "solution_design", gate.Payload["stage"])

	folded := anyList(gate.Payload["folded"])
	require.Len(t, folded, 1)
	assert.Equal(t, sliced, folded[0].(map[string]any)["body"], "slicing's artifact, which the panel has not shown near the gate")
	assert.NotContains(t, fmt.Sprint(gate.Payload), designed, "the design was posted just above the gate")

	var shown int

	for _, a := range rec.all(EventArtifact) {
		if a.Payload["body"] == designed {
			shown++
		}
	}

	assert.Equal(t, 1, shown)
}

func TestFold_AnAnswerFromBeforeAReSliceIsNotOwed(t *testing.T) {
	type submits struct{ omitting, citing error }

	var designs atomic.Int32

	seen := make(chan submits, 1)

	script := dialogScript()
	script["solution_design"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			n := designs.Add(1)

			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Question", fmt.Sprintf("Question %d?", n))}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		if designs.Load() == 1 {
			return done("# Design", goodDesign("Q1")), nil
		}

		// After the re-slice only this attempt's answer is owed a decision.
		seen <- submits{
			omitting: in.Validate(ctx, stage.Artifact{Body: "# Design", Data: goodDesign("assumption")}),
			citing:   in.Validate(ctx, stage.Artifact{Body: "# Design", Data: goodDesign("Q2")}),
		}

		return done("# Design", goodDesign("Q2")), nil
	}

	c, _, _, _, _ := newDialog(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "Postgres"))
	c.Wait("BP1")

	require.NoError(t, c.Message(ctx, "BP1", "reject slicing merge them", "m3"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 2, true, own(2, "eu-north"))
	c.Wait("BP1")

	var got submits

	select {
	case got = <-seen:
	default:
		require.Fail(t, "solution design did not run again after the re-slice")
	}

	require.Error(t, got.omitting)
	assert.Contains(t, got.omitting.Error(), "Q2: unused_answer: ", "the answer given since the re-slice is owed")
	assert.NotContains(t, got.omitting.Error(), "Q1: unused_answer: ", "the one from before it is not")
	assert.NoError(t, got.citing, "a design citing only the new answer stands")
}

func TestFold_AnUnmatchedReplyIsToldTheFoldedReject(t *testing.T) {
	c, _, _, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "looks fine to me", "m2"))
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)

	hint, _ := note.Payload["hint"].(string)
	assert.Contains(t, hint, "reject slicing", "the hint names the folded stage's reject")
}

func TestFold_APolicyChangeAfterTheFoldKeepsItFolded(t *testing.T) {
	c, _, _, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.SetPolicy(ctx, "BP1", "slicing", "auto", "m2"))
	require.NoError(t, c.Message(ctx, "BP1", "reject choose again", "m3"))
	c.Wait("BP1")

	gates := rec.all(EventGate)
	require.Len(t, gates, 2, "solution design ran again to a new gate")
	assert.Equal(t, "solution_design", gates[1].Payload["stage"])
	assert.Equal(t, []any{map[string]any{"stage": "slicing", "version": 1, "body": "# Slices"}}, gates[1].Payload["folded"])
	assert.Contains(t, gateReplies(gates[1]), "reject slicing")
}

func TestFold_AFoldedGateStaysHumanWhileAFoldedStageIs(t *testing.T) {
	designs, goes := make(chan struct{}), make(chan struct{})

	script := dialogScript()
	script["solution_design"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		close(designs)
		parkUntil(ctx, goes)

		return done("# Design", goodDesign("assumption")), nil
	}

	c, st, _, rec, _ := newDialog(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))

	// Folded under human and human; then the design's policy turns auto.
	signalled(t, designs)
	require.NoError(t, c.SetPolicy(ctx, "BP1", "solution_design", "auto", "m2"))
	close(goes)
	c.Wait("BP1")

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "slicing's policy is still human, so the folded gate is shown")
	assert.Equal(t, "solution_design", gate.Payload["stage"])
	assert.Equal(t, []string{"slicing@1:fold"}, gateRows(t, st), "nothing was approved for slicing's human")
}

func TestFold_AReopenAtTheFoldedGateStillTakesRejectSlicing(t *testing.T) {
	c, st, _, _, ws := newDialog(t, dialogScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Shutdown(ctx))

	c2, runner2, rec2 := newDialogOn(t, st, dialogScript(), ws)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	again, ok := rec2.first(EventGate)
	require.True(t, ok, "the folded gate is shown again")
	assert.Contains(t, gateReplies(again), "reject slicing")

	require.NoError(t, c2.Message(ctx, "BP1", "reject slicing merge them", "m2"))
	c2.Wait("BP1")

	slicings := inputsOf(runner2, "slicing")
	require.Len(t, slicings, 1, "slicing runs again")
	assert.Contains(t, slicings[0].Task, "merge them", "with the feedback")
	assert.Equal(t, []string{"slicing@1:fold", "slicing@1:reject", "solution_design@1:superseded", "slicing@2:fold"}, gateRows(t, st))
}

func TestFold_AReopenThatPassesSlicingRecordsTheFold(t *testing.T) {
	// A crash fell between storing slicing's artifact and starting solution
	// design: no run was left running and no fold recorded.
	c, st, runner, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))

	sliced, err := json.Marshal(oneSlice())
	require.NoError(t, err)

	now := time.Now().UTC()

	for _, a := range []store.Artifact{
		{Stage: needStage, Body: "the need"},
		{Stage: "intake", Body: "# Need"},
		{Stage: "slicing", Body: "# Slices", Data: sliced},
	} {
		a.BlueprintID = "BP1"
		_, err := st.PutArtifact(ctx, a)
		require.NoError(t, err)

		if a.Stage == needStage {
			continue
		}

		id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: a.Stage, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
		require.NoError(t, err)
		require.NoError(t, st.EndRun(ctx, id, store.RunDone, "done", store.RunUsage{}, now))
	}

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Equal(t, []string{"solution_design"}, stagesRun(runner))
	assert.Equal(t, []string{"slicing@1:fold"}, gateRows(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, []any{map[string]any{"stage": "slicing", "version": 1, "body": "# Slices"}}, gate.Payload["folded"])
}

func TestFold_ARejectedSlicingThatFailsRunsAgainOnTheNextMessage(t *testing.T) {
	var slicings atomic.Int32

	script := dialogScript()
	script["slicing"] = func(context.Context, stage.Input) (stage.Output, error) {
		if slicings.Add(1) == 2 {
			return stage.Output{}, errors.New("the model went away")
		}

		return done("# Slices", oneSlice()), nil
	}

	c, st, runner, rec, _ := newDialog(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "reject slicing merge them", "m2"))
	c.Wait("BP1")
	require.Len(t, inputsOf(runner, "slicing"), 2, "the re-run of slicing failed")

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m3"))
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, "slicing", note.Payload["retries"], "the rejected slicing runs again, not the design's gate")
	assert.Len(t, inputsOf(runner, "slicing"), 3)
	assert.Equal(t, []string{"slicing@1:fold", "slicing@1:reject", "solution_design@1:superseded", "slicing@2:fold"}, gateRows(t, st))
}

func TestFold_ABareRejectSlicingRejectsTheDesign(t *testing.T) {
	c, st, runner, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "reject slicing", "m2"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "slicing"), 1, "slicing does not run again")
	assert.Len(t, inputsOf(runner, "solution_design"), 2, "solution design runs again")
	assert.Equal(t, []string{"slicing@1:fold", "solution_design@1:reject"}, gateRows(t, st))

	rejected, err := st.ListGates(ctx, "BP1", "solution_design")
	require.NoError(t, err)
	require.Len(t, rejected, 1)
	assert.Equal(t, "slicing", rejected[0].Feedback, "the word after reject is the design's feedback")
}

func TestFold_AnAutoGateHandsSlicingToTheDesignReviewer(t *testing.T) {
	c, st, runner, rec, _ := newDialog(t, dialogScript())

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	driveIn(t, c, in)

	assert.Empty(t, inputsOf(runner, "slicing.review"), "slicing's review is folded into the design's")
	assert.Len(t, inputsOf(runner, "solution_design.review"), 1)
	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"slicing@1:fold", "slicing@1:approve", "solution_design@1:approve", "acceptance@1:approve"}, gateRows(t, st))
}

func TestFold_AnAutoFoldedGateApprovesTheRevisedDesign(t *testing.T) {
	script := dialogScript()
	script["solution_design.review"] = verdicts(scored(4, "name the owner"), fixed(4, "R1"))

	c, st, runner, rec, _ := newDialog(t, script)

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	driveIn(t, c, in)

	assert.Len(t, inputsOf(runner, "solution_design"), 2, "the design is revised once")
	assert.Len(t, inputsOf(runner, "solution_design.review"), 1, "the revision is not reviewed")
	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"slicing@1:fold", "slicing@1:approve", "solution_design@2:approve", "acceptance@1:approve"}, gateRows(t, st))

	gates, err := st.ListGates(context.Background(), "BP1", "solution_design")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestFold_TheFoldedGateShowsTheRevisedDesign(t *testing.T) {
	script := dialogScript()
	script["solution_design.review"] = verdicts(scored(4, "name the owner"), fixed(4, "R1"))

	c, st, runner, rec, _ := newDialog(t, script)

	drive(t, c, "BP1")

	designs := inputsOf(runner, "solution_design")
	require.Len(t, designs, 2, "the design is revised")
	assert.Contains(t, designs[1].Task, "name the owner")
	assert.Len(t, inputsOf(runner, "solution_design.review"), 1, "the revision is not reviewed")
	assert.Empty(t, inputsOf(runner, "slicing.review"))
	assert.Equal(t, []string{"slicing@1:fold"}, gateRows(t, st))

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, "solution_design", gate.Payload["stage"])
	assert.Equal(t, 2, gate.Payload["version"])
	assert.Equal(t, []any{map[string]any{"stage": "slicing", "version": 1, "body": "# Slices"}}, gate.Payload["folded"])

	verdict := reviewOf(t, gate)
	assert.Equal(t, true, verdict["revised"])
	assert.Equal(t, 1, verdict["judged"])
}

func TestFold_ARevisionThatCutsToOneSliceDoesNotFold(t *testing.T) {
	var slicings atomic.Int32

	script := dialogScript()
	script["slicing.review"] = verdicts(scored(4, "merge them"), fixed(4, "R1"))
	script["slicing"] = func(context.Context, stage.Input) (stage.Output, error) {
		if slicings.Add(1) == 1 {
			return done("# Slices", twoSlices()), nil
		}

		return done("# Slices", oneSlice()), nil
	}

	c, st, runner, rec, _ := newDialog(t, script)

	drive(t, c, "BP1")

	assert.Len(t, inputsOf(runner, "slicing"), 2, "slicing is revised")
	assert.Len(t, inputsOf(runner, "slicing.review"), 1)
	assert.Empty(t, inputsOf(runner, "solution_design"), "its gate waits")
	assert.Empty(t, gateRows(t, st), "the revision has its own gate")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, "slicing", gate.Payload["stage"])
	assert.Equal(t, 2, gate.Payload["version"])
	assert.Equal(t, true, reviewOf(t, gate)["revised"])
}

func TestSlicesValidator(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	tests := []struct {
		name   string
		slices []any
		want   string
	}{
		{name: "one slice", slices: oneSlice()["slices"].([]any)},
		{name: "none", slices: []any{}, want: "no_slices"},
		{name: "a repeated id", slices: []any{
			map[string]any{"id": "SL1", "title": "a", "projects": []any{"alpha"}},
			map[string]any{"id": "SL1", "title": "b", "projects": []any{"alpha"}},
		}, want: "slice_id"},
		{name: "no title", slices: []any{map[string]any{"id": "SL1", "title": " ", "projects": []any{"alpha"}}}, want: "slice_title"},
		{name: "a project the blueprint does not target", slices: []any{map[string]any{"id": "SL1", "title": "a", "projects": []any{"beta"}}}, want: "slice_project"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validate, _ := c.slicesValidator(bp)

			err := validate(ctx, stage.Artifact{Body: "# Slices", Data: map[string]any{"slices": tt.slices}})
			if tt.want == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), ": "+tt.want+": ")
		})
	}
}

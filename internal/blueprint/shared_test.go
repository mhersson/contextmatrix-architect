package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestBlueprintSteps_ASharedStageCollapsesBelowTwoSlices(t *testing.T) {
	pk := slicedPack(t)

	stepsFor := func(runs []store.Run, data map[string]any) []pipelineStep {
		t.Helper()

		var art *store.Artifact

		if data != nil {
			raw, err := json.Marshal(data)
			require.NoError(t, err)

			art = &store.Artifact{Stage: "slicing", Version: 1, Data: raw}
		}

		steps, _ := blueprintSteps(pk, runs, art, false)

		return steps
	}

	collapsed := func(runs []store.Run, data map[string]any) bool {
		t.Helper()

		steps := stepsFor(runs, data)

		return steps[stepIndex(steps, "shared_decisions")].Collapsed
	}

	assert.False(t, collapsed(nil, nil), "before slicing stores a version nothing is decided")
	assert.True(t, collapsed(nil, oneSlice()))
	assert.False(t, collapsed(nil, twoSlices()))
	assert.True(t, collapsed([]store.Run{{Stage: "solution_design", Kind: store.RunKindStage}}, twoSlices()), "a record from before slices never runs it")

	empty := map[string]any{"slices": []any{}}
	assert.True(t, collapsed(nil, empty), "a slicing with no slices collapses it")
	assert.Equal(t, []string{"solution_design", "grounding"}, keysIn(stepsFor(nil, empty), "solution_design", "grounding"),
		"and its per-slice stages are plain steps")
}

func TestShared_TwoSlicesRunItBeforeTheFirstSlice(t *testing.T) {
	c, st, runner, _ := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, []string{"shared_decisions", "solution_design@SL1", "grounding@SL1", "solution_design@SL2", "grounding@SL2"},
		stepRuns(runner, "shared_decisions", "solution_design", "grounding"))

	design2 := inputsOf(runner, "solution_design@SL2")
	require.Len(t, design2, 1)
	assert.Contains(t, design2[0].Task, "# Shared decisions", "every slice reads the shared decisions")
	assert.NotContains(t, gateRows(t, st), "shared_decisions@0:fold")
}

func TestShared_OneSliceCollapsesIntoTheFirstSliceDesign(t *testing.T) {
	script := slicedScript()
	script["slicing"] = returns("# Slices", oneSlice())

	c, st, runner, rec := newSliced(t, script)
	drive(t, c, "BP1")

	assert.Empty(t, inputsOf(runner, "shared_decisions"), "a collapsed stage never runs")
	assert.Equal(t, []string{"slicing@1:fold", "shared_decisions@0:fold"}, gateRows(t, st),
		"slicing folds, and the collapse is a fold at version 0")

	decided := rec.all(EventDecided)
	require.Len(t, decided, 2)
	assert.Equal(t, "solution_design@SL1", decided[1].Payload["feedback"], "the collapse names the step that settles the shared decisions")

	gate := lastGate(t, rec)
	assert.Equal(t, "solution_design@SL1", gate.Payload["stage"], "one design gate judges the slicing too")
	assert.Equal(t, []string{"approve", "reject", "reject slicing"}, gateReplies(gate))
}

func TestShared_TheCollapseIsRecordedOnceAcrossAReopen(t *testing.T) {
	script := slicedScript()
	script["slicing"] = returns("# Slices", oneSlice())

	st := slicedStore(t)
	c, _, _ := newSlicedOn(t, st, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.End(ctx, "BP1"))

	c2, _, _ := newSlicedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")
	require.NoError(t, c2.Message(ctx, "BP1", "approve", "m2"))
	c2.Wait("BP1")

	assert.Equal(t, 1, collapseRows(t, st), "a reopen and the steps after it record no second collapse")
}

// collapseRows counts the collapse rows of the shared stage.
func collapseRows(t *testing.T, st *store.Store) int {
	t.Helper()

	n := 0

	for _, row := range gateRows(t, st) {
		if row == "shared_decisions@0:fold" {
			n++
		}
	}

	return n
}

func TestShared_AReopenThatRerunsTheFirstSliceDesignRecordsNoSecondCollapse(t *testing.T) {
	var calls atomic.Int32

	entered := make(chan struct{})

	script := slicedScript()
	script["slicing"] = returns("# Slices", oneSlice())
	script["solution_design"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Design "+sliceOf(in), nil), nil
	}

	c, st, runner, rec := newSliced(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, entered, "SL1's design")
	require.NoError(t, c.End(ctx, "BP1"))
	c.Wait("BP1")

	// The record's last completed step is slicing, so the reopen passes the
	// collapsed stage again on its way to SL1's design.
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.Len(t, inputsOf(runner, "solution_design@SL1"), 2, "the design the End cut runs again")
	assert.Equal(t, "solution_design@SL1", lastGate(t, rec).Payload["stage"])
	assert.Equal(t, 1, collapseRows(t, st))
}

func TestShared_ANoteThatRerunsTheFirstSliceDesignRecordsNoSecondCollapse(t *testing.T) {
	var failed atomic.Bool

	script := slicedScript()
	script["slicing"] = returns("# Slices", oneSlice())
	script["solution_design"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		if !failed.Swap(true) {
			return stage.Output{}, errors.New("model unavailable")
		}

		return done("# Design "+sliceOf(in), nil), nil
	}

	c, st, runner, rec := newSliced(t, script)
	drive(t, c, "BP1")

	require.NoError(t, c.Message(context.Background(), "BP1", "try again", "m2"))
	c.Wait("BP1")

	require.Len(t, inputsOf(runner, "solution_design@SL1"), 2, "the note runs the failed design again")
	assert.Equal(t, "solution_design@SL1", lastGate(t, rec).Payload["stage"])
	assert.Equal(t, 1, collapseRows(t, st))
}

func TestShared_AReSliceToOneSliceCollapsesIt(t *testing.T) {
	var cuts atomic.Int32

	script := slicedScript()
	script["slicing"] = func(context.Context, stage.Input) (stage.Output, error) {
		if cuts.Add(1) == 1 {
			return done("# Slices", twoSlices()), nil
		}

		return done("# Slices", oneSlice()), nil
	}

	c, st, runner, rec := newSliced(t, script)
	drive(t, c, "BP1")
	require.Equal(t, "slicing", lastGate(t, rec).Payload["stage"], "two slices give slicing its own gate")

	require.NoError(t, c.Message(context.Background(), "BP1", "reject one slice is enough", "m2"))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "shared_decisions"))
	assert.Contains(t, gateRows(t, st), "shared_decisions@0:fold")
	assert.Equal(t, 1, collapseRows(t, st), "only the pass after the re-slice collapses it")
	assert.Equal(t, "solution_design@SL1", lastGate(t, rec).Payload["stage"])
}

func TestShared_AReSliceFromOneSliceToOneRecordsItsPassAgain(t *testing.T) {
	script := slicedScript()
	script["slicing"] = returns("# Slices", oneSlice())

	c, st, runner, rec := newSliced(t, script)
	drive(t, c, "BP1")
	require.Equal(t, "solution_design@SL1", lastGate(t, rec).Payload["stage"])

	require.NoError(t, c.Message(context.Background(), "BP1", "reject slicing name the export first", "m2"))
	c.Wait("BP1")

	require.Len(t, inputsOf(runner, "slicing"), 2, "slicing ran again")
	assert.Empty(t, inputsOf(runner, "shared_decisions"))
	assert.Equal(t, 2, collapseRows(t, st), "each live pass of the collapsed stage is recorded")
	assert.Equal(t, "solution_design@SL1", lastGate(t, rec).Payload["stage"])
}

func TestShared_AReSliceToTwoSlicesRunsIt(t *testing.T) {
	var cuts atomic.Int32

	script := slicedScript()
	script["slicing"] = func(context.Context, stage.Input) (stage.Output, error) {
		if cuts.Add(1) == 1 {
			return done("# Slices", oneSlice()), nil
		}

		return done("# Slices", twoSlices()), nil
	}

	c, _, runner, rec := newSliced(t, script)
	drive(t, c, "BP1")
	require.Equal(t, "solution_design@SL1", lastGate(t, rec).Payload["stage"])

	require.NoError(t, c.Message(context.Background(), "BP1", "reject slicing keep the schedule apart", "m2"))
	c.Wait("BP1")
	require.Equal(t, "slicing", lastGate(t, rec).Payload["stage"])

	approveUntil(t, c, rec, "shared_decisions")
	assert.Len(t, inputsOf(runner, "shared_decisions"), 1, "with two slices the shared stage runs")
}

func TestShared_TheGatePlanSaysWhatRepeatsAndWhatCollapses(t *testing.T) {
	c, _, _, rec := newSliced(t, slicedScript())
	drive(t, c, "BP1")

	plan, ok := rec.first(EventGatePlan)
	require.True(t, ok)

	entries := map[string]map[string]any{}

	for _, e := range anyList(plan.Payload["stages"]) {
		m := e.(map[string]any)
		entries[m["stage"].(string)] = m
	}

	assert.Equal(t, true, entries["shared_decisions"]["shared"])
	assert.Equal(t, true, entries["solution_design"]["per_slice"])
	assert.NotContains(t, entries["intake"], "per_slice")
	assert.Equal(t, "solution_design", entries["slicing"]["folds_into"], "slicing folds into the first slice's design, never into the shared stage")
}

func TestShared_AGatePlanAfterSlicingNamesThePackStageSlicingFoldsInto(t *testing.T) {
	c, st, _, rec := newSliced(t, slicedScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	for _, data := range []map[string]any{oneSlice(), twoSlices()} {
		raw, err := json.Marshal(data)
		require.NoError(t, err)

		_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "slicing", Body: "# Slices", Data: raw})
		require.NoError(t, err)

		c.postGatePlan(ctx, sessionOf(t, c, "BP1"), bp)

		plans := rec.all(EventGatePlan)
		require.NotEmpty(t, plans)

		entries := map[string]map[string]any{}

		for _, e := range anyList(plans[len(plans)-1].Payload["stages"]) {
			m := e.(map[string]any)
			entries[m["stage"].(string)] = m
		}

		assert.Equal(t, "solution_design", entries["slicing"]["folds_into"],
			"the plan names pack stages: never a slice's step and never the shared stage")
	}
}

func TestShared_ASlicingThatHoldsNoSlicesFailsItsRun(t *testing.T) {
	empty := map[string]any{"slices": []any{}}

	var runs atomic.Int32

	script := slicedScript()
	// The first run keeps submitting no slices: the check refuses twice and
	// then for good, as the runner reports it. The run continue starts
	// submits two slices.
	script["slicing"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if runs.Add(1) > 1 {
			return done("# Slices", twoSlices()), nil
		}

		var err error

		for range maxContractRounds {
			err = in.Validate(ctx, stage.Artifact{Body: "# Slices", Data: empty})
		}

		var refused *stage.FatalCheckError
		if !errors.As(err, &refused) {
			return stage.Output{}, fmt.Errorf("the third submit was not refused for good: %v", err)
		}

		return stage.Output{Result: harness.Result{Reason: stage.ReasonFatalCheck}}, fmt.Errorf("stage %s: %w", in.Stage, err)
	}

	c, st, runner, rec := newSliced(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")

	_, err := st.LatestArtifact(ctx, "BP1", "slicing")
	require.ErrorIs(t, err, store.ErrNotFound, "a submit with no slices is never stored")
	assert.False(t, rec.has(EventReport), "the pipeline does not end")

	e := lastError(t, rec)
	assert.Equal(t, stage.ReasonFatalCheck, e.Payload["reason"])
	assert.Contains(t, e.Payload["message"], sliceProblems(empty, []string{"alpha"})[0], "the failure names the fatal problem")

	all, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	last := all[len(all)-1]
	assert.Equal(t, "slicing", last.Stage)
	assert.Equal(t, store.RunFailed, last.Status)
	assert.Equal(t, stage.ReasonFatalCheck, last.Reason)

	require.NoError(t, c.Resume(ctx, "BP1", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{"slicing", "slicing"}, stepRuns(runner, "slicing"), "continue runs slicing again")

	_, err = st.LatestArtifact(ctx, "BP1", "slicing")
	require.NoError(t, err)
}

func TestAfter_ASlicingWithNoSlices(t *testing.T) {
	stored := problem("slicing", "no_slices", "slicing needs at least one slice")

	tests := []struct {
		name string
		// before are the plain runs of per-slice stages before slicing's: a
		// record from before slices.
		before []string
		data   map[string]any
		end    bool
		// finding is in the only finding of an end.
		finding string
	}{
		{name: "a fresh blueprint ends with the problems its check accepted", data: map[string]any{"slices": []any{}, problemsKey: []any{stored}}, end: true, finding: stored},
		{name: "one with no stored problems ends with the controller's", data: map[string]any{"slices": []any{}}, end: true, finding: "Slicing"},
		{name: "a record from before slices goes on as it did", before: []string{"solution_design"}, data: map[string]any{"slices": []any{}, problemsKey: []any{stored}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _ := newSliced(t, slicedScript())
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))

			for _, s := range append([]string{"intake"}, tt.before...) {
				ran(t, st, s, store.RunKindStage, 1, 1, 0.1)
				put(t, st, s, nil)
			}

			ran(t, st, "slicing", store.RunKindStage, 1, 1, 0.1)
			put(t, st, "slicing", tt.data)

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			out, err := c.after(ctx, bp, "slicing")
			require.NoError(t, err)
			assert.Equal(t, tt.end, out.end)

			if !tt.end {
				assert.Equal(t, "solution_design", out.stage, "the plain design follows, as before slices")

				return
			}

			assert.False(t, out.ready)
			require.Len(t, out.findings, 1)
			assert.Contains(t, out.findings[0], tt.finding)
		})
	}
}

// gateLabels lists the labels a gate event offers, in order.
func gateLabels(e Event) []string {
	var out []string

	for _, a := range anyList(e.Payload["actions"]) {
		out = append(out, a.(map[string]any)["label"].(string))
	}

	return out
}

func TestShared_ASliceDesignGateRejectsTheSharedDecisions(t *testing.T) {
	c, st, runner, rec := newSliced(t, slicedScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	approveUntil(t, c, rec, "solution_design@SL1")

	first := lastGate(t, rec)
	require.Equal(t, []string{"approve", "reject", "reject slicing", "reject shared_decisions"}, gateReplies(first))
	assert.Equal(t, "Reject slicing", gateLabels(first)[2])
	assert.Equal(t, "Reject shared decisions", gateLabels(first)[3])

	approveUntil(t, c, rec, "solution_design@SL2")
	require.NoError(t, c.Message(ctx, "BP1", "reject shared_decisions name exports by date", "m-shared"))
	c.Wait("BP1")

	shared := inputsOf(runner, "shared_decisions")
	require.Len(t, shared, 2)
	assert.Contains(t, shared[1].Task, "name exports by date", "the shared decisions run again with the feedback")

	assert.Equal(t, []string{
		"slicing@1:approve", "shared_decisions@1:approve", "solution_design@SL1@1:approve", "acceptance@SL1@1:approve",
		"shared_decisions@1:reject",
		"solution_design@SL1@1:superseded", "acceptance@SL1@1:superseded", "solution_design@SL2@1:superseded",
	}, gateRows(t, st), "the reject, then every later gated version it replaces: the first slice's design and acceptance, and the design at the gate")
	assert.Equal(t, "shared_decisions", lastGate(t, rec).Payload["stage"])

	approveUntil(t, c, rec, "solution_design@SL1")
	assert.Len(t, inputsOf(runner, "solution_design@SL1"), 2, "every slice runs again from the first")
}

func TestShared_AReopenAfterTheRejectRunsTheSharedDecisions(t *testing.T) {
	// The rejection is recorded and the process died before the shared
	// decisions ran again.
	c, st := newSlicedView(t)
	throughSliced(t, st, "decomposition@SL1")
	ran(t, st, "solution_design@SL2", store.RunKindStage, 1, 1, 0.1)
	put(t, st, "solution_design@SL2", nil)
	gateRow(t, st, "shared_decisions", 1, store.GateReject, store.ResponderHuman, "name exports by date")

	bp, err := st.GetBlueprint(context.Background(), viewID)
	require.NoError(t, err)

	out, _, err := c.position(context.Background(), bp)
	require.NoError(t, err)
	assert.Equal(t, "shared_decisions", out.stage)
}

func TestShared_ASecondSharedRejectSupersedesEachVersionOnce(t *testing.T) {
	c, st, _, rec := newSliced(t, slicedScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	approveUntil(t, c, rec, "solution_design@SL2")
	require.NoError(t, c.Message(ctx, "BP1", "reject shared_decisions name exports by date", "m-shared"))
	c.Wait("BP1")
	approveUntil(t, c, rec, "solution_design@SL1")

	before := len(gateRows(t, st))
	require.NoError(t, c.Message(ctx, "BP1", "reject shared_decisions keep the dates", "m-again"))
	c.Wait("BP1")

	assert.Equal(t, []string{"shared_decisions@2:reject", "solution_design@SL1@2:superseded"}, gateRows(t, st)[before:],
		"SL1's acceptance and SL2's design are already recorded superseded")
}

func TestShared_APlainRejectAtTheSharedGateSupersedesNothing(t *testing.T) {
	st := slicedStore(t)
	c, _, rec := newSlicedOn(t, st, slicedScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	approveUntil(t, c, rec, "solution_design@SL2")
	require.NoError(t, c.End(ctx, "BP1"))

	// The shared reject was recorded and the process died before its
	// superseded rows, so the later versions stand unmarked.
	gateRow(t, st, "shared_decisions", 1, store.GateReject, store.ResponderHuman, "name exports by date")

	c2, runner2, rec2 := newSlicedOn(t, st, slicedScript())
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")
	require.Len(t, inputsOf(runner2, "shared_decisions"), 1, "the reopen runs the rejected shared decisions")
	require.Equal(t, "shared_decisions", lastGate(t, rec2).Payload["stage"])

	before := len(gateRows(t, st))
	require.NoError(t, c2.Message(ctx, "BP1", "reject keep the dates", "m-own"))
	c2.Wait("BP1")

	assert.Equal(t, []string{"shared_decisions@2:reject"}, gateRows(t, st)[before:],
		"the shared stage's own gate rejects it as any gate rejects its own stage")
}

func TestShared_TheSupersededRowsSkipARejectedVersion(t *testing.T) {
	c, st := newSlicedView(t)
	throughSliced(t, st, "decomposition@SL1")
	gateRow(t, st, "acceptance@SL1", 1, store.GateReject, store.ResponderHuman, "split the criteria")
	ran(t, st, "solution_design@SL2", store.RunKindStage, 1, 1, 0.1)
	put(t, st, "solution_design@SL2", nil)

	before := len(gateRows(t, st))
	require.True(t, c.supersedeAfter(context.Background(), c.newAudit(), viewID, "shared_decisions", "m1"))

	assert.Equal(t, []string{"solution_design@SL1@1:superseded", "solution_design@SL2@1:superseded"}, gateRows(t, st)[before:],
		"a rejected version runs again anyway and stays rejected")
}

package blueprint

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestSignoff_RejectASliceRunsItsDesignAndEveryLaterSlice(t *testing.T) {
	script := slicedScript()
	script["slicing"] = returns("# Slices", threeSlices())
	script["readiness"] = returns("# Ready", readinessFor(true, "D1", "D2", "D3"))

	c, st, runner, rec := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))

	signoff, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []string{"reject slicing", "reject shared_decisions", "reject SL1", "reject SL2", "reject SL3"}, signoff.Payload["typed"])

	before, rowsBefore := len(stagesRun(runner)), len(gateRows(t, st))

	require.NoError(t, c.Message(context.Background(), "BP1", "reject SL2 export one report per team", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{
		"solution_design@SL2", "grounding@SL2", "acceptance@SL2", "decomposition@SL2",
		"solution_design@SL3", "grounding@SL3", "acceptance@SL3", "decomposition@SL3", "readiness",
	}, stagesRun(runner)[before:], "the slice's design and every later slice run again; SL1 stays")

	designs := inputsOf(runner, "solution_design@SL2")
	require.Len(t, designs, 2)
	assert.Contains(t, designs[1].Task, "export one report per team")

	rows := gateRows(t, st)
	assert.Contains(t, rows, "solution_design@SL2@1:reject")
	assert.Contains(t, rows, "solution_design@SL3@1:superseded")
	assert.NotContains(t, rows, "solution_design@SL1@1:superseded")
	require.GreaterOrEqual(t, len(rows), rowsBefore+5)
	assert.Equal(t, []string{
		"signoff@1:reject", "solution_design@SL2@1:reject",
		"acceptance@SL2@1:superseded", "solution_design@SL3@1:superseded", "acceptance@SL3@1:superseded",
	}, rows[rowsBefore:rowsBefore+5], "the sign-off's rejection, the target's, then what the re-run replaces")
	assert.Equal(t, 2, rec.count(EventSignOff), "the sign-off is shown again")

	var reruns []any

	for _, e := range rec.all(EventDecided) {
		if e.Payload["stage"] == signoffStage {
			reruns = append(reruns, e.Payload["reruns"])
		}
	}

	assert.Equal(t, []any{"solution_design@SL2"}, reruns, "the sign-off's rejection names what runs again")
}

func TestSignoff_RejectTheSharedDecisionsRunsEverySliceAgain(t *testing.T) {
	c, _, runner, _ := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))

	before := len(stagesRun(runner))

	require.NoError(t, c.Message(context.Background(), "BP1", "reject shared_decisions keep one file format", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{
		"shared_decisions",
		"solution_design@SL1", "grounding@SL1", "acceptance@SL1", "decomposition@SL1",
		"solution_design@SL2", "grounding@SL2", "acceptance@SL2", "decomposition@SL2", "readiness",
	}, stagesRun(runner)[before:])
}

func TestSignoff_ATypedRejectNamingNoStepIsANote(t *testing.T) {
	oneSliceScript := slicedScript()
	oneSliceScript["slicing"] = returns("# Slices", oneSlice())

	for _, tc := range []struct {
		name, reply string
		script      stageScript
	}{
		{"a slice the blueprint lacks", "reject SL9 the third slice", slicedScript()},
		{"shared decisions that collapsed", "reject shared_decisions one format", oneSliceScript},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, runner, rec := newSliced(t, tc.script)
			driveIn(t, c, autoStart("BP1"))

			before := len(stagesRun(runner))

			require.NoError(t, c.Message(context.Background(), "BP1", tc.reply, "m2"))
			c.Wait("BP1")

			assert.Len(t, stagesRun(runner), before, "nothing runs")

			notes := rec.all(EventNote)
			require.NotEmpty(t, notes)
			assert.NotEmpty(t, notes[len(notes)-1].Payload["hint"])
		})
	}
}

func TestSignoff_ATypedRejectWithoutFeedbackIsANote(t *testing.T) {
	c, _, runner, rec := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))

	before := len(stagesRun(runner))

	require.NoError(t, c.Message(context.Background(), "BP1", "reject SL2", "m2"))
	c.Wait("BP1")

	assert.Len(t, stagesRun(runner), before, "nothing runs")

	notes := rec.all(EventNote)
	require.NotEmpty(t, notes)

	hint := notes[len(notes)-1].Payload["hint"]
	assert.Contains(t, hint, "reject SL2 <feedback>")
	assert.NotContains(t, hint, "SL1", "the hint asks for the feedback the typed reply lacks, naming no other step")
}

func TestSignoff_PunctuationAfterTheStepWordIsANote(t *testing.T) {
	for _, tc := range []struct{ reply, names string }{
		{"reject SL2: fix the export", "`reject SL2 <feedback>`"},
		{"reject shared_decisions, keep one format", "`reject shared_decisions <feedback>`"},
		{"reject SL2:fix the export", "`reject SL2 <feedback>`"},
		{"reject shared_decisions;keep one format", "`reject shared_decisions <feedback>`"},
	} {
		t.Run(tc.reply, func(t *testing.T) {
			c, st, runner, rec := newSliced(t, slicedScript())
			driveIn(t, c, autoStart("BP1"))

			before, rowsBefore := len(stagesRun(runner)), len(gateRows(t, st))

			require.NoError(t, c.Message(context.Background(), "BP1", tc.reply, "m2"))
			c.Wait("BP1")

			assert.Len(t, stagesRun(runner), before, "nothing runs")
			assert.Len(t, gateRows(t, st), rowsBefore, "nothing is recorded")

			notes := rec.all(EventNote)
			require.NotEmpty(t, notes)

			hint := notes[len(notes)-1].Payload["hint"]
			assert.Contains(t, hint, tc.names)
			assert.NotContains(t, hint, "SL1", "the hint names the step typed and no other")
		})
	}
}

func TestNamesAStep(t *testing.T) {
	c, _, _, _ := newSliced(t, slicedScript())

	for _, tc := range []struct {
		reply string
		names bool
	}{
		{"reject SL2 fix the export", true},
		{"reject SL2:fix the export", true},
		{"reject shared_decisions;keep one format", true},
		{"reject SLA is unclear, fix it", false},
		{"reject the cards.", false},
		{"reject :fix the export", false},
	} {
		assert.Equal(t, tc.names, c.namesAStep(tc.reply), tc.reply)
	}
}

func TestSignoff_ATypedRejectRetargets(t *testing.T) {
	oneSliceScript := slicedScript()
	oneSliceScript["slicing"] = returns("# Slices", oneSlice())
	oneSliceScript["readiness"] = returns("# Ready", readinessFor(true, "D1"))

	tests := []struct {
		name, reply string
		script      stageScript
		runs, rows  []string
	}{
		{
			name: "one slice's design, which no text names", reply: "reject SL1 keep one export format", script: oneSliceScript,
			runs: []string{"solution_design@SL1", "grounding@SL1", "acceptance@SL1", "decomposition@SL1", "readiness"},
			rows: []string{"signoff@1:reject", "solution_design@SL1@1:reject"},
		},
		{
			name: "a lower-case reply", reply: "reject sl2 export one report per team", script: slicedScript(),
			runs: []string{"solution_design@SL2", "grounding@SL2", "acceptance@SL2", "decomposition@SL2", "readiness"},
			rows: []string{"signoff@1:reject", "solution_design@SL2@1:reject"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, _ := newSliced(t, tt.script)
			driveIn(t, c, autoStart("BP1"))

			before, rowsBefore := len(stagesRun(runner)), len(gateRows(t, st))

			require.NoError(t, c.Message(context.Background(), "BP1", tt.reply, "m2"))
			c.Wait("BP1")

			assert.Equal(t, tt.runs, stagesRun(runner)[before:], "the design and everything after it run again")

			rows := gateRows(t, st)
			require.GreaterOrEqual(t, len(rows), rowsBefore+2)
			assert.Equal(t, tt.rows, rows[rowsBefore:rowsBefore+2], "the sign-off's rejection, then the design's")
		})
	}
}

// A reopen reads a typed rejection's target from the loop stage's verdict,
// so a pack whose last stage loops to nothing records none.
func TestSignoff_ATypedRejectWithoutALoopIsANote(t *testing.T) {
	c, st, runner, rec := newSliced(t, slicedScript())

	for i := range c.opts.Pack.Stages {
		if c.opts.Pack.Stages[i].ID == "readiness" {
			c.opts.Pack.Stages[i].LoopsTo = ""
		}
	}

	driveIn(t, c, autoStart("BP1"))
	require.True(t, rec.has(EventSignOff))

	before, rowsBefore := len(stagesRun(runner)), len(gateRows(t, st))

	require.NoError(t, c.Message(context.Background(), "BP1", "reject SL2 export one report per team", "m2"))
	c.Wait("BP1")

	assert.Len(t, stagesRun(runner), before, "nothing runs")
	assert.Len(t, gateRows(t, st), rowsBefore, "nothing is recorded")

	notes := rec.all(EventNote)
	require.NotEmpty(t, notes)
	assert.NotEmpty(t, notes[len(notes)-1].Payload["hint"])
}

// With one slice nothing slice-related shows: the hints name no typed reply.
func TestSignoff_OneSlicesHintsNameNoSlice(t *testing.T) {
	script := slicedScript()
	script["slicing"] = returns("# Slices", oneSlice())
	script["readiness"] = returns("# Ready", readinessFor(true, "D1"))

	for _, reply := range []string{"looks good to me", "reject SL9 the third slice"} {
		t.Run(reply, func(t *testing.T) {
			c, _, _, rec := newSliced(t, script)
			driveIn(t, c, autoStart("BP1"))

			signoff, ok := rec.first(EventSignOff)
			require.True(t, ok)
			require.Equal(t, []string{"reject slicing", "reject SL1"}, signoff.Payload["typed"], "the payload keeps the raw replies")

			require.NoError(t, c.Message(context.Background(), "BP1", reply, "m2"))
			c.Wait("BP1")

			notes := rec.all(EventNote)
			require.NotEmpty(t, notes)

			hint := notes[len(notes)-1].Payload["hint"]
			assert.NotEmpty(t, hint)
			assert.NotContains(t, hint, "SL1")
			assert.Contains(t, hint, "reject slicing")
		})
	}
}

func TestSignoff_AReopenAfterATypedRejectRunsItsTarget(t *testing.T) {
	// The rejections are recorded and the process died before the target ran.
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{name: "a typed rejection runs its target", target: "solution_design@SL2", want: "solution_design@SL2"},
		{name: "a plain rejection runs every slice's cards from the first", want: "decomposition@SL1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st := newSlicedView(t)
			throughSliced(t, st, "readiness")
			gateRow(t, st, signoffStage, 1, store.GateReject, store.ResponderHuman, "export one report per team")

			if tt.target != "" {
				gateRow(t, st, tt.target, 1, store.GateReject, store.ResponderHuman, "export one report per team")
			}

			bp, err := st.GetBlueprint(context.Background(), viewID)
			require.NoError(t, err)

			out, _, err := c.position(context.Background(), bp)
			require.NoError(t, err)
			assert.Equal(t, tt.want, out.stage)
		})
	}
}

func TestSignoff_TwoRejectedStepsRunTheEarliest(t *testing.T) {
	// Each typed rejection's target stores a new version before the next
	// sign-off, so two never stand at once; should they, the earliest runs,
	// and every later step with it.
	c, st := newSlicedView(t)
	throughSliced(t, st, "readiness")
	gateRow(t, st, signoffStage, 1, store.GateReject, store.ResponderHuman, "one format")
	gateRow(t, st, "solution_design@SL2", 1, store.GateReject, store.ResponderHuman, "one report")
	gateRow(t, st, "shared_decisions", 1, store.GateReject, store.ResponderHuman, "one format")

	steps, _, err := c.steps(context.Background(), viewID)
	require.NoError(t, err)

	target, err := c.signoffTarget(context.Background(), viewID, steps)
	require.NoError(t, err)
	assert.Equal(t, "shared_decisions", target)
}

func TestSignoff_AnEndBeforeATypedRejectsRerunResumesItOnTheReopen(t *testing.T) {
	var rejected, cut atomic.Bool

	script := slicedScript()
	design := script["solution_design"]
	script["solution_design"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		// The End cuts the rejected design's re-run short.
		if rejected.Load() && !cut.Swap(true) {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return design(ctx, in)
	}

	c, _, runner, rec := newSliced(t, script)
	ctx := context.Background()

	driveIn(t, c, autoStart("BP1"))
	require.True(t, rec.has(EventSignOff))

	before := len(stagesRun(runner))

	rejected.Store(true)
	require.NoError(t, c.Message(ctx, "BP1", "reject SL2 export one report per team", "m2"))
	require.Eventually(t, func() bool { return len(inputsOf(runner, "solution_design@SL2")) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.End(ctx, "BP1"))
	c.Wait("BP1")

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	c.Wait("BP1")

	assert.Equal(t, []string{
		"solution_design@SL2", "solution_design@SL2", "grounding@SL2", "acceptance@SL2", "decomposition@SL2", "readiness",
	}, stagesRun(runner)[before:], "the cut re-run runs again under its key, and SL1 never runs again")

	designs := inputsOf(runner, "solution_design@SL2")
	require.Len(t, designs, 3)
	assert.Contains(t, designs[2].Task, "export one report per team", "the reopen's run gets the feedback")
	assert.Equal(t, 2, rec.count(EventSignOff), "the sign-off is shown again")
}

func TestRejectSlicing_EverySliceDesignGateOffersIt(t *testing.T) {
	var slicings atomic.Int32

	script := slicedScript()
	script["slicing"] = func(context.Context, stage.Input) (stage.Output, error) {
		if slicings.Add(1) == 1 {
			return done("# Slices", twoSlices()), nil
		}

		return done("# Slices", threeSlices()), nil
	}

	c, st, runner, rec := newSliced(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	approveUntil(t, c, rec, "solution_design@SL1")

	first := lastGate(t, rec)
	assert.Equal(t, []string{"approve", "reject", "reject slicing", "reject shared_decisions"}, gateReplies(first))
	assert.Equal(t, "Reject slicing", gateLabels(first)[2])

	approveUntil(t, c, rec, "solution_design@SL2")
	assert.Equal(t, []string{"approve", "reject", "reject slicing", "reject shared_decisions"}, gateReplies(lastGate(t, rec)))

	before := len(gateRows(t, st))

	require.NoError(t, c.Message(ctx, "BP1", "reject slicing cut the exports by team", "m-slicing"))
	c.Wait("BP1")

	assert.Equal(t, []string{
		"slicing@1:reject",
		"shared_decisions@1:superseded", "solution_design@SL1@1:superseded", "acceptance@SL1@1:superseded", "solution_design@SL2@1:superseded",
	}, gateRows(t, st)[before:], "the reject, then every later gated version it replaces, the design at the gate included")

	slicingRuns := inputsOf(runner, "slicing")
	require.Len(t, slicingRuns, 2)
	assert.Contains(t, slicingRuns[1].Task, "cut the exports by team", "slicing runs again with the feedback")

	approveUntil(t, c, rec, "solution_design@SL3")
	assert.Len(t, inputsOf(runner, "shared_decisions"), 2, "everything after slicing runs again from the new slicing")
}

func TestRejectSlicing_ToFewerSlicesSupersedesTheDroppedSlice(t *testing.T) {
	var slicings atomic.Int32

	script := slicedScript()
	script["slicing"] = func(context.Context, stage.Input) (stage.Output, error) {
		if slicings.Add(1) == 1 {
			return done("# Slices", twoSlices()), nil
		}

		return done("# Slices", oneSlice()), nil
	}

	c, _, _, rec := newSliced(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	approveUntil(t, c, rec, "solution_design@SL2")
	require.NoError(t, c.Message(ctx, "BP1", "reject slicing one slice is enough", "m-slicing"))
	c.Wait("BP1")

	gate := lastGate(t, rec)
	assert.Equal(t, "solution_design@SL1", gate.Payload["stage"], "the new slicing folds into SL1's design gate")
	assert.Equal(t, []string{"approve", "reject", "reject slicing"}, gateReplies(gate))

	v, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, v.Slices, 1)
	require.True(t, slices.ContainsFunc(v.Artifacts, func(r protocol.ArtifactRef) bool { return r.Slice == "SL2" }))

	for _, r := range v.Artifacts {
		if r.Slice == "SL2" {
			assert.Equal(t, artSuperseded, r.Status, "%s %s v%d", r.Stage, r.Slice, r.Version)
		}
	}
}

func TestRejectSlicing_AtTheSignoff(t *testing.T) {
	c, st, runner, rec := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))

	signoff, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []string{"reject slicing", "reject shared_decisions", "reject SL1", "reject SL2"}, signoff.Payload["typed"])

	before, rowsBefore := len(stagesRun(runner)), len(gateRows(t, st))

	require.NoError(t, c.Message(context.Background(), "BP1", "reject slicing put the UI first", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{
		"slicing", "shared_decisions",
		"solution_design@SL1", "grounding@SL1", "acceptance@SL1", "decomposition@SL1",
		"solution_design@SL2", "grounding@SL2", "acceptance@SL2", "decomposition@SL2", "readiness",
	}, stagesRun(runner)[before:])

	rows := gateRows(t, st)
	assert.Equal(t, []string{"signoff@1:reject", "slicing@1:reject"}, rows[rowsBefore:rowsBefore+2])
	assert.Equal(t, []string{
		"shared_decisions@1:superseded", "solution_design@SL1@1:superseded", "acceptance@SL1@1:superseded",
		"solution_design@SL2@1:superseded", "acceptance@SL2@1:superseded",
	}, rows[rowsBefore+2:rowsBefore+7], "every later gated version the reject replaces")

	var reruns []any

	for _, e := range rec.all(EventDecided) {
		if e.Payload["stage"] == signoffStage {
			reruns = append(reruns, e.Payload["reruns"])
		}
	}

	assert.Equal(t, []any{"slicing"}, reruns)
}

func TestRejectSlicing_AReopenAfterTheRejectRunsSlicing(t *testing.T) {
	// The rejection at SL2's design gate is recorded and the process died
	// before slicing ran again.
	c, st := newSlicedView(t)
	throughSliced(t, st, "decomposition@SL1")
	ran(t, st, "solution_design@SL2", store.RunKindStage, 1, 1, 0.1)
	put(t, st, "solution_design@SL2", nil)
	gateRow(t, st, "slicing", 1, store.GateReject, store.ResponderHuman, "one slice is enough")

	bp, err := st.GetBlueprint(context.Background(), viewID)
	require.NoError(t, err)

	out, _, err := c.position(context.Background(), bp)
	require.NoError(t, err)
	assert.Equal(t, "slicing", out.stage)
}

func TestRejectSlicing_AOneSliceDesignGateReopenedOffersItOnce(t *testing.T) {
	script := slicedScript()
	script["slicing"] = returns("# Slices", oneSlice())
	script["readiness"] = returns("# Ready", readinessFor(true, "D1"))

	c, _, _, rec := newSliced(t, script)
	drive(t, c, "BP1")

	for i := 0; i < 20 && !rec.has(EventSignOff); i++ {
		require.NoError(t, c.Message(context.Background(), "BP1", "approve", fmt.Sprintf("approve-%d", i)))
		c.Wait("BP1")
	}

	require.True(t, rec.has(EventSignOff))

	// reject SL1 matches below two slices though it is not listed there.
	require.NoError(t, c.Message(context.Background(), "BP1", "reject SL1 redo the design", "m2"))
	c.Wait("BP1")

	gate := lastGate(t, rec)
	assert.Equal(t, "solution_design@SL1", gate.Payload["stage"])
	assert.Equal(t, []string{"approve", "reject", "reject slicing"}, gateReplies(gate))
}

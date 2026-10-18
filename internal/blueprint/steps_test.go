package blueprint

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func slicedPack(t *testing.T) *pack.Pack {
	t.Helper()

	pk, err := pack.LoadDir("testdata/sliced")
	require.NoError(t, err)

	return pk
}

// newSliced is newPipeline on the sliced test pack: slicing behind a
// reviewed gate that folds with one slice, then the per-slice stages.
func newSliced(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	c, st, runner, rec, _ := newPipeline(t, script)
	c.opts.Pack = slicedPack(t)

	return c, st, runner, rec
}

// newSlicedOn is newSliced on an existing store, as a restart finds it.
func newSlicedOn(t *testing.T, st *store.Store, script stageScript) (*Controller, *fakeRunner, *recorder) {
	t.Helper()

	c, _, runner, rec, _ := newPipelineOn(t, st, script, nil)
	c.opts.Pack = slicedPack(t)

	return c, runner, rec
}

func slicedStore(t *testing.T) *store.Store {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	return st
}

// sliceOf is the slice a run is for, a stage's, its reviewer's or its
// responder's; "" for a whole-blueprint run.
func sliceOf(in stage.Input) string {
	_, slice := pack.SplitStep(in.Stage)
	slice, _, _ = strings.Cut(slice, ".")

	return slice
}

// bySlice submits title and the slice of the run, so a test can see which
// slice's artifact reached a task.
func bySlice(title string) func(context.Context, stage.Input) (stage.Output, error) {
	return func(_ context.Context, in stage.Input) (stage.Output, error) {
		return done(strings.TrimSpace(title+" "+sliceOf(in)), nil), nil
	}
}

// slicedScript answers every stage of the sliced test pack: two slices, and
// every review passing.
func slicedScript() stageScript {
	return stageScript{
		"intake":                  returns("# Need", nil),
		"slicing":                 returns("# Slices", twoSlices()),
		"slicing.review":          passing(),
		"shared_decisions":        returns("# Shared decisions", sharedDecisions()),
		"shared_decisions.review": passing(),
		"solution_design":         bySlice("# Design"),
		"solution_design.review":  passing(),
		"grounding":               bySlice("# Grounding"),
		"acceptance":              bySlice("# Acceptance"),
		"acceptance.review":       passing(),
		"decomposition":           sliceCards(),
		"readiness":               returns("# Ready", readinessFor(true, "D1", "D2")),
	}
}

// sliceCards submits one alpha card per slice, D<n> for SL<n>, every later
// one depending on D1; every draft carries the playbook's title.
func sliceCards() func(context.Context, stage.Input) (stage.Output, error) {
	return func(_ context.Context, in stage.Input) (stage.Output, error) {
		n := cmp.Or(strings.TrimPrefix(sliceOf(in), "SL"), "1")
		id := "D" + n

		var deps []any
		if n != "1" {
			deps = []any{"D1"}
		}

		return done("# Cards "+sliceOf(in), draftOf("Export the reports", cardFor(id, nil, deps...))), nil
	}
}

// cardFor is goodCards' alpha card as id, carrying criteria and depending
// on deps.
func cardFor(id string, criteria []any, deps ...any) map[string]any {
	card := goodCards()["cards"].([]any)[0].(map[string]any)

	card["id"], card["title"], card["depends_on"] = id, "Add export step "+id, append([]any{}, deps...)

	if criteria != nil {
		card["criteria"] = criteria
	}

	return card
}

// draftOf is a draft of cards in their order, with title.
func draftOf(title string, cards ...map[string]any) map[string]any {
	list, order := make([]any, 0, len(cards)), make([]any, 0, len(cards))

	for _, c := range cards {
		list = append(list, c)
		order = append(order, c["id"])
	}

	return map[string]any{"title": title, "cards": list, "order": order}
}

// stepRuns lists, in order, the step of every stage run of the given stages
// the fake runner was asked for.
func stepRuns(r *fakeRunner, stages ...string) []string {
	var out []string

	for _, key := range stagesRun(r) {
		if id, _ := pack.SplitStep(key); slices.Contains(stages, id) {
			out = append(out, key)
		}
	}

	return out
}

// lastGate is the newest gate shown.
func lastGate(t *testing.T, rec *recorder) Event {
	t.Helper()

	gates := rec.all(EventGate)
	require.NotEmpty(t, gates, "a gate was shown")

	return gates[len(gates)-1]
}

// approveUntil approves every gate BP1 shows until key's is, which it
// leaves open, so a pack that grows gates before key's keeps a test as it
// is.
func approveUntil(t *testing.T, c *Controller, rec *recorder, key string) {
	t.Helper()

	for range 20 {
		if lastGate(t, rec).Payload["stage"] == key {
			return
		}

		require.NoError(t, c.Message(context.Background(), "BP1", "approve", fmt.Sprintf("approve-%d", len(rec.all(EventGate)))))
		c.Wait("BP1")
	}

	t.Fatalf("no gate of %s was shown", key)
}

func TestBlueprintSteps(t *testing.T) {
	pk := slicedPack(t)

	slicing := func(data map[string]any) *store.Artifact {
		raw, err := json.Marshal(data)
		require.NoError(t, err)

		return &store.Artifact{Stage: "slicing", Version: 1, Data: raw}
	}

	plain := []string{"intake", "slicing", "solution_design", "grounding"}
	two := []string{"intake", "slicing", "solution_design@SL1", "grounding@SL1", "solution_design@SL2", "grounding@SL2"}
	twoPieces := []piece{{id: "SL1", title: "Ship the endpoint"}, {id: "SL2", title: "Show it in the UI"}}

	tests := []struct {
		name    string
		runs    []store.Run
		slicing *store.Artifact
		want    []string
		pieces  []piece
	}{
		{name: "before slicing stores a version", want: plain},
		{name: "two slices", slicing: slicing(twoSlices()), want: two, pieces: twoPieces},
		{
			name: "one slice is SL1", slicing: slicing(oneSlice()),
			want: []string{"intake", "slicing", "solution_design@SL1", "grounding@SL1"}, pieces: twoPieces[:1],
		},
		{
			name: "ids follow slicing's order, whatever the data says",
			slicing: slicing(map[string]any{"slices": []any{
				map[string]any{"id": "B", "title": "Ship the endpoint"}, map[string]any{"id": "A", "title": "Show it in the UI"},
			}}),
			want: two, pieces: twoPieces,
		},
		{name: "runs under step keys keep the slices", runs: []store.Run{{Stage: "solution_design@SL1", Kind: store.RunKindStage}}, slicing: slicing(twoSlices()), want: two, pieces: twoPieces},
		{name: "a record from before slices", runs: []store.Run{{Stage: "solution_design", Kind: store.RunKindStage}}, slicing: slicing(twoSlices()), want: plain},
		{name: "a review under the plain id is from before slices too", runs: []store.Run{{Stage: "solution_design", Kind: store.RunKindReviewer}}, slicing: slicing(twoSlices()), want: plain},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			steps, pieces := blueprintSteps(pk, tt.runs, tt.slicing)

			assert.Equal(t, tt.want, keysIn(steps, "intake", "slicing", "solution_design", "grounding"))
			assert.Equal(t, tt.pieces, pieces)
		})
	}
}

// keysIn lists the keys of the steps of the given stages, in order, so a
// test keeps its meaning while the test pack grows stages around them.
func keysIn(steps []pipelineStep, stages ...string) []string {
	var out []string

	for _, s := range steps {
		if slices.Contains(stages, s.Stage.ID) {
			out = append(out, s.ID)
		}
	}

	return out
}

func TestConsumedSteps(t *testing.T) {
	steps := stepsOf(slicedPack(t), []piece{{id: "SL1"}, {id: "SL2"}, {id: "SL3"}}, false)

	keys := func(st pipelineStep) []string {
		return keysIn(consumedSteps(steps, st), "intake", "slicing", "solution_design", "grounding")
	}

	at := func(key string) pipelineStep { return steps[stepIndex(steps, key)] }

	assert.Equal(t, []string{"intake", "slicing", "solution_design@SL3", "solution_design@SL1", "solution_design@SL2"}, keys(at("grounding@SL3")),
		"its own slice's design, then every earlier slice's design, slice by slice")
	assert.Equal(t, []string{"intake", "slicing"}, keys(at("solution_design@SL1")), "the first slice has no earlier one")

	whole := pipelineStep{Stage: pack.Stage{ID: "check", Consumes: []string{"slicing", "grounding"}}, ID: "check"}
	assert.Equal(t, []string{"slicing", "grounding@SL1", "grounding@SL2", "grounding@SL3"}, keys(whole),
		"a whole-blueprint step reads every slice's, in slicing order")
}

func TestEarlierSteps(t *testing.T) {
	steps := stepsOf(slicedPack(t), []piece{{id: "SL1"}, {id: "SL2"}, {id: "SL3"}}, false)
	at := func(key string) pipelineStep { return steps[stepIndex(steps, key)] }

	tests := []struct {
		name string
		st   pipelineStep
		want []string
	}{
		{name: "the last slice's step", st: at("grounding@SL3"), want: []string{"grounding@SL1", "grounding@SL2"}},
		{name: "the first slice has no earlier one", st: at("solution_design@SL1")},
		{name: "a whole-blueprint step is no slice's", st: at("slicing")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, keysIn(earlierSteps(steps, tt.st), "intake", "slicing", "solution_design", "grounding"))
		})
	}
}

func TestStepName_NamesTheSliceOnlyWithTwoOrMore(t *testing.T) {
	c, st, _, _ := newSliced(t, slicedScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	assert.Equal(t, "Solution design", c.stepName(ctx, "BP1", "solution_design"), "before slicing stores a version")

	for _, tc := range []struct {
		slicing map[string]any
		want    string
	}{
		{oneSlice(), "Solution design"},
		{twoSlices(), "Solution design (SL1)"},
	} {
		raw, err := json.Marshal(tc.slicing)
		require.NoError(t, err)

		_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "slicing", Body: "# Slices", Data: raw})
		require.NoError(t, err)

		assert.Equal(t, tc.want, c.stepName(ctx, "BP1", "solution_design@SL1"), "slicing's newest version decides")
	}
}

func TestSlices_EachPerSliceStageRunsOncePerSliceInSlicingOrder(t *testing.T) {
	c, st, runner, _ := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, []string{"solution_design@SL1", "grounding@SL1", "solution_design@SL2", "grounding@SL2"},
		stepRuns(runner, "solution_design", "grounding"))

	art, err := st.LatestArtifact(context.Background(), "BP1", "grounding@SL2")
	require.NoError(t, err)
	assert.Equal(t, "# Grounding SL2", art.Body, "a slice's artifact is stored under its step")

	_, err = st.LatestArtifact(context.Background(), "BP1", "grounding")
	assert.ErrorIs(t, err, store.ErrNotFound, "nothing is stored under the plain id")
}

func TestSlices_ASliceReadsItsOwnInputsAndTheEarlierSlicesItNames(t *testing.T) {
	c, _, runner, _ := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))

	design1 := inputsOf(runner, "solution_design@SL1")
	design2 := inputsOf(runner, "solution_design@SL2")
	ground2 := inputsOf(runner, "grounding@SL2")

	require.Len(t, design1, 1)
	require.Len(t, design2, 1)
	require.Len(t, ground2, 1)

	assert.Contains(t, design1[0].Task, "# Slices", "a slice reads the whole-blueprint stages it consumes")
	assert.NotContains(t, design1[0].Task, "# Design SL2")
	assert.Contains(t, design2[0].Task, "# Design SL1", "a later slice reads every earlier slice's design")
	assert.NotContains(t, design2[0].Task, "# Grounding SL1", "an earlier slice's grounding is not among what it names")
	assert.Contains(t, ground2[0].Task, "# Design SL2", "grounding reads its own slice's design")
	assert.Contains(t, ground2[0].Task, "# Design SL1")
	assert.NotContains(t, ground2[0].Task, "# Grounding SL1")
}

func TestSlices_OneSliceIsSL1AndSlicingFoldsIntoItsDesignGate(t *testing.T) {
	script := slicedScript()
	script["slicing"] = returns("# Slices", oneSlice())

	c, st, runner, rec := newSliced(t, script)
	drive(t, c, "BP1")

	assert.Equal(t, []string{"solution_design@SL1"}, stepRuns(runner, "solution_design", "grounding"))
	assert.Contains(t, gateRows(t, st), "slicing@1:fold")

	gate := lastGate(t, rec)
	assert.Equal(t, "solution_design@SL1", gate.Payload["stage"], "slicing's gate folds into the first slice's design gate")
	assert.Equal(t, []string{"approve", "reject", "reject slicing"}, gateReplies(gate))
}

func TestSlices_ARejectAtASliceGateRunsThatSliceAgain(t *testing.T) {
	c, _, runner, rec := newSliced(t, slicedScript())
	drive(t, c, "BP1")

	approveUntil(t, c, rec, "solution_design@SL2")
	require.NoError(t, c.Message(context.Background(), "BP1", "reject name the export format", "m-reject"))
	c.Wait("BP1")

	runs := inputsOf(runner, "solution_design@SL2")
	require.Len(t, runs, 2)
	assert.Contains(t, runs[1].Task, "name the export format", "the rejection's feedback reaches the slice's next run")
	assert.Len(t, inputsOf(runner, "solution_design@SL1"), 1, "an earlier slice stays")
	assert.Equal(t, "solution_design@SL2", lastGate(t, rec).Payload["stage"])
}

func TestSlices_ARestartMidSliceRerunsTheStepItCut(t *testing.T) {
	script := slicedScript()
	script["grounding"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if sliceOf(in) == "SL2" {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Grounding "+sliceOf(in), nil), nil
	}

	st := slicedStore(t)
	c, runner, _ := newSlicedOn(t, st, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return len(inputsOf(runner, "grounding@SL2")) == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	c2, runner2, rec2 := newSlicedOn(t, st, slicedScript())
	require.NoError(t, c2.Start(ctx, autoStart("BP1")))
	c2.Wait("BP1")

	assert.Equal(t, []string{"grounding@SL2"}, stepRuns(runner2, "solution_design", "grounding"),
		"the cut step runs again under its key, and no earlier step does")
	assert.True(t, rec2.has(EventReport))
}

func TestSlices_AnEndDuringASliceRoundResumesThatRound(t *testing.T) {
	script := slicedScript()
	script["solution_design"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if sliceOf(in) == "SL2" && in.History == nil {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Format", "CSV or JSON?", "CSV", "JSON")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		return done("# Design "+sliceOf(in)+"\n\n"+in.Task, nil), nil
	}

	st := slicedStore(t)
	c, _, rec := newSlicedOn(t, st, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "solution_design", "human", "m0"))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	// A gate reply covers every slice of the stage: SL1's design gate waits.
	approveUntil(t, c, rec, "solution_design@SL1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Equal(t, "solution_design@SL2", latestRound(t, c, "BP1", 1).Frame.Stage)
	require.NoError(t, c.End(ctx, "BP1"))

	c2, runner2, _ := newSlicedOn(t, st, script)
	require.NoError(t, c2.Start(ctx, autoStart("BP1")))
	c2.Wait("BP1")
	assert.Empty(t, stagesRun(runner2), "the round waits on the reopen; nothing runs")

	answerRound(t, c2, "BP1", 1, true, pick(1, 2))
	c2.Wait("BP1")

	runs := inputsOf(runner2, "solution_design@SL2")
	require.Len(t, runs, 1)
	assert.NotNil(t, runs[0].History, "the slice's design resumes the conversation that asked")
	assert.Contains(t, runs[0].Task, "JSON")
	assert.Empty(t, inputsOf(runner2, "solution_design@SL1"))
}

func TestSlices_AReopenAtASliceGateShowsThatGateAgain(t *testing.T) {
	st := slicedStore(t)
	c, _, rec := newSlicedOn(t, st, slicedScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	approveUntil(t, c, rec, "solution_design@SL2")
	require.NoError(t, c.End(ctx, "BP1"))

	c2, runner2, rec2 := newSlicedOn(t, st, slicedScript())
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	assert.Equal(t, "solution_design@SL2", lastGate(t, rec2).Payload["stage"])
	assert.Empty(t, stagesRun(runner2))

	require.NoError(t, c2.Message(ctx, "BP1", "approve", "m9"))
	c2.Wait("BP1")
	assert.Equal(t, []string{"grounding@SL2"}, stepRuns(runner2, "solution_design", "grounding"))
}

func TestSlices_ANoteAfterASliceStepFailedRunsThatStepAgain(t *testing.T) {
	var failed atomic.Bool

	script := slicedScript()
	script["grounding"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		if sliceOf(in) == "SL2" && !failed.Swap(true) {
			return stage.Output{}, errors.New("model unavailable")
		}

		return done("# Grounding "+sliceOf(in), nil), nil
	}

	c, _, runner, rec := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))
	require.False(t, rec.has(EventReport), "the pipeline stopped at the failed step")

	require.NoError(t, c.Message(context.Background(), "BP1", "try again", "m2"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "grounding@SL2"), 2)
	assert.Len(t, inputsOf(runner, "grounding@SL1"), 1)
	assert.True(t, rec.has(EventReport))
}

func TestSlices_ARecordFromBeforeSlicesRunsItsStagesOnce(t *testing.T) {
	// A blueprint whose design covered every slice at once, under its plain
	// id, waits at that design's gate when the pack starts repeating it per
	// slice.
	st := slicedStore(t)
	c, runner, rec := newSlicedOn(t, st, slicedScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))

	_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: needStage, Body: "the need"})
	require.NoError(t, err)

	for _, s := range []string{"intake", "slicing", "solution_design"} {
		ran(t, st, s, store.RunKindStage, 1, 1, 0.1)
		v := put(t, st, s, stageData(s))

		if s == "slicing" {
			gateRow(t, st, s, v, store.GateApprove, store.ResponderHuman, "")
		}
	}

	put(t, st, reviewStage("solution_design"), map[string]any{"score": 5, "findings": []any{}, "version": 1, "attempt": 1, "round": 1})

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")
	assert.Equal(t, "solution_design", lastGate(t, rec).Payload["stage"], "the plain design's gate is shown again")

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{"grounding"}, stepRuns(runner, "solution_design", "grounding"), "grounding runs once, for the whole blueprint")
	assert.Empty(t, inputsOf(runner, "shared_decisions"), "a record from before slices never runs the shared stage")

	signoff, ok := rec.first(EventSignOff)
	require.True(t, ok, "the cards stored under the plain id reach the sign-off")
	assert.Equal(t, []string{"D1"}, signoff.Payload["order"])
}

// codeSlicedPack is the sliced test pack with the code tools on grounding,
// so the first stage that checks the code out runs per slice.
func codeSlicedPack(t *testing.T) *pack.Pack {
	t.Helper()

	pk := slicedPack(t)

	i := slices.IndexFunc(pk.Stages, func(s pack.Stage) bool { return s.ID == "grounding" })
	require.GreaterOrEqual(t, i, 0, "the sliced pack has grounding")

	pk.Stages[i].Tools = []string{pack.ToolCode}

	return pk
}

func TestSlices_TheSetupIsReportedOnceAfterTheFirstSliceOfTheCheckoutStage(t *testing.T) {
	var unset atomic.Bool

	script := slicedScript()
	script["grounding"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		// The verify command goes from here on: a report after this slice
		// would carry a gap the first one did not, so it would be published.
		if sliceOf(in) == "SL2" {
			unset.Store(true)
		}

		return done("# Grounding "+sliceOf(in), nil), nil
	}

	c, _, _, rec, ws := newPipeline(t, script)
	c.opts.Pack = codeSlicedPack(t)
	ws.noAgents = true
	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		if unset.Load() {
			return listAlphaUnset(ctx, key)
		}

		return listAlpha(ctx, key)
	}

	driveIn(t, c, autoStart("BP1"))
	require.True(t, rec.has(EventReport), "every step ran")
	require.Equal(t, 1, rec.count(EventSetupGaps), "the first slice's step reports, and no later one")

	rec.mu.Lock()
	events := slices.Clone(rec.events)
	rec.mu.Unlock()

	at := func(kind, stage string) int {
		return slices.IndexFunc(events, func(e Event) bool {
			return e.Kind == kind && (stage == "" || e.Payload["stage"] == stage)
		})
	}

	assert.Less(t, at(EventStageEnded, "grounding@SL1"), at(EventSetupGaps, ""), "reported when SL1's step ends")
	assert.Less(t, at(EventSetupGaps, ""), at(EventStageStarted, "solution_design@SL2"), "before the next slice runs")
}

func TestSlices_AStepTheSlicesDoNotHoldIsAnUnreadablePosition(t *testing.T) {
	// The record's last step is a third slice's design, while slicing's
	// newest version holds two: the key names a pack stage, but no step of
	// the blueprint's pipeline.
	st := slicedStore(t)
	c, runner, rec := newSlicedOn(t, st, slicedScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))

	_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: needStage, Body: "the need"})
	require.NoError(t, err)

	for _, s := range []string{"intake", "slicing", "solution_design@SL3"} {
		ran(t, st, s, store.RunKindStage, 1, 1, 0.1)
		v := put(t, st, s, stageData(s))

		if s == "slicing" {
			gateRow(t, st, s, v, store.GateApprove, store.ResponderHuman, "")
		}
	}

	require.NoError(t, c.Start(ctx, startInput("BP1")), "the reopen attaches the session")
	c.Wait("BP1")

	ev, ok := rec.first(EventError)
	require.True(t, ok, "the user is told why nothing runs")
	assert.Contains(t, ev.Payload["message"], "Solution design (SL3)")
	assert.Equal(t, 0, runner.calls(), "neither the step nor its reviewer runs")

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m1"))
	c.Wait("BP1")

	assert.Equal(t, 1, rec.count(EventNote), "the message is kept")
	assert.Equal(t, 0, runner.calls())
}

func TestRunFloor_ASliceIsFlooredByEverySliceOfItsStage(t *testing.T) {
	c, st, _, _ := newSliced(t, slicedScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	ranBefore(t, st, store.RunKindStage, "solution_design@SL1", store.RunDone, 0.6)
	ranBefore(t, st, store.RunKindStage, "solution_design@SL2", store.RunDone, 0.8)
	ranBefore(t, st, store.RunKindStage, "grounding@SL1", store.RunDone, 5)

	got, err := c.runFloor(ctx, "BP1", store.RunKindStage, "solution_design@SL3")
	require.NoError(t, err)
	assert.InDelta(t, 0.7, got, 1e-9, "a new slice's first run is floored by the earlier slices' runs of its stage")
}

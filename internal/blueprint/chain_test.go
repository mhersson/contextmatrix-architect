package blueprint

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestInChain(t *testing.T) {
	done := func(keys ...string) []store.Run {
		out := make([]store.Run, 0, len(keys))
		for _, k := range keys {
			out = append(out, store.Run{Stage: k, Kind: store.RunKindStage, Status: store.RunDone})
		}

		return out
	}

	cut := append(done("readiness", "decomposition@SL1"),
		store.Run{Stage: "decomposition@SL2", Kind: store.RunKindStage, Status: store.RunInterrupted},
		store.Run{Stage: "decomposition@SL1", Kind: store.RunKindReviewer, Status: store.RunDone},
		store.Run{Stage: "decomposition@SL2", Kind: store.RunKindStage, Status: store.RunDone})

	tests := []struct {
		name string
		runs []store.Run
		key  string
		want bool
	}{
		{name: "a first pass follows its slice's acceptance", runs: done("acceptance@SL2", "decomposition@SL2"), key: "decomposition@SL2"},
		{name: "the loop's first target", runs: done("decomposition@SL2", "readiness", "decomposition@SL1"), key: "decomposition@SL1", want: true},
		{name: "a later slice in the chain", runs: done("readiness", "decomposition@SL1", "decomposition@SL2"), key: "decomposition@SL2", want: true},
		{name: "a pass a rejection started at a design", runs: done("readiness", "solution_design@SL2", "acceptance@SL2", "decomposition@SL2"), key: "decomposition@SL2"},
		{name: "runs that did not complete, and other kinds, place nothing", runs: cut, key: "decomposition@SL2", want: true},
		{name: "no run of the key", runs: done("readiness"), key: "decomposition@SL1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, inChain(tt.runs, tt.key, "readiness"))
		})
	}
}

func TestOwningTarget(t *testing.T) {
	targets := []pipelineStep{{ID: "decomposition@SL1"}, {ID: "decomposition@SL2"}, {ID: "decomposition@SL3"}}
	drafts := map[string][]string{"decomposition@SL1": {"D1"}, "decomposition@SL2": {"D2", "D3"}, "decomposition@SL3": {"D4"}}

	assert.Equal(t, "decomposition@SL2", owningTarget(targets, drafts, []string{"D3", "D4"}), "the earliest slice holding a failing card")
	assert.Equal(t, "decomposition@SL1", owningTarget(targets, drafts, nil), "no failing card: the first slice")
	assert.Equal(t, "decomposition@SL1", owningTarget(targets, drafts, []string{"D9"}), "a card no slice holds: the first slice")
}

func TestFailingCards(t *testing.T) {
	art := store.Artifact{Data: []byte(`{"cards":[{"id":"D1","ready":true,"findings":[]},{"id":"D2","ready":true,"findings":["too big"]},{"id":"D3","ready":false}]}`)}

	_, failing := cardVerdicts(art, []string{"D1", "D2", "D3", "D4"})
	assert.Equal(t, []string{"D2", "D3", "D4"}, failing, "a finding, not ready, or not judged")

	_, failing = cardVerdicts(store.Artifact{Data: []byte(`{"cards":[]}`)}, []string{"D1"})
	assert.Empty(t, failing, "a verdict that judged no card fails none")
}

func TestReadiness_JudgesEverySlicesCards(t *testing.T) {
	c, _, runner, rec := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))

	ready := inputsOf(runner, "readiness")
	require.Len(t, ready, 1)
	assert.Contains(t, ready[0].Task, "# Cards SL1")
	assert.Contains(t, ready[0].Task, "# Cards SL2")
	assert.True(t, rec.has(EventSignOff))
}

func TestReadiness_LoopsToTheOwningSliceThenEveryLaterOne(t *testing.T) {
	var rounds atomic.Int32

	script := slicedScript()
	script["slicing"] = returns("# Slices", threeSlices())
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		if rounds.Add(1) == 1 {
			v := readinessFor(true, "D1", "D3")
			v["cards"] = append(v["cards"].([]any), map[string]any{"id": "D2", "ready": false, "findings": []any{"two deliverables"}})

			return done("# Not ready", v), nil
		}

		return done("# Ready", readinessFor(true, "D1", "D2", "D3")), nil
	}

	c, _, runner, _ := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, []string{
		"decomposition@SL1", "decomposition@SL2", "decomposition@SL3", "readiness",
		"decomposition@SL2", "decomposition@SL3", "readiness",
	}, stepRuns(runner, "decomposition", "readiness"))
	assert.Len(t, inputsOf(runner, "solution_design@SL3"), 1, "the chain runs no later slice's design again")
}

func TestReadiness_AVerdictNamingNoCardLoopsFromTheFirstSlice(t *testing.T) {
	var rounds atomic.Int32

	script := slicedScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		if rounds.Add(1) == 1 {
			return done("# Nothing judged", map[string]any{"cards": []any{}}), nil
		}

		return done("# Ready", readinessFor(true, "D1", "D2")), nil
	}

	c, _, runner, _ := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, []string{"decomposition@SL1", "decomposition@SL2", "readiness", "decomposition@SL1", "decomposition@SL2", "readiness"},
		stepRuns(runner, "decomposition", "readiness"))
}

func TestReadiness_TheRoundCapCountsPerBlueprint(t *testing.T) {
	script := slicedScript()
	script["readiness"] = returns("# Not ready", readinessFor(false, "D1", "D2"))

	c, _, runner, rec := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.Len(t, inputsOf(runner, "readiness"), 3)

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, false, report.Payload["ready"])
	assert.Equal(t, 3, report.Payload["rounds"])
}

func TestSignoff_ARejectRerunsEverySlicesCards(t *testing.T) {
	c, _, runner, rec := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))
	require.True(t, rec.has(EventSignOff))

	require.NoError(t, c.Message(context.Background(), "BP1", "reject split the export card", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{"decomposition@SL1", "decomposition@SL2", "readiness", "decomposition@SL1", "decomposition@SL2", "readiness"},
		stepRuns(runner, "decomposition", "readiness"))

	cards := inputsOf(runner, "decomposition@SL2")
	require.Len(t, cards, 2)
	assert.Contains(t, cards[1].Task, "split the export card", "every slice's cards get the sign-off's feedback")
	assert.Len(t, inputsOf(runner, "solution_design@SL2"), 1)
	assert.Equal(t, 2, rec.count(EventSignOff), "the sign-off is shown again")
}

func TestSignoff_AFailedFirstRunOfARejectResumesUnderItsKey(t *testing.T) {
	var rejected, failed atomic.Bool

	script := slicedScript()
	cards := script["decomposition"]
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		// The reject's first run, SL1's, fails once.
		if rejected.Load() && sliceOf(in) == "SL1" && !failed.Swap(true) {
			return stage.Output{}, errors.New("model unavailable")
		}

		return cards(ctx, in)
	}

	c, st, runner, rec := newSliced(t, script)
	ctx := context.Background()

	driveIn(t, c, autoStart("BP1"))
	require.True(t, rec.has(EventSignOff))

	rejected.Store(true)
	require.NoError(t, c.Message(ctx, "BP1", "reject split the export card", "m2"))
	c.Wait("BP1")
	require.Len(t, inputsOf(runner, "decomposition@SL1"), 2, "the reject's first run failed")

	// The verdict the sign-off rejected is still the latest done stage run:
	// the resume reads the re-run from it.
	require.NoError(t, c.Resume(ctx, "BP1", "m-continue"))
	c.Wait("BP1")

	assert.Equal(t, []string{
		"decomposition@SL1", "decomposition@SL2", "readiness",
		"decomposition@SL1", "decomposition@SL1", "decomposition@SL2", "readiness",
	}, stepRuns(runner, "decomposition", "readiness"))
	assert.Equal(t, 2, rec.count(EventSignOff))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	for _, r := range runs {
		assert.NotEqual(t, "decomposition", r.Stage, "no run of any kind is recorded under the plain id")
	}
}

func TestReadiness_ACrashInTheChainResumesTheChain(t *testing.T) {
	var rounds atomic.Int32

	readiness := func(context.Context, stage.Input) (stage.Output, error) {
		if rounds.Add(1) == 1 {
			return done("# Not ready", readinessFor(false, "D1", "D2", "D3")), nil
		}

		return done("# Ready", readinessFor(true, "D1", "D2", "D3")), nil
	}

	script := slicedScript()
	script["slicing"] = returns("# Slices", threeSlices())
	script["readiness"] = readiness
	cards := script["decomposition"]
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		// The chain's run of SL2 is cut short; the first pass's is not.
		if sliceOf(in) == "SL2" && rounds.Load() == 1 {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return cards(ctx, in)
	}

	st := slicedStore(t)
	c, runner, _ := newSlicedOn(t, st, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return len(inputsOf(runner, "decomposition@SL2")) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	again := slicedScript()
	again["slicing"] = returns("# Slices", threeSlices())
	again["readiness"] = readiness

	c2, runner2, rec2 := newSlicedOn(t, st, again)
	require.NoError(t, c2.Start(ctx, autoStart("BP1")))
	c2.Wait("BP1")

	assert.Equal(t, []string{"decomposition@SL2", "decomposition@SL3", "readiness"}, stepRuns(runner2, "solution_design", "decomposition", "readiness"),
		"the cut run goes on through the later slices' cards, and no design runs")
	assert.True(t, rec2.has(EventSignOff))
}

func TestReadiness_AnEndInTheChainResumesTheChainOnTheReopen(t *testing.T) {
	var (
		rounds atomic.Int32
		cut    atomic.Bool
	)

	script := slicedScript()
	script["slicing"] = returns("# Slices", threeSlices())
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		if rounds.Add(1) == 1 {
			return done("# Not ready", readinessFor(false, "D1", "D2", "D3")), nil
		}

		return done("# Ready", readinessFor(true, "D1", "D2", "D3")), nil
	}
	cards := script["decomposition"]
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		// The End cuts the chain's first run of SL2 short.
		if sliceOf(in) == "SL2" && rounds.Load() == 1 && !cut.Swap(true) {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return cards(ctx, in)
	}

	c, _, runner, rec := newSliced(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return len(inputsOf(runner, "decomposition@SL2")) == 2 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.End(ctx, "BP1"))
	c.Wait("BP1")

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	c.Wait("BP1")

	assert.Equal(t, []string{
		"decomposition@SL1", "decomposition@SL2", "decomposition@SL3", "readiness",
		"decomposition@SL1", "decomposition@SL2", "decomposition@SL2", "decomposition@SL3", "readiness",
	}, stepRuns(runner, "decomposition", "readiness"), "the cut step runs again under its key, then the later slices' cards")
	assert.Len(t, inputsOf(runner, "solution_design@SL2"), 1, "no design runs again")
	assert.True(t, rec.has(EventSignOff))
}

func TestReadiness_TheLoopAndTheRejectNeverRunThePlainStage(t *testing.T) {
	var rounds atomic.Int32

	script := slicedScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		if rounds.Add(1) == 1 {
			return done("# Not ready", readinessFor(false, "D1", "D2")), nil
		}

		return done("# Ready", readinessFor(true, "D1", "D2")), nil
	}

	c, st, runner, rec := newSliced(t, script)
	ctx := context.Background()

	driveIn(t, c, autoStart("BP1"))
	require.True(t, rec.has(EventSignOff))

	require.NoError(t, c.Message(ctx, "BP1", "reject split the export card", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{
		"decomposition@SL1", "decomposition@SL2", "readiness",
		"decomposition@SL1", "decomposition@SL2", "readiness",
		"decomposition@SL1", "decomposition@SL2", "readiness",
	}, stepRuns(runner, "decomposition", "readiness"))
	assert.Equal(t, 2, rec.count(EventSignOff))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	for _, r := range runs {
		assert.NotEqual(t, "decomposition", r.Stage, "no run of any kind is recorded under the plain id")
	}

	assert.False(t, unsliced(c.opts.Pack, runs), "the record still reads as one that runs per slice")
	assert.True(t, c.sliced(ctx, "BP1"))
}

func TestReadiness_ARecordFromBeforeSlicesLoopsToThePlainStage(t *testing.T) {
	var rounds atomic.Int32

	script := slicedScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		if rounds.Add(1) == 1 {
			return done("# Not ready", readinessFor(false, "D1")), nil
		}

		return done("# Ready", readinessFor(true, "D1")), nil
	}

	// The design covered every slice at once, under its plain id, and waits
	// at its gate when the pack starts repeating it per slice.
	st := slicedStore(t)
	c, runner, rec := newSlicedOn(t, st, script)
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
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")
	require.True(t, rec.has(EventSignOff))

	require.NoError(t, c.Message(ctx, "BP1", "reject split the export card", "m3"))
	c.Wait("BP1")

	assert.Equal(t, []string{"decomposition", "readiness", "decomposition", "readiness", "decomposition", "readiness"},
		stepRuns(runner, "decomposition", "readiness"), "the loop and the sign-off re-run the one plain step")
	assert.Equal(t, 2, rec.count(EventSignOff))
}

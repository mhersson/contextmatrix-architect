package blueprint

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		text string
		name string
		args []string
	}{
		{text: "abort", name: CommandAbort},
		{text: "  Export ", name: CommandExport},
		{text: "gate intake auto", name: CommandGate, args: []string{"intake", "auto"}},
		{text: "Gate Intake HUMAN", name: CommandGate, args: []string{"intake", "human"}},
		{text: "cap 4", name: CommandCap, args: []string{"4"}},
		{text: "Cap 4.50", name: CommandCap, args: []string{"4.50"}},
		{text: "CAP OFF", name: CommandCap, args: []string{"off"}},
		{text: "cap -1", name: CommandCap, args: []string{"-1"}},
		{text: "cap"},
		{text: "cap four"},
		{text: "cap 4 now"},
		{text: "cap 1e3"},
		{text: "mode auto", name: CommandMode, args: []string{"auto"}},
		{text: "Mode AUTO+EMIT", name: CommandMode, args: []string{"auto+emit"}},
		{text: " mode yolo ", name: CommandMode, args: []string{"yolo"}},
		{text: "MODE Default", name: CommandMode, args: []string{"default"}},
		{text: "mode"},
		{text: "mode fast"},
		{text: "mode auto now"},
		{text: "Mode of transport is rail"},
		{text: "resume", name: CommandResume},
		{text: " Continue ", name: CommandResume},
		{text: "RESUME", name: CommandResume},
		{text: "continue with the plan"},
		{text: "resume the old plan"},
		{text: "gate intake"},
		{text: "gate intake sometimes"},
		{text: "Gate it behind a feature flag"},
		{text: "Abort the old plan"},
		{text: "export the data as csv"},
		{text: "the design"},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			name, args := ParseCommand(tt.text)
			assert.Equal(t, tt.name, name)
			assert.Equal(t, tt.args, args)
		})
	}
}

func TestPolicy_FirstMatchWins(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		plain   bool
		stored  string      // the mode stored after the start, as a switch leaves it; "" none
		replies [][2]string // gate replies in order: stage, policy
		want    string
	}{
		{name: "the pack", mode: protocol.ArchitectModeDefault, want: pack.PolicyHuman},
		{name: "the auto mode", mode: protocol.ArchitectModeAuto, want: pack.PolicyAuto},
		{name: "yolo", mode: protocol.ArchitectModeYolo, want: pack.PolicyAuto},
		{name: "a reply beats the pack", mode: protocol.ArchitectModeDefault, replies: [][2]string{{"intake", "auto"}}, want: pack.PolicyAuto},
		{name: "a reply beats the mode", mode: protocol.ArchitectModeAuto, replies: [][2]string{{"intake", "human"}}, want: pack.PolicyHuman},
		{name: "the latest reply wins", mode: protocol.ArchitectModeDefault, replies: [][2]string{{"intake", "auto"}, {"intake", "human"}}, want: pack.PolicyHuman},
		{name: "a stored switch beats the mode a chain started with", mode: protocol.ArchitectModeDefault, stored: protocol.ArchitectModeAuto, want: pack.PolicyAuto},
		{name: "a stored switch back to default", mode: protocol.ArchitectModeYolo, stored: protocol.ArchitectModeDefault, want: pack.PolicyHuman},
		{name: "a reply beats a stored switch", mode: protocol.ArchitectModeDefault, stored: protocol.ArchitectModeAuto, replies: [][2]string{{"intake", "human"}}, want: pack.PolicyHuman},
		{name: "plain keeps the questions' pack policy", mode: protocol.ArchitectModeDefault, plain: true, want: pack.PolicyHuman},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _ := newController(t, artifactRun("# Need"))
			ctx := context.Background()

			in := startInput("BP1")
			in.Mode = tt.mode
			in.PlainLanguage = tt.plain
			require.NoError(t, c.Start(ctx, in))

			// Read at the start, as a chain of runs holds it.
			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			if tt.stored != "" {
				require.NoError(t, st.SetBlueprintMode(ctx, "BP1", tt.stored))
			}

			for i, r := range tt.replies {
				require.NoError(t, c.SetPolicy(ctx, "BP1", r[0], r[1], fmt.Sprintf("m%d", i)))
			}

			got, err := c.policy(ctx, bp, "intake")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSetPolicy_RefusesTheSignoffAndUnknownStages(t *testing.T) {
	c, _, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, c.SetPolicy(ctx, "BP1", "signoff", "auto", "m1"))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "nosuch", "auto", "m2"))

	events := rec.all(EventPolicy)
	require.Len(t, events, 2)

	for _, e := range events {
		refused, _ := e.Payload["refused"].(string)
		assert.NotEmpty(t, refused)
		assert.Equal(t, false, e.Payload["during_run"])
	}

	unknown, _ := events[1].Payload["refused"].(string)
	assert.Contains(t, unknown, "intake", "an unknown stage's refusal names the stages a reply may name")

	require.ErrorIs(t, c.SetPolicy(ctx, "BP2", "intake", "auto", "m3"), ErrNotFound, "a session that is not attached")
}

func TestSetPolicy_ARefusedReplyNeverBecomesAPolicy(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, c.SetPolicy(ctx, "BP1", "nosuch", "human", "m1"))

	events := rec.all(EventPolicy)
	require.Len(t, events, 1)

	unknown, _ := events[0].Payload["refused"].(string)
	assert.NotEmpty(t, unknown)

	for _, id := range c.stageIDs() {
		assert.Contains(t, unknown, id, "a refusal names the stages a reply may name")
	}

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	got, err := c.policy(ctx, bp, "nosuch")
	require.NoError(t, err)
	assert.Equal(t, pack.PolicyAuto, got, "nothing was stored for it")
}

func TestAsk_ACancelledRunRecordsNoRound(t *testing.T) {
	c, _, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	require.NoError(t, c.Start(ctx, in))

	bp, err := c.opts.Store.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	iv := &interviewer{c: c, bp: bp, st: intake, r: &run{}}
	_, err = iv.Ask(cancelled, stage.Round{Questions: []stage.RoundQuestion{roundQ("Port", "Which port?")}})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, rec.has(EventRound))
}

func TestSetPolicy_AnOpenRoundKeepsItsResponder(t *testing.T) {
	c, st, runner, rec := newController(t, asks("# Need", roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.NoError(t, c.SetPolicy(ctx, "BP1", "intake", "auto", "m2"))

	change, ok := rec.first(EventPolicy)
	require.True(t, ok)
	assert.Equal(t, "round", change.Payload["open"])
	assert.Equal(t, false, change.Payload["during_run"])

	answerRound(t, c, "BP1", 1, true, pick(1, 1))
	c.Wait("BP1")

	require.Equal(t, 2, runner.calls(), "the round closes and its stage goes on")
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 1)
	assert.Equal(t, store.ResponderHuman, qs[0].Responder, "the human the round was put to answered it")
}

func TestGatePlan_NamesWhereAGateFolds(t *testing.T) {
	c, _, _, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	rec.waitFor(t, EventGatePlan)

	plan, ok := rec.first(EventGatePlan)
	require.True(t, ok)

	stages, ok := plan.Payload["stages"].([]any)
	require.True(t, ok)

	byStage := map[string]map[string]any{}

	for _, s := range stages {
		m, ok := s.(map[string]any)
		require.True(t, ok)

		byStage[m["stage"].(string)] = m
	}

	slicing := byStage["slicing"]
	require.NotNil(t, slicing)

	intake := byStage["intake"]
	require.NotNil(t, intake)
	assert.NotContains(t, intake, "folds_into")
	assert.NotContains(t, intake, "fold_field")
	assert.NotContains(t, intake, "fold_max")

	assert.Equal(t, "solution_design", slicing["folds_into"])
	assert.Equal(t, "slices", slicing["fold_field"])
	assert.EqualValues(t, 1, slicing["fold_max"])
}

// cappedInput is BP1's start, priced, under a blueprint cap of capUSD.
func cappedInput(capUSD float64) StartInput {
	in := startInput("BP1")
	in.CostCapUSD = capUSD
	in.Price = testPrice()

	return in
}

// spending is fn with its run's cost set to usd.
func spending(usd float64, fn func(context.Context, stage.Input) (stage.Output, error)) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		out, err := fn(ctx, in)
		out.Result.TotalCostUSD = usd

		return out, err
	}
}

func storedCap(t *testing.T, st *store.Store) float64 {
	t.Helper()

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)

	return bp.CostCapUSD
}

// onlyCap is the one cap command recorded.
func onlyCap(t *testing.T, rec *recorder) Event {
	t.Helper()

	events := rec.all(EventCap)
	require.Len(t, events, 1)

	return events[0]
}

// shapeRefused leaves BP1 on the gated pack idle on a cap stop of shape:
// intake spent 0.95 of a 1 USD cap, less left than shape's floor.
func shapeRefused(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	script["intake"] = spending(0.95, script["intake"])

	c, st, runner, rec := newGated(t, script)
	driveIn(t, c, cappedInput(1))
	require.Equal(t, []string{"shape"}, capStops(t, st))

	return c, st, runner, rec
}

func TestSetCap_AValueNotAboveWhatIsSpentChangesNothing(t *testing.T) {
	for _, value := range []string{"1.5", "1.2", "0", "-1"} {
		t.Run(value, func(t *testing.T) {
			c, st, runner, rec := newController(t, artifactRun("# Need"))
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, cappedInput(2)))
			ranBefore(t, st, store.RunKindResponder, "intake", store.RunDone, 1.5)

			require.NoError(t, c.SetCap(ctx, "BP1", value, "m1"))
			c.Wait("BP1")

			assert.InDelta(t, 2.0, storedCap(t, st), 1e-9, "the cap stays")
			assert.Zero(t, runner.calls())

			ev := onlyCap(t, rec)
			assert.NotEmpty(t, ev.Payload["refused"])
			assert.InDelta(t, 2.0, ev.Payload["from"], 1e-9, "from is the cap in effect")

			asked, err := strconv.ParseFloat(value, 64)
			require.NoError(t, err)
			assert.InDelta(t, asked, ev.Payload["to"], 1e-9, "to is the value asked for")
			assert.Equal(t, "m1", ev.Payload["message_id"])
			assert.NotContains(t, ev.Payload, "resumes")

			v, err := c.View(ctx, "BP1")
			require.NoError(t, err)
			assert.InDelta(t, 2.0, v.Cost.CapUSD, 1e-9)
		})
	}
}

func TestSetCap_StoresTheCapAndTheViewShowsIt(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		from, to float64
	}{
		{name: "a raise", value: "4", from: 2, to: 4},
		{name: "a lowering above what is spent", value: "1.75", from: 2, to: 1.75},
		{name: "off removes the cap", value: "off", from: 2, to: 0},
		{name: "a cap where there was none", value: "3", from: 0, to: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := newController(t, artifactRun("# Need"))
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, cappedInput(tt.from)))
			ranBefore(t, st, store.RunKindResponder, "intake", store.RunDone, 0.5)

			require.NoError(t, c.SetCap(ctx, "BP1", tt.value, "m1"))
			c.Wait("BP1")

			assert.InDelta(t, tt.to, storedCap(t, st), 1e-9)
			assert.Zero(t, runner.calls(), "nothing was stopped, so nothing starts")

			ev := onlyCap(t, rec)
			assert.NotContains(t, ev.Payload, "refused")
			assert.InDelta(t, tt.from, ev.Payload["from"], 1e-9)
			assert.InDelta(t, tt.to, ev.Payload["to"], 1e-9)
			assert.Equal(t, "m1", ev.Payload["message_id"])
			assert.Equal(t, false, ev.Payload["during_run"])

			v, err := c.View(ctx, "BP1")
			require.NoError(t, err)
			assert.InDelta(t, tt.to, v.Cost.CapUSD, 1e-9)
		})
	}
}

func TestSetCap_ASessionThatIsNotAttachedIsNotFound(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, cappedInput(2)))
	require.NoError(t, c.End(ctx, "BP1"))

	require.ErrorIs(t, c.SetCap(ctx, "BP1", "4", "m1"), ErrNotFound)
	assert.InDelta(t, 2.0, storedCap(t, st), 1e-9)
	assert.False(t, rec.has(EventCap))
}

func TestStart_AReopenKeepsTheStoredCap(t *testing.T) {
	c, st, _, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, cappedInput(2)))
	require.NoError(t, c.SetCap(ctx, "BP1", "5", "m1"))
	require.NoError(t, c.End(ctx, "BP1"))

	// CM's row may still carry the cap the blueprint started with.
	require.NoError(t, c.Start(ctx, cappedInput(3)))
	c.Wait("BP1")

	assert.InDelta(t, 5.0, storedCap(t, st), 1e-9, "only the cap command changes the cap")
}

// cutShortAfterARaise leaves BP1 on the single pack idle on a cap stop of
// intake: its run started under a 1.5 USD cap, the cap went up to 4 while it
// ran, and what the old cap left cut it short.
func cutShortAfterARaise(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	started, held := make(chan struct{}), make(chan struct{})

	var n atomic.Int32

	c, st, runner, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		if n.Add(1) > 1 {
			return artifactOutput("# Need", in), nil
		}

		close(started)
		<-held

		return stage.Output{Result: harness.Result{Reason: "max_cost", TotalCostUSD: 1.6}}, errors.New("stage intake: no artifact")
	})
	release := closer(t, held)

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, cappedInput(1.5)))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	signalled(t, started)
	require.NoError(t, c.SetCap(ctx, "BP1", "4", "m-during"))
	release()
	c.Wait("BP1")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	require.InDelta(t, 1.5, ev.Payload["cap_usd"], 1e-9, "the stop names the cap its run started under")

	return c, st, runner, rec
}

func TestSetCap_ACapAboveTheStopsOwnRunsTheStoppedStageOnce(t *testing.T) {
	question := roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")

	tests := []struct {
		name string
		// setup leaves BP1 idle on a cap stop of stage.
		setup func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder)
		stage string
		// runs is the runner stage the resumed run is.
		runs string
		// value is the cap sent; empty sends 4.
		value string
		check func(t *testing.T, st *store.Store, runner *fakeRunner)
	}{
		{
			name: "a stage the cap refused", stage: "shape", runs: "shape",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				return shapeRefused(t, gatedScript())
			},
			check: func(t *testing.T, _ *store.Store, runner *fakeRunner) {
				t.Helper()
				assert.Len(t, inputsOf(runner, "shape.review"), 1, "the chain goes on to its gate")
			},
		},
		{
			name: "a stage the cap refused, the cap removed", stage: "shape", runs: "shape", value: capOff,
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				return shapeRefused(t, gatedScript())
			},
		},
		{
			name: "a run the old cap cut short after a raise, the stored cap sent again", stage: "intake", runs: "intake", value: "4",
			setup: cutShortAfterARaise,
		},
		{
			name: "a run the old cap cut short after a raise, a lower cap above the stop's", stage: "intake", runs: "intake", value: "3",
			setup: cutShortAfterARaise,
		},
		{
			name: "a stage the cap refused, a stale reply since", stage: "shape", runs: "shape",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				c, st, runner, rec := shapeRefused(t, gatedScript())
				require.NoError(t, c.Reply(context.Background(), "BP1", protocol.RoundReply{PromptID: roundPromptID(9)}, "m-stale"))

				return c, st, runner, rec
			},
		},
		{
			name: "a review the cap refused", stage: "shape", runs: "shape.review",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				script := gatedScript()
				script["shape"] = spending(0.95, script["shape"])

				c, st, runner, rec := newGated(t, script)
				driveIn(t, c, cappedInput(1))

				return c, st, runner, rec
			},
			check: func(t *testing.T, _ *store.Store, runner *fakeRunner) {
				t.Helper()
				assert.Equal(t, []string{"intake", "shape"}, stagesRun(runner), "the review runs, not the stage it judges")
			},
		},
		{
			name: "an intake the cap cut short, its need stored", stage: "intake", runs: "intake",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				var n atomic.Int32

				c, st, runner, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
					if n.Add(1) == 1 {
						return stage.Output{Result: harness.Result{Reason: "max_cost", TotalCostUSD: 1.1}}, errors.New("stage intake: no artifact")
					}

					return artifactOutput("# Need", in), nil
				})
				driveIn(t, c, cappedInput(1))

				return c, st, runner, rec
			},
			check: func(t *testing.T, st *store.Store, _ *fakeRunner) {
				t.Helper()

				arts, err := st.ListArtifacts(context.Background(), "BP1")
				require.NoError(t, err)

				var needs int

				for _, a := range arts {
					if a.Stage == needStage {
						needs++
					}
				}

				assert.Equal(t, 1, needs, "the stored need runs again; no new version")
			},
		},
		{
			name: "a round's continuation the cap refused", stage: "intake", runs: "intake",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				ask := asks("# Need", question)

				c, st, runner, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
					out, err := ask(ctx, in)
					if out.Asked {
						out.Result.TotalCostUSD = 0.95
					}

					return out, err
				})
				driveIn(t, c, cappedInput(1))
				answerRound(t, c, "BP1", 1, true, pick(1, 1))
				c.Wait("BP1")

				return c, st, runner, rec
			},
			check: func(t *testing.T, st *store.Store, _ *fakeRunner) {
				t.Helper()

				_, err := st.LatestArtifact(context.Background(), "BP1", "intake")
				require.NoError(t, err, "the continuation goes on from the round and submits")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := tt.setup(t)
			ctx := context.Background()

			require.Equal(t, []string{tt.stage}, capStops(t, st), "the setup ends on a cap stop")
			require.Equal(t, string(StateIdle), stateOf(t, st, "BP1"))

			before := len(inputsOf(runner, tt.runs))
			changes := len(rec.all(EventCap))

			value := tt.value
			if value == "" {
				value = "4"
			}

			require.NoError(t, c.SetCap(ctx, "BP1", value, "m-raise"))
			c.Wait("BP1")
			require.NoError(t, c.SetCap(ctx, "BP1", "5", "m-again"))
			c.Wait("BP1")

			assert.Len(t, inputsOf(runner, tt.runs), before+1, "the stopped run runs once")
			assert.Equal(t, []string{tt.stage}, capStops(t, st), "under the new cap it runs")

			events := rec.all(EventCap)[changes:]
			require.Len(t, events, 2)
			assert.Equal(t, tt.stage, events[0].Payload["resumes"])
			assert.NotContains(t, events[1].Payload, "resumes", "nothing waits once it ran")
			assert.False(t, rec.has(EventNote), "a cap command stores no note")

			if tt.check != nil {
				tt.check(t, st, runner)
			}
		})
	}
}

func TestSetCap_StartsNothingWithoutACapStop(t *testing.T) {
	tests := []struct {
		name  string
		value string
		// setup leaves BP1 idle with no run in flight.
		setup func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder)
	}{
		{
			name: "a stage that failed", value: "4",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				script := gatedScript()
				script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
					return stage.Output{}, errors.New("model unavailable")
				}

				c, st, runner, rec := newGated(t, script)
				driveIn(t, c, cappedInput(1))

				return c, st, runner, rec
			},
		},
		{
			name: "a cap stop the stage ran past", value: "5",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				script := gatedScript()
				script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
					return stage.Output{}, errors.New("model unavailable")
				}

				c, st, runner, rec := shapeRefused(t, script)

				// The cap goes up without the command, and a message retries
				// shape, which fails on its own.
				require.NoError(t, st.SetBlueprintCostCap(context.Background(), "BP1", 4))
				require.NoError(t, c.Message(context.Background(), "BP1", "go on", "m2"))
				c.Wait("BP1")
				require.Len(t, inputsOf(runner, "shape"), 1)

				return c, st, runner, rec
			},
		},
		{
			name: "a first need the cap refused", value: "4",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				c, st, runner, rec := newController(t, artifactRun("# Need"))
				require.NoError(t, c.Start(context.Background(), cappedInput(1)))
				ranBefore(t, st, store.RunKindStage, "intake", store.RunDone, 1)
				require.NoError(t, c.Message(context.Background(), "BP1", "the need", "m1"))
				c.Wait("BP1")
				require.Equal(t, []string{"intake"}, capStops(t, st))

				return c, st, runner, rec
			},
		},
		{
			name: "an open gate", value: "4",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				c, st, runner, rec := newGated(t, gatedScript())
				driveIn(t, c, cappedInput(1))
				require.True(t, rec.has(EventGate))

				return c, st, runner, rec
			},
		},
		{
			name: "the cap the stop happened under", value: "1",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				return shapeRefused(t, gatedScript())
			},
		},
		{
			name: "a lowering at a cap stop", value: "0.99",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				return shapeRefused(t, gatedScript())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := tt.setup(t)
			ctx := context.Background()
			before := runner.calls()

			require.NoError(t, c.SetCap(ctx, "BP1", tt.value, "m-cap"))
			c.Wait("BP1")

			assert.Equal(t, before, runner.calls(), "no run starts")

			want, err := strconv.ParseFloat(tt.value, 64)
			require.NoError(t, err)
			assert.InDelta(t, want, storedCap(t, st), 1e-9, "the cap is stored all the same")

			ev := onlyCap(t, rec)
			assert.NotContains(t, ev.Payload, "resumes")
			assert.NotContains(t, ev.Payload, "refused")
		})
	}
}

func TestSetCap_DuringARunAppliesFromTheNextRun(t *testing.T) {
	started, held := make(chan struct{}), make(chan struct{})

	script := gatedScript()
	script["intake"] = spending(0.95, func(_ context.Context, _ stage.Input) (stage.Output, error) {
		close(started)
		<-held

		return done("# Need", map[string]any{"slices": 1}), nil
	})

	c, st, runner, rec := newGated(t, script)
	release := closer(t, held)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, cappedInput(1)))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	signalled(t, started)

	require.NoError(t, c.SetCap(ctx, "BP1", "4", "m2"))

	ev := onlyCap(t, rec)
	assert.Equal(t, true, ev.Payload["during_run"])
	assert.NotContains(t, ev.Payload, "resumes")
	assert.InDelta(t, 4.0, storedCap(t, st), 1e-9)

	release()
	c.Wait("BP1")

	assert.InDelta(t, 1.0, inputsOf(runner, "intake")[0].MaxCostUSD, 1e-9, "the run in flight keeps the cap it started with")
	assert.Equal(t, []string{"intake", "shape"}, stagesRun(runner), "the next run starts under the new cap; nothing else starts")
	assert.Empty(t, capStops(t, st))
}

func TestSetCap_ARunCutShortAfterAChangeNamesTheCapItStartedUnder(t *testing.T) {
	started, held := make(chan struct{}), make(chan struct{})

	script := gatedScript()
	script["intake"] = spending(0.95, func(_ context.Context, _ stage.Input) (stage.Output, error) {
		close(started)
		<-held

		return done("# Need", map[string]any{"slices": 1}), nil
	})
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		return stage.Output{Result: harness.Result{Reason: "max_cost", TotalCostUSD: 3.1}}, errors.New("stage shape: no artifact")
	}

	c, st, _, rec := newGated(t, script)
	release := closer(t, held)
	// Without a per-run cap what the blueprint has left binds shape.
	c.opts.MaxRunCostUSD = 0

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, cappedInput(1)))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	signalled(t, started)
	require.NoError(t, c.SetCap(ctx, "BP1", "4", "m2"))
	release()
	c.Wait("BP1")

	require.Equal(t, []string{"shape"}, capStops(t, st))

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.InDelta(t, 4.0, ev.Payload["cap_usd"], 1e-9, "shape started under the raised cap")
}

// stopsFirst is fn, except that the first run match selects stops: it
// fails, or, given started, it closes started and runs until it is
// canceled.
func stopsFirst(match func(stage.Input) bool, started chan struct{}, fn func(context.Context, stage.Input) (stage.Output, error)) func(context.Context, stage.Input) (stage.Output, error) {
	var stopped atomic.Bool

	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if !match(in) || !stopped.CompareAndSwap(false, true) {
			return fn(ctx, in)
		}

		if started == nil {
			return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("provider down")
		}

		close(started)
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	}
}

func anyRun(stage.Input) bool { return true }

func continuationRun(in stage.Input) bool { return in.History != nil && !in.Chat }

func chatRun(in stage.Input) bool { return in.Chat }

// nth selects the nth run it is asked about.
func nth(n int32) func(stage.Input) bool {
	var seen atomic.Int32

	return func(stage.Input) bool { return seen.Add(1) == n }
}

// stopAt runs start, which starts the run stopsFirst stops, aborts that run
// once it started when started is set, and waits until nothing runs.
func stopAt(t *testing.T, c *Controller, started chan struct{}, start func()) {
	t.Helper()

	start()

	if started != nil {
		signalled(t, started)
		require.NoError(t, c.Abort(context.Background(), "BP1", "m-abort"))
	}

	c.Wait("BP1")
}

// stateTheNeed starts BP1 and states its need.
func stateTheNeed(t *testing.T, c *Controller) {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
}

// resumesOnce sends continue at BP1's stop of stageID, then again, and checks
// that the stopped run, a run of runs, ran once more and that neither stored
// a note.
func resumesOnce(t *testing.T, c *Controller, runner *fakeRunner, rec *recorder, stageID, runs string) {
	t.Helper()

	ctx := context.Background()
	before := len(inputsOf(runner, runs))
	notes := rec.count(EventNote)

	require.NoError(t, c.Resume(ctx, "BP1", "m-continue"))
	c.Wait("BP1")
	require.NoError(t, c.Resume(ctx, "BP1", "m-again"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, runs), before+1, "the stopped run runs once")
	assert.Equal(t, notes, rec.count(EventNote), "resume stores no note")

	events := rec.all(EventResume)
	require.Len(t, events, 2)
	assert.Equal(t, stageID, events[0].Payload["stage"])
	assert.Equal(t, "m-continue", events[0].Payload["message_id"])
	assert.NotContains(t, events[0].Payload, "nothing")
	assert.Equal(t, true, events[1].Payload["nothing"], "nothing waits once it ran")
}

func TestResume_RunsAFailedOrAbortedStopOnceWithoutANote(t *testing.T) {
	question := roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")

	gatedStop := func(stopped func(stageScript, chan struct{})) func(*testing.T, chan struct{}) (*Controller, *store.Store, *fakeRunner, *recorder) {
		return func(t *testing.T, started chan struct{}) (*Controller, *store.Store, *fakeRunner, *recorder) {
			t.Helper()

			script := gatedScript()
			stopped(script, started)

			c, st, runner, rec := newGated(t, script)
			stopAt(t, c, started, func() { stateTheNeed(t, c) })

			return c, st, runner, rec
		}
	}

	kinds := []struct {
		name string
		// stage is the stopped stage; runs is the runner stage of the run
		// that runs again.
		stage, runs string
		// stop leaves BP1 idle at the stop: the run fails, or, given
		// started, is aborted.
		stop  func(t *testing.T, started chan struct{}) (*Controller, *store.Store, *fakeRunner, *recorder)
		check func(t *testing.T, st *store.Store, runner *fakeRunner)
	}{
		{
			name: "a model stage", stage: "shape", runs: "shape",
			stop: gatedStop(func(s stageScript, started chan struct{}) {
				s["shape"] = stopsFirst(anyRun, started, s["shape"])
			}),
		},
		{
			name: "a review", stage: "shape", runs: "shape.review",
			stop: gatedStop(func(s stageScript, started chan struct{}) {
				s["shape.review"] = stopsFirst(anyRun, started, s["shape.review"])
			}),
			check: func(t *testing.T, _ *store.Store, runner *fakeRunner) {
				t.Helper()
				assert.Len(t, inputsOf(runner, "shape"), 1, "the review runs, not the stage it judges")
			},
		},
		{
			name: "a builder round", stage: "shape", runs: "shape",
			stop: gatedStop(func(s stageScript, started chan struct{}) {
				s["shape.review"] = reviews(2, 5)
				s["shape"] = stopsFirst(nth(2), started, s["shape"])
			}),
		},
		{
			name: "a revision", stage: "shape", runs: "shape",
			stop: gatedStop(func(s stageScript, started chan struct{}) {
				s["shape.review"] = verdicts(scored(4, "name the owner"), fixed(4, "R1"))
				s["shape"] = stopsFirst(nth(2), started, s["shape"])
			}),
			check: func(t *testing.T, _ *store.Store, runner *fakeRunner) {
				t.Helper()
				assert.Len(t, inputsOf(runner, "shape.review"), 2, "the revision is reviewed again")
			},
		},
		{
			name: "a round's continuation", stage: "intake", runs: "intake",
			stop: func(t *testing.T, started chan struct{}) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				c, st, runner, rec := newController(t, stopsFirst(continuationRun, started, asks("# Need", question)))
				stopAt(t, c, started, func() {
					stateTheNeed(t, c)
					c.Wait("BP1")
					answerRound(t, c, "BP1", 1, true, pick(1, 1))
				})

				return c, st, runner, rec
			},
			check: func(t *testing.T, _ *store.Store, runner *fakeRunner) {
				t.Helper()

				runs := inputsOf(runner, "intake")
				assert.NotNil(t, runs[len(runs)-1].History, "it goes on from the round's conversation")
			},
		},
		{
			name: "a round's chat run", stage: "intake", runs: "intake",
			stop: func(t *testing.T, started chan struct{}) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				replied := make(chan struct{})
				close(replied)

				fn := chatting([]stage.RoundQuestion{question}, stage.ChatAnswer{Reply: "Helm keeps it in one chart."}, make(chan struct{}, 4), replied)
				c, st, runner, rec := newController(t, stopsFirst(chatRun, started, fn))
				stopAt(t, c, started, func() {
					stateTheNeed(t, c)
					c.Wait("BP1")
					require.NoError(t, c.Message(context.Background(), "BP1", "Why Helm?", "m2"))
				})

				return c, st, runner, rec
			},
			check: func(t *testing.T, _ *store.Store, runner *fakeRunner) {
				t.Helper()

				runs := inputsOf(runner, "intake")
				assert.True(t, runs[len(runs)-1].Chat, "the chat run runs again")
			},
		},
		{
			name: "an intake before its first artifact, its need stored", stage: "intake", runs: "intake",
			stop: func(t *testing.T, started chan struct{}) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				c, st, runner, rec := newController(t, stopsFirst(anyRun, started, artifactRun("# Need")))
				stopAt(t, c, started, func() { stateTheNeed(t, c) })

				return c, st, runner, rec
			},
			check: func(t *testing.T, st *store.Store, _ *fakeRunner) {
				t.Helper()

				arts, err := st.ListArtifacts(context.Background(), "BP1")
				require.NoError(t, err)

				var needs int

				for _, a := range arts {
					if a.Stage == needStage {
						needs++
					}
				}

				assert.Equal(t, 1, needs, "it runs on the stored need; no new version")
			},
		},
	}

	for _, k := range kinds {
		for _, abort := range []bool{false, true} {
			how := "failed"
			if abort {
				how = "aborted"
			}

			t.Run(k.name+" that "+how, func(t *testing.T) {
				var started chan struct{}
				if abort {
					started = make(chan struct{})
				}

				c, st, runner, rec := k.stop(t, started)
				resumesOnce(t, c, runner, rec, k.stage, k.runs)

				if k.check != nil {
					k.check(t, st, runner)
				}
			})
		}
	}
}

func TestResume_RunsACapStopOnceWithoutANote(t *testing.T) {
	tests := []struct {
		name        string
		stage, runs string
		setup       func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder)
	}{
		{
			name: "a stage the cap refused, the cap since above its floor", stage: "shape", runs: "shape",
			setup: func(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder) {
				t.Helper()

				c, st, runner, rec := shapeRefused(t, gatedScript())
				require.NoError(t, st.SetBlueprintCostCap(context.Background(), "BP1", 4))

				return c, st, runner, rec
			},
		},
		{
			name: "a run the old cap cut short after a raise", stage: "intake", runs: "intake",
			setup: cutShortAfterARaise,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := tt.setup(t)
			require.Equal(t, []string{tt.stage}, capStops(t, st), "the setup ends on a cap stop")

			resumesOnce(t, c, runner, rec, tt.stage, tt.runs)
			assert.Equal(t, []string{tt.stage}, capStops(t, st), "the cap covers the run")
		})
	}
}

func TestResume_ACapStopTheCapStillCannotCoverStopsAgain(t *testing.T) {
	c, st, runner, rec := shapeRefused(t, gatedScript())

	require.NoError(t, c.Resume(context.Background(), "BP1", "m-continue"))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "shape"), "the run meets the cap check again")
	assert.Equal(t, []string{"shape", "shape"}, capStops(t, st), "and is refused again")

	ev, ok := rec.first(EventResume)
	require.True(t, ok)
	assert.Equal(t, "shape", ev.Payload["stage"])
}

func TestResume_StartsNothingWhereNothingIsStopped(t *testing.T) {
	question := roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")

	tests := []struct {
		name string
		// setup leaves BP1 with no run in flight.
		setup func(t *testing.T) (*Controller, *fakeRunner, *recorder)
		check func(t *testing.T, c *Controller)
	}{
		{
			name: "no need yet",
			setup: func(t *testing.T) (*Controller, *fakeRunner, *recorder) {
				t.Helper()

				c, _, runner, rec := newController(t, artifactRun("# Need"))
				require.NoError(t, c.Start(context.Background(), startInput("BP1")))

				return c, runner, rec
			},
		},
		{
			name: "a round that waits for answers",
			setup: func(t *testing.T) (*Controller, *fakeRunner, *recorder) {
				t.Helper()

				c, _, runner, rec := newController(t, asks("# Need", question))
				drive(t, c, "BP1")

				return c, runner, rec
			},
			check: func(t *testing.T, c *Controller) {
				t.Helper()
				assert.Empty(t, latestRound(t, c, "BP1", 1).Frame.Chat, "a command is never chat about the round")
			},
		},
		{
			name: "an open gate",
			setup: func(t *testing.T) (*Controller, *fakeRunner, *recorder) {
				t.Helper()

				c, _, runner, rec := newGated(t, gatedScript())
				drive(t, c, "BP1")
				require.True(t, rec.has(EventGate))

				return c, runner, rec
			},
		},
		{
			name: "the sign-off",
			setup: func(t *testing.T) (*Controller, *fakeRunner, *recorder) {
				t.Helper()

				c, _, runner, rec, _ := newPipeline(t, happyScript())
				drive(t, c, "BP1")
				require.True(t, rec.has(EventSignOff))

				return c, runner, rec
			},
		},
		{
			name: "an ended pipeline",
			setup: func(t *testing.T) (*Controller, *fakeRunner, *recorder) {
				t.Helper()

				c, st, runner, rec := newController(t, artifactRun("# Need"))
				drive(t, c, "BP1")

				_, err := st.LatestArtifact(context.Background(), "BP1", "intake")
				require.NoError(t, err)

				return c, runner, rec
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, runner, rec := tt.setup(t)
			before := runner.calls()

			require.NoError(t, c.Resume(context.Background(), "BP1", "m-continue"))
			c.Wait("BP1")

			assert.Equal(t, before, runner.calls(), "no run starts")
			assert.False(t, rec.has(EventNote), "resume stores no note")

			events := rec.all(EventResume)
			require.Len(t, events, 1)
			assert.Equal(t, true, events[0].Payload["nothing"])
			require.Contains(t, events[0].Payload, "running")
			assert.Empty(t, events[0].Payload["running"], "nothing runs")
			assert.Equal(t, false, events[0].Payload["during_run"])
			assert.Equal(t, "m-continue", events[0].Payload["message_id"])
			assert.NotContains(t, events[0].Payload, "stage")

			if tt.check != nil {
				tt.check(t, c)
			}
		})
	}
}

func TestResume_DuringARunStartsNothing(t *testing.T) {
	started, held := make(chan struct{}), make(chan struct{})

	c, _, runner, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		close(started)
		<-held

		return artifactOutput("# Need", in), nil
	})
	release := closer(t, held)

	stateTheNeed(t, c)
	signalled(t, started)

	require.NoError(t, c.Resume(context.Background(), "BP1", "m-continue"))

	events := rec.all(EventResume)
	require.Len(t, events, 1)
	assert.Equal(t, true, events[0].Payload["nothing"])
	assert.Equal(t, "intake", events[0].Payload["running"])
	assert.Equal(t, true, events[0].Payload["during_run"])

	release()
	c.Wait("BP1")

	assert.Equal(t, 1, runner.calls(), "only the run in flight ran")
	assert.False(t, rec.has(EventNote))
}

func TestResume_ASessionThatIsNotAttachedIsNotFound(t *testing.T) {
	c, _, runner, rec := newController(t, stopsFirst(anyRun, nil, artifactRun("# Need")))
	ctx := context.Background()

	stateTheNeed(t, c)
	c.Wait("BP1")
	require.NoError(t, c.End(ctx, "BP1"))

	require.ErrorIs(t, c.Resume(ctx, "BP1", "m-continue"), ErrNotFound)
	assert.Equal(t, 1, runner.calls())
	assert.False(t, rec.has(EventResume))
}

func TestMessage_StillRetriesAFailedStageWithANote(t *testing.T) {
	script := gatedScript()
	script["shape"] = stopsFirst(anyRun, nil, script["shape"])

	c, _, runner, rec := newGated(t, script)
	drive(t, c, "BP1")
	require.Len(t, inputsOf(runner, "shape"), 1)

	require.NoError(t, c.Message(context.Background(), "BP1", "go on", "m2"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "shape"), 2, "the message runs the failed stage again")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, "shape", note.Payload["retries"])
	assert.False(t, rec.has(EventResume))
}

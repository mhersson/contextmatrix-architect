package blueprint

import (
	"context"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestMode_ASwitchDuringARunAppliesFromTheNextRound(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})

	script := happyScript()
	script["intake.responder"] = respondsWith("Kustomize")
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		signal(entered)
		<-release

		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")}}); err != nil {
			return stage.Output{}, err
		}

		return done("# Need", map[string]any{"slices": 1}), nil
	}

	c, st, runner, _, _ := newPipeline(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, entered, "intake")

	require.NoError(t, st.SetBlueprintMode(ctx, "BP1", protocol.ArchitectModeAuto))
	close(release)
	c.Wait("BP1")

	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Responder, "the round goes to the responder")
	assert.Len(t, inputsOf(runner, "intake.responder"), 1)
}

func TestMode_BackToDefaultTheResponderFinishesAndTheNextRoundIsTheHumans(t *testing.T) {
	responding, release := make(chan struct{}, 2), make(chan struct{})

	script := happyScript()
	script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		signal(responding)
		<-release

		return answering("Kustomize", in), nil
	}
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			return done("# Need", map[string]any{"slices": 1}), nil
		}

		for _, q := range []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize"), roundQ("Region", "Which region?", "EU", "US")} {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{q}}); err != nil {
				return stage.Output{}, err
			}
		}

		return askedOutput(in), nil
	}

	c, st, runner, _, _ := newPipeline(t, script)
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, responding, "the responder")

	require.NoError(t, st.SetBlueprintMode(ctx, "BP1", protocol.ArchitectModeDefault))
	close(release)
	c.Wait("BP1")

	first := latestRound(t, c, "BP1", 1)
	assert.True(t, first.Frame.Responder, "the responder run in flight finishes its round")
	assert.True(t, first.Frame.Closed)

	second := latestRound(t, c, "BP1", 2)
	assert.False(t, second.Frame.Responder, "the next round goes to the human")
	assert.False(t, second.Frame.Closed)
	assert.Len(t, inputsOf(runner, "intake.responder"), 1)
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

func TestMode_TheSignoffAndEmissionFollowTheStoredMode(t *testing.T) {
	cases := []struct {
		name, start, stored string
		emits, runs         bool
	}{
		{name: "default switched to auto+emit approves itself", start: protocol.ArchitectModeDefault, stored: protocol.ArchitectModeAutoEmit, emits: true},
		{name: "auto+emit switched to default waits for a human", start: protocol.ArchitectModeAutoEmit, stored: protocol.ArchitectModeDefault},
		{name: "auto+emit switched to yolo starts the run", start: protocol.ArchitectModeAutoEmit, stored: protocol.ArchitectModeYolo, emits: true, runs: true},
		{name: "yolo switched to auto+emit starts none", start: protocol.ArchitectModeYolo, stored: protocol.ArchitectModeAutoEmit, emits: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entered, release := make(chan struct{}, 1), make(chan struct{})

			script := happyScript()
			script["intake"] = func(context.Context, stage.Input) (stage.Output, error) {
				signal(entered)
				<-release

				return done("# Need", map[string]any{"slices": 1}), nil
			}

			c, st, _, rec, _ := newPipeline(t, script)
			fake := newFakeCM()
			c.opts.ConnectCM = fake.connect
			ctx := context.Background()

			in := startInput("BP1")
			in.Mode = tc.start
			require.NoError(t, c.Start(ctx, in))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			receive(t, entered, "intake")

			require.NoError(t, st.SetBlueprintMode(ctx, "BP1", tc.stored))
			close(release)
			c.Wait("BP1")

			assert.Equal(t, tc.emits, len(fake.titles()) > 0)
			assert.Equal(t, !tc.emits, rec.has(EventSignOff), "a human sees the sign-off only when nothing approved it")
			assert.Equal(t, tc.runs, rec.has(EventRunStarted))
		})
	}
}

func TestGatePlan_FollowsTheStoredMode(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	drive(t, c, "BP1")

	// Read before the switch, as a chain of runs holds it.
	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	require.NoError(t, st.SetBlueprintMode(ctx, "BP1", protocol.ArchitectModeYolo))
	c.postGatePlan(ctx, sessionOf(t, c, "BP1"), bp)

	plans := rec.all(EventGatePlan)
	require.NotEmpty(t, plans)

	latest := plans[len(plans)-1]
	assert.Equal(t, pack.PolicyAuto, latest.Payload["signoff"])

	for _, s := range anyList(latest.Payload["stages"]) {
		entry, ok := s.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, pack.PolicyAuto, entry["policy"], "stage %v", entry["stage"])
	}
}

// storedMode is BP1's mode as stored.
func storedMode(t *testing.T, st *store.Store) string {
	t.Helper()

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)

	return bp.Mode
}

// onlyMode is the one mode event the recorder holds.
func onlyMode(t *testing.T, rec *recorder) Event {
	t.Helper()

	events := rec.all(EventMode)
	require.Len(t, events, 1)

	return events[0]
}

func TestSetMode_StoresTheModeAndTheViewShowsIt(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m1"))

	assert.Equal(t, protocol.ArchitectModeAuto, storedMode(t, st))

	e := onlyMode(t, rec)
	assert.Equal(t, protocol.ArchitectModeDefault, e.Payload["from"])
	assert.Equal(t, protocol.ArchitectModeAuto, e.Payload["to"])
	assert.Equal(t, "m1", e.Payload["message_id"])
	assert.Equal(t, false, e.Payload["during_run"])
	assert.NotContains(t, e.Payload, "reason")

	view, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, protocol.ArchitectModeAuto, view.Mode)

	require.ErrorIs(t, c.SetMode(ctx, "BP2", protocol.ArchitectModeAuto, "m2"), ErrNotFound, "a session that is not attached")
}

func TestSetMode_TheModeInEffectChangesNothing(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeDefault, "m1"))

	e := onlyMode(t, rec)
	assert.Equal(t, modeUnchanged, e.Payload["reason"])
	assert.NotEmpty(t, e.Payload["refused"])
	assert.Equal(t, protocol.ArchitectModeDefault, storedMode(t, st))
}

func TestSetMode_RefusedOnceEmissionStarted(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	c.opts.ConnectCM = newFakeCM().connect

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit
	driveIn(t, c, in)
	require.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))

	require.NoError(t, c.SetMode(context.Background(), "BP1", protocol.ArchitectModeDefault, "m-mode"))

	e := onlyMode(t, rec)
	assert.Equal(t, modeEmissionStarted, e.Payload["reason"])
	assert.NotEmpty(t, e.Payload["refused"])
	assert.Equal(t, protocol.ArchitectModeAutoEmit, storedMode(t, st))
}

func TestSetMode_RefusedOnceTheSignoffIsApprovedThoughEmissionStopped(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	drive(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)

	version, ok := so.Payload["version"].(int)
	require.True(t, ok)

	// An approval whose emission a stop left idle, as a blocked preflight does.
	require.True(t, c.decide(ctx, sessionOf(t, c, "BP1").audit, "BP1", signoffStage, version, store.GateApprove, "", store.ResponderHuman, "m-ok"))
	c.setState(ctx, "BP1", StateIdle)

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeYolo, "m-mode"))

	assert.Equal(t, modeEmissionStarted, onlyMode(t, rec).Payload["reason"])
	assert.Equal(t, protocol.ArchitectModeDefault, storedMode(t, st))
}

func TestSetMode_RefusedAfterThePipelineEnded(t *testing.T) {
	script := happyScript()
	script["readiness"] = returns("# Not ready", readiness(false))

	c, st, _, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")

	view, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	require.Equal(t, viewEnded, view.State)

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))

	assert.Equal(t, modeEnded, onlyMode(t, rec).Payload["reason"])
	assert.Equal(t, protocol.ArchitectModeDefault, storedMode(t, st))
}

func TestSetMode_DuringARunSaysTheRunGoesOn(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})

	c, _, _, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		signal(entered)
		<-release

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, entered, "intake")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	close(release)
	c.Wait("BP1")

	assert.Equal(t, true, onlyMode(t, rec).Payload["during_run"])
}

func TestSetMode_SurvivesAReopenAndAColdOpen(t *testing.T) {
	c, st, _, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeYolo, "m1"))
	require.NoError(t, c.End(ctx, "BP1"))

	// CM's start may still carry the mode the blueprint was created with.
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")
	assert.Equal(t, protocol.ArchitectModeYolo, storedMode(t, st), "a reopen never reverts a switch")

	require.NoError(t, c.Shutdown(ctx))

	cold, _, _, _ := newControllerOn(t, st, artifactRun("# Need"))
	require.NoError(t, cold.Start(ctx, startInput("BP1")))
	cold.Wait("BP1")

	view, err := cold.View(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, protocol.ArchitectModeYolo, view.Mode, "a cold open serves the switched mode")
}

func TestSetMode_PostsTheGatePlanAgain(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.Equal(t, 1, rec.count(EventGatePlan))

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	plans := rec.all(EventGatePlan)
	require.Len(t, plans, 2)

	for _, s := range anyList(plans[1].Payload["stages"]) {
		entry, ok := s.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, pack.PolicyAuto, entry["policy"], "stage %v", entry["stage"])
	}

	assert.Equal(t, pack.PolicyHuman, plans[1].Payload["signoff"], "auto stops at the sign-off")
}

func TestSetMode_PostsNoGatePlanBeforeTheFirstStage(t *testing.T) {
	c, _, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m1"))

	assert.False(t, rec.has(EventGatePlan))
}

func TestSetMode_AppliesWhenTheRecordCannotBePlaced(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	// A run of a stage the pack does not hold: the view tolerates it, so a switch does too.
	_, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "nosuch", Attempt: 1, Round: 1, Status: store.RunDone, StartedAt: time.Now()})
	require.NoError(t, err)

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m1"))

	assert.NotContains(t, onlyMode(t, rec).Payload, "reason")
	assert.Equal(t, protocol.ArchitectModeAuto, storedMode(t, st))
}

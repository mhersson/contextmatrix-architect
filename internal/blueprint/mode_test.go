package blueprint

import (
	"context"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
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

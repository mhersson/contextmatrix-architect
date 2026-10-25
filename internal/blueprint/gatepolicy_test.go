package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// newReviewed builds a controller on the reviewed test pack: intake, whose
// questions a human answers and whose gate its reviewer's verdict decides,
// then plan. It has a workspace, alpha in ContextMatrix and research
// support, which a start with research settings turns on.
func newReviewed(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	pk, err := pack.LoadDir("testdata/reviewed")
	require.NoError(t, err)

	runner := &fakeRunner{fn: scripted(script)}
	rec := newRecorder()
	c := New(Options{
		Store: st, Pack: pk, Runner: runner, MaxRunCostUSD: 2, MaxTurns: 10,
		Sink: rec.sink, HarnessLine: rec.line,
		Workspace: &fakeWorkspace{root: t.TempDir()}, Projects: listAlpha, Credentials: fakeCredentials,
		Search: searchOK, Fetch: fakeFetcher{},
	})
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	return c, st, runner, rec
}

func reviewedScript() stageScript {
	return stageScript{
		"intake":        returns("# Need", nil),
		"intake.review": returns("# Review", map[string]any{"score": float64(5), "findings": []any{}}),
		"plan":          returns("# Plan", nil),
	}
}

func TestGatePolicy_IntakeAsksAHumanAndItsVerdictDecidesItsGate(t *testing.T) {
	var intakes atomic.Int32

	script := reviewedScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if intakes.Add(1) == 1 {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Users", "Who uses it?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		return done("# Need", nil), nil
	}

	c, st, runner, rec := newReviewed(t, script)
	driveIn(t, c, startInput("BP1"))

	require.Equal(t, []string{"intake"}, stagesRun(runner), "the round waits for its human")
	assert.Empty(t, inputsOf(runner, "intake.responder"), "no responder answers intake")

	answerRound(t, c, "BP1", 1, true, own(1, "the people who run releases"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "intake.review"), 1)
	assert.False(t, rec.has(EventGate), "the verdict decides the gate")
	assert.Equal(t, []string{"intake", "intake", "plan"}, stagesRun(runner))

	gates, err := st.ListGates(context.Background(), "BP1", "intake")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestGatePolicy_AGateReplyMakesTheGateHuman(t *testing.T) {
	c, st, runner, rec := newReviewed(t, reviewedScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "intake", pack.PolicyHuman, "m0"))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the gate waits for its human")
	assert.Equal(t, "intake", gate.Payload["stage"])
	assert.Equal(t, []string{"intake"}, stagesRun(runner))

	gates, err := st.ListGates(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.Empty(t, gates)
}

func TestGatePolicy_FirstMatchWins(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		replies [][2]string // gate replies in order: stage, policy
		stage   string
		gate    bool
		want    string
	}{
		{name: "the pack's policy answers the questions", mode: protocol.ArchitectModeDefault, stage: "intake", want: pack.PolicyHuman},
		{name: "the pack's gate policy decides the gate", mode: protocol.ArchitectModeDefault, stage: "intake", gate: true, want: pack.PolicyAuto},
		{name: "a stage without a gate policy uses its policy", mode: protocol.ArchitectModeDefault, stage: "plan", gate: true, want: pack.PolicyAuto},
		{name: "a reply sets the questions", mode: protocol.ArchitectModeDefault, replies: [][2]string{{"intake", "auto"}}, stage: "intake", want: pack.PolicyAuto},
		{name: "and the gate", mode: protocol.ArchitectModeDefault, replies: [][2]string{{"intake", "human"}}, stage: "intake", gate: true, want: pack.PolicyHuman},
		{name: "the auto mode makes the questions auto", mode: protocol.ArchitectModeAuto, stage: "intake", want: pack.PolicyAuto},
		{name: "and the gate", mode: protocol.ArchitectModeAuto, stage: "intake", gate: true, want: pack.PolicyAuto},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _ := newReviewed(t, reviewedScript())
			ctx := context.Background()

			in := startInput("BP1")
			in.Mode = tt.mode
			require.NoError(t, c.Start(ctx, in))

			for i, r := range tt.replies {
				require.NoError(t, c.SetPolicy(ctx, "BP1", r[0], r[1], fmt.Sprintf("m%d", i)))
			}

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			got, err := c.policy(ctx, bp, tt.stage)
			if tt.gate {
				got, err = c.gatePolicy(ctx, bp, tt.stage)
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGatePolicy_TheGatePlanNamesBoth(t *testing.T) {
	c, _, _, rec := newReviewed(t, reviewedScript())
	driveIn(t, c, startInput("BP1"))

	plan, ok := rec.first(EventGatePlan)
	require.True(t, ok)
	assert.Equal(t, []any{
		map[string]any{"stage": "intake", "policy": pack.PolicyHuman, "gate": true, "gate_policy": pack.PolicyAuto},
		map[string]any{"stage": "plan", "policy": pack.PolicyAuto, "gate": false},
	}, plan.Payload["stages"])
}

// seedBeforeIntakeGate stores BP1 on the reviewed pack as a pack that gave
// intake no gate left it: intake built and completed with no verdict and no
// gate row, then a plan run that ended with status. It returns plan's run.
func seedBeforeIntakeGate(t *testing.T, st *store.Store, status string, state State) int64 {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{
		ID: "BP1", Projects: []string{"alpha"}, Mode: protocol.ArchitectModeDefault, Model: "m",
		State: string(state), CreatedAt: now, UpdatedAt: now,
	}))

	_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: needStage, Body: "the need"})
	require.NoError(t, err)

	intake, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
	require.NoError(t, err)
	require.NoError(t, st.EndRun(ctx, intake, store.RunDone, "", store.RunUsage{}, now))

	_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "intake", Body: "# Need"})
	require.NoError(t, err)

	plan, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "plan", Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
	require.NoError(t, err)
	require.NoError(t, st.EndRun(ctx, plan, status, "", store.RunUsage{}, now))

	return plan
}

func TestGatePolicy_AStagePassedBeforeItHadAGateIsNotJudgedWhenTheNextFailed(t *testing.T) {
	c, st, runner, rec := newReviewed(t, reviewedScript())
	ctx := context.Background()

	seedBeforeIntakeGate(t, st, store.RunFailed, StateFailed)

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "intake.review"), "no review of intake")
	assert.Empty(t, stagesRun(runner), "the failed plan waits for the next message")
	assert.False(t, rec.has(EventGate))

	v, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, v.Pipeline, 2)
	assert.Equal(t, stepDone, v.Pipeline[0].Status)
	assert.Equal(t, stepFailed, v.Pipeline[1].Status)

	require.NoError(t, c.Message(ctx, "BP1", "try again", "m1"))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "intake.review"), "nor after the note")
	assert.Equal(t, []string{"plan"}, stagesRun(runner), "the note runs plan again")

	gates, err := st.ListGates(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.Empty(t, gates)
}

func TestGatePolicy_AStagePassedBeforeItHadAGateIsNotJudgedWhileTheNextAsks(t *testing.T) {
	c, st, runner, rec := newReviewed(t, reviewedScript())
	ctx := context.Background()

	plan := seedBeforeIntakeGate(t, st, store.RunAsked, StateWaiting)

	raw, err := json.Marshal(roundState{
		Number: 1, Attempt: 1, BuilderRound: 1, PostedBy: plan, RunID: plan, Change: changePosted,
		Frame: protocol.LogRound{Stage: "plan", Questions: []protocol.RoundQuestion{{
			Number: 1, Title: "Users", Text: "Who uses it?", Options: []protocol.RoundOption{{Title: "Operators"}},
		}}},
	})
	require.NoError(t, err)
	require.NoError(t, st.AppendEvent(ctx, store.Event{Time: time.Now().UTC(), BlueprintID: "BP1", RunID: plan, Kind: EventRound, Payload: raw}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "intake.review"), "no review of intake")
	assert.Empty(t, stagesRun(runner), "the round waits for its human")
	assert.False(t, rec.has(EventGate))

	v, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, v.Pipeline, 2)
	assert.Equal(t, stepDone, v.Pipeline[0].Status)
	assert.Equal(t, stepWaiting, v.Pipeline[1].Status)
	require.NotNil(t, v.Open)
	assert.Equal(t, roundPromptID(1), v.Open.PromptID)

	answerRound(t, c, "BP1", 1, true, own(1, "the people who run releases"))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "intake.review"), "nor after the answers")
	assert.Equal(t, []string{"plan"}, stagesRun(runner), "plan goes on from its round")
}

func TestGatePolicy_AShownGateStaysWithItsHumanWhateverRanAfterIt(t *testing.T) {
	c, st, runner, rec := newReviewed(t, reviewedScript())
	ctx := context.Background()

	seedBeforeIntakeGate(t, st, store.RunFailed, StateFailed)
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventGate, Payload: []byte(`{"stage":"intake","version":1}`),
	}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the gate is shown again")
	assert.Equal(t, "intake", gate.Payload["stage"])
	assert.Empty(t, inputsOf(runner, "intake.review"), "a shown gate is not reviewed")
	assert.Empty(t, stagesRun(runner))
}

func TestGatePolicy_TheViewShowsTheGatePolicy(t *testing.T) {
	c, _, _, _ := newReviewed(t, reviewedScript())
	require.NoError(t, c.Start(context.Background(), startInput("BP1")))

	v, err := c.View(context.Background(), "BP1")
	require.NoError(t, err)
	require.NotEmpty(t, v.Pipeline)
	assert.Equal(t, "intake", v.Pipeline[0].Stage)
	assert.True(t, v.Pipeline[0].Gate)
	assert.Equal(t, pack.PolicyAuto, v.Pipeline[0].Policy)
}

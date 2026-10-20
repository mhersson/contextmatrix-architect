package blueprint

import (
	"context"
	"database/sql"
	"errors"
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
	// Readiness's spent rounds end only a pack with no stage that checks cards.
	withoutCardsCheck(c.opts.Pack)

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

func TestSetMode_AWaitingSignoffSwitchedToAnEmittingModeApprovesItself(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ctx := context.Background()

	drive(t, c, "BP1")
	require.True(t, rec.has(EventSignOff))
	require.Empty(t, fake.titles())

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAutoEmit, "m-mode"))
	c.Wait("BP1")

	assert.Equal(t, true, onlyMode(t, rec).Payload["signoff"])
	assert.Equal(t, []string{"Add the thing"}, fake.titles())

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)

	decided, ok := rec.first(EventDecided)
	require.True(t, ok)
	assert.Equal(t, true, decided.Payload["shown"], "the decision closes the sign-off prompt the human was shown")
}

func TestSignoff_AnApprovalBeforeTheSignoffWasShownClosesNoPrompt(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	c.opts.ConnectCM = newFakeCM().connect

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit
	driveIn(t, c, in)

	decided, ok := rec.first(EventDecided)
	require.True(t, ok)
	assert.NotContains(t, decided.Payload, "shown")
}

func TestSetMode_AWaitingSignoffOverACriticalFindingStillStopsForAHuman(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, crossScript(true))
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ctx := context.Background()

	drive(t, c, "BP1")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeYolo, "m-mode"))
	c.Wait("BP1")

	assert.Empty(t, fake.titles(), "nothing is emitted over a critical finding")

	shown := rec.all(EventSignOff)
	require.Len(t, shown, 2, "the sign-off runs again and stops")
	assert.Equal(t, true, shown[1].Payload["auto_stopped"])

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	assert.Empty(t, gates)
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

func TestSetMode_ASwitchToAModeThatStopsAtTheSignoffRunsNothing(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ctx := context.Background()

	drive(t, c, "BP1")

	connects := fake.connectCount()

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	assert.NotContains(t, onlyMode(t, rec).Payload, "signoff")
	assert.Equal(t, connects, fake.connectCount(), "no preflight runs")
	assert.Len(t, rec.all(EventSignOff), 1)
}

func TestSetMode_BackToDefaultDuringTheAutomaticPreflightShowsTheSignoff(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	slow := newSlowCM(t, fake)
	c.opts.ConnectCM = func(context.Context, string) (CMClient, error) { return slow, nil }
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, slow.entered, "the preflight")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeDefault, "m-mode"))
	slow.free()
	c.Wait("BP1")

	assert.Empty(t, fake.titles())

	so, ok := rec.first(EventSignOff)
	require.True(t, ok, "the sign-off returns to the human")
	assert.Equal(t, false, so.Payload["auto_stopped"])

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	assert.Empty(t, gates)
}

// handoverScript is the pipeline with an intake whose first run asks one
// round of two questions, with assumptions on its tab, and whose
// continuation submits; its responder answers "operators".
func handoverScript(assumptions ...string) stageScript {
	script := happyScript()
	script["intake.responder"] = respondsWith("operators")
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			return done("# Need", map[string]any{"slices": 1}), nil
		}

		if _, err := in.Interviewer.Ask(ctx, stage.Round{
			Questions:   []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize"), roundQ("Users", "Who uses it?")},
			Assumptions: assumptions,
		}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}

	return script
}

// blockingSecond is a responder that answers "operators" except on its
// second run, which signals blocked and waits for its cancel.
func blockingSecond(blocked chan struct{}) func(context.Context, stage.Input) (stage.Output, error) {
	var n atomic.Int32

	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if n.Add(1) == 2 {
			signal(blocked)
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return answering("operators", in), nil
	}
}

// responderRounds are the question numbers BP1's responder runs answered, in
// run order.
func responderRounds(t *testing.T, st *store.Store) []int {
	t.Helper()

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	var out []int

	for _, r := range runs {
		if r.Kind == store.RunKindResponder {
			out = append(out, r.Round)
		}
	}

	return out
}

func TestSetMode_HandsAWaitingRoundToTheResponderAndKeepsTheHumansAnswers(t *testing.T) {
	c, st, runner, rec, _ := newPipeline(t, handoverScript())
	ctx := context.Background()

	drive(t, c, "BP1")

	answerRound(t, c, "BP1", 1, false, pick(1, 2))
	c.Wait("BP1")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	e := onlyMode(t, rec)
	assert.Equal(t, 1, e.Payload["hands_over"])
	assert.Equal(t, false, e.Payload["during_run"])
	assert.Equal(t, []int{2}, responderRounds(t, st), "the responder answers only the open question")

	round := latestRound(t, c, "BP1", 1)
	require.True(t, round.Frame.Closed)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "operators"}}, round.Frame.Answers)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 2, "one row per question")

	who := map[int]string{}
	for _, q := range qs {
		who[q.Number] = q.Responder
	}

	assert.Equal(t, map[int]string{1: store.ResponderHuman, 2: store.ResponderAuto}, who)

	var cont stage.Input

	for _, in := range inputsOf(runner, "intake") {
		if in.History != nil {
			cont = in
		}
	}

	require.NotNil(t, cont.History, "the stage continues from its conversation")
	assertAttributed(t, cont.Task)
}

func TestSetMode_AHandedOverTabKeepsTheHumansDecisionsAndAcceptsTheRest(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, handoverScript("One cluster per environment.", "Nightly backups are enough.", "No multi-region."))
	ctx := context.Background()

	drive(t, c, "BP1")

	require.NoError(t, c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: roundPromptID(1), Decisions: []protocol.AssumptionDecision{
		{ID: "A1", Accept: true}, {ID: "A2", Accept: false},
	}}, "m-tab"))
	c.Wait("BP1")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	round := latestRound(t, c, "BP1", 1)
	require.True(t, round.Frame.Closed)
	assert.ElementsMatch(t, []protocol.AssumptionDecision{{ID: "A1", Accept: true}, {ID: "A2", Accept: false}, {ID: "A3", Accept: true}}, round.Frame.Decisions)
	assert.Equal(t, []string{"A2"}, round.Converted, "the human's conversion stands")

	accepted, err := c.acceptedAssumptions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, accepted, 1, "only the human's acceptance is confirmed")
	assert.Equal(t, "A1", accepted[0].ID)
}

func TestSetMode_AGateReplyKeepsTheWaitingRoundWithTheHuman(t *testing.T) {
	c, st, runner, rec, _ := newPipeline(t, handoverScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.SetPolicy(ctx, "BP1", "intake", "human", "m-gate"))

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	assert.NotContains(t, onlyMode(t, rec).Payload, "hands_over")

	round := latestRound(t, c, "BP1", 1)
	assert.False(t, round.Frame.Responder)
	assert.False(t, round.Frame.Closed)
	assert.Empty(t, inputsOf(runner, "intake.responder"))
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

func TestSetMode_ARoundIsHandedOverWhenTheChatRunHoldingTheSlotEnds(t *testing.T) {
	cases := []struct {
		name  string
		modes []string
	}{
		{name: "switched to auto", modes: []string{protocol.ArchitectModeAuto}},
		{name: "and back to default before the responder ran", modes: []string{protocol.ArchitectModeAuto, protocol.ArchitectModeDefault}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entered, release := make(chan struct{}, 1), make(chan struct{})

			script := handoverScript()
			asking := script["intake"]
			script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				if !in.Chat {
					return asking(ctx, in)
				}

				signal(entered)
				<-release

				if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Kustomize keeps plain manifests."}); err != nil {
					return stage.Output{}, err
				}

				return askedOutput(in), nil
			}

			c, st, _, rec, _ := newPipeline(t, script)
			ctx := context.Background()

			drive(t, c, "BP1")

			require.NoError(t, c.Message(ctx, "BP1", "Why Kustomize?", "m-chat"))
			receive(t, entered, "the chat run")

			for i, m := range tc.modes {
				require.NoError(t, c.SetMode(ctx, "BP1", m, fmt.Sprintf("m-mode-%d", i)))
			}

			first := rec.all(EventMode)[0]
			assert.Equal(t, 1, first.Payload["hands_over"])
			assert.Equal(t, true, first.Payload["during_run"])

			close(release)
			c.Wait("BP1")

			assert.Equal(t, []int{1, 2}, responderRounds(t, st), "the responder answers both questions")
			assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
		})
	}
}

func TestSetMode_AnAbortedHandoverAnswersOnlyWhatIsLeftOnContinue(t *testing.T) {
	blocked := make(chan struct{}, 1)
	script := handoverScript()
	script["intake.responder"] = blockingSecond(blocked)

	c, st, _, _, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	receive(t, blocked, "the second responder run")

	view, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, stepRunning, stepOf(t, view, "intake").Status, "as an auto round's step reads while its responder answers")

	require.NoError(t, c.Abort(ctx, "BP1", "m-abort"))
	c.Wait("BP1")

	round := latestRound(t, c, "BP1", 1)
	require.False(t, round.Frame.Closed)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Text: "operators"}}, round.Frame.Answers, "the answer given before the abort is kept")

	view, err = c.View(ctx, "BP1")
	require.NoError(t, err)
	assert.Nil(t, view.Open, "a handed-over round waits for nobody")
	assert.Equal(t, stepPending, stepOf(t, view, "intake").Status, "as an auto round's step reads after an abort")

	require.NoError(t, c.Resume(ctx, "BP1", "m-continue"))
	c.Wait("BP1")

	assert.Equal(t, []int{1, 2, 2}, responderRounds(t, st), "continue answers only the open question")
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 2, "one row per question")
}

func TestSetMode_ARestartInTheMiddleOfAHandoverAnswersOnlyWhatIsLeft(t *testing.T) {
	blocked := make(chan struct{}, 1)
	script := handoverScript()
	script["intake.responder"] = blockingSecond(blocked)

	c, st, _, _, ws := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	receive(t, blocked, "the second responder run")
	require.NoError(t, c.Shutdown(ctx))

	c2, runner2, _ := restart(t, st, ws, handoverScript(), newFakeCM().connect, startInput("BP1"))
	c2.Wait("BP1")

	assert.Len(t, inputsOf(runner2, "intake.responder"), 1, "the restart answers only the open question")
	assert.True(t, latestRound(t, c2, "BP1", 1).Frame.Closed)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 2, "one row per question")
}

func TestSetMode_AnAssumptionAChatRunTabsAfterTheHandoverIsTheResponders(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})

	script := handoverScript("One cluster per environment.")
	asking := script["intake"]
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if !in.Chat {
			return asking(ctx, in)
		}

		signal(entered)
		<-release

		if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{
			Reply: "Backups belong on the tab too.", Assumptions: []string{"One cluster per environment.", "Nightly backups are enough."},
		}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}

	c, st, _, _, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")

	require.NoError(t, c.Message(ctx, "BP1", "What about backups?", "m-chat"))
	receive(t, entered, "the chat run")
	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	close(release)
	c.Wait("BP1")

	round := latestRound(t, c, "BP1", 1)
	require.True(t, round.Frame.Closed, "the round closes once the responder has answered")
	assert.ElementsMatch(t, []protocol.AssumptionDecision{{ID: "A1", Accept: true}, {ID: "A2", Accept: true}}, round.Frame.Decisions)
	assert.Equal(t, []int{1, 2}, responderRounds(t, st))

	accepted, err := c.acceptedAssumptions(ctx, "BP1")
	require.NoError(t, err)
	assert.Empty(t, accepted, "the responder's acceptances confirm nothing")
}

func TestSetMode_AnAnswerAChatRunDropsAfterTheHandoverIsTheResponders(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})

	script := handoverScript()
	asking := script["intake"]
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if !in.Chat {
			return asking(ctx, in)
		}

		signal(entered)
		<-release

		// New options drop the human's answer to Q1.
		revisedQ := roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize", "Plain manifests")
		revisedQ.Number = 1

		if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Plain manifests are an option too.", Questions: []stage.RoundQuestion{revisedQ}}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}

	c, st, runner, _, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")

	answerRound(t, c, "BP1", 1, false, pick(1, 2))
	c.Wait("BP1")

	require.NoError(t, c.Message(ctx, "BP1", "Is there a third way?", "m-chat"))
	receive(t, entered, "the chat run")
	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	close(release)
	c.Wait("BP1")

	assert.Equal(t, []int{1, 2}, responderRounds(t, st))
	require.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	var cont stage.Input

	for _, in := range inputsOf(runner, "intake") {
		if in.History != nil && !in.Chat {
			cont = in
		}
	}

	require.NotNil(t, cont.History, "the stage continues from its conversation")

	q1 := blockOf(cont.Task, "Q1")
	assert.Contains(t, q1, answeredByResponder)
	assert.NotContains(t, q1, answeredByUser)
}

func TestSetMode_AHandoverWhoseAnswersCannotBeStoredFailsInsteadOfSpinning(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "architect.db")

	st, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	c, _, runner, _, _ := newPipelineOn(t, st, handoverScript(), nil)

	drive(t, c, "BP1")

	// A second connection refuses every store of the responder's answers on
	// the round, as a full disk would, while reads still work.
	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	_, err = raw.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER probe BEFORE INSERT ON events
		WHEN NEW.kind = '%s' AND json_extract(NEW.payload, '$.change') = '%s'
		BEGIN SELECT RAISE(ABORT, 'disk full'); END`, EventRound, changeResponded))
	require.NoError(t, err)

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	assert.Equal(t, string(StateFailed), stateOf(t, st, "BP1"))
	assert.Len(t, inputsOf(runner, "intake.responder"), 2, "one pass, then the blueprint stops")
	assert.False(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	_, err = raw.ExecContext(ctx, `DROP TRIGGER probe`)
	require.NoError(t, err)

	require.NoError(t, c.Resume(ctx, "BP1", "m-continue"))
	c.Wait("BP1")

	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed, "continue stores the answers the responder's rows hold")
	assert.Len(t, inputsOf(runner, "intake.responder"), 2, "no question is answered twice")
}

func TestSetMode_AStalledRoundHandedOverIsAnsweredAndItsStageGoesOn(t *testing.T) {
	script := handoverScript()
	asking := script["intake"]
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.Chat {
			return stage.Output{}, errors.New("the chat run failed")
		}

		return asking(ctx, in)
	}

	c, st, runner, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")

	require.NoError(t, c.Message(ctx, "BP1", "Why Kustomize?", "m-chat"))
	c.Wait("BP1")
	require.Equal(t, string(StateFailed), stateOf(t, st, "BP1"))

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	assert.Equal(t, []int{1, 2}, responderRounds(t, st))
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	continued := false

	for _, in := range inputsOf(runner, "intake") {
		continued = continued || (in.History != nil && !in.Chat)
	}

	assert.True(t, continued, "the stage continues from its conversation")
	assert.True(t, rec.has(EventSignOff), "the pipeline goes on to the sign-off")
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"), "the sign-off waits for a human under auto")
}

func TestRowAnswer_TakesTheOptionARowNamesElseItsText(t *testing.T) {
	q := protocol.RoundQuestion{Number: 3, Options: []protocol.RoundOption{{Title: "Helm"}, {Title: "Kustomize"}}}

	cases := []struct {
		name, answer string
		want         protocol.RoundAnswer
	}{
		{name: "an option's title", answer: "Helm", want: protocol.RoundAnswer{Number: 3, Option: 1}},
		{name: "an option's title, case and spacing aside", answer: "  kustomize ", want: protocol.RoundAnswer{Number: 3, Option: 2}},
		{name: "a free answer", answer: "operators", want: protocol.RoundAnswer{Number: 3, Text: "operators"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, rowAnswer(q, store.Question{Number: 3, Answer: tc.answer}))
		})
	}
}

// shapeDecisions are the decided events of BP1's shape gate.
func shapeDecisions(rec *recorder) []Event {
	var out []Event

	for _, e := range rec.all(EventDecided) {
		if e.Payload["stage"] == "shape" {
			out = append(out, e)
		}
	}

	return out
}

func TestSetMode_AGateShownForItsPolicyIsApprovedByTheNewMode(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.True(t, rec.has(EventGate))

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	assert.Equal(t, "shape", onlyMode(t, rec).Payload["gate"])
	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner), "the pipeline goes on")

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)

	decided := shapeDecisions(rec)
	require.Len(t, decided, 1)
	assert.Equal(t, true, decided[0].Payload["shown"], "the decision closes the gate prompt the human was shown")
}

func TestSetMode_AGateWhoseVerdictDoesNotPassStaysWithItsHuman(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = reviews(2)

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeAuto)

	shown := rec.count(EventGate)
	require.Positive(t, shown, "the attempt's last verdict did not pass, so a human decides")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAutoEmit, "m-mode"))
	c.Wait("BP1")

	assert.NotContains(t, onlyMode(t, rec).Payload, "gate")
	assert.NotContains(t, stagesRun(runner), "plan")
	assert.Equal(t, shown, rec.count(EventGate))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates)
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

func TestSetMode_AGateReplyKeepsTheShownGateWithItsHuman(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", pack.PolicyHuman, "m0"))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.True(t, rec.has(EventGate))

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	assert.NotContains(t, stagesRun(runner), "plan")

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates)
}

func TestSetMode_ASwitchBackToDefaultLeavesAShownGateAlone(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", pack.PolicyHuman, "m0"))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.True(t, rec.has(EventGate))

	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", pack.PolicyAuto, "m2"))
	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeDefault, "m-mode"))
	c.Wait("BP1")

	assert.NotContains(t, onlyMode(t, rec).Payload, "gate", "a switch to default asks for more human control")
	assert.NotContains(t, stagesRun(runner), "plan")
	assert.Empty(t, shapeDecisions(rec))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates)
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

func TestSetMode_AFoldedGateMarksOnlyItsOwnDecision(t *testing.T) {
	c, st, _, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	drive(t, c, "BP1")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	require.Equal(t, "solution_design", gate.Payload["stage"])

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	assert.Equal(t, []string{"slicing@1:fold", "slicing@1:approve", "solution_design@1:approve", "acceptance@1:approve"}, gateRows(t, st))
	assert.True(t, rec.has(EventSignOff), "auto stops at the sign-off")

	var marked []any

	for _, e := range rec.all(EventDecided) {
		if e.Payload["shown"] == true {
			marked = append(marked, e.Payload["stage"])
		}
	}

	assert.Equal(t, []any{"solution_design"}, marked, "one prompt was shown, so one decision closes it")
}

// lockProbe wraps the sink to record, at each sign-off approval, whether the
// mode lock was free. The sink runs inside the decision, so a free lock
// means the approval was recorded outside the lock.
func lockProbe(c *Controller, id string) *[]bool {
	ml := c.modeLock(id)
	next := c.opts.Sink
	free := &[]bool{}

	c.opts.Sink = func(e Event) {
		next(e)

		if e.Kind != EventDecided || e.Payload["stage"] != signoffStage || e.Payload["decision"] != store.GateApprove {
			return
		}

		got := ml.TryLock()
		if got {
			ml.Unlock()
		}

		*free = append(*free, got)
	}

	return free
}

func TestSignoff_ApprovalsHoldTheModeLock(t *testing.T) {
	t.Run("the mode approving a shown sign-off", func(t *testing.T) {
		c, _, _, rec, _ := newPipeline(t, happyScript())
		c.opts.ConnectCM = newFakeCM().connect
		ctx := context.Background()

		drive(t, c, "BP1")
		require.True(t, rec.has(EventSignOff))

		free := lockProbe(c, "BP1")

		require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAutoEmit, "m-mode"))
		c.Wait("BP1")

		assert.Equal(t, []bool{false}, *free)
	})

	t.Run("a human approving the sign-off", func(t *testing.T) {
		c, _, _, rec, _ := newPipeline(t, happyScript())
		c.opts.ConnectCM = newFakeCM().connect
		ctx := context.Background()

		drive(t, c, "BP1")
		require.True(t, rec.has(EventSignOff))

		free := lockProbe(c, "BP1")

		require.NoError(t, c.Message(ctx, "BP1", "approve", "m-approve"))
		c.Wait("BP1")

		assert.Equal(t, []bool{false}, *free)
	})
}

func TestSetMode_YoloTurnsTheVerifyStepIntoANotice(t *testing.T) {
	notice := verifyNotice("alpha", "go test ./...")

	tests := []struct {
		mode    string
		steps   []string
		notices []string
	}{
		{mode: protocol.ArchitectModeYolo, notices: []string{notice}},
		{mode: protocol.ArchitectModeAutoEmit, steps: []string{verifyStep}},
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			c, _, rec, _ := newSetupPipeline(t)
			ctx := context.Background()

			drive(t, c, "BP1")

			so, ok := rec.first(EventSignOff)
			require.True(t, ok)
			assert.Equal(t, []string{verifyStep}, so.Payload["steps"])

			require.NoError(t, c.SetMode(ctx, "BP1", tt.mode, "m-mode"))
			c.Wait("BP1")

			for _, kind := range []string{EventSignOffAuto, EventEmitted} {
				e, ok := rec.first(kind)
				require.True(t, ok, kind)

				if tt.steps == nil {
					assert.Empty(t, e.Payload["steps"], kind)
				} else {
					assert.Equal(t, tt.steps, e.Payload["steps"], kind)
				}

				if tt.notices == nil {
					assert.Empty(t, e.Payload["notices"], kind)
				} else {
					assert.Equal(t, tt.notices, e.Payload["notices"], kind)
				}
			}
		})
	}
}

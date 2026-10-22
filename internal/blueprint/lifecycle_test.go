package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// asking is the stored conversation of a seeded run that posted a round.
var asking = []llm.Message{{Role: "user", Content: "the stored need"}, {Role: "assistant", Content: "asking"}}

// seeded stores blueprint BP1 with its need, as a reopen finds it.
func seeded(t *testing.T, st *store.Store) store.Blueprint {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{
		ID: "BP1", Projects: []string{"alpha"}, Model: "m", State: string(StateWaiting), CreatedAt: now, UpdatedAt: now,
	}))

	_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: needStage, Body: "the stored need"})
	require.NoError(t, err)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	return bp
}

// answered is round n as posted, with both questions answered.
func answered(n int, run int64) roundState {
	s := twoQuestionRound(n, 1, run)
	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "the ops team"}}
	s.Change, s.MessageID = changeAnswers, "m9"

	return s
}

// closedSnapshots counts the closed snapshots stored for round n.
func closedSnapshots(t *testing.T, st *store.Store, n int) int {
	t.Helper()

	events, err := st.ListEventsOfKind(context.Background(), "BP1", EventRound)
	require.NoError(t, err)

	count := 0

	for _, e := range events {
		var s roundState
		require.NoError(t, json.Unmarshal(e.Payload, &s))

		if s.Number == n && s.Change == changeClosed {
			count++
		}
	}

	return count
}

func TestStart_ReopenShowsAnOpenRoundAgain(t *testing.T) {
	c, st, runner, _ := newController(t, asks("# Need", roundQ("A", "One?"), roundQ("B", "Two?")))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, false, own(1, "one"))
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, changeShown, s.Change)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Text: "one"}}, s.Frame.Answers)
	assert.Equal(t, 1, runner.calls(), "a reopen runs nothing while the round waits")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateWaiting), bp.State)

	answerRound(t, c, "BP1", 1, true, own(1, "one"), own(2, "two"))
	c.Wait("BP1")
	assert.Equal(t, 2, runner.calls())
}

func TestAbort_LeavesAnOpenRoundOpen(t *testing.T) {
	c, _, runner, _ := newController(t, asks("# Need", roundQ("A", "One?")))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Abort(ctx, "BP1", "m2"))
	answerRound(t, c, "BP1", 1, true, own(1, "one"))
	c.Wait("BP1")

	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
	assert.Equal(t, 2, runner.calls())
}

func TestStart_RunsAContinuationEndCutShortAgain(t *testing.T) {
	entered := make(chan struct{}, 1)
	calls := 0

	c, st, runner, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		calls++

		switch {
		case in.History == nil:
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "One?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		case calls == 2:
			signal(entered)
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		default:
			return artifactOutput("# Need", in), nil
		}
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "one"))
	receive(t, entered, "the continuation")
	require.NoError(t, c.End(ctx, "BP1"))
	c.Wait("BP1")
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 3, runner.calls())
	assert.NotNil(t, runner.input(2).History, "the same conversation again, not a fresh attempt")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, runs[0].Attempt, runs[len(runs)-1].Attempt)

	art, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.Equal(t, "# Need", art.Body)
}

func TestStart_ContinuesAfterARestartMidContinuation(t *testing.T) {
	entered := make(chan struct{}, 1)

	c, st, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "One?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		signal(entered)
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "one"))
	receive(t, entered, "the continuation")
	require.NoError(t, c.Shutdown(ctx))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunRunning, runs[1].Status, "the continuation is left as after a crash")

	c2, _, runner2, _ := newControllerOn(t, st, artifactRun("# Need"))
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	require.Equal(t, 1, runner2.calls(), "one continuation")
	assert.NotNil(t, runner2.input(0).History, "the same conversation again, not a fresh attempt")

	runs, err = st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 3)
	assert.Equal(t, store.RunInterrupted, runs[1].Status)
	assert.Equal(t, store.RunDone, runs[2].Status)
	assert.Equal(t, runs[0].Attempt, runs[2].Attempt)
}

func TestStart_ContinuesARoundThatClosedBeforeItsContinuation(t *testing.T) {
	c, st, runner, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	bp := seeded(t, st)
	au := c.newAudit()

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", asking)
	c.putRound(ctx, au, "BP1", poster, twoQuestionRound(1, 1, poster))

	_, err := c.closeRound(ctx, au, bp, answered(1, poster))
	require.NoError(t, err)

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 1, runner.calls(), "the continuation runs once")
	assert.Equal(t, asking, runner.input(0).History, "it resumes the conversation that asked")
	assert.Equal(t, changeClosed, latestRound(t, c, "BP1", 1).Change, "a closed round is not shown again")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 2, "no question row is written twice")
	assert.Equal(t, 1, closedSnapshots(t, st, 1))

	// The continuation stored the artifact, so a later reopen runs nothing.
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")
	assert.Equal(t, 1, runner.calls())
}

func TestStart_FinishesACloseCutShort(t *testing.T) {
	c, st, runner, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	au := c.newAudit()

	seeded(t, st)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", asking)
	c.putRound(ctx, au, "BP1", poster, twoQuestionRound(1, 1, poster))
	c.putRound(ctx, au, "BP1", 0, answered(1, poster))

	// The close wrote Q1's row before the process stopped.
	require.NoError(t, st.AddQuestion(ctx, store.Question{
		BlueprintID: "BP1", RunID: poster, Stage: "intake", Number: 1, Text: "Helm or Kustomize?", Options: []string{"Helm", "Kustomize"},
		Context: "Every manifest.", Answer: "Kustomize", Responder: store.ResponderHuman, MessageID: "m9", AnsweredAt: time.Now().UTC(),
	}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 2)
	assert.Equal(t, 2, qs[1].Number)
	assert.Equal(t, "the ops team", qs[1].Answer)
	assert.Equal(t, "m9", qs[1].MessageID, "the finished close keeps the answering message")

	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
	assert.Equal(t, 1, closedSnapshots(t, st, 1))
	require.Equal(t, 1, runner.calls(), "one continuation")
	assert.Equal(t, asking, runner.input(0).History)
}

func TestStart_ContinuesAClosedResponderRoundWithoutFinishingItsClose(t *testing.T) {
	c, st, runner, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	au := c.newAudit()

	seeded(t, st)

	// An auto round is stored closed by its asking run, which then ended
	// asked. Q2's responder failed, so its answer is the failure and it has
	// no questions row.
	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", asking)
	s := twoQuestionRound(1, 1, poster)
	s.Change, s.Frame.Responder, s.Frame.Closed = changeClosed, true, true
	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "no answer (responder down)"}}
	c.putRound(ctx, au, "BP1", poster, s)

	require.NoError(t, st.AddQuestion(ctx, store.Question{
		BlueprintID: "BP1", RunID: poster, Stage: "intake", Number: 1, Text: "Helm or Kustomize?", Options: []string{"Helm", "Kustomize"},
		Answer: "Kustomize", Responder: store.ResponderAuto, Assumption: true, AnsweredAt: time.Now().UTC(),
	}))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 1, runner.calls(), "one continuation")
	assert.Equal(t, asking, runner.input(0).History)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 1, "a closed responder round is no close cut short")
	assert.Equal(t, 1, closedSnapshots(t, st, 1))
}

func TestStart_ContinuesABuilderRoundsRound(t *testing.T) {
	var calls atomic.Int32

	entered := make(chan struct{}, 1)

	c, st, runner, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			signal(entered)
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return artifactOutput("# Need, revised", in), nil
	})
	ctx := context.Background()
	bp := seeded(t, st)
	au := c.newAudit()

	_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "intake", Body: "# Need"})
	require.NoError(t, err)

	// Builder round 2 of attempt 1, given one finding, posted a round.
	poster := postedRun(t, st, "BP1", 1, 2, store.RunAsked, "round", asking)
	s := twoQuestionRound(1, 1, poster)
	s.BuilderRound, s.RevisedFor = 2, 1
	c.putRound(ctx, au, "BP1", poster, s)

	done := answered(1, poster)
	done.BuilderRound, done.RevisedFor = 2, 1
	_, err = c.closeRound(ctx, au, bp, done)
	require.NoError(t, err)

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	receive(t, entered, "the continuation")
	require.NoError(t, c.End(ctx, "BP1"))
	c.Wait("BP1")
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 2, runner.calls())

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	rerun := runs[len(runs)-1]
	assert.Equal(t, store.RunDone, rerun.Status)
	assert.Equal(t, 1, rerun.Attempt)
	assert.Equal(t, 2, rerun.Round, "the builder round, not a fresh attempt")

	in := runner.input(1)
	assert.Equal(t, asking, in.History)
	require.NotNil(t, in.Validate)
	require.Error(t, in.Validate(ctx, stage.Artifact{Data: map[string]any{"addressed": []any{map[string]any{"finding": "R2", "note": "x"}}}}),
		"the round was given one finding")
	require.NoError(t, in.Validate(ctx, stage.Artifact{Data: map[string]any{"addressed": []any{map[string]any{"finding": "R1", "note": "x"}}}}))
}

func TestMessage_AfterAFailedContinuationRunsItAgain(t *testing.T) {
	var failed atomic.Bool

	c, _, runner, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "One?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		if failed.CompareAndSwap(false, true) {
			return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("provider down")
		}

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "one"))
	c.Wait("BP1")
	require.Equal(t, 2, runner.calls(), "the continuation ran and failed")

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")
	assert.Equal(t, 2, runner.calls(), "a reopen leaves a failed continuation to the next message")

	require.NoError(t, c.Message(ctx, "BP1", "we deploy on Fridays", "m2"))
	c.Wait("BP1")
	require.Equal(t, 3, runner.calls())
	assert.NotNil(t, runner.input(2).History, "the same conversation again")
	assert.Contains(t, runner.input(2).Task, "we deploy on Fridays")
}

func TestStart_CollapsesARoundThePipelineLeft(t *testing.T) {
	c, st, runner, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	seeded(t, st)

	// The run posted the round and never recorded its end.
	poster := postedRun(t, st, "BP1", 1, 1, store.RunRunning, "", nil)
	c.putRound(ctx, c.newAudit(), "BP1", poster, twoQuestionRound(1, 1, poster))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, ChangeLeft, s.Change)
	assert.True(t, s.Frame.Closed, "the panel collapses it")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Empty(t, qs, "a left round records no answers")

	require.Equal(t, 1, runner.calls())
	assert.Nil(t, runner.input(0).History, "intake runs again from its inputs")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunInterrupted, runs[0].Status)
	assert.Equal(t, 2, runs[1].Attempt, "a fresh attempt")
}

func TestStart_AContinuationNamesTheModelThatWroteItsConversation(t *testing.T) {
	for _, model := range []string{"session/model", "other/model"} {
		t.Run(model, func(t *testing.T) {
			c, st, runner, _ := newController(t, artifactRun("# Need"))
			ctx := context.Background()
			bp := seeded(t, st)
			au := c.newAudit()

			poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", asking)
			c.putRound(ctx, au, "BP1", poster, twoQuestionRound(1, 1, poster))

			_, err := c.closeRound(ctx, au, bp, answered(1, poster))
			require.NoError(t, err)

			in := startInput("BP1")
			in.Model = model
			require.NoError(t, c.Start(ctx, in))
			c.Wait("BP1")

			require.Equal(t, 1, runner.calls())
			assert.Equal(t, model, runner.input(0).Model)
			assert.Equal(t, "session/model", runner.input(0).HistoryModel, "the runner drops reasoning another model wrote")
		})
	}
}

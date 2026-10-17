package blueprint

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func roundQ(title, text string, options ...string) stage.RoundQuestion {
	q := stage.RoundQuestion{Title: title, Text: text}
	for _, o := range options {
		q.Options = append(q.Options, stage.RoundOption{Title: o})
	}

	return q
}

// askedOutput is what a run that posted a round returns: its conversation,
// for the continuation to resume.
func askedOutput(in stage.Input) stage.Output {
	return stage.Output{Asked: true, Result: harness.Result{
		Completed: true, Reason: "done", TotalCostUSD: 0.1,
		Messages: []llm.Message{{Role: "user", Content: in.Task}, {Role: "assistant", Content: "asking"}},
	}}
}

// asks is a stage fake: a fresh run posts one round of qs, and a continuation
// submits body followed by the answers its task carries.
func asks(body string, qs ...stage.RoundQuestion) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: qs}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		return artifactOutput(body+"\n\n"+in.Task, in), nil
	}
}

func pick(number, option int) protocol.RoundAnswer {
	return protocol.RoundAnswer{Number: number, Option: option}
}

func own(number int, text string) protocol.RoundAnswer {
	return protocol.RoundAnswer{Number: number, Text: text}
}

func answerRound(t *testing.T, c *Controller, id string, round int, final bool, answers ...protocol.RoundAnswer) {
	t.Helper()

	msgID := fmt.Sprintf("r%d-%d-%t", round, len(answers), final)
	require.NoError(t, c.Reply(context.Background(), id, protocol.RoundReply{PromptID: roundPromptID(round), Answers: answers, Final: final}, msgID))
}

func latestRound(t *testing.T, c *Controller, id string, n int) roundState {
	t.Helper()

	rounds, err := c.rounds(context.Background(), id)
	require.NoError(t, err)

	for _, s := range rounds {
		if s.Number == n {
			return s
		}
	}

	t.Fatalf("no round %d", n)

	return roundState{}
}

func TestRound_HumanAnswersInBatchesAndTheStageResumes(t *testing.T) {
	c, st, runner, _ := newController(t, asks("# Need",
		roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize"), roundQ("Logs", "Own logging stack?", "Yes", "No")))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunAsked, runs[0].Status)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateWaiting), bp.State)

	view, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	require.NotNil(t, view.Open)
	assert.Equal(t, protocol.OpenPrompt{PromptID: "round:1", Kind: "round"}, *view.Open)
	assert.Nil(t, view.Current, "no run is in flight while the round waits")
	assert.Equal(t, stepWaiting, view.Pipeline[0].Status)

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, []int{1, 2}, []int{s.Frame.Questions[0].Number, s.Frame.Questions[1].Number})
	assert.True(t, s.Frame.ResearchOff)

	// One answer now, the other later: the round keeps the first and waits.
	answerRound(t, c, "BP1", 1, false, pick(1, 2))
	c.Wait("BP1")
	assert.Equal(t, 1, runner.calls())
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Option: 2}}, latestRound(t, c, "BP1", 1).Frame.Answers)

	// The reply carries every answer CM holds; the last one closes the round.
	answerRound(t, c, "BP1", 1, true, pick(1, 2), own(2, "Loki, later"))
	c.Wait("BP1")
	require.Equal(t, 2, runner.calls())

	cont := runner.input(1)
	assert.NotNil(t, cont.History, "the continuation resumes the conversation that asked")
	assert.Contains(t, cont.Task, "Kustomize")
	assert.Contains(t, cont.Task, "Loki, later")
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 2)
	assert.Equal(t, "Kustomize", qs[0].Answer)
	assert.Equal(t, store.ResponderHuman, qs[1].Responder)

	art, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(art.Body, "# Need"))

	runs, err = st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, runs[0].Attempt, runs[1].Attempt, "the continuation stays in its attempt")
}

func TestRound_ANumberingAcrossRounds(t *testing.T) {
	second := false
	c, st, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch {
		case in.History == nil:
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "First?"), roundQ("B", "Second?")}})
			if err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		case !second:
			second = true

			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("C", "A follow-up?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		default:
			return artifactOutput("# Need", in), nil
		}
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "one"), own(2, "two"))
	c.Wait("BP1")

	follow := latestRound(t, c, "BP1", 2)
	assert.Equal(t, 3, follow.Frame.Questions[0].Number, "numbers continue across rounds")

	answerRound(t, c, "BP1", 2, true, own(3, "three"))
	c.Wait("BP1")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 3)
}

func TestRound_HumansHaveNoQuestionCap(t *testing.T) {
	c, _, _, _ := newController(t, asks("# Need", roundQ("A", "One?"), roundQ("B", "Two?"), roundQ("C", "Three?")))
	c.opts.Pack.Stages[0].MaxQuestions = 1

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	assert.Len(t, latestRound(t, c, "BP1", 1).Frame.Questions, 3)
}

func TestRound_AnAnsweredQuestionIsNotAskedAgain(t *testing.T) {
	var same, changed error

	c, _, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		_, same = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "  helm or kustomize ", "helm", "kustomize")}})
		_, changed = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize", "Both")}})

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, pick(1, 2))
	c.Wait("BP1")

	require.Error(t, same, "the same question with the same options")
	require.NoError(t, changed, "a changed option set is a new question")
}

func TestRound_ASourceNoToolReturnedIsRefused(t *testing.T) {
	var refused error

	c, _, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		q := roundQ("Deploy", "Helm?")
		q.Options = []stage.RoundOption{{Title: "Helm", Sources: []string{"S4"}}}
		_, refused = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{q}})

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.Error(t, refused)
}

func TestReply_StaleAndMismatchedEntries(t *testing.T) {
	c, _, runner, rec := newController(t, asks("# Need", roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize"), roundQ("Users", "Who?")))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	// A reply naming another round stores nothing and says so.
	require.NoError(t, c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: "round:9", Answers: []protocol.RoundAnswer{pick(1, 1)}}, "m2"))
	assert.Empty(t, latestRound(t, c, "BP1", 1).Frame.Answers)

	errs := rec.all(EventError)
	require.NotEmpty(t, errs)
	assert.Equal(t, "stale_reply", errs[len(errs)-1].Payload["reason"])

	// Entries the round lacks are dropped; the valid one is kept.
	answerRound(t, c, "BP1", 1, false, pick(1, 7), pick(5, 1), pick(2, 1))
	s := latestRound(t, c, "BP1", 1)
	assert.Empty(t, s.Frame.Answers, "option 1 of a question with no options is out of range")
	assert.Equal(t, 3, s.Dropped)

	answerRound(t, c, "BP1", 1, false, pick(1, 2), own(2, " "))
	s = latestRound(t, c, "BP1", 1)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Option: 2}}, s.Frame.Answers)
	assert.Equal(t, 1, runner.calls(), "an incomplete round never resumes")
}

func TestReply_ConcurrentRepliesKeepEveryAnswer(t *testing.T) {
	qs := make([]stage.RoundQuestion, 0, 8)
	for i := range 8 {
		qs = append(qs, roundQ(fmt.Sprintf("Q%d", i+1), fmt.Sprintf("Question %d?", i+1)))
	}

	c, _, runner, _ := newController(t, asks("# Need", qs...))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	var wg sync.WaitGroup

	for i := range 8 {
		wg.Go(func() {
			_ = c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: "round:1", Answers: []protocol.RoundAnswer{own(i+1, "yes")}}, fmt.Sprintf("m%d", i))
		})
	}

	wg.Wait()
	c.Wait("BP1")

	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
	assert.Equal(t, 2, runner.calls(), "one continuation, however the replies interleave")
}

func TestMessage_WhileARoundIsOpenIsANoteItsContinuationGets(t *testing.T) {
	c, _, runner, rec := newController(t, asks("# Need", roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.NoError(t, c.Message(ctx, "BP1", "we deploy twice a week", "m2"))
	c.Wait("BP1")
	assert.Equal(t, 1, runner.calls(), "a note never re-runs the stage or reads as the need")
	assert.True(t, rec.has(EventNote))

	answerRound(t, c, "BP1", 1, true, pick(1, 1))
	c.Wait("BP1")
	require.Equal(t, 2, runner.calls())
	assert.Contains(t, runner.input(1).Task, "we deploy twice a week")
}

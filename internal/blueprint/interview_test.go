package blueprint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	var continuations atomic.Int32

	c, _, runner, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			qs := []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize"), roundQ("Users", "Who?")}
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: qs}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		// The first continuation fails, the second asks round 2, and the
		// third submits.
		switch continuations.Add(1) {
		case 1:
			return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("provider down")
		case 2:
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Logs", "Own logging stack?", "Yes", "No")}}); err != nil {
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

	// stale sends a reply that takes no answers: it stores nothing, runs
	// nothing and says so.
	stale := func(t *testing.T, promptID, msgID string) {
		t.Helper()

		snapshots, calls := rec.count(EventRound), runner.calls()

		require.NoError(t, c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: promptID, Answers: []protocol.RoundAnswer{pick(1, 1), pick(3, 1)}}, msgID))
		c.Wait("BP1")

		assert.Equal(t, snapshots, rec.count(EventRound), "a stale reply stores nothing")
		assert.Equal(t, calls, runner.calls(), "a stale reply runs nothing")

		errs := rec.all(EventError)
		require.NotEmpty(t, errs)
		assert.Equal(t, "stale_reply", errs[len(errs)-1].Payload["reason"])
		assert.Equal(t, msgID, errs[len(errs)-1].Payload["message_id"])
		assert.Equal(t, false, errs[len(errs)-1].Payload["during_run"], "nothing runs after it")
	}

	// A reply naming another round stores nothing and says so.
	stale(t, "round:9", "m2")
	assert.Empty(t, latestRound(t, c, "BP1", 1).Frame.Answers)

	// Entries the round lacks are dropped; the valid one is kept.
	answerRound(t, c, "BP1", 1, false, pick(1, 7), pick(5, 1), pick(2, 1))
	s := latestRound(t, c, "BP1", 1)
	assert.Empty(t, s.Frame.Answers, "option 1 of a question with no options is out of range")
	assert.Equal(t, 3, s.Dropped)

	answerRound(t, c, "BP1", 1, false, pick(1, 2), own(2, " "))
	s = latestRound(t, c, "BP1", 1)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Option: 2}}, s.Frame.Answers)
	assert.Equal(t, 1, runner.calls(), "an incomplete round never resumes")

	// A reply mixing entries the round lacks with valid ones keeps the valid
	// ones, which answer every question: the round closes and goes on, and
	// its continuation fails.
	answerRound(t, c, "BP1", 1, true, pick(5, 1), pick(2, 1), own(2, "the ops team"), pick(1, 2))
	c.Wait("BP1")

	s = latestRound(t, c, "BP1", 1)
	assert.True(t, s.Frame.Closed)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "the ops team"}}, s.Frame.Answers)
	assert.Equal(t, 2, s.Dropped)
	require.Equal(t, 2, runner.calls(), "the continuation ran and failed")

	// A second device's reply to round 1, closed but still the round the
	// pipeline stands at: it neither reopens the round nor runs the
	// continuation again.
	stale(t, "round:1", "m3")

	// A message runs the continuation again, which asks round 2.
	require.NoError(t, c.Message(ctx, "BP1", "go on", "m4"))
	c.Wait("BP1")
	require.Equal(t, 3, runner.calls(), "the continuation asked round 2")

	// A reply to round 1 while round 2 is open.
	stale(t, "round:1", "m5")
	assert.Empty(t, latestRound(t, c, "BP1", 2).Frame.Answers, "nothing reached the open round")

	// A reply to round 2 after it closed and its stage went on.
	answerRound(t, c, "BP1", 2, true, pick(3, 1))
	c.Wait("BP1")
	require.True(t, latestRound(t, c, "BP1", 2).Frame.Closed)
	require.Equal(t, 4, runner.calls())

	stale(t, "round:2", "m6")
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

func TestReply_CompletingARoundWhileTheSlotIsHeldSettlesItOnce(t *testing.T) {
	for _, ending := range []string{"fails", "aborted"} {
		t.Run(ending, func(t *testing.T) {
			var continuations atomic.Int32

			started := make(chan struct{}, 4)

			c, _, runner, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
				if in.History == nil {
					if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")}}); err != nil {
						return stage.Output{}, err
					}

					return askedOutput(in), nil
				}

				if continuations.Add(1) > 1 {
					return artifactOutput("# Need", in), nil
				}

				if ending == "aborted" {
					signal(started)
					<-ctx.Done()

					return stage.Output{}, ctx.Err()
				}

				return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("provider down")
			})
			ctx := context.Background()

			require.NoError(t, c.Start(ctx, startInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")

			// The completing reply lands while a message holds the slot, before
			// the holder reads the round: the message's note reaches the sink
			// first.
			var once sync.Once

			c.opts.Sink = func(e Event) {
				rec.sink(e)

				if e.Kind == EventNote {
					once.Do(func() { answerRound(t, c, "BP1", 1, true, pick(1, 2)) })
				}
			}

			require.NoError(t, c.Message(ctx, "BP1", "a note", "m2"))

			if ending == "aborted" {
				receive(t, started, "the continuation")
				require.NoError(t, c.Abort(ctx, "BP1", "m3"))
			}

			c.Wait("BP1")
			assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
			assert.Equal(t, 2, runner.calls(), "the continuation ran once and waits for the next message")

			require.NoError(t, c.Message(ctx, "BP1", "go on", "m4"))
			c.Wait("BP1")
			assert.Equal(t, 3, runner.calls(), "the next message runs it again")
		})
	}
}

func TestRelease_AnAbortedHolderLeavesARoundChangeToTheNextMessage(t *testing.T) {
	c, _, runner, _ := newController(t, asks("# Need", roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	// A holder that never reads the round is marked by a completing reply,
	// then aborted.
	c.mu.Lock()
	r, _, ok := c.claimLocked("BP1")
	c.mu.Unlock()
	require.True(t, ok)

	answerRound(t, c, "BP1", 1, true, pick(1, 1))
	require.NoError(t, c.Abort(ctx, "BP1", "m2"))
	c.release("BP1", r)
	c.Wait("BP1")

	assert.Equal(t, 1, runner.calls(), "the aborted holder's release runs nothing")
	assert.False(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	require.NoError(t, c.Message(ctx, "BP1", "go on", "m3"))
	c.Wait("BP1")
	assert.Equal(t, 2, runner.calls(), "the next message closes the round and goes on")
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
}

func TestRelease_ARoundChangeTheHolderNeverReadIsActedOn(t *testing.T) {
	c, _, runner, _ := newController(t, asks("# Need", roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	// A holder that never reads the round is marked by a completing reply.
	c.mu.Lock()
	r, _, ok := c.claimLocked("BP1")
	c.mu.Unlock()
	require.True(t, ok)

	answerRound(t, c, "BP1", 1, true, pick(1, 1))
	assert.False(t, latestRound(t, c, "BP1", 1).Frame.Closed, "the held slot defers the close")

	c.release("BP1", r)
	c.Wait("BP1")

	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
	assert.Equal(t, 2, runner.calls(), "the release closes the round and goes on")
}

func TestRelease_AHolderTheSessionEndCutShortLeavesARoundChangeToTheReopen(t *testing.T) {
	c, st, runner, _ := newController(t, asks("# Need", roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	// A holder that never reads the round is marked by a completing reply,
	// then the session ends and is reopened while the holder still has the
	// slot.
	c.mu.Lock()
	r, _, ok := c.claimLocked("BP1")
	c.mu.Unlock()
	require.True(t, ok)

	answerRound(t, c, "BP1", 1, true, pick(1, 1))
	require.NoError(t, c.End(ctx, "BP1"))

	reopened := make(chan error, 1)

	go func() { reopened <- c.Start(ctx, startInput("BP1")) }()

	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()

		_, attached := c.sessions["BP1"]

		return attached
	}, 5*time.Second, time.Millisecond)

	c.release("BP1", r)

	select {
	case err := <-reopened:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the reopen never returned")
	}

	c.Wait("BP1")

	assert.Equal(t, 2, runner.calls(), "the ended holder's release runs nothing; the reopen closes the round and goes on")
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
	assert.Equal(t, 1, closedSnapshots(t, st, 1), "the round closes once")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 1)
}

func autoStart(id string) StartInput {
	in := startInput(id)
	in.Mode = "auto"

	return in
}

// answering is a responder fake: it answers every question with answer.
func answering(answer string, in stage.Input) stage.Output {
	return stage.Output{
		Artifact: stage.Artifact{Body: answer, Data: map[string]any{"answer": answer, "reasoning": "fits the need", "grounded": false}},
		Result:   harness.Result{Completed: true, Reason: "done", Messages: []llm.Message{{Role: "user", Content: in.Task}}},
	}
}

func respondsWith(answer string) func(context.Context, stage.Input) (stage.Output, error) {
	return func(_ context.Context, in stage.Input) (stage.Output, error) { return answering(answer, in), nil }
}

func TestRound_TheAutoResponderAnswersAndTheStageContinues(t *testing.T) {
	c, st, runner, _, _ := newDialog(t, stageScript{
		"intake.responder": respondsWith("Kustomize"),
		"intake": func(ctx context.Context, in stage.Input) (stage.Output, error) {
			if in.History != nil {
				// The later stages are not this test's subject.
				return stage.Output{}, errors.New("stop after intake")
			}

			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
				roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize"), roundQ("Users", "Who uses it?"),
			}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		},
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	s := latestRound(t, c, "BP1", 1)
	assert.True(t, s.Frame.Responder)
	assert.True(t, s.Frame.Closed)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "Kustomize"}}, s.Frame.Answers,
		"an answer that names an option picks it")

	posted, err := st.ListEventsOfKind(ctx, "BP1", EventRound)
	require.NoError(t, err)
	require.Len(t, posted, 1, "an auto round is published once, closed")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 2)
	assert.Equal(t, store.ResponderAuto, qs[0].Responder)
	assert.True(t, qs[0].Assumption)

	// stage run, two responders, then the continuation with the answers.
	require.Equal(t, 4, runner.calls())
	cont := runner.input(3)
	assert.NotNil(t, cont.History)
	assert.Contains(t, cont.Task, "[Q1]")
}

func TestRound_TheAutoCapCountsTheAttempt(t *testing.T) {
	var refused error

	c, _, _, _, _ := newDialog(t, stageScript{
		"intake.responder": respondsWith("yes"),
		"intake": func(ctx context.Context, in stage.Input) (stage.Output, error) {
			if in.History == nil {
				if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "One?"), roundQ("B", "Two?")}}); err != nil {
					return stage.Output{}, err
				}

				return askedOutput(in), nil
			}

			_, refused = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("C", "Three?")}})

			return stage.Output{}, errors.New("stop")
		},
	})
	c.opts.Pack.Stages[0].MaxQuestions = 2

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.Error(t, refused, "the continuation is in the same attempt, so the cap is spent")
}

func TestRound_AnAutoPolicyWithoutAResponderTellsTheStageToDecide(t *testing.T) {
	var refused error

	// The single pack carries no stages/responder.md.
	c, _, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		_, refused = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "One?")}})

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.ErrorIs(t, refused, errNoResponder)
}

func TestRound_AGateReplyDuringARunAppliesToItsNextRound(t *testing.T) {
	asked := make(chan struct{})
	gate := make(chan struct{})
	closeGate := closer(t, gate)

	c, _, _, _, _ := newDialog(t, stageScript{
		"intake.responder": respondsWith("yes"),
		"intake": func(ctx context.Context, in stage.Input) (stage.Output, error) {
			if in.History != nil {
				return stage.Output{}, errors.New("stop")
			}

			signal(asked)
			<-gate

			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "One?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		},
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, asked, "intake's run")
	require.NoError(t, c.SetPolicy(ctx, "BP1", "intake", "auto", "m2"))
	closeGate()
	c.Wait("BP1")

	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Responder)
}

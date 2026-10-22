package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// chatting is a stage fake: a fresh run posts qs, a chat run answers with
// answer (its revisions included) after a signal on entered and a value on
// release (a closed release does not block), and a continuation submits.
func chatting(qs []stage.RoundQuestion, answer stage.ChatAnswer, entered chan struct{}, release <-chan struct{}) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch {
		case in.Chat:
			signal(entered)
			<-release

			if _, err := in.Interviewer.Answer(ctx, answer); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		case in.History == nil:
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: qs}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		default:
			return artifactOutput("# Need\n\n"+in.Task, in), nil
		}
	}
}

// mustLastRunID is the id of the blueprint's newest stage run.
func mustLastRunID(t *testing.T, c *Controller, id string) int64 {
	t.Helper()

	runs, err := c.opts.Store.ListRuns(context.Background(), id)
	require.NoError(t, err)

	var last int64

	for _, r := range runs {
		if r.Kind == store.RunKindStage {
			last = r.ID
		}
	}

	require.NotZero(t, last, "no stage run")

	return last
}

func TestChat_AMessageWhileARoundWaitsIsAnsweredInItsChat(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	close(release)

	c, _, runner, _ := newController(t, chatting(
		[]stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")},
		stage.ChatAnswer{Reply: "Kustomize keeps plain manifests."}, entered, release))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "Why Kustomize?", "m2"))
	c.Wait("BP1")

	require.Equal(t, 2, runner.calls())
	chat := runner.input(1)
	assert.True(t, chat.Chat)
	assert.NotNil(t, chat.History)
	assert.Equal(t, "session/model", chat.HistoryModel, "with the model that wrote it")
	assert.Contains(t, chat.Task, "Why Kustomize?")

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, []protocol.RoundChat{{From: "user", Text: "Why Kustomize?"}, {From: "product_owner", Text: "Kustomize keeps plain manifests."}}, s.Frame.Chat)
	assert.False(t, s.Frame.Closed)
	assert.Equal(t, s.RunID, mustLastRunID(t, c, "BP1"), "the next segment resumes the chat run's conversation")
}

func TestChat_ARevisionClearsOnlyTheChangedPick(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	close(release)

	qs := []stage.RoundQuestion{
		roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize"),
		roundQ("Logs", "Own logging stack?", "Yes", "No"),
		roundQ("Users", "Who uses it?"),
	}
	revise := stage.ChatAnswer{Reply: "Both is an option too.", Questions: []stage.RoundQuestion{
		{Number: 1, Title: "Deploy", Text: "Helm, Kustomize or both?", Options: []stage.RoundOption{{Title: "Helm"}, {Title: "Kustomize"}, {Title: "Both"}}},
		{Number: 2, Title: "Logs", Text: "Run your own logging stack?", Options: []stage.RoundOption{{Title: "Yes"}, {Title: "No"}}},
		roundQ("Region", "Which region?"),
	}}

	c, _, _, _ := newController(t, chatting(qs, revise, entered, release))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, false, pick(1, 2), pick(2, 1))
	require.NoError(t, c.Message(ctx, "BP1", "What about both?", "m2"))
	c.Wait("BP1")

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 2, Option: 1}}, s.Frame.Answers, "Q1's options changed; Q2's did not")
	require.Len(t, s.Frame.Questions, 4)
	assert.Equal(t, 4, s.Frame.Questions[3].Number, "an added question takes the next number")
	assert.Equal(t, "Helm, Kustomize or both?", s.Frame.Questions[0].Text)
}

func TestChat_ReplyDuringAChatSegmentClosesTheRoundAfterIt(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	closeRelease := closer(t, release)

	c, _, runner, _ := newController(t, chatting(
		[]stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")},
		stage.ChatAnswer{Reply: "Plain manifests."}, entered, release))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "Why Kustomize?", "m2"))
	receive(t, entered, "the chat run")

	answerRound(t, c, "BP1", 1, true, pick(1, 2))
	assert.False(t, latestRound(t, c, "BP1", 1).Frame.Closed, "a round never closes under its running chat")

	closeRelease()
	c.Wait("BP1")

	require.Equal(t, 3, runner.calls())
	cont := runner.input(2)
	assert.False(t, cont.Chat)
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	qs, err := c.opts.Store.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 1)
	assert.Equal(t, "Kustomize", qs[0].Answer)
	assert.Equal(t, "r1-1-true", qs[0].MessageID, "the row keeps the answering message, not the chat's")
}

func TestChat_MessagesDuringAChatSegmentAreAnsweredNext(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	closeRelease := closer(t, release)

	c, _, runner, _ := newController(t, chatting(
		[]stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")},
		stage.ChatAnswer{Reply: "Noted."}, entered, release))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "first", "m2"))
	receive(t, entered, "the first chat run")
	require.NoError(t, c.Message(ctx, "BP1", "second", "m3"))
	closeRelease()
	c.Wait("BP1")

	require.Equal(t, 3, runner.calls())
	assert.Contains(t, runner.input(2).Task, "second")

	chat := latestRound(t, c, "BP1", 1).Frame.Chat
	require.Len(t, chat, 4)
	assert.Equal(t, []string{"first", "Noted.", "second", "Noted."}, []string{chat[0].Text, chat[1].Text, chat[2].Text, chat[3].Text},
		"a reply follows what its run saw")
}

func TestChat_ARevisionNamingNoQuestionIsRefused(t *testing.T) {
	var refused error

	c, _, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch {
		case in.Chat:
			_, refused = in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "x", Questions: []stage.RoundQuestion{{Number: 9, Title: "X", Text: "X?"}}})
			if refused == nil {
				return askedOutput(in), nil
			}

			_, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "x"})

			return askedOutput(in), err
		case in.History == nil:
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "One?")}})

			return askedOutput(in), err
		default:
			return artifactOutput("# Need", in), nil
		}
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "hm", "m2"))
	c.Wait("BP1")

	require.Error(t, refused)
}

func TestChat_AnAdditionPastTheRoundCapIsRefused(t *testing.T) {
	qs := make([]stage.RoundQuestion, 0, stage.MaxRoundQuestions)
	for i := range stage.MaxRoundQuestions {
		qs = append(qs, roundQ(fmt.Sprintf("Q%d", i+1), fmt.Sprintf("Question %d?", i+1)))
	}

	var refused error

	c, _, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch {
		case in.Chat:
			_, refused = in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "x", Questions: []stage.RoundQuestion{roundQ("More", "One more?")}})
			_, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "x", Questions: []stage.RoundQuestion{{Number: 1, Title: "Q1", Text: "Question one, reworded?"}}})

			return askedOutput(in), err
		case in.History == nil:
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: qs})

			return askedOutput(in), err
		default:
			return artifactOutput("# Need", in), nil
		}
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "and also?", "m2"))
	c.Wait("BP1")

	require.Error(t, refused, "a full round takes no added question")

	s := latestRound(t, c, "BP1", 1)
	assert.Len(t, s.Frame.Questions, stage.MaxRoundQuestions)
	assert.Equal(t, "Question one, reworded?", s.Frame.Questions[0].Text, "a revision still fits")
}

// failingChat is a stage fake whose chat runs fail: a fresh run posts one
// question, and a continuation submits.
func failingChat(ctx context.Context, in stage.Input) (stage.Output, error) {
	switch {
	case in.Chat:
		return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("provider down")
	case in.History == nil:
		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")}}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	default:
		return artifactOutput("# Need\n\n"+in.Task, in), nil
	}
}

func TestChat_TheContinuationGetsTheChatItsConversationHasNotSeen(t *testing.T) {
	c, _, runner, _ := newController(t, failingChat)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "we deploy twice a week", "m2"))
	c.Wait("BP1")
	require.Equal(t, 2, runner.calls(), "the chat run ran and failed")

	answerRound(t, c, "BP1", 1, true, pick(1, 1))
	c.Wait("BP1")

	require.Equal(t, 3, runner.calls())
	cont := runner.input(2)
	assert.False(t, cont.Chat)
	assert.Contains(t, cont.Task, "we deploy twice a week")
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
}

func TestChat_AMessageAfterAFailedChatRunHasBothAnswered(t *testing.T) {
	var chats atomic.Int32

	c, _, runner, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.Chat && chats.Add(1) > 1 {
			if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Both noted."}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		return failingChat(ctx, in)
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "first", "m2"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "second", "m3"))
	c.Wait("BP1")

	require.Equal(t, 3, runner.calls())
	assert.Contains(t, runner.input(2).Task, "first")
	assert.Contains(t, runner.input(2).Task, "second")

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, []string{"first", "second", "Both noted."}, chatTexts(s))
	assert.False(t, s.Frame.Closed)
}

func chatTexts(s roundState) []string {
	out := make([]string, 0, len(s.Frame.Chat))
	for _, m := range s.Frame.Chat {
		out = append(out, m.Text)
	}

	return out
}

func TestChat_TheReplyReachesTheLogOnlyInTheRoundFrame(t *testing.T) {
	const reply = "Kustomize keeps plain manifests."

	c, st, _, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch {
		case in.Chat:
			// The model also writes the reply as text, and its call carries it.
			for _, line := range []string{
				`{"seq":1,"kind":"model_response","data":{"content":"` + reply + `"}}`,
				`{"seq":2,"kind":"tool_call","data":{"id":"c1","name":"answer_chat","raw_args":"{\"reply\":\"` + reply + `\"}"}}`,
				`{"seq":3,"kind":"usage","data":{"prompt_tokens":10,"completion_tokens":5}}`,
			} {
				_, _ = in.Events.Write([]byte(line + "\n"))
			}

			if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: reply}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		case in.History == nil:
			// A stage run's text reaches the panel as before.
			_, _ = in.Events.Write([]byte(`{"seq":1,"kind":"model_response","data":{"content":"asking"}}` + "\n"))

			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")}}); err != nil {
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
	require.NoError(t, c.Message(ctx, "BP1", "Why Kustomize?", "m2"))
	c.Wait("BP1")

	rec.mu.Lock()
	lines, events := slices.Clone(rec.lines), slices.Clone(rec.recorded)
	rec.mu.Unlock()

	var forwarded []string
	for _, l := range lines {
		forwarded = append(forwarded, string(l))
	}

	assert.True(t, slices.ContainsFunc(forwarded, func(l string) bool { return strings.Contains(l, `"asking"`) }), "a stage run's text is forwarded")
	assert.True(t, slices.ContainsFunc(forwarded, func(l string) bool { return strings.Contains(l, `"usage"`) }), "the chat run's usage is forwarded")

	for _, l := range forwarded {
		assert.NotContains(t, l, reply, "the chat run's text and call never reach the panel")
	}

	for _, e := range events {
		if e.Kind == EventRound {
			continue
		}

		raw, err := json.Marshal(e.Payload)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), reply, "only the round frame carries the reply; %s does not", e.Kind)
	}

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, 1, slices.IndexFunc(s.Frame.Chat, func(m protocol.RoundChat) bool { return m.Text == reply }))
	assert.Len(t, s.Frame.Chat, 2)

	stored, err := st.ListEventsOfKind(ctx, "BP1", EventHarness)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(stored, func(e store.Event) bool { return strings.Contains(string(e.Payload), reply) }),
		"the record keeps every line")
}

func TestChat_AnAnsweredChatKeepsItsRoundAcrossAReopen(t *testing.T) {
	entered, release := make(chan struct{}, 4), make(chan struct{})
	close(release)

	c, _, runner, _ := newController(t, chatting(
		[]stage.RoundQuestion{roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")},
		stage.ChatAnswer{Reply: "Noted."}, entered, release))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "first", "m2"))
	c.Wait("BP1")
	require.Equal(t, 2, runner.calls())

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, changeShown, s.Change, "the reopen shows the round, never collapses it")
	assert.False(t, s.Frame.Closed)
	assert.Equal(t, []string{"first", "Noted."}, chatTexts(s))
	assert.Equal(t, 2, runner.calls(), "a reopen runs nothing while the round waits")

	answerRound(t, c, "BP1", 1, true, pick(1, 2))
	c.Wait("BP1")
	require.Equal(t, 3, runner.calls())
	assert.NotNil(t, runner.input(2).History, "the continuation resumes the chat run's conversation")
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
}

func TestChat_AReopenAnswersAChatTheSessionEndCutShort(t *testing.T) {
	entered := make(chan struct{}, 4)

	var chats atomic.Int32

	c, _, runner, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch {
		case in.Chat && chats.Add(1) == 1:
			signal(entered)
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		case in.Chat:
			if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Noted."}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		case in.History == nil:
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("A", "One?")}}); err != nil {
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
	require.NoError(t, c.Message(ctx, "BP1", "first", "m2"))
	receive(t, entered, "the chat run")

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 3, runner.calls())
	assert.True(t, runner.input(2).Chat, "the reopen answers the chat")
	assert.Contains(t, runner.input(2).Task, "first")

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, []string{"first", "Noted."}, chatTexts(s))
	assert.False(t, s.Frame.Closed)
}

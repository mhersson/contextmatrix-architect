package blueprint

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// twoQuestionRound is round n of intake as run posted it: Q(first) with
// options Helm and Kustomize, Q(first+1) with none.
func twoQuestionRound(n, first int, run int64) roundState {
	return roundState{
		Number: n, Attempt: 1, BuilderRound: 1, PostedBy: run, RunID: run, Change: changePosted,
		Frame: protocol.LogRound{Persona: "product owner", Stage: "intake", ResearchOff: true, Questions: []protocol.RoundQuestion{
			{Number: first, Title: "Deploy", Text: "Helm or Kustomize?", Why: "Every manifest.", Options: []protocol.RoundOption{{Title: "Helm"}, {Title: "Kustomize"}}, Recommended: 2},
			{Number: first + 1, Title: "Users", Text: "Who uses it?", Options: []protocol.RoundOption{}},
		}},
	}
}

// postedRun records a stage run of intake that ended as status, with a
// transcript when messages is not nil.
func postedRun(t *testing.T, st *store.Store, id string, attempt, round int, status, reason string, messages []llm.Message) int64 {
	t.Helper()

	ctx := context.Background()
	runID, err := st.StartRun(ctx, store.Run{BlueprintID: id, Stage: "intake", Kind: store.RunKindStage, Attempt: attempt, Round: round, Status: store.RunRunning, StartedAt: time.Now().UTC()})
	require.NoError(t, err)

	if status != store.RunRunning {
		require.NoError(t, st.EndRun(ctx, runID, status, reason, store.RunUsage{}, time.Now().UTC()))
	}

	if messages != nil {
		raw, err := json.Marshal(messages)
		require.NoError(t, err)
		require.NoError(t, st.PutTranscript(ctx, runID, raw))
	}

	return runID
}

func newRoundController(t *testing.T) (*Controller, *store.Store, audit) {
	t.Helper()

	c, st, _, _ := newController(t, artifactRun("# Need"))
	require.NoError(t, c.Start(context.Background(), startInput("BP1")))

	return c, st, c.newAudit()
}

func TestRounds_TheNewestSnapshotOfEachRoundWins(t *testing.T) {
	c, _, au := newRoundController(t)
	ctx := context.Background()

	r1 := twoQuestionRound(1, 1, 7)
	c.putRound(ctx, au, "BP1", 7, r1)

	answered := r1
	answered.Change, answered.Frame.Answers = changeAnswers, []protocol.RoundAnswer{{Number: 1, Option: 2}}
	c.putRound(ctx, au, "BP1", 0, answered)
	c.putRound(ctx, au, "BP1", 9, twoQuestionRound(2, 3, 9))

	got, err := c.rounds(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, 1, got[0].Number)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Option: 2}}, got[0].Frame.Answers)
	assert.Equal(t, 2, got[1].Number)
}

func TestNextQuestionNumber_FollowsEveryNumberedQuestion(t *testing.T) {
	c, st, au := newRoundController(t)
	ctx := context.Background()

	// Two questions asked alone, before rounds; an answered Q5; a round asking Q6 and Q7.
	for range 2 {
		require.NoError(t, st.AppendEvent(ctx, store.Event{Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventQuestion, Payload: json.RawMessage(`{}`)}))
	}

	require.NoError(t, st.AddQuestion(ctx, store.Question{BlueprintID: "BP1", Stage: "intake", Number: 5, Text: "q", Answer: "a", Responder: store.ResponderHuman, AnsweredAt: time.Now().UTC()}))
	c.putRound(ctx, au, "BP1", 1, twoQuestionRound(1, 6, 1))

	n, err := c.nextQuestionNumber(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, 8, n)

	r, err := c.nextRoundNumber(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, 2, r)
}

func TestMergeReply(t *testing.T) {
	base := twoQuestionRound(1, 1, 7)
	base.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}}

	tests := []struct {
		name      string
		answers   []protocol.RoundAnswer
		decisions []protocol.AssumptionDecision
		want      []protocol.RoundAnswer
		decided   []protocol.AssumptionDecision
		dropped   int
	}{
		{name: "an option", answers: []protocol.RoundAnswer{{Number: 1, Option: 2}}, want: []protocol.RoundAnswer{{Number: 1, Option: 2}}},
		{name: "own words, trimmed", answers: []protocol.RoundAnswer{{Number: 2, Text: "  ops team "}}, want: []protocol.RoundAnswer{{Number: 2, Text: "ops team"}}},
		{name: "blank own words", answers: []protocol.RoundAnswer{{Number: 2, Text: "  "}}, dropped: 1},
		{name: "option out of range", answers: []protocol.RoundAnswer{{Number: 1, Option: 3}}, dropped: 1},
		{name: "a negative option", answers: []protocol.RoundAnswer{{Number: 1, Option: -1}}, dropped: 1},
		{name: "a number the round lacks", answers: []protocol.RoundAnswer{{Number: 9, Option: 1}}, dropped: 1},
		{name: "an option clears own words", answers: []protocol.RoundAnswer{{Number: 1, Option: 1, Text: "x"}}, want: []protocol.RoundAnswer{{Number: 1, Option: 1}}},
		{name: "the later of two wins", answers: []protocol.RoundAnswer{{Number: 1, Option: 1}, {Number: 1, Option: 2}}, want: []protocol.RoundAnswer{{Number: 1, Option: 2}}},
		{name: "a decision", decisions: []protocol.AssumptionDecision{{ID: "A1", Accept: true}}, decided: []protocol.AssumptionDecision{{ID: "A1", Accept: true}}},
		{name: "an unknown assumption", decisions: []protocol.AssumptionDecision{{ID: "A2", Accept: true}}, dropped: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, dropped := mergeReply(base, tt.answers, tt.decisions)
			assert.Equal(t, tt.want, got.Frame.Answers)
			assert.Equal(t, tt.decided, got.Frame.Decisions)
			assert.Equal(t, tt.dropped, dropped)
			assert.Empty(t, base.Frame.Answers, "the merge never writes into the snapshot it read")
		})
	}
}

func TestRoundState_Complete(t *testing.T) {
	s := twoQuestionRound(1, 1, 7)
	assert.False(t, s.complete())

	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 1}, {Number: 2, Text: "ops"}}
	assert.True(t, s.complete())

	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}}
	assert.False(t, s.complete(), "an undecided assumption keeps the round open")

	s.Frame.Decisions = []protocol.AssumptionDecision{{ID: "A1"}}
	assert.True(t, s.complete())
}

func TestCloseRound_IsIdempotent(t *testing.T) {
	c, st, au := newRoundController(t)
	ctx := context.Background()

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	s := twoQuestionRound(1, 1, 7)
	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "the ops team"}}
	s.MessageID = "m9"

	closed, err := c.closeRound(ctx, au, bp, s)
	require.NoError(t, err)
	assert.True(t, closed.Frame.Closed)

	// A close the process cut short is run again; nothing is written twice.
	_, err = c.closeRound(ctx, au, bp, s)
	require.NoError(t, err)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 2)
	assert.Equal(t, store.Question{
		BlueprintID: "BP1", RunID: 7, Stage: "intake", Number: 1, Text: "Helm or Kustomize?", Options: []string{"Helm", "Kustomize"},
		Context: "Every manifest.", Answer: "Kustomize", Responder: store.ResponderHuman, MessageID: "m9", AnsweredAt: qs[0].AnsweredAt,
	}, qs[0])
	assert.Equal(t, "the ops team", qs[1].Answer)

	latest, err := c.rounds(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, latest[0].Frame.Closed)
}

func TestRoundAt(t *testing.T) {
	open := func(t *testing.T, st *store.Store, c *Controller, au audit, poster int64) roundState {
		t.Helper()

		s := twoQuestionRound(1, 1, poster)
		c.putRound(context.Background(), au, "BP1", poster, s)

		return s
	}

	tests := []struct {
		name    string
		setup   func(t *testing.T, st *store.Store, c *Controller, au audit)
		found   bool
		next    roundNext
		stalled bool
	}{
		{name: "no round", setup: func(*testing.T, *store.Store, *Controller, audit) {}},
		{name: "open, unanswered", found: true, next: roundWait, setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
		}},
		{name: "open, every question answered", found: true, next: roundClose, setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			s := open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
			s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 1}, {Number: 2, Text: "ops"}}
			c.putRound(context.Background(), au, "BP1", 0, s)
		}},
		{name: "open with a chat message unanswered", found: true, next: roundChat, setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			s := open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
			s.Frame.Chat = []protocol.RoundChat{{From: "user", Text: "Why Helm?"}}
			c.putRound(context.Background(), au, "BP1", 0, s)
		}},
		{name: "closed, nothing after it", found: true, next: roundContinue, setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			s := open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
			s.Frame.Closed = true
			c.putRound(context.Background(), au, "BP1", 0, s)
		}},
		{name: "closed, its continuation cut short by End", found: true, next: roundContinue, setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			s := open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
			s.Frame.Closed = true
			c.putRound(context.Background(), au, "BP1", 0, s)
			postedRun(t, st, "BP1", 1, 1, store.RunCanceled, reasonSessionEnded, nil)
		}},
		{name: "closed, its continuation failed", found: true, next: roundContinue, stalled: true, setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			s := open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
			s.Frame.Closed = true
			c.putRound(context.Background(), au, "BP1", 0, s)
			postedRun(t, st, "BP1", 1, 1, store.RunFailed, "error", nil)
		}},
		{name: "closed, its continuation aborted", found: true, next: roundContinue, stalled: true, setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			s := open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
			s.Frame.Closed = true
			c.putRound(context.Background(), au, "BP1", 0, s)
			postedRun(t, st, "BP1", 1, 1, store.RunCanceled, "canceled", nil)
		}},
		{name: "closed, its continuation stored the artifact", setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			s := open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
			s.Frame.Closed = true
			c.putRound(context.Background(), au, "BP1", 0, s)
			postedRun(t, st, "BP1", 1, 1, store.RunDone, "done", nil)
		}},
		{name: "a later attempt runs", setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))
			postedRun(t, st, "BP1", 2, 1, store.RunFailed, "error", nil)
		}},
		{name: "its poster never ended asked", setup: func(t *testing.T, st *store.Store, c *Controller, au audit) {
			open(t, st, c, au, postedRun(t, st, "BP1", 1, 1, store.RunCanceled, "canceled", nil))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, au := newRoundController(t)
			tt.setup(t, st, c, au)

			place, found, err := c.roundAt(context.Background(), "BP1")
			require.NoError(t, err)
			assert.Equal(t, tt.found, found)

			if found {
				assert.Equal(t, tt.next, place.next)
				assert.Equal(t, tt.stalled, place.stalled)
			}
		})
	}
}

func TestContinuation_ResumesTheConversationWithTheAnswers(t *testing.T) {
	c, st, au := newRoundController(t)
	ctx := context.Background()

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	history := []llm.Message{
		{Role: "system", Content: "persona"},
		{Role: "user", Content: "the need"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "ask_questions", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "Round 1 is posted."},
	}
	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", history)

	s := twoQuestionRound(1, 1, poster)
	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "the ops team"}}
	s.Frame.Closed = true
	c.putRound(ctx, au, "BP1", 0, s)

	rd, err := c.continuation(ctx, bp, s)
	require.NoError(t, err)
	require.NotNil(t, rd)
	assert.Equal(t, history[1:], rd.history, "the conversation without its system message")
	assert.Equal(t, SegmentAnswers, rd.segment)
	assert.Equal(t, 1, rd.qround)
	assert.Equal(t, 1, rd.attempt)
	assert.Equal(t, 1, rd.number)
	assert.Contains(t, rd.task, "[Q1]")
	assert.Contains(t, rd.task, "Kustomize")
	assert.Contains(t, rd.task, "the ops team")
}

func TestContinuation_WithoutAConversation(t *testing.T) {
	c, st, _ := newRoundController(t)
	ctx := context.Background()

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	s := twoQuestionRound(1, 1, postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil))

	rd, err := c.continuation(ctx, bp, s)
	require.NoError(t, err)
	require.NotNil(t, rd, "a first build starts over in its attempt; its task lists every answered question")
	assert.Nil(t, rd.history)
	assert.Empty(t, rd.task)

	s.BuilderRound, s.RevisedFor = 2, 3

	rd, err = c.continuation(ctx, bp, s)
	require.NoError(t, err)
	assert.Nil(t, rd, "a builder round without its conversation goes back to its critique")
}

func TestRoundSources(t *testing.T) {
	c, st, _ := newRoundController(t)
	ctx := context.Background()

	_, err := st.AddSource(ctx, store.Source{BlueprintID: "BP1", Kind: "repo", Identity: "alpha", Version: "abc", FetchedAt: time.Now().UTC()})
	require.NoError(t, err)
	_, err = st.AddSource(ctx, store.Source{BlueprintID: "BP1", Kind: "url", Identity: "https://helm.sh/docs", FetchedAt: time.Now().UTC()})
	require.NoError(t, err)

	got, err := c.roundSources(ctx, "BP1", []stage.RoundQuestion{{Options: []stage.RoundOption{{Title: "Helm", Sources: []string{"S1", "S2"}}}}})
	require.NoError(t, err)
	assert.Equal(t, protocol.RoundSource{Tag: "S1", Title: "alpha"}, got["S1"])
	assert.Equal(t, protocol.RoundSource{Tag: "S2", Title: "https://helm.sh/docs", URL: "https://helm.sh/docs"}, got["S2"])

	_, err = c.roundSources(ctx, "BP1", []stage.RoundQuestion{{Text: "Which?", Options: []stage.RoundOption{{Title: "Helm", Sources: []string{"S9"}}}}})
	require.Error(t, err)
}

func TestFrameQuestion(t *testing.T) {
	sources := map[string]protocol.RoundSource{"S1": {Tag: "S1", Title: "alpha"}}

	got := frameQuestion(stage.RoundQuestion{
		Title: "Deploy", Text: "Which?", Why: "Every manifest.", Recommended: 1,
		Options: []stage.RoundOption{{Title: "Helm", Summary: "s", Pros: []string{"p"}, Cons: []string{"c"}, Sources: []string{"S1"}}},
	}, 4, sources)

	assert.Equal(t, protocol.RoundQuestion{
		Number: 4, Title: "Deploy", Text: "Which?", Why: "Every manifest.", Recommended: 1,
		Options: []protocol.RoundOption{{Title: "Helm", Summary: "s", Pros: []string{"p"}, Cons: []string{"c"}, Sources: []protocol.RoundSource{{Tag: "S1", Title: "alpha"}}}},
	}, got)

	bare := frameQuestion(stage.RoundQuestion{Text: "Who?"}, 5, nil)
	assert.NotNil(t, bare.Options, "the wire keeps an empty option list")
	assert.Empty(t, bare.Options)
}

func TestRoundPromptID(t *testing.T) {
	assert.Equal(t, "round:3", roundPromptID(3), "CM parses this prompt id")
}

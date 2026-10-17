package stage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubInterviewer struct {
	asked   []Round
	answers []ChatAnswer
	err     error
}

func (s *stubInterviewer) Ask(_ context.Context, r Round) (string, error) {
	s.asked = append(s.asked, r)
	if s.err != nil {
		return "", s.err
	}

	return "Round 1 is posted.", nil
}

func (s *stubInterviewer) Answer(_ context.Context, a ChatAnswer) (string, error) {
	s.answers = append(s.answers, a)
	if s.err != nil {
		return "", s.err
	}

	return "Your reply is posted.", nil
}

const twoQuestions = `{"questions":[
	{"title":"Deploy tooling","question":"Helm, Kustomize or both?","why":"Shapes every manifest.",
	 "options":[{"title":"Kustomize (recommended)","summary":"Overlays.","pros":["no templates"," "],"cons":["no chart"],"sources":["[s1]","S1"]},
	            {"title":"Helm"},{"title":"  "}]},
	{"title":"Logs","question":"Own logging stack?","options":[{"title":"Yes"},{"title":"No"}],"recommended":2}]}`

func TestRun_AskQuestionsEndsTheRunWithARound(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("c1", "ask_questions", twoQuestions)}, FinishReason: "tool_calls"}}}
	iv := &stubInterviewer{}

	in := baseInput(&bytes.Buffer{})
	in.Interviewer = iv

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, out.Asked)
	assert.Empty(t, out.Artifact.Body)
	assert.Len(t, f.requests, 1, "the round ends the run")

	require.Len(t, iv.asked, 1)
	qs := iv.asked[0].Questions
	require.Len(t, qs, 2)
	assert.Equal(t, RoundQuestion{
		Title: "Deploy tooling", Text: "Helm, Kustomize or both?", Why: "Shapes every manifest.",
		Options: []RoundOption{
			{Title: "Kustomize", Summary: "Overlays.", Pros: []string{"no templates"}, Cons: []string{"no chart"}, Sources: []string{"S1"}},
			{Title: "Helm"},
		},
		Recommended: 1,
	}, qs[0], "a trailing marker recommends its option, blanks are dropped, tags are normalised once each")
	assert.Equal(t, 2, qs[1].Recommended)

	last := out.Result.Messages[len(out.Result.Messages)-1]
	assert.Equal(t, "c1", last.ToolCallID)
}

func TestRun_AskQuestionsRefusalsGoBackToTheModel(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{"no questions", `{"questions":[]}`},
		{"not a list", `{"questions":"which?"}`},
		{"no title", `{"questions":[{"question":"Which?"}]}`},
		{"title too long", `{"questions":[{"title":"` + string(bytes.Repeat([]byte("x"), 49)) + `","question":"Which?"}]}`},
		{"no question", `{"questions":[{"title":"Scope"}]}`},
		{"options not a list", `{"questions":[{"title":"Scope","question":"Which?","options":"a, b"}]}`},
		{"option not an object", `{"questions":[{"title":"Scope","question":"Which?","options":["a"]}]}`},
		{"recommended out of range", `{"questions":[{"title":"Scope","question":"Which?","options":[{"title":"a"}],"recommended":2}]}`},
		{"recommended without options", `{"questions":[{"title":"Scope","question":"Which?","recommended":1}]}`},
		{"eleven questions", `{"questions":[` + strings.Repeat(`{"title":"Scope","question":"Which?"},`, 10) + `{"title":"Scope","question":"Which?"}]}`},
		{"seven options", `{"questions":[{"title":"Scope","question":"Which?","options":[` + strings.Repeat(`{"title":"a"},`, 6) + `{"title":"a"}]}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{
				{ToolCalls: []llm.ToolCall{toolCall("c1", "ask_questions", tt.args)}, FinishReason: "tool_calls"},
				{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
			}}
			iv := &stubInterviewer{}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer = iv

			out, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			assert.False(t, out.Asked)
			assert.Equal(t, "# Need", out.Artifact.Body)
			assert.Empty(t, iv.asked)
			assert.Equal(t, 1, out.Result.ToolCallFailures)
		})
	}
}

func TestRun_AnInterviewerRefusalIsAToolError(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "ask_questions", twoQuestions)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
	}}

	in := baseInput(&bytes.Buffer{})
	in.Interviewer = &stubInterviewer{err: errors.New("Q4 already asked this")}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.False(t, out.Asked)
	assert.Equal(t, "# Need", out.Artifact.Body)
}

func TestRun_ChatRunOffersOnlyAnswerChat(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{toolCall("c1", "answer_chat",
			`{"reply":"Kustomize still works.","questions":[{"number":2,"title":"Logs","question":"Own logging stack?","options":[{"title":"Yes"}]}]}`)},
		FinishReason: "tool_calls",
	}}}
	iv := &stubInterviewer{}

	in := baseInput(&bytes.Buffer{})
	in.Interviewer, in.Chat = iv, true
	in.Tools = []tools.Tool{echoTool{}}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, out.Asked)
	assert.ElementsMatch(t, []string{"answer_chat", "echo"}, toolNames(f.requests[0].Tools))

	require.Len(t, iv.answers, 1)
	assert.Equal(t, "Kustomize still works.", iv.answers[0].Reply)
	require.Len(t, iv.answers[0].Questions, 1)
	assert.Equal(t, 2, iv.answers[0].Questions[0].Number)
}

func TestRun_AnswerChatNeedsAReply(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "answer_chat", `{"reply":"  "}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "answer_chat", `{"reply":"Yes."}`)}, FinishReason: "tool_calls"},
	}}
	iv := &stubInterviewer{}

	in := baseInput(&bytes.Buffer{})
	in.Interviewer, in.Chat = iv, true

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, out.Asked)
	require.Len(t, iv.answers, 1)
	assert.Equal(t, "Yes.", iv.answers[0].Reply)
}

func TestRun_AChatRunNeedsAnInterviewer(t *testing.T) {
	in := baseInput(&bytes.Buffer{})
	in.Chat = true

	_, err := runnerWith(&fakeLLM{}).Run(context.Background(), in)
	require.Error(t, err)
}

func TestRun_TextOnlyStopOffersTheRoundTools(t *testing.T) {
	tests := []struct {
		name   string
		chat   bool
		choice string
		tools  []string
	}{
		{"stage run", false, `"required"`, []string{"ask_questions", "submit_artifact"}},
		{"chat run", true, `{"type":"function","function":{"name":"answer_chat"}}`, []string{"answer_chat"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{Content: "Let me think about it.", FinishReason: "stop"}}}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer, in.Chat = &stubInterviewer{}, tt.chat

			_, _ = runnerWith(f).Run(context.Background(), in)
			require.Len(t, f.requests, 2)
			assert.JSONEq(t, tt.choice, string(f.requests[1].ToolChoice))
			assert.ElementsMatch(t, tt.tools, toolNames(f.requests[1].Tools))
		})
	}
}

func TestRun_ToolSetFollowsTheInput(t *testing.T) {
	tests := []struct {
		name  string
		set   func(*Input)
		tools []string
	}{
		{"no asker, no interviewer", func(*Input) {}, []string{"submit_artifact"}},
		{"interviewer", func(in *Input) { in.Interviewer = &stubInterviewer{} }, []string{"ask_questions", "submit_artifact"}},
		{"asker", func(in *Input) { in.Asker = &stubAsker{answer: "yes"} }, []string{"ask_human", "submit_artifact"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"x"}`)}, FinishReason: "tool_calls"}}}

			in := baseInput(&bytes.Buffer{})
			tt.set(&in)

			_, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.tools, toolNames(f.requests[0].Tools))
		})
	}
}

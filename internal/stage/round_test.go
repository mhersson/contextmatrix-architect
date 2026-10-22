package stage

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRun_ARoundOfAssumptionsAlonePosts(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{{
		ToolCalls:    []llm.ToolCall{toolCall("c1", "ask_questions", `{"questions":[],"assumptions":["One region."," "]}`)},
		FinishReason: "tool_calls",
	}}}
	iv := &stubInterviewer{}

	in := baseInput(&bytes.Buffer{})
	in.Interviewer = iv

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, out.Asked)

	require.Len(t, iv.asked, 1)
	assert.Empty(t, iv.asked[0].Questions)
	assert.Equal(t, []string{"One region."}, iv.asked[0].Assumptions, "blanks are dropped")
}

func TestRun_ARoundWithNeitherQuestionsNorAssumptionsIsRefused(t *testing.T) {
	for _, args := range []string{`{}`, `{"questions":[],"assumptions":[" "]}`} {
		t.Run(args, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{
				{ToolCalls: []llm.ToolCall{toolCall("c1", "ask_questions", args)}, FinishReason: "tool_calls"},
				{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
			}}
			iv := &stubInterviewer{}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer = iv

			out, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			assert.False(t, out.Asked)
			assert.Empty(t, iv.asked)
			assert.Equal(t, 1, out.Result.ToolCallFailures)
		})
	}
}

func TestRun_AnswerChatRefusesAnEmptyOrNonListTab(t *testing.T) {
	for _, args := range []string{
		`{"reply":"Yes.","assumptions":[]}`,
		`{"reply":"Yes.","assumptions":"One region."}`,
		`{"reply":"Yes.","assumptions":{"a":1}}`,
	} {
		t.Run(args, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{
				{ToolCalls: []llm.ToolCall{toolCall("c1", "answer_chat", args)}, FinishReason: "tool_calls"},
				{ToolCalls: []llm.ToolCall{toolCall("c2", "answer_chat", `{"reply":"Yes."}`)}, FinishReason: "tool_calls"},
			}}
			iv := &stubInterviewer{}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer, in.Chat = iv, true

			out, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			assert.Equal(t, 1, out.Result.ToolCallFailures)
			require.Len(t, iv.answers, 1, "only the corrected call reaches the interviewer")
			assert.Nil(t, iv.answers[0].Assumptions)
		})
	}
}

func TestRun_AnswerChatAssumptionsReplaceTheTabOnlyWhenGiven(t *testing.T) {
	tests := []struct {
		name string
		args string
		want []string
	}{
		{"left out keeps the tab", `{"reply":"Yes."}`, nil},
		{"a list replaces it", `{"reply":"Yes.","assumptions":["Two zones."," "]}`, []string{"Two zones."}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("c1", "answer_chat", tt.args)}, FinishReason: "tool_calls"}}}
			iv := &stubInterviewer{}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer, in.Chat = iv, true

			_, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			require.Len(t, iv.answers, 1)
			assert.Equal(t, tt.want, iv.answers[0].Assumptions)

			if tt.want != nil {
				assert.NotNil(t, iv.answers[0].Assumptions)
			}
		})
	}
}

func TestRun_TheRoundToolsOfferTheAssumptionsTab(t *testing.T) {
	tests := []struct {
		name string
		chat bool
		tool string
	}{
		{"ask_questions posts it", false, "ask_questions"},
		{"answer_chat replaces it", true, "answer_chat"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("c1", "answer_chat", `{"reply":"Yes."}`)}, FinishReason: "tool_calls"}}}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer, in.Chat = &stubInterviewer{}, tt.chat

			_, _ = runnerWith(f).Run(context.Background(), in)
			require.NotEmpty(t, f.requests)

			var schema struct {
				Properties map[string]struct {
					Type  string `json:"type"`
					Items struct {
						Type string `json:"type"`
					} `json:"items"`
				} `json:"properties"`
				Required []string `json:"required"`
			}

			for _, offered := range f.requests[0].Tools {
				if offered.Function.Name == tt.tool {
					require.NoError(t, json.Unmarshal(offered.Function.Parameters, &schema))
				}
			}

			tab, ok := schema.Properties["assumptions"]
			require.True(t, ok, "the model is offered the tab")
			assert.Equal(t, "array", tab.Type)
			assert.Equal(t, "string", tab.Items.Type)
			assert.NotContains(t, schema.Required, "assumptions")
		})
	}
}

func TestRun_APlainLanguageRunsRoundToolsTakeAConsequence(t *testing.T) {
	for _, tt := range []struct {
		tool string
		chat bool
	}{
		{askQuestionsName, false},
		{AnswerChatName, true},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("c1", AnswerChatName, `{"reply":"Yes."}`)}, FinishReason: "tool_calls"}}}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer, in.Chat, in.PlainLanguage = &stubInterviewer{}, tt.chat, true

			_, _ = runnerWith(f).Run(context.Background(), in)
			require.NotEmpty(t, f.requests)

			var schema struct {
				Properties map[string]struct {
					Items struct {
						OneOf []struct {
							Type string `json:"type"`
						} `json:"oneOf"`
						Properties map[string]any `json:"properties"`
					} `json:"items"`
				} `json:"properties"`
			}

			for _, offered := range f.requests[0].Tools {
				if offered.Function.Name == tt.tool {
					require.NoError(t, json.Unmarshal(offered.Function.Parameters, &schema))
				}
			}

			var entries []string
			for _, o := range schema.Properties["assumptions"].Items.OneOf {
				entries = append(entries, o.Type)
			}

			assert.Equal(t, []string{"string", "object"}, entries, "an entry is a sentence or {text, consequence}")

			_, reasks := schema.Properties["questions"].Items.Properties["reasks"]
			assert.Equal(t, !tt.chat, reasks, "ask_questions offers reasks")
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

func TestRun_EveryRunOfAStageSendsTheSameTools(t *testing.T) {
	kinds := []struct {
		name     string
		designer bool
		want     []string
	}{
		{"stage", false, []string{"ask_questions", "answer_chat", "submit_artifact", "echo"}},
		{"mockup stage", true, []string{"answer_chat", "show_mockup", "echo"}},
	}

	for _, endpoint := range []string{"openrouter", "openai"} {
		for _, k := range kinds {
			t.Run(endpoint+"/"+k.name, func(t *testing.T) {
				var sent []string

				for _, chat := range []bool{false, true} {
					in := baseInput(&bytes.Buffer{})
					in.Interviewer, in.Chat, in.Tools = &stubInterviewer{}, chat, []tools.Tool{echoTool{}}

					if k.designer {
						in.Designer = &fakeDesigner{}
					}

					bodies := wireBodies(t, endpoint, &Runner{NewLLM: ClientFor, Effort: "high"}, in)
					require.NotEmpty(t, bodies)

					var offered []llm.Tool
					require.NoError(t, json.Unmarshal(bodies[0]["tools"], &offered))
					assert.Equal(t, k.want, toolNames(offered), "chat %v, in this order", chat)

					sent = append(sent, string(bodies[0]["tools"]))
				}

				assert.Equal(t, sent[0], sent[1], "a chat run sends the same tools, byte for byte")
			})
		}
	}
}

func lastToolMessage(t *testing.T, msgs []llm.Message) string {
	t.Helper()

	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "tool" {
			return msgs[i].Content
		}
	}

	require.Fail(t, "no tool message in the request")

	return ""
}

func TestRun_TheToolARunsModeDoesNotTakeIsRefused(t *testing.T) {
	answer := toolCall("c2", "answer_chat", `{"reply":"Kustomize still works."}`)

	tests := []struct {
		name     string
		chat     bool
		refused  llm.ToolCall
		accepted llm.ToolCall
		finals   []bool
	}{
		{"answer_chat outside a chat", false, toolCall("c1", "answer_chat", `{"reply":"Hi."}`), toolCall("c2", "submit_artifact", `{"body":"# Need"}`), []bool{false, true}},
		{"ask_questions in a chat", true, toolCall("c1", "ask_questions", twoQuestions), answer, []bool{false, false}},
		{"submit_artifact in a chat", true, toolCall("c1", "submit_artifact", `{"body":"# Need"}`), answer, []bool{false, false}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{
				{ToolCalls: []llm.ToolCall{tt.refused}, FinishReason: "tool_calls"},
				{ToolCalls: []llm.ToolCall{tt.accepted}, FinishReason: "tool_calls"},
			}}
			iv := &stubInterviewer{}

			var events bytes.Buffer

			in := baseInput(&events)
			in.Interviewer, in.Chat = iv, tt.chat

			out, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			assert.Equal(t, 2, f.i, "the refused call did not end the run")

			// The refusal is the mode's own, not the harness's unknown-tool error.
			require.Len(t, f.requests, 2)
			assert.Contains(t, lastToolMessage(t, f.requests[1].Messages), "end the run with")

			assert.Equal(t, 1, out.Result.ToolCallFailures)
			assert.Empty(t, iv.asked, "a refused ask_questions posts no round")
			assert.Equal(t, tt.finals, finals(t, events.String()), "a refused submit is not the reply the run ends on")

			if tt.chat {
				assert.True(t, out.Asked)
				assert.Len(t, iv.answers, 1)
			} else {
				assert.Empty(t, iv.answers, "a refused answer_chat replies nothing")
				assert.Equal(t, "# Need", out.Artifact.Body)
			}
		})
	}
}

func TestRun_AnswerChatCarriesQuestionRevisions(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{{
		ToolCalls: []llm.ToolCall{toolCall("c1", "answer_chat",
			`{"reply":"Kustomize still works.","questions":[{"number":2,"title":"Logs","question":"Own logging stack?","options":[{"title":"Yes"}]}]}`)},
		FinishReason: "tool_calls",
	}}}
	iv := &stubInterviewer{}

	in := baseInput(&bytes.Buffer{})
	in.Interviewer, in.Chat = iv, true

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, out.Asked)

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

func TestRun_AnswerChatCapsTheReplyInCharacters(t *testing.T) {
	// Two-byte runes: the cap counts characters, so 4000 of them pass.
	tooLong, longest := strings.Repeat("é", 4001), strings.Repeat("é", 4000)

	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "answer_chat", `{"reply":"`+tooLong+`"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "answer_chat", `{"reply":"`+longest+`"}`)}, FinishReason: "tool_calls"},
	}}
	iv := &stubInterviewer{}

	in := baseInput(&bytes.Buffer{})
	in.Interviewer, in.Chat = iv, true

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, out.Asked)
	require.Len(t, iv.answers, 1, "the long reply never reaches the round")
	assert.Equal(t, longest, iv.answers[0].Reply)
}

func TestRun_AChatRunNeedsAnInterviewer(t *testing.T) {
	in := baseInput(&bytes.Buffer{})
	in.Chat = true

	_, err := runnerWith(&fakeLLM{}).Run(context.Background(), in)
	require.Error(t, err)
}

func TestRun_TextOnlyStopOffersTheRoundTools(t *testing.T) {
	tests := []struct {
		name     string
		chat     bool
		designer bool
		choice   string
		tools    []string
	}{
		{"stage run", false, false, `"required"`, []string{"ask_questions", "submit_artifact"}},
		{"chat run", true, false, `{"type":"function","function":{"name":"answer_chat"}}`, []string{"answer_chat"}},
		{"mockup run", false, true, `{"type":"function","function":{"name":"show_mockup"}}`, []string{"show_mockup"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{Content: "Let me think about it.", FinishReason: "stop"}}}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer, in.Chat = &stubInterviewer{}, tt.chat

			if tt.designer {
				in.Designer = &fakeDesigner{}
			}

			_, _ = runnerWith(f).Run(context.Background(), in)
			require.Len(t, f.requests, 2)
			assert.JSONEq(t, tt.choice, string(f.requests[1].ToolChoice))
			assert.ElementsMatch(t, tt.tools, toolNames(f.requests[1].Tools))
		})
	}
}

func TestRun_ToolSetFollowsTheInput(t *testing.T) {
	showArgs, err := json.Marshal(mockupArgs(terminalSource))
	require.NoError(t, err)

	submit := toolCall("c1", "submit_artifact", `{"body":"x"}`)
	show := toolCall("c1", "show_mockup", string(showArgs))

	tests := []struct {
		name  string
		set   func(*Input)
		tools []string
		// call is the run's one tool call; submit_artifact unless the mode refuses it.
		call llm.ToolCall
	}{
		{"no interviewer", func(*Input) {}, []string{"submit_artifact"}, submit},
		{"interviewer", func(in *Input) { in.Interviewer = &stubInterviewer{} }, []string{"ask_questions", "answer_chat", "submit_artifact"}, submit},
		{"designer, no interviewer", func(in *Input) { in.Designer = &fakeDesigner{} }, []string{"show_mockup"}, show},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{tt.call}, FinishReason: "tool_calls"}}}

			in := baseInput(&bytes.Buffer{})
			tt.set(&in)

			_, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.tools, toolNames(f.requests[0].Tools))
		})
	}
}

func TestRun_AskQuestionsRecommendedOption(t *testing.T) {
	titles := func(ts ...string) []any {
		out := make([]any, 0, len(ts))
		for _, x := range ts {
			out = append(out, map[string]any{"title": x})
		}

		return out
	}

	tests := []struct {
		name        string
		options     []any
		recommended any
		wantOptions []string
		wantRec     int
		wantRefused bool
	}{
		{name: "none", options: titles("a", "b"), wantOptions: []string{"a", "b"}},
		{name: "the second", options: titles("a", "b"), recommended: float64(2), wantOptions: []string{"a", "b"}, wantRec: 2},
		{name: "past the options", options: titles("a", "b"), recommended: float64(3), wantRefused: true},
		{name: "zero", options: titles("a"), recommended: float64(0), wantRefused: true},
		{name: "not a whole number", options: titles("a", "b"), recommended: 1.5, wantRefused: true},
		{name: "a blank option before it", options: titles("a", " ", "c"), recommended: float64(3), wantOptions: []string{"a", "c"}, wantRec: 2},
		{name: "pointing at a blank option", options: titles("a", " ", "c"), recommended: float64(2), wantRefused: true},
		{name: "a marker names its option", options: titles("a", "b (recommended)"), wantOptions: []string{"a", "b"}, wantRec: 2},
		{name: "any case, with the whitespace around it", options: titles("a  (Recommended)  ", "b"), wantOptions: []string{"a", "b"}, wantRec: 1},
		{name: "the position wins over a marker", options: titles("a", "b (recommended)"), recommended: float64(1), wantOptions: []string{"a", "b"}, wantRec: 1},
		{name: "markers on two options name none", options: titles("a (recommended)", "b (RECOMMENDED)"), wantOptions: []string{"a", "b"}},
		{name: "a marker alone is a blank option", options: titles("a", "(recommended)"), wantOptions: []string{"a"}},
		{name: "text before the marker that is not ASCII", options: titles("\u00e9 (recommended)", "b"), wantOptions: []string{"\u00e9", "b"}, wantRec: 1},
		{name: "a marker before the end stays", options: titles("(recommended) a", "a (recommended) b"), wantOptions: []string{"(recommended) a", "a (recommended) b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := map[string]any{"title": "Scope", "question": "Which?", "options": tt.options}
			if tt.recommended != nil {
				q["recommended"] = tt.recommended
			}

			args, err := json.Marshal(map[string]any{"questions": []any{q}})
			require.NoError(t, err)

			f := &fakeLLM{responses: []llm.Response{
				{ToolCalls: []llm.ToolCall{toolCall("c1", "ask_questions", string(args))}, FinishReason: "tool_calls"},
				{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"x"}`)}, FinishReason: "tool_calls"},
			}}
			iv := &stubInterviewer{}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer = iv

			out, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)

			if tt.wantRefused {
				assert.Empty(t, iv.asked, "the interviewer is not called")
				assert.Equal(t, 1, out.Result.ToolCallFailures)

				return
			}

			require.Len(t, iv.asked, 1)

			got := iv.asked[0].Questions[0]

			var titlesGot []string
			for _, o := range got.Options {
				titlesGot = append(titlesGot, o.Title)
			}

			assert.Equal(t, tt.wantOptions, titlesGot)
			assert.Equal(t, tt.wantRec, got.Recommended)
		})
	}
}

func TestParseQuestion_Audience(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{name: "experience", value: "experience", want: AudienceExperience},
		{name: "normalised", value: "Technical ", want: AudienceTechnical},
		{name: "absent", value: nil, want: ""},
		{name: "unknown", value: "design", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := map[string]any{"title": "Users", "question": "Who uses it?"}
			if tt.value != nil {
				obj["audience"] = tt.value
			}

			q, err := parseQuestion(obj, false)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, q.Audience)
		})
	}
}

func TestParseQuestion_Reasks(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    int
		wantErr bool
	}{
		{name: "a question number", value: float64(3), want: 3},
		{name: "zero", value: float64(0), want: 0},
		{name: "absent", value: nil, want: 0},
		{name: "negative", value: float64(-1), wantErr: true},
		{name: "fractional", value: 2.5, wantErr: true},
		{name: "a tag", value: "Q3", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := map[string]any{"title": "Dates", "question": "How should each photo's date be found?"}
			if tt.value != nil {
				obj["reasks"] = tt.value
			}

			q, err := parseQuestion(obj, false)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, q.Reasks)
		})
	}

	t.Run("the chat form reads none", func(t *testing.T) {
		q, err := parseQuestion(map[string]any{"title": "Dates", "question": "How should each photo's date be found?", "reasks": float64(3)}, true)
		require.NoError(t, err)
		assert.Zero(t, q.Reasks)
	})

	for _, revise := range []bool{false, true} {
		for _, plain := range []bool{false, true} {
			var schema struct {
				Properties map[string]any `json:"properties"`
			}

			require.NoError(t, json.Unmarshal([]byte(questionSchema(revise, plain)), &schema))

			_, offered := schema.Properties["reasks"]
			assert.Equal(t, !revise && plain, offered, "only a plain-language run's ask_questions offers reasks")
		}
	}
}

func TestAskQuestions_AssumptionEntries(t *testing.T) {
	tests := []struct {
		name             string
		tab              string
		wantTexts        []string
		wantConsequences []string
	}{
		{
			name: "sentences and entries", tab: `["plain sentence",{"text":"t","consequence":"c"},{"text":"u"}]`,
			wantTexts: []string{"plain sentence", "t", "u"}, wantConsequences: []string{"", "c", ""},
		},
		{
			name: "no consequence", tab: `["plain sentence",{"text":"t"}]`,
			wantTexts: []string{"plain sentence", "t"},
		},
		{
			name: "an entry without text is dropped", tab: `[" ",{"consequence":"c"},{"text":" "},{"text":"t","consequence":" d "}]`,
			wantTexts: []string{"t"}, wantConsequences: []string{"d"},
		},
	}

	for _, tool := range []string{askQuestionsName, AnswerChatName} {
		for _, tt := range tests {
			t.Run(tool+"/"+tt.name, func(t *testing.T) {
				chat := tool == AnswerChatName

				args := `{"assumptions":` + tt.tab + `}`
				if chat {
					args = `{"reply":"Yes.","assumptions":` + tt.tab + `}`
				}

				f := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("c1", tool, args)}, FinishReason: "tool_calls"}}}
				iv := &stubInterviewer{}

				in := baseInput(&bytes.Buffer{})
				in.Interviewer, in.Chat = iv, chat

				out, err := runnerWith(f).Run(context.Background(), in)
				require.NoError(t, err)
				require.True(t, out.Asked)

				var texts, consequences []string

				if chat {
					require.Len(t, iv.answers, 1)
					texts, consequences = iv.answers[0].Assumptions, iv.answers[0].Consequences
				} else {
					require.Len(t, iv.asked, 1)
					texts, consequences = iv.asked[0].Assumptions, iv.asked[0].Consequences
				}

				assert.Equal(t, tt.wantTexts, texts)
				assert.Equal(t, tt.wantConsequences, consequences)
			})
		}
	}
}

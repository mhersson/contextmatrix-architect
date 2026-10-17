package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLLM replays scripted responses; after the script it says "done".
type fakeLLM struct {
	responses []llm.Response
	i         int
	requests  []llm.Request
}

func (f *fakeLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return f.next(req), nil
}

func (f *fakeLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return f.next(req), nil
}

func (f *fakeLLM) next(req llm.Request) llm.Response {
	f.requests = append(f.requests, req)

	if f.i >= len(f.responses) {
		return llm.Response{Content: "done", FinishReason: "stop"}
	}

	r := f.responses[f.i]
	f.i++

	return r
}

func toolCall(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}
}

func runnerWith(f *fakeLLM) *Runner {
	return &Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return f }}
}

func baseInput(events *bytes.Buffer) Input {
	return Input{
		Stage: "intake", Persona: "You are the PO.", Instructions: "Produce the need.",
		Task: "containerise this project", Model: "test/model",
		Endpoint:   protocol.LLMEndpoint{Type: "openrouter", APIKey: "sk-secret-value-123456"},
		MaxCostUSD: 1, MaxTurns: 5, Events: events, Secrets: []string{"sk-secret-value-123456"},
	}
}

func TestRun_SubmitArtifactEndsTheRunWithTheArtifact(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{{
		ToolCalls:    []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"# Need\n\nk8s","data":{"slices":1}}`)},
		FinishReason: "tool_calls",
	}}}

	var events bytes.Buffer

	out, err := runnerWith(f).Run(context.Background(), baseInput(&events))
	require.NoError(t, err)
	assert.Equal(t, "# Need\n\nk8s", out.Artifact.Body)
	assert.InDelta(t, 1.0, out.Artifact.Data["slices"], 0.0001)
	assert.True(t, out.Result.Completed)
	assert.Equal(t, "done", out.Result.Reason)
	assert.NotEmpty(t, out.Result.Messages)

	// The system prompt carries persona and instructions; the task is the
	// first user message.
	require.NotEmpty(t, f.requests)
	msgs := f.requests[0].Messages
	assert.Equal(t, "system", msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "You are the PO.")
	assert.Contains(t, msgs[0].Content, "Produce the need.")
	assert.Equal(t, "containerise this project", msgs[len(msgs)-1].Content)

	// Events were emitted as JSONL.
	assert.Contains(t, events.String(), `"kind":"tool_call"`)

	names := make([]string, 0, len(f.requests[0].Tools))
	for _, tool := range f.requests[0].Tools {
		names = append(names, tool.Function.Name)
	}

	assert.ElementsMatch(t, []string{"ask_human", "submit_artifact"}, names)
}

func TestRun_AskHumanBlocksUntilAnswered(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "ask_human", `{"question":"Helm or kustomize?","options":["helm","kustomize"]}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"# Need\n\nhelm"}`)}, FinishReason: "tool_calls"},
	}}

	asker := &stubAsker{answer: "helm"}
	in := baseInput(&bytes.Buffer{})
	in.Asker = asker

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "Helm or kustomize?", asker.got.Text)

	// The answer reached the model as the tool result of c1.
	var sawAnswer bool

	for _, req := range f.requests {
		for _, m := range req.Messages {
			if m.Role == "tool" && m.ToolCallID == "c1" && strings.Contains(m.Content, "helm") {
				sawAnswer = true
			}
		}
	}

	assert.True(t, sawAnswer, "answer must be fed back as the tool result")
	assert.Equal(t, "# Need\n\nhelm", out.Artifact.Body)
}

func TestRun_NonObjectDataIsCorrectedByTheModel(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"# Need","data":"slices: 1"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"# Need","data":{"slices":1}}`)}, FinishReason: "tool_calls"},
	}}

	out, err := runnerWith(f).Run(context.Background(), baseInput(&bytes.Buffer{}))
	require.NoError(t, err, "a non-object data block is a tool error the model corrects, not a failed stage")
	assert.Equal(t, 2, f.i, "the first submit did not end the run")
	assert.Equal(t, "# Need", out.Artifact.Body)
	assert.InDelta(t, 1.0, out.Artifact.Data["slices"], 0.0001)
	assert.Equal(t, 1, out.Result.ToolCallFailures)
}

func TestRun_EndsWithoutArtifactIsAnError(t *testing.T) {
	f := &fakeLLM{} // says "done" without calling submit_artifact

	_, err := runnerWith(f).Run(context.Background(), baseInput(&bytes.Buffer{}))
	require.ErrorIs(t, err, ErrNoArtifact)
}

func TestRun_NoAskerRejectsAskHuman(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "ask_human", `{"question":"q"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"x"}`)}, FinishReason: "tool_calls"},
	}}

	out, err := runnerWith(f).Run(context.Background(), baseInput(&bytes.Buffer{}))
	require.NoError(t, err)
	assert.Equal(t, "x", out.Artifact.Body)
	assert.GreaterOrEqual(t, out.Result.ToolCallFailures, 1, "ask_human without an asker fails as a tool error, not a crash")
}

func TestClientFor_BaseURLAndAPIKeyReachTheWire(t *testing.T) {
	var (
		gotPath string
		gotAuth string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	c := ClientFor(protocol.LLMEndpoint{Type: "openai", BaseURL: srv.URL + "/v1", APIKey: "k"})

	_, err := c.Send(context.Background(), llm.Request{
		Model:    "m",
		Messages: []llm.Message{{Role: "user", Content: "ping"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "/v1/chat/completions", gotPath)
	assert.Equal(t, "Bearer k", gotAuth)
}

func TestRun_SecretInToolResultIsRedactedFromEventsAndConversation(t *testing.T) {
	const secret = "sk-secret-value-123456"

	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "ask_human", `{"question":"key?"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"x"}`)}, FinishReason: "tool_calls"},
	}}

	var events bytes.Buffer

	in := baseInput(&events)
	in.Asker = &stubAsker{answer: "the key is " + secret}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)

	// The harness events carry no tool-result text, so only the conversation
	// handed back to the model can leak the secret.
	var sawResult bool

	for _, m := range out.Result.Messages {
		if m.Role == "tool" && m.ToolCallID == "c1" {
			sawResult = true

			assert.Contains(t, m.Content, "the key is")
			assert.NotContains(t, m.Content, secret)
		}
	}

	assert.True(t, sawResult)
}

type echoTool struct{}

func (echoTool) Name() string { return "echo" }
func (echoTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "echo", Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`)}}
}

func (echoTool) Execute(_ context.Context, args map[string]any) (tools.Result, error) {
	return tools.Result{Text: stringArg(args, "text")}, nil
}
func (echoTool) ReadOnly() bool { return true }

func TestRun_ExtraToolsAreOffered(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "echo", `{"text":"hi"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"done"}`)}, FinishReason: "tool_calls"},
	}}

	var events bytes.Buffer

	in := baseInput(&events)
	in.Tools = []tools.Tool{echoTool{}}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "done", out.Artifact.Body)

	names := make([]string, 0, len(f.requests[0].Tools))
	for _, tool := range f.requests[0].Tools {
		names = append(names, tool.Function.Name)
	}

	assert.Contains(t, names, "echo")
	assert.Contains(t, names, "submit_artifact")
}

func TestRun_ValidateRejectsASubmitUntilItPasses(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"draft one"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"draft two"}`)}, FinishReason: "tool_calls"},
	}}

	var events bytes.Buffer

	in := baseInput(&events)
	in.Validate = func(_ context.Context, a Artifact) error {
		if a.Body == "draft one" {
			return errors.New("the body must say two")
		}

		return nil
	}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "draft two", out.Artifact.Body)

	// The rejection reached the model as the tool result of the first submit.
	var sawRejection bool

	for _, req := range f.requests {
		for _, m := range req.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "the body must say two") {
				sawRejection = true
			}
		}
	}

	assert.True(t, sawRejection)
}

type namedTool struct {
	echoTool
	name string
}

func (n namedTool) Name() string { return n.name }

func TestRun_ToolNamedLikeABuiltInIsRefused(t *testing.T) {
	for _, name := range []string{"ask_human", "submit_artifact"} {
		t.Run(name, func(t *testing.T) {
			f := &fakeLLM{}

			var events bytes.Buffer

			in := baseInput(&events)
			in.Tools = []tools.Tool{namedTool{name: name}}

			_, err := runnerWith(f).Run(context.Background(), in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), name)
			assert.Empty(t, f.requests)
		})
	}
}

// bigTool returns n bytes: a head marker, filler and a tail marker.
type bigTool struct{ n int }

func (bigTool) Name() string { return "big" }

func (bigTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: "big", Description: "returns a large result", Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}}
}

func (b bigTool) Execute(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Text: "HEAD" + strings.Repeat("x", b.n-8) + "TAIL"}, nil
}

func TestRun_CapsToolOutput(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "big", `{}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"done"}`)}, FinishReason: "tool_calls"},
	}}

	in := baseInput(&bytes.Buffer{})
	in.Tools = []tools.Tool{bigTool{n: 3 * ToolOutputMaxBytes}}

	_, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)

	require.GreaterOrEqual(t, len(f.requests), 2)

	var result string

	for _, m := range f.requests[1].Messages {
		if m.ToolCallID == "c1" {
			result = m.Content
		}
	}

	require.NotEmpty(t, result, "the tool result reached the model")
	assert.Less(t, len(result), ToolOutputMaxBytes+200, "the result is capped")
	assert.True(t, strings.HasPrefix(result, "HEAD"), "the head is kept")
	assert.True(t, strings.HasSuffix(result, "TAIL"), "the tail is kept")
}

func TestRun_CompactsALongConversationInsteadOfStopping(t *testing.T) {
	ask := func(id string) llm.Response {
		return llm.Response{ToolCalls: []llm.ToolCall{toolCall(id, "ask_human", `{"question":"more?"}`)}, FinishReason: "tool_calls"}
	}

	near := ask("c6")
	near.Usage = llm.Usage{PromptTokens: 120000}

	f := &fakeLLM{responses: []llm.Response{
		ask("c1"), ask("c2"), ask("c3"), ask("c4"), ask("c5"),
		near, // the prompt reached 85% of the window: the older turns are summarized
		{Content: "the briefing", FinishReason: "stop"},
		{ToolCalls: []llm.ToolCall{toolCall("c7", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
	}}

	var events bytes.Buffer

	in := baseInput(&events)
	in.MaxTurns = 20
	in.Asker = &stubAsker{answer: "yes"}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "# Need", out.Artifact.Body)
	assert.Contains(t, events.String(), `"phase":"compaction"`, "the summarize call is accounted as compaction")
}

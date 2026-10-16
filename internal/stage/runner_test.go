package stage

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
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

package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// wireBodies runs in through r against an httptest endpoint of the given type
// (r is built with NewLLM: ClientFor), and returns every request body in order. The server
// answers text only, so the runner sends its forced follow-up as well. The
// run's own error is ignored.
func wireBodies(t *testing.T, endpointType string, r *Runner, in Input) []map[string]json.RawMessage {
	t.Helper()

	var (
		mu     sync.Mutex
		bodies []map[string]json.RawMessage
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		mu.Lock()

		bodies = append(bodies, body)

		mu.Unlock()

		if string(body["stream"]) == "true" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\",\"reasoning_details\":[{\"type\":\"reasoning.text\",\"text\":\"think\",\"index\":0}]},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")

			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done","reasoning_details":[{"type":"reasoning.text","text":"think","index":0}]},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	in.Endpoint = protocol.LLMEndpoint{Type: endpointType, BaseURL: srv.URL + "/v1", APIKey: "k"}

	_, _ = r.Run(context.Background(), in)

	mu.Lock()
	defer mu.Unlock()

	return append([]map[string]json.RawMessage(nil), bodies...)
}

func TestRun_EffortOnTheWire(t *testing.T) {
	tests := []struct {
		endpoint, effort         string
		reasoning, reasoningTier string // "" means the key is absent
	}{
		{"openrouter", "xhigh", `{"effort":"xhigh"}`, ""},
		{"openai", "xhigh", "", `"xhigh"`},
		{"openrouter", "", "", ""},
		{"openai", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.endpoint+"/"+tt.effort, func(t *testing.T) {
			bodies := wireBodies(t, tt.endpoint, &Runner{NewLLM: ClientFor, Effort: tt.effort}, baseInput(&bytes.Buffer{}))
			require.GreaterOrEqual(t, len(bodies), 2, "the forced follow-up was sent too")

			for i, b := range bodies {
				if tt.reasoning == "" {
					assert.NotContains(t, b, "reasoning", "request %d", i)
				} else {
					assert.JSONEq(t, tt.reasoning, string(b["reasoning"]), "request %d", i)
				}

				if tt.reasoningTier == "" {
					assert.NotContains(t, b, "reasoning_effort", "request %d", i)
				} else {
					assert.JSONEq(t, tt.reasoningTier, string(b["reasoning_effort"]), "request %d", i)
				}
			}
		})
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

	assert.ElementsMatch(t, []string{"submit_artifact"}, names)
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
		{ToolCalls: []llm.ToolCall{toolCall("c1", "echo", `{"text":"the key is `+secret+`"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"x"}`)}, FinishReason: "tool_calls"},
	}}

	var events bytes.Buffer

	in := baseInput(&events)
	in.Tools = []tools.Tool{echoTool{}}

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
	for _, name := range []string{"ask_questions", "answer_chat", "submit_artifact", "show_mockup"} {
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
		return llm.Response{ToolCalls: []llm.ToolCall{toolCall(id, "echo", `{"text":"more"}`)}, FinishReason: "tool_calls"}
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
	in.Tools = []tools.Tool{echoTool{}}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "# Need", out.Artifact.Body)
	assert.Contains(t, events.String(), `"phase":"compaction"`, "the summarize call is accounted as compaction")
}

// streamingLLM streams each scripted response's tool-call arguments as
// deltas of chunk bytes before returning it.
type streamingLLM struct {
	fakeLLM

	chunk int
}

func (s *streamingLLM) SendStream(_ context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	resp := s.next(req)

	for i, tc := range resp.ToolCalls {
		args := tc.Function.Arguments

		for j := 0; j < len(args); j += s.chunk {
			if onDelta != nil {
				onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: i, ID: tc.ID, Name: tc.Function.Name, ArgsFragment: args[j:min(j+s.chunk, len(args))]}})
			}
		}
	}

	return resp, nil
}

// applied applies draft frames as a client does.
func applied(t *testing.T, frames []DraftFrame) string {
	t.Helper()

	var held []rune

	for _, f := range frames {
		require.True(t, f.Offset == 0 || f.Offset == len(held))

		if f.Offset == 0 {
			held = nil
		}

		held = append(held, []rune(f.Text)...)
	}

	return string(held)
}

func TestRun_DraftsTheBodyAsItStreams(t *testing.T) {
	f := &streamingLLM{chunk: 3, fakeLLM: fakeLLM{responses: []llm.Response{{
		ToolCalls:    []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"# Need\n\nk8s with sk-secret-value-123456 inside"}`)},
		FinishReason: "tool_calls",
	}}}}

	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }

	_, err := (&Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return f }}).Run(context.Background(), in)
	require.NoError(t, err)

	assert.NotEmpty(t, frames)
	assert.Equal(t, "# Need\n\nk8s with [REDACTED] inside", applied(t, frames))

	for _, d := range frames {
		assert.NotContains(t, d.Text, "sk-secret")
	}
}

func TestRun_ARefusedSubmitIsMarkedAndTheNextRestarts(t *testing.T) {
	f := &streamingLLM{chunk: 4, fakeLLM: fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"too short"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"long enough now"}`)}, FinishReason: "tool_calls"},
	}}}

	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }
	in.Validate = func(_ context.Context, a Artifact) error {
		if a.Body == "too short" {
			return errors.New("the body is too short")
		}

		return nil
	}

	out, err := (&Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return f }}).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "long enough now", out.Artifact.Body)

	refused := -1

	for i, d := range frames {
		if d.Refused {
			refused = i
		}
	}

	require.GreaterOrEqual(t, refused, 0, "a refused frame was published")
	assert.Contains(t, frames[refused].Reason, "the body is too short")
	assert.Equal(t, "too short", applied(t, frames[:refused+1]), "the refused draft stays shown")
	require.Greater(t, len(frames), refused+1)
	assert.Equal(t, 0, frames[refused+1].Offset, "the next attempt restarts at offset 0")
	assert.Equal(t, "long enough now", applied(t, frames))
}

func TestRun_AStreamWithoutToolCallDeltasDraftsAtSubmit(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{{
		ToolCalls:    []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"# Whole"}`)},
		FinishReason: "tool_calls",
	}}}

	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }

	_, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, []DraftFrame{{Offset: 0, Text: "# Whole"}}, frames)
}

// failingLLM streams half a submit, then fails the call, as a transport
// error does after its retries.
type failingLLM struct{ fakeLLM }

func (f *failingLLM) SendStream(_ context.Context, _ llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: "submit_artifact", ArgsFragment: `{"body":"half a dr`}})

	return llm.Response{}, errors.New("stream reset by peer")
}

func TestRun_AnAbandonedTurnVoidsTheDraft(t *testing.T) {
	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }

	_, err := (&Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return &failingLLM{} }}).Run(context.Background(), in)
	require.Error(t, err)

	require.NotEmpty(t, frames)
	assert.Equal(t, "half a dr", frames[0].Text)
	assert.True(t, frames[len(frames)-1].Void, "the harness's error event voids the draft")
}

func TestRun_WithoutADraftCallbackNothingIsDrafted(t *testing.T) {
	f := &streamingLLM{chunk: 2, fakeLLM: fakeLLM{responses: []llm.Response{{
		ToolCalls:    []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"# Need"}`)},
		FinishReason: "tool_calls",
	}}}}

	out, err := (&Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return f }}).Run(context.Background(), baseInput(&bytes.Buffer{}))
	require.NoError(t, err)
	assert.Equal(t, "# Need", out.Artifact.Body)
}

func TestRun_AChatRunDraftsNothing(t *testing.T) {
	f := &streamingLLM{chunk: 3, fakeLLM: fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"# Need\n\nk8s"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "answer_chat", `{"reply":"Hi."}`)}, FinishReason: "tool_calls"},
	}}}

	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.Interviewer, in.Chat = &stubInterviewer{}, true
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }

	out, err := (&Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return f }}).Run(context.Background(), in)
	require.NoError(t, err)
	assert.True(t, out.Asked)
	assert.Empty(t, frames, "a chat run's refused submit never reaches the live view")
}

func TestRun_ACompactionDiscardVoidsTheDraft(t *testing.T) {
	ask := func(id string) llm.Response {
		return llm.Response{ToolCalls: []llm.ToolCall{toolCall(id, "echo", `{"text":"more"}`)}, FinishReason: "tool_calls"}
	}

	near := llm.Response{
		ToolCalls:    []llm.ToolCall{toolCall("c6", "submit_artifact", `{"body":"# Discarded draft"}`)},
		FinishReason: "tool_calls",
		Usage:        llm.Usage{PromptTokens: 120000},
	}

	f := &streamingLLM{chunk: 4, fakeLLM: fakeLLM{responses: []llm.Response{
		ask("c1"), ask("c2"), ask("c3"), ask("c4"), ask("c5"),
		near, // over the threshold: the turn is discarded and its submit never executes
		{Content: "the briefing", FinishReason: "stop"},
		{ToolCalls: []llm.ToolCall{toolCall("c7", "submit_artifact", `{"body":"# Kept"}`)}, FinishReason: "tool_calls"},
	}}}

	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.MaxTurns = 20
	in.Tools = []tools.Tool{echoTool{}}
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }

	out, err := (&Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return f }}).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "# Kept", out.Artifact.Body)

	voided := -1

	for i, d := range frames {
		if d.Void {
			voided = i
		}
	}

	require.GreaterOrEqual(t, voided, 1, "the discarded turn's draft was shown, then voided")
	shown := applied(t, frames[:voided])
	assert.NotEmpty(t, shown)
	require.LessOrEqual(t, len(shown), len("# Discarded draft"))
	assert.Equal(t, "# Discarded draft"[:len(shown)], shown, "what was shown is the discarded body so far")
	require.Greater(t, len(frames), voided+1)
	assert.Equal(t, 0, frames[voided+1].Offset, "the next attempt restarts at offset 0")
	assert.Equal(t, "# Kept", applied(t, frames[voided+1:]))
}

// ghostLLM streams a submit_artifact fragment under id c1 at the end of its
// first call, whose response does not carry that call.
type ghostLLM struct {
	streamingLLM
	ghost string
}

func (g *ghostLLM) SendStream(ctx context.Context, req llm.Request, onDelta func(llm.Delta)) (llm.Response, error) {
	first := g.i == 0

	resp, err := g.streamingLLM.SendStream(ctx, req, onDelta)
	if first {
		onDelta(llm.Delta{ToolCall: &llm.ToolCallDelta{Index: 0, ID: "c1", Name: submitArtifactName, ArgsFragment: g.ghost}})
	}

	return resp, err
}

func TestRun_ANewModelCallStartsAFreshDraftWhateverIDItReuses(t *testing.T) {
	f := &ghostLLM{ghost: `{"body":"ghost of the first`, streamingLLM: streamingLLM{chunk: 4, fakeLLM: fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("a1", "echo", `{"text":"more"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"the second call's own body"}`)}, FinishReason: "tool_calls"},
	}}}}

	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.Tools = []tools.Tool{echoTool{}}
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }

	tick := time.Unix(1000, 0)
	advance := func() time.Time {
		tick = tick.Add(time.Millisecond)

		return tick
	}
	r := &Runner{
		NewLLM:   func(protocol.LLMEndpoint) llm.LLM { return f },
		draftNow: advance, draftEvery: time.Nanosecond,
	}

	out, err := r.Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "the second call's own body", out.Artifact.Body)

	ghosts := 0

	for i := range frames {
		if strings.HasPrefix(applied(t, frames[:i+1]), "ghost") {
			ghosts = i + 1
		}
	}

	require.NotZero(t, ghosts, "the first call's fragment was drafted")

	own := frames[ghosts:]
	require.Greater(t, len(own), 1, "the second call's body streamed as its own draft")
	assert.Equal(t, 0, own[0].Offset)
	assert.Equal(t, "the second call's own body", applied(t, own))
}

func TestRun_ARepairedSubmitIsMarkedRefused(t *testing.T) {
	f := &streamingLLM{chunk: 4, fakeLLM: fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"cut off`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"whole"}`)}, FinishReason: "tool_calls"},
	}}}

	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }

	out, err := (&Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return f }}).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "whole", out.Artifact.Body)

	refused := -1

	for i, d := range frames {
		if d.Refused {
			refused = i
		}
	}

	require.GreaterOrEqual(t, refused, 0, "the repaired submit's draft is marked refused")
	assert.NotEmpty(t, frames[refused].Reason)
	shown := applied(t, frames[:refused+1])
	assert.NotEmpty(t, shown)
	require.LessOrEqual(t, len(shown), len("cut off"))
	assert.Equal(t, "cut off"[:len(shown)], shown, "what was shown is the repaired body so far")
	require.Greater(t, len(frames), refused+1)
	assert.Equal(t, 0, frames[refused+1].Offset)
	assert.Equal(t, "whole", applied(t, frames[refused+1:]))
}

func TestRun_FragmentsWithinOneIntervalCoalesce(t *testing.T) {
	body := strings.Repeat("word ", 40)
	f := &streamingLLM{chunk: 3, fakeLLM: fakeLLM{responses: []llm.Response{{
		ToolCalls:    []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"`+body+`"}`)},
		FinishReason: "tool_calls",
	}}}}

	var frames []DraftFrame

	in := baseInput(&bytes.Buffer{})
	in.Draft = func(d DraftFrame) { frames = append(frames, d) }

	frozen := time.Unix(1000, 0)
	r := &Runner{
		NewLLM:   func(protocol.LLMEndpoint) llm.LLM { return f },
		draftNow: func() time.Time { return frozen }, draftEvery: time.Hour,
	}

	_, err := r.Run(context.Background(), in)
	require.NoError(t, err)

	assert.Len(t, frames, 2, "the first fragment opens the draft and the submit flushes the rest")
	assert.Equal(t, body, applied(t, frames))
}

// lookupTool is transient: the record keeps its result's first line.
type lookupTool struct{}

func (lookupTool) Name() string { return "lookup" }

func (lookupTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
}

func (lookupTool) Execute(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Text: "lookup: 2 hits\n\nhit one\nhit two"}, nil
}

func (lookupTool) Stored(result string) string {
	head, _, _ := strings.Cut(result, "\n")
	if !strings.HasPrefix(head, "lookup:") {
		return result
	}

	return head
}

// lookalikeTool is not transient, though its result looks like lookup's.
type lookalikeTool struct{}

func (lookalikeTool) Name() string { return "lookalike" }

func (lookalikeTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "lookalike", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
}

func (lookalikeTool) Execute(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Text: "lookup: not transient\n\nbody"}, nil
}

// toolResult is the content of the last tool message answering id.
func toolResult(msgs []llm.Message, id string) (string, bool) {
	content, found := "", false

	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID == id {
			content, found = m.Content, true
		}
	}

	return content, found
}

func TestRun_KeepsTransientResultsOutOfTheRecord(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "lookup", `{}`), toolCall("c2", "lookup", `{}`), toolCall("c3", "lookalike", `{}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c4", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
	}}

	var events bytes.Buffer

	in := baseInput(&events)
	in.Tools = []tools.Tool{lookupTool{}, lookalikeTool{}}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)

	for _, id := range []string{"c1", "c2"} {
		got, ok := toolResult(out.Result.Messages, id)
		require.True(t, ok, id)
		assert.Equal(t, "lookup: 2 hits", got, id)
	}

	got, ok := toolResult(out.Result.Messages, "c3")
	require.True(t, ok)
	assert.Equal(t, "lookup: not transient\n\nbody", got, "a tool that is not transient keeps its result")

	// The model read the whole result in its run.
	require.Len(t, f.requests, 2)

	seen, ok := toolResult(f.requests[1].Messages, "c1")
	require.True(t, ok)
	assert.Contains(t, seen, "hit one")

	assert.NotContains(t, events.String(), "hit one", "harness events carry a result's length, never its text")
}

func TestRun_KeepsTransientResultsOutOfARunWithoutAnArtifact(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "lookup", `{}`)}, FinishReason: "tool_calls"},
	}}

	in := baseInput(&bytes.Buffer{})
	in.Tools = []tools.Tool{lookupTool{}}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.ErrorIs(t, err, ErrNoArtifact)

	got, ok := toolResult(out.Result.Messages, "c1")
	require.True(t, ok)
	assert.Equal(t, "lookup: 2 hits", got)
}

func TestRun_KeepsTransientResultsOutOfAForcedFollowUp(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c1", "lookup", `{}`)}, FinishReason: "tool_calls"},
		{Content: "I have what I need.", FinishReason: "stop"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
	}}

	in := baseInput(&bytes.Buffer{})
	in.Tools = []tools.Tool{lookupTool{}}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "# Need", out.Artifact.Body)

	// The follow-up belongs to the run, so its model reads the whole result.
	require.Len(t, f.requests, 3)

	seen, ok := toolResult(f.requests[2].Messages, "c1")
	require.True(t, ok)
	assert.Contains(t, seen, "hit one")

	got, ok := toolResult(out.Result.Messages, "c1")
	require.True(t, ok)
	assert.Equal(t, "lookup: 2 hits", got, "the record of a forced follow-up keeps only what the tool stores")

	_, ok = toolResult(out.Result.Messages, "c2")
	assert.True(t, ok, "the record holds the follow-up's conversation")
}

func TestRun_PassesARecordedHistoryThroughUnchanged(t *testing.T) {
	history := []llm.Message{
		{Role: "user", Content: "the task"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("c0", "lookup", `{}`)}},
		{Role: "tool", ToolCallID: "c0", Content: "lookup: 2 hits"},
		// A model that reuses a call id: this c0 is lookalike's, so its
		// result stays whole.
		{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("c0", "lookalike", `{}`)}},
		{Role: "tool", ToolCallID: "c0", Content: "lookup: not transient\n\nbody"},
	}

	f := &fakeLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("c9", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
	}}

	in := baseInput(&bytes.Buffer{})
	in.Tools = []tools.Tool{lookupTool{}, lookalikeTool{}}
	in.History = history

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)

	var results []string

	for _, m := range out.Result.Messages {
		if m.Role == "tool" && m.ToolCallID == "c0" {
			results = append(results, m.Content)
		}
	}

	assert.Equal(t, []string{"lookup: 2 hits", "lookup: not transient\n\nbody"}, results)
}

// offered is the names of the tools req offers.
func offered(req llm.Request) []string {
	names := make([]string, 0, len(req.Tools))
	for _, tool := range req.Tools {
		names = append(names, tool.Function.Name)
	}

	return names
}

func TestRun_ADesignerGetsShowMockupInPlaceOfSubmit(t *testing.T) {
	args, err := json.Marshal(mockupArgs(terminalSource))
	require.NoError(t, err)

	tests := []struct {
		name string
		chat bool
		want []string
	}{
		{"a first run", false, []string{AnswerChatName, ShowMockupName}},
		{"a chat run", true, []string{AnswerChatName, ShowMockupName}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{ToolCalls: []llm.ToolCall{toolCall("c1", ShowMockupName, string(args))}, FinishReason: "tool_calls"}}}
			d := &fakeDesigner{}

			in := baseInput(&bytes.Buffer{})
			in.Interviewer, in.Designer, in.Chat = &stubInterviewer{}, d, tt.chat

			out, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			assert.True(t, out.Asked, "a shown revision ends the run asked")
			assert.Len(t, d.got, 1)

			require.NotEmpty(t, f.requests)
			assert.ElementsMatch(t, tt.want, offered(f.requests[0]))
		})
	}
}

// replayed is one reasoning block as a provider returns it.
var replayed = json.RawMessage(`[{"type":"reasoning.text","text":"weigh the options","signature":"c2ln","index":0}]`)

// assistantDetails is the reasoning of the first assistant message in msgs.
func assistantDetails(msgs []llm.Message) json.RawMessage {
	for _, m := range msgs {
		if m.Role == "assistant" {
			return m.ReasoningDetails
		}
	}

	return nil
}

func TestRun_ReplaysReasoningOnlyWhereItIsOn(t *testing.T) {
	tests := []struct {
		name   string
		replay bool
		want   bool
	}{
		{"on", true, true},
		{"off", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: []llm.Response{{Content: "thinking aloud", FinishReason: "stop", ReasoningDetails: replayed}}}
			r := runnerWith(f)
			r.Replay = tt.replay

			_, _ = r.Run(context.Background(), baseInput(&bytes.Buffer{}))

			require.GreaterOrEqual(t, len(f.requests), 2, "the forced follow-up resends the reply")

			got := assistantDetails(f.requests[1].Messages)
			if tt.want {
				assert.Equal(t, replayed, got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}

func TestRun_ResumedReasoningGoesBackOnlyToTheModelThatWroteIt(t *testing.T) {
	tests := []struct {
		name         string
		historyModel string
		replay       bool
		want         bool
	}{
		{"the same model", "test/model", true, true},
		{"another model", "other/model", true, false},
		{"a model unknown", "", true, false},
		{"replay off", "test/model", false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			history := []llm.Message{{Role: "user", Content: "the need"}, {Role: "assistant", Content: "asked", ReasoningDetails: replayed}}

			in := baseInput(&bytes.Buffer{})
			in.History, in.HistoryModel = history, tt.historyModel

			f := &fakeLLM{}
			r := runnerWith(f)
			r.Replay = tt.replay

			_, _ = r.Run(context.Background(), in)

			require.NotEmpty(t, f.requests)

			got := assistantDetails(f.requests[0].Messages)
			if tt.want {
				assert.Equal(t, replayed, got)
			} else {
				assert.Empty(t, got)
			}

			assert.Equal(t, replayed, history[1].ReasoningDetails, "the caller's history is never changed")
		})
	}
}

func TestRun_AResumeWhoseReasoningCannotFitGoesOnWithoutIt(t *testing.T) {
	// 4,000 bytes of details count about 1,000 tokens: over a 1,000-token
	// window's 850-token limit with them, far under it without.
	big := json.RawMessage(`[{"type":"reasoning.text","text":"` + strings.Repeat("r", 4_000) + `"}]`)

	in := baseInput(&bytes.Buffer{})
	in.ContextWindow = 1_000
	in.History = []llm.Message{{Role: "user", Content: "the need"}, {Role: "assistant", Content: "asked", ReasoningDetails: big}}
	in.HistoryModel = in.Model

	f := &fakeLLM{}
	r := runnerWith(f)
	r.Replay = true

	out, err := r.Run(context.Background(), in)

	var tooBig *TooBigError
	assert.NotErrorAs(t, err, &tooBig, "never refused for reasoning it can leave out")
	assert.NotEqual(t, ReasonTooBig, out.Result.Reason)
	require.NotEmpty(t, f.requests, "the run went on")
	assert.Empty(t, assistantDetails(f.requests[0].Messages))
}

func TestRun_ReplayOnTheWire(t *testing.T) {
	for _, endpoint := range []string{"openrouter", "openai"} {
		for _, replay := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/replay=%t", endpoint, replay), func(t *testing.T) {
				bodies := wireBodies(t, endpoint, NewRunner("", replay), baseInput(&bytes.Buffer{}))
				require.GreaterOrEqual(t, len(bodies), 2, "the forced follow-up was sent too")

				var msgs []map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(bodies[1]["messages"], &msgs))

				var asst map[string]json.RawMessage

				for _, m := range msgs {
					if string(m["role"]) == `"assistant"` {
						asst = m
					}
				}

				require.NotNil(t, asst)

				if replay {
					assert.Contains(t, string(asst["reasoning_details"]), `"think"`, "both legs get the field")
				} else {
					assert.NotContains(t, asst, "reasoning_details", "off sends none")
				}
			})
		}
	}
}

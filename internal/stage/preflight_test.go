package stage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noCallLLM fails the test on any model call.
type noCallLLM struct{ t *testing.T }

func (n noCallLLM) Send(context.Context, llm.Request) (llm.Response, error) {
	n.t.Error("a refused run calls no model")

	return llm.Response{}, nil
}

func (n noCallLLM) SendStream(context.Context, llm.Request, func(llm.Delta)) (llm.Response, error) {
	n.t.Error("a refused run calls no model")

	return llm.Response{}, nil
}

func TestRun_ATaskThatFillsTheWindowIsRefusedWithoutAModelCall(t *testing.T) {
	r := &Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return noCallLLM{t: t} }}

	var events bytes.Buffer

	in := baseInput(&events)
	in.ContextWindow = 10_000
	in.Task = strings.Repeat("x", 25_000) // 10,000 tokens at 2.5 bytes each; the limit is 8,500

	out, err := r.Run(context.Background(), in)

	var big *TooBigError
	require.ErrorAs(t, err, &big)
	assert.Equal(t, 10_000, big.Window)
	assert.Equal(t, 8_500, big.Limit)
	assert.GreaterOrEqual(t, big.Estimate, big.Limit)
	assert.Equal(t, ReasonTooBig, out.Result.Reason)
}

func TestPreflight(t *testing.T) {
	messages := func(n, size int) []llm.Message {
		out := make([]llm.Message, 0, n)
		for range n {
			out = append(out, llm.Message{Role: "user", Content: strings.Repeat("y", size)})
		}

		return out
	}

	// toolGroup is an assistant tool call carrying size bytes of arguments
	// and its tool result.
	toolGroup := func(size int) []llm.Message {
		return []llm.Message{
			{Role: "assistant", ToolCalls: []llm.ToolCall{toolCall("g", "echo", strings.Repeat("a", size))}},
			{Role: "tool", ToolCallID: "g", Content: "ok"},
		}
	}
	join := func(parts ...[]llm.Message) []llm.Message {
		var out []llm.Message
		for _, p := range parts {
			out = append(out, p...)
		}

		return out
	}

	// A 30,000-token window has a compaction limit of 25,500 tokens (63,750
	// bytes) and holds 75,000 bytes.
	tests := []struct {
		name      string
		task      string
		history   []llm.Message
		refused   bool
		wantLimit int
	}{
		{"a task under the limit runs", strings.Repeat("x", 60_000), nil, false, 0},
		{"a task at the limit is refused", strings.Repeat("x", 64_000), nil, true, 25_500},
		{"a long history that fits the window runs: compaction summarizes all but its newest messages", "go on", messages(10, 7_000), false, 0},
		{"a history over the window is refused: the first call carries all of it", "go on", messages(10, 10_000), true, 30_000},
		{"a short history compaction cannot shrink counts whole", "go on", messages(4, 17_500), true, 25_500},
		// The sixth-newest message (counting the task) is a tool result, so the
		// boundary snaps back over it to the 70,000-byte tool call: the newest
		// five alone are tiny.
		{"a boundary inside tool results snaps back to the call that issued them", "go on", join(messages(3, 10), toolGroup(70_000), messages(4, 10)), true, 25_500},
		{"a history too short to summarize counts whole, its tool calls included", "go on", join(toolGroup(70_000), messages(4, 10)), true, 25_500},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := preflight("You are the PO.\n\nProduce the need.", tt.task, tt.history, 30_000)

			var big *TooBigError

			require.Equal(t, tt.refused, errors.As(err, &big))

			if tt.refused {
				assert.Equal(t, tt.wantLimit, big.Limit)
				assert.Equal(t, 30_000, big.Window)
				assert.GreaterOrEqual(t, big.Estimate, big.Limit)
			}
		})
	}
}

func TestRun_TheModelsWindowSizesCompaction(t *testing.T) {
	tests := []struct {
		name     string
		window   int
		compacts bool
	}{
		{"an unknown window is 128000, and 120000 tokens is past 85% of it", 0, true},
		{"a negative window is unknown too", -5, true},
		{"a window of a million leaves 120000 tokens far below its limit", 1_000_000, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ask := func(id string) llm.Response {
				return llm.Response{ToolCalls: []llm.ToolCall{toolCall(id, "echo", `{"text":"more"}`)}, FinishReason: "tool_calls"}
			}

			near := ask("c6")
			near.Usage = llm.Usage{PromptTokens: 120000}

			f := &fakeLLM{responses: []llm.Response{
				ask("c1"), ask("c2"), ask("c3"), ask("c4"), ask("c5"), near,
				{Content: "the briefing", FinishReason: "stop"},
				{ToolCalls: []llm.ToolCall{toolCall("c7", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
			}}

			var events bytes.Buffer

			in := baseInput(&events)
			in.MaxTurns = 20
			in.Tools = []tools.Tool{echoTool{}}
			in.ContextWindow = tt.window

			_, err := runnerWith(f).Run(context.Background(), in)
			require.NoError(t, err)
			assert.Equal(t, tt.compacts, strings.Contains(events.String(), `"phase":"compaction"`))
		})
	}
}

func TestRun_AFullWindowStopIsAContextFullError(t *testing.T) {
	first := llm.Response{
		ToolCalls: []llm.ToolCall{toolCall("c1", "echo", `{"text":"x"}`)}, FinishReason: "tool_calls",
		Usage: llm.Usage{PromptTokens: 120_000},
	}
	f := &fakeLLM{responses: []llm.Response{first}}

	var events bytes.Buffer

	in := baseInput(&events)
	in.Tools = []tools.Tool{echoTool{}}

	// Two messages leave compaction nothing to summarize, so the harness stops.
	out, err := runnerWith(f).Run(context.Background(), in)

	var full *ContextFullError
	require.ErrorAs(t, err, &full)
	assert.Equal(t, 128_000, full.Window)
	assert.Equal(t, 108_800, full.Limit)
	require.ErrorIs(t, err, ErrNoArtifact, "a full window is still a run without an artifact")
	assert.Equal(t, ReasonContextLimit, out.Result.Reason)
}

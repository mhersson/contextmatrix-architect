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

// finals is FinalField on each model_response line, in order.
func finals(t *testing.T, events string) []bool {
	t.Helper()

	var out []bool

	for line := range strings.Lines(events) {
		var ev struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		}

		require.NoError(t, json.Unmarshal([]byte(line), &ev))

		if ev.Kind == "model_response" {
			final, _ := ev.Data[FinalField].(bool)
			out = append(out, final)
		}
	}

	return out
}

func TestRun_TheReplyARunEndsOnIsMarkedFinal(t *testing.T) {
	submit := toolCall("s1", "submit_artifact", `{"body":"# Need"}`)
	echo := toolCall("e1", "echo", `{"text":"x"}`)
	refused := toolCall("s0", "submit_artifact", `{"body":"bad"}`)

	tests := []struct {
		name      string
		responses []llm.Response
		want      []bool
	}{
		{
			"a reply that submits",
			[]llm.Response{{Content: "# Need\n\nthe whole artifact again", ToolCalls: []llm.ToolCall{submit}, FinishReason: "tool_calls"}},
			[]bool{true},
		},
		{
			"a reply that calls another tool, then one that submits",
			[]llm.Response{
				{Content: "Reading the code first.", ToolCalls: []llm.ToolCall{echo}, FinishReason: "tool_calls"},
				{Content: "# Need\n\nthe whole artifact again", ToolCalls: []llm.ToolCall{submit}, FinishReason: "tool_calls"},
			},
			[]bool{false, true},
		},
		{
			"a reply with no tool call, which the forced submit follows",
			[]llm.Response{
				{Content: "# Need\n\nthe whole artifact again", FinishReason: "stop"},
				{ToolCalls: []llm.ToolCall{submit}, FinishReason: "tool_calls"},
			},
			[]bool{true, true},
		},
		{
			"a refused submit, then one that is accepted",
			[]llm.Response{
				{Content: "# Need\n\nthe whole artifact again", ToolCalls: []llm.ToolCall{refused}, FinishReason: "tool_calls"},
				{Content: "# Need\n\nthe whole artifact again", ToolCalls: []llm.ToolCall{submit}, FinishReason: "tool_calls"},
			},
			[]bool{true, true},
		},
		{
			"a reply beside a submit and another tool",
			[]llm.Response{{Content: "# Need\n\nthe whole artifact again", ToolCalls: []llm.ToolCall{echo, submit}, FinishReason: "tool_calls"}},
			[]bool{true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events bytes.Buffer

			in := baseInput(&events)
			in.Tools = []tools.Tool{echoTool{}}
			in.Validate = func(_ context.Context, a Artifact) error {
				if a.Body == "bad" {
					return errors.New("not yet")
				}

				return nil
			}

			_, err := runnerWith(&fakeLLM{responses: tt.responses}).Run(context.Background(), in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, finals(t, events.String()))
			assert.Contains(t, events.String(), "the whole artifact again", "the record keeps the reply")
		})
	}
}

func TestRun_ACompactionBetweenRepliesLeavesTheNextMarkItsOwn(t *testing.T) {
	ask := func(id string) llm.Response {
		return llm.Response{ToolCalls: []llm.ToolCall{toolCall(id, "echo", `{"text":"more"}`)}, FinishReason: "tool_calls"}
	}

	near := ask("c6")
	near.Usage = llm.Usage{PromptTokens: 120000}

	f := &fakeLLM{responses: []llm.Response{
		ask("c1"), ask("c2"), ask("c3"), ask("c4"), ask("c5"), near,
		{Content: "the briefing", FinishReason: "stop"},
		ask("c7"),
		{ToolCalls: []llm.ToolCall{toolCall("c8", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
	}}

	var events bytes.Buffer

	in := baseInput(&events)
	in.MaxTurns = 20
	in.Tools = []tools.Tool{echoTool{}}

	_, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	require.Contains(t, events.String(), `"phase":"compaction"`, "the run compacted")

	marks := finals(t, events.String())
	require.NotEmpty(t, marks)
	assert.True(t, marks[len(marks)-1], "the submit is final")
	assert.NotContains(t, marks[:len(marks)-1], true, "no echo reply is final, the one after the compaction included")
}

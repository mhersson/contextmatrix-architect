package stage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func toolNames(ts []llm.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Function.Name)
	}

	return out
}

func roles(msgs []llm.Message, role string) int {
	n := 0

	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}

	return n
}

func TestRun_TextOnlyStopIsForcedToSubmit(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{Content: "Score 4: it passes.", FinishReason: "stop", Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 10, Cost: 0.01}},
		{
			ToolCalls:    []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"# Review","data":{"score":4}}`)},
			FinishReason: "tool_calls", Usage: llm.Usage{PromptTokens: 200, CompletionTokens: 20, Cost: 0.02},
		},
	}}

	out, err := runnerWith(f).Run(context.Background(), baseInput(&bytes.Buffer{}))
	require.NoError(t, err)
	assert.Equal(t, "# Review", out.Artifact.Body)

	require.Len(t, f.requests, 2)
	assert.Nil(t, f.requests[0].ToolChoice, "the run itself is never forced")
	assert.JSONEq(t, `{"type":"function","function":{"name":"submit_artifact"}}`, string(f.requests[1].ToolChoice))
	assert.Equal(t, []string{"submit_artifact"}, toolNames(f.requests[1].Tools), "the follow-up offers only the terminal tools")

	// The follow-up continues the conversation under one system message, and
	// the stored conversation ends with the forced call's result.
	assert.Equal(t, 1, roles(f.requests[1].Messages, "system"))
	last := out.Result.Messages[len(out.Result.Messages)-1]
	assert.Equal(t, "tool", last.Role)
	assert.Equal(t, "c1", last.ToolCallID)

	// Both runs count.
	assert.InDelta(t, 0.03, out.Result.TotalCostUSD, 1e-9)
	assert.Equal(t, int64(300), out.Result.PromptTokens)
	assert.Equal(t, int64(30), out.Result.CompletionTokens)
}

func TestRun_ForcedFollowUpThatStillTalksFails(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{Content: "Here is my review.", FinishReason: "stop"},
		{Content: "Still prose.", FinishReason: "stop"},
	}}

	_, err := runnerWith(f).Run(context.Background(), baseInput(&bytes.Buffer{}))
	require.ErrorIs(t, err, ErrNoArtifact)
	assert.Len(t, f.requests, 2, "one follow-up, never a loop")
}

func TestRun_ForcedFollowUpRetriesARefusedSubmit(t *testing.T) {
	f := &fakeLLM{responses: []llm.Response{
		{Content: "Done thinking.", FinishReason: "stop"},
		{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"bad"}`)}, FinishReason: "tool_calls"},
		{ToolCalls: []llm.ToolCall{toolCall("c2", "submit_artifact", `{"body":"good"}`)}, FinishReason: "tool_calls"},
	}}

	in := baseInput(&bytes.Buffer{})
	in.Validate = func(_ context.Context, a Artifact) error {
		if a.Body == "bad" {
			return errors.New("not yet")
		}

		return nil
	}

	out, err := runnerWith(f).Run(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "good", out.Artifact.Body)
	assert.Len(t, f.requests, 3)
}

// choiceRefusingLLM fails a forced call the way a gateway that rejects
// tool_choice does, then serves the script.
type choiceRefusingLLM struct{ fakeLLM }

func (c *choiceRefusingLLM) SendStream(ctx context.Context, req llm.Request, d func(llm.Delta)) (llm.Response, error) {
	if req.ToolChoice != nil {
		c.requests = append(c.requests, req)

		return llm.Response{}, errors.New("llm endpoint status 400: tool_choice is not supported")
	}

	return c.fakeLLM.SendStream(ctx, req, d)
}

func TestRun_ForcedChoiceRefusedIsSentOnceMoreWithoutIt(t *testing.T) {
	f := &choiceRefusingLLM{fakeLLM{responses: []llm.Response{
		{Content: "Done.", FinishReason: "stop"},
		{ToolCalls: []llm.ToolCall{toolCall("c1", "submit_artifact", `{"body":"# Need"}`)}, FinishReason: "tool_calls"},
	}}}

	r := &Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return f }}

	out, err := r.Run(context.Background(), baseInput(&bytes.Buffer{}))
	require.NoError(t, err)
	assert.Equal(t, "# Need", out.Artifact.Body)
	require.Len(t, f.requests, 3)
	assert.NotNil(t, f.requests[1].ToolChoice)
	assert.Nil(t, f.requests[2].ToolChoice)
}

func TestRun_AForcedFollowUpWithoutAnAcceptedCallSaysWhy(t *testing.T) {
	const malformed = `{"body":"x"`

	var m map[string]any

	decodeErr := json.Unmarshal([]byte(`{"body":`), &m)
	require.Error(t, decodeErr)

	text := llm.Response{Content: "Here is my review.", FinishReason: "stop", Usage: llm.Usage{Cost: 0.01}}
	submit := func(id, args string) llm.Response {
		return llm.Response{
			ToolCalls:    []llm.ToolCall{toolCall(id, "submit_artifact", args)},
			FinishReason: "tool_calls", Usage: llm.Usage{Cost: 0.01},
		}
	}

	tests := []struct {
		name       string
		responses  []llm.Response
		maxCost    float64
		validate   func(context.Context, Artifact) error
		wantReason string
		wantRefuse bool
		wantLast   string
		lastIsPart bool // the submit tool prefixes the validator's error
		wantReqs   int
	}{
		{
			name:       "two malformed submits",
			responses:  []llm.Response{text, submit("c1", malformed), submit("c2", `{"body":`)},
			wantReason: ReasonTerminalRefused, wantRefuse: true,
			wantLast: "its arguments were not valid JSON: " + decodeErr.Error(),
			wantReqs: 3,
		},
		{
			name:      "a malformed submit then a validator refusal",
			responses: []llm.Response{text, submit("c1", malformed), submit("c2", `{"body":"x"}`)},
			validate: func(context.Context, Artifact) error {
				return errors.New("cards missing")
			},
			wantReason: ReasonTerminalRefused, wantRefuse: true,
			wantLast: "cards missing", lastIsPart: true,
			wantReqs: 3,
		},
		{
			name:      "both turns reuse one call id",
			responses: []llm.Response{text, submit("c1", `{"body":"one"}`), submit("c1", `{"body":"two"}`)},
			validate: func(_ context.Context, a Artifact) error {
				if a.Body == "one" {
					return errors.New("first")
				}

				return errors.New("second")
			},
			wantReason: ReasonTerminalRefused, wantRefuse: true,
			wantLast: "second", lastIsPart: true,
			wantReqs: 3,
		},
		{
			name:       "a text reply",
			responses:  []llm.Response{text, {Content: "Still prose.", FinishReason: "stop"}},
			wantReason: ReasonTerminalRefused, wantRefuse: true,
			wantLast: "",
			wantReqs: 2,
		},
		{
			name:       "the run cap stops the follow-up",
			responses:  []llm.Response{text, submit("c1", malformed), submit("c2", malformed)},
			maxCost:    0.015,
			wantReason: "max_cost",
			wantReqs:   2,
		},
		{
			name:       "a first run that spends the whole cap",
			responses:  []llm.Response{text, submit("c1", malformed)},
			maxCost:    0.01,
			wantReason: "max_cost",
			wantReqs:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: tt.responses}
			in := baseInput(&bytes.Buffer{})
			in.Validate = tt.validate

			if tt.maxCost > 0 {
				in.MaxCostUSD = tt.maxCost
			}

			out, err := runnerWith(f).Run(context.Background(), in)
			require.ErrorIs(t, err, ErrNoArtifact)
			assert.Equal(t, tt.wantReason, out.Result.Reason)
			assert.Len(t, f.requests, tt.wantReqs)

			var refused *RefusedError

			if !tt.wantRefuse {
				assert.NotErrorAs(t, err, &refused)

				return
			}

			require.ErrorAs(t, err, &refused)

			if tt.lastIsPart {
				assert.Contains(t, refused.Last, tt.wantLast)
			} else {
				assert.Equal(t, tt.wantLast, refused.Last)
			}

			assert.Equal(t, []string{"submit_artifact"}, refused.Tools)
		})
	}
}

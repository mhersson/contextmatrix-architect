package stage

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_AFatalRefusalEndsTheRunOnItsCall(t *testing.T) {
	const fatal = "slicing: no_slices: slicing needs at least one slice"

	submit := func(id string) llm.Response {
		return llm.Response{ToolCalls: []llm.ToolCall{toolCall(id, "submit_artifact", `{"body":"# Slices"}`)}, FinishReason: "tool_calls"}
	}

	tests := []struct {
		name      string
		responses []llm.Response
		// fatalFrom is the submit the validator refuses for good; earlier
		// ones it refuses as any check does.
		fatalFrom int
		calls     int
	}{
		{name: "the third submit", responses: []llm.Response{submit("c1"), submit("c2"), submit("c3")}, fatalFrom: 3, calls: 3},
		{name: "a forced follow-up's submit", responses: []llm.Response{{Content: "Here are the slices.", FinishReason: "stop"}, submit("c1")}, fatalFrom: 1, calls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeLLM{responses: tt.responses}
			submits := 0

			in := baseInput(&bytes.Buffer{})
			in.Validate = func(context.Context, Artifact) error {
				submits++

				if submits < tt.fatalFrom {
					return errors.New(fatal)
				}

				return &FatalCheckError{Problems: []string{fatal}}
			}

			out, err := runnerWith(f).Run(context.Background(), in)

			var refused *FatalCheckError
			require.ErrorAs(t, err, &refused)
			assert.Equal(t, []string{fatal}, refused.Problems)
			assert.Equal(t, ReasonFatalCheck, out.Result.Reason)
			assert.Empty(t, out.Artifact.Body, "nothing is submitted")
			assert.Len(t, f.requests, tt.calls, "the refusal ends the run on its call")

			last := out.Result.Messages[len(out.Result.Messages)-1]
			assert.Equal(t, "tool", last.Role)
			assert.Contains(t, last.Content, fatal, "the transcript keeps the refusal as the call's result")
		})
	}
}

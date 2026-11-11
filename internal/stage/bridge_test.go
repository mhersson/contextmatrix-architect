package stage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/redact"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubTool struct {
	name string
	res  tools.Result
	err  error
}

func (s stubTool) Name() string     { return s.name }
func (s stubTool) Schema() llm.Tool { return llm.Tool{} }

func (s stubTool) Execute(context.Context, map[string]any) (tools.Result, error) { return s.res, s.err }

type roStub struct{ stubTool }

func (roStub) ReadOnly() bool { return true }

func TestBridgedRedactsResultAndError(t *testing.T) {
	apply := redact.New([]string{"s3cr3t"}).Apply

	res, err := bridged(stubTool{name: "a", res: tools.Result{Text: "key=s3cr3t"}}, apply).Execute(t.Context(), nil)
	require.NoError(t, err)
	assert.NotContains(t, res.Text, "s3cr3t")

	_, err = bridged(stubTool{name: "b", err: errors.New("bad s3cr3t")}, apply).Execute(t.Context(), nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cr3t")

	orig := fmt.Errorf("wrapped: %w", context.Canceled)
	_, err = bridged(stubTool{name: "c", err: orig}, apply).Execute(t.Context(), nil)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestBridgedCapsOutput(t *testing.T) {
	text := strings.Repeat("x", 200<<10)

	res, err := bridged(stubTool{name: "a", res: tools.Result{Text: text}}, func(s string) string { return s }).Execute(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, tools.HeadTail(text, ToolOutputMaxBytes), res.Text)
}

func TestBridgedKeepsMarkers(t *testing.T) {
	id := func(s string) string { return s }
	submit := bridged(submitArtifactTool{}, id)
	off := bridged(offMode{Tool: submitArtifactTool{}}, id)
	ro := bridged(tagWatch{Tool: roStub{stubTool{name: "r"}}, tags: &returnedTags{}}, id)

	_ = tools.NewRegistry(submit, off, ro)
	assert.True(t, isTerminal(submit))
	assert.False(t, isTerminal(off))

	_, isRO := ro.(tools.ReadOnly)
	assert.True(t, isRO)
	assert.True(t, ro.(tools.ReadOnly).ReadOnly())
}

func isTerminal(t tools.Tool) bool {
	term, ok := t.(tools.Terminal)

	return ok && term.Terminal()
}

func TestCallWatchFinal(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  map[bool]bool // submits -> final
	}{
		{"none", nil, map[bool]bool{true: true, false: true}},
		{"ask", []string{"ask_questions"}, map[bool]bool{true: false, false: false}},
		{"submit", []string{"submit_artifact"}, map[bool]bool{true: true, false: false}},
		{"read then submit", []string{"read_file", "submit_artifact"}, map[bool]bool{true: true, false: false}},
	}

	for _, tt := range tests {
		for submits, want := range tt.want {
			t.Run(fmt.Sprintf("%s/submits=%v", tt.name, submits), func(t *testing.T) {
				w := &callWatch{w: &bytes.Buffer{}, submits: submits}
				for _, n := range tt.names {
					w.delta(llm.Delta{ToolCall: &llm.ToolCallDelta{Name: n}})
				}

				assert.Equal(t, want, w.final())
			})
		}
	}

	t.Run("model_request resets", func(t *testing.T) {
		var buf bytes.Buffer

		w := &callWatch{w: &buf, submits: true}
		w.delta(llm.Delta{ToolCall: &llm.ToolCallDelta{Name: "ask_questions"}})
		require.False(t, w.final())

		_, err := w.Write([]byte(`{"kind":"model_request","data":{}}` + "\n"))
		require.NoError(t, err)
		assert.True(t, w.final())
		assert.Contains(t, buf.String(), "model_request")
	})
}

package stage

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubAsker struct {
	got    Question
	answer string
	err    error
}

func (s *stubAsker) Ask(_ context.Context, q Question) (string, error) {
	s.got = q

	return s.answer, s.err
}

func TestAskHumanTool_ReturnsTheAnswerAsToolResult(t *testing.T) {
	asker := &stubAsker{answer: "helm"}
	tool := askHumanTool{asker: asker}

	res, err := tool.Execute(context.Background(), map[string]any{
		"question": "Helm or kustomize?",
		"options":  []any{"helm", "kustomize"},
		"context":  "both exist in the org",
	})
	require.NoError(t, err)
	assert.Equal(t, "helm", res.Text)
	assert.Equal(t, Question{Text: "Helm or kustomize?", Options: []string{"helm", "kustomize"}, Context: "both exist in the org"}, asker.got)
	assert.Equal(t, "ask_human", tool.Name())
}

func TestAskHumanTool_EmptyQuestionIsAnError(t *testing.T) {
	tool := askHumanTool{asker: &stubAsker{}}

	_, err := tool.Execute(context.Background(), map[string]any{"question": "  "})
	require.Error(t, err)
}

func TestAskHumanTool_PropagatesAskerError(t *testing.T) {
	want := errors.New("session ended")
	tool := askHumanTool{asker: &stubAsker{err: want}}

	_, err := tool.Execute(context.Background(), map[string]any{"question": "q"})
	require.ErrorIs(t, err, want)
}

func TestSubmitArtifactTool_IsTerminalAndValidatesArgs(t *testing.T) {
	tool := submitArtifactTool{}
	assert.True(t, tool.Terminal())
	assert.Equal(t, "submit_artifact", tool.Name())

	tests := []struct {
		name    string
		args    map[string]any
		wantErr bool
	}{
		{"body and object data", map[string]any{"body": "# Need", "data": map[string]any{"slices": 1}}, false},
		{"body without data", map[string]any{"body": "# Need"}, false},
		{"null data is absent", map[string]any{"body": "# Need", "data": nil}, false},
		{"empty body", map[string]any{"body": ""}, true},
		{"string data", map[string]any{"body": "# Need", "data": "slices: 1"}, true},
		{"array data", map[string]any{"body": "# Need", "data": []any{1}}, true},
		{"number data", map[string]any{"body": "# Need", "data": 1.0}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tool.Execute(context.Background(), tt.args)
			if tt.wantErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestSubmitArtifactTool_ValidatesWithTheCallsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tool := submitArtifactTool{validate: func(ctx context.Context, _ Artifact) error { return ctx.Err() }}

	_, err := tool.Execute(ctx, map[string]any{"body": "# Cards"})
	require.ErrorIs(t, err, context.Canceled)
}

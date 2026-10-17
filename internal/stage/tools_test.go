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

func TestAskHumanTool_TakesTheRecommendedOption(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		want    int
		wantErr bool
	}{
		{name: "none", args: map[string]any{"question": "q?", "options": []any{"a", "b"}}},
		{name: "the second", args: map[string]any{"question": "q?", "options": []any{"a", "b"}, "recommended": float64(2)}, want: 2},
		{name: "past the options", args: map[string]any{"question": "q?", "options": []any{"a", "b"}, "recommended": float64(3)}, wantErr: true},
		{name: "zero", args: map[string]any{"question": "q?", "options": []any{"a"}, "recommended": float64(0)}, wantErr: true},
		{name: "not a whole number", args: map[string]any{"question": "q?", "options": []any{"a", "b"}, "recommended": 1.5}, wantErr: true},
		{name: "without options", args: map[string]any{"question": "q?", "recommended": float64(1)}, wantErr: true},
		{name: "a blank option before it", args: map[string]any{"question": "q?", "options": []any{"a", " ", "c"}, "recommended": float64(3)}, want: 2},
		{name: "pointing at a blank option", args: map[string]any{"question": "q?", "options": []any{"a", " ", "c"}, "recommended": float64(2)}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asker := &stubAsker{answer: "a"}

			_, err := askHumanTool{asker: asker}.Execute(context.Background(), tt.args)
			if tt.wantErr {
				require.Error(t, err)
				assert.Empty(t, asker.got.Text, "the asker is not called")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, asker.got.Recommended)
		})
	}
}

func TestAskHumanTool_CutsARecommendedMarkerFromAnOption(t *testing.T) {
	tests := []struct {
		name        string
		options     []any
		recommended any
		wantOptions []string
		wantRec     int
	}{
		{name: "a marker names its option", options: []any{"a", "b (recommended)"}, wantOptions: []string{"a", "b"}, wantRec: 2},
		{name: "any case, with the whitespace around it", options: []any{"a  (Recommended)  ", "b"}, wantOptions: []string{"a", "b"}, wantRec: 1},
		{name: "the position wins over a marker", options: []any{"a", "b (recommended)"}, recommended: float64(1), wantOptions: []string{"a", "b"}, wantRec: 1},
		{name: "markers on two options name none", options: []any{"a (recommended)", "b (RECOMMENDED)"}, wantOptions: []string{"a", "b"}},
		{name: "a marker alone is a blank option", options: []any{"a", "(recommended)"}, wantOptions: []string{"a"}},
		{name: "text before the marker that is not ASCII", options: []any{"é (recommended)", "b"}, wantOptions: []string{"é", "b"}, wantRec: 1},
		{name: "a marker before the end stays", options: []any{"(recommended) a", "a (recommended) b"}, wantOptions: []string{"(recommended) a", "a (recommended) b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := map[string]any{"question": "q?", "options": tt.options}
			if tt.recommended != nil {
				args["recommended"] = tt.recommended
			}

			asker := &stubAsker{answer: "a"}

			_, err := askHumanTool{asker: asker}.Execute(context.Background(), args)
			require.NoError(t, err)
			assert.Equal(t, tt.wantOptions, asker.got.Options)
			assert.Equal(t, tt.wantRec, asker.got.Recommended)
		})
	}
}

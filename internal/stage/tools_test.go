package stage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

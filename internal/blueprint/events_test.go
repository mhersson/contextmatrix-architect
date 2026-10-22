package blueprint

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranscript_KeepsReasoningOnlyWhereMaskingLeavesItWhole(t *testing.T) {
	c, st, _, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	au := c.newAudit(testKey)

	clean := json.RawMessage(`[{"type":"reasoning.text","text":"plan the steps","signature":"c2ln","format":"anthropic-claude-v1","index":0}]`)
	leaky := json.RawMessage(`[{"type":"reasoning.text","text":"the key is ` + testKey + `","signature":"c2ln","index":0}]`)
	msgs := []llm.Message{
		{Role: "user", Content: "the need"},
		{Role: "assistant", Content: "first", ReasoningDetails: clean},
		{Role: "assistant", Content: "second", ReasoningDetails: leaky},
	}

	require.NoError(t, au.transcript(ctx, 1, msgs))

	raw, err := st.GetTranscript(ctx, 1)
	require.NoError(t, err)

	var got []llm.Message
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Len(t, got, 3)
	assert.JSONEq(t, string(clean), string(got[1].ReasoningDetails), "details masking leaves whole are kept for a resume")
	assert.Nil(t, got[2].ReasoningDetails, "details masking would change are dropped, never stored masked")
	assert.Equal(t, "second", got[2].Content)
	assert.NotContains(t, string(raw), testKey)
	assert.Equal(t, leaky, msgs[2].ReasoningDetails, "the run's own messages are never changed")
}

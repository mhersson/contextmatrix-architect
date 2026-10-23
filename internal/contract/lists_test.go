package contract

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverlay_StoresTheListNames(t *testing.T) {
	o := Overlay{EdgeCases: []string{"Limits"}, ForbiddenReferences: []Pattern{{Name: "n", Pattern: "p"}}}

	raw, err := json.Marshal(o)
	require.NoError(t, err)
	assert.JSONEq(t, `{"edge_cases":["Limits"],"forbidden_references":[{"name":"n","pattern":"p"}]}`, string(raw))

	var back Overlay

	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, o, back)
}

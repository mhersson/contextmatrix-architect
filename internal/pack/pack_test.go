package pack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadEmbedded_ReferencesResolve(t *testing.T) {
	p, err := LoadEmbedded()
	require.NoError(t, err)
	assert.NotEmpty(t, p.Name)
	assert.NotEmpty(t, p.Version)
	require.NotEmpty(t, p.Stages)

	for _, s := range p.Stages {
		assert.NotEmpty(t, p.Persona(s.Persona), "persona %q of stage %q", s.Persona, s.ID)
		assert.NotEmpty(t, p.Instructions(s.ID), "instructions of stage %q", s.ID)
	}

	first, ok := p.Stage(p.Stages[0].ID)
	require.True(t, ok)
	assert.Equal(t, p.Stages[0], first)

	_, ok = p.Stage("nope")
	assert.False(t, ok)
}

func TestLoadDir_MissingPersonaFails(t *testing.T) {
	_, err := LoadDir("testdata/broken")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persona")
}

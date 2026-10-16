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

	ids := map[string]bool{}

	for _, s := range p.Stages {
		ids[s.ID] = true

		if s.Source == SourceModel {
			assert.NotEmpty(t, p.Persona(s.Persona), "persona %q of stage %q", s.Persona, s.ID)
			assert.NotEmpty(t, p.Instructions(s.ID), "instructions of stage %q", s.ID)
		}

		for _, c := range s.Consumes {
			assert.True(t, ids[c], "stage %q consumes %q, which must come earlier", s.ID, c)
		}

		if s.LoopsTo != "" {
			assert.True(t, ids[s.LoopsTo], "stage %q loops to %q, which must come earlier", s.ID, s.LoopsTo)
			assert.Positive(t, s.MaxRounds)
		}
	}

	lists := p.Lists()
	assert.NotEmpty(t, lists.RequiredSections)
	assert.NotEmpty(t, lists.StrippedHeadings)
	assert.NotEmpty(t, lists.DefectPhrases)
	assert.NotEmpty(t, p.CardTemplate())

	first, ok := p.Stage(p.Stages[0].ID)
	require.True(t, ok)
	assert.Equal(t, p.Stages[0], first)

	_, ok = p.Stage("nope")
	assert.False(t, ok)
}

func TestLoadEmbedded_DefaultPipeline(t *testing.T) {
	p, err := LoadEmbedded()
	require.NoError(t, err)

	var ids []string
	for _, s := range p.Stages {
		ids = append(ids, s.ID)
	}

	assert.Equal(t, []string{"intake", "design", "grounding", "decomposition", "readiness"}, ids)

	design, _ := p.Stage("design")
	assert.Equal(t, SourceHuman, design.Source)

	readiness, _ := p.Stage("readiness")
	assert.Equal(t, "decomposition", readiness.LoopsTo)
	assert.Equal(t, 3, readiness.MaxRounds)
	assert.Contains(t, readiness.Tools, ToolCode)

	decomposition, _ := p.Stage("decomposition")
	assert.Contains(t, decomposition.Checks, CheckCards)
}

func TestLoadDir_Invalid(t *testing.T) {
	tests := []struct {
		dir  string
		want string
	}{
		{dir: "testdata/broken", want: "persona"},
		{dir: "testdata/dupstage", want: "duplicate stage"},
		{dir: "testdata/badconsumes", want: "consumes"},
	}

	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			_, err := LoadDir(tt.dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

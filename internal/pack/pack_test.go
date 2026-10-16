package pack

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
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

// forbiddenRefs runs the embedded pack's card checks over one card with body
// appended to a good body and returns the forbidden_reference findings.
func forbiddenRefs(t *testing.T, extra string) []string {
	t.Helper()

	p, err := LoadEmbedded()
	require.NoError(t, err)

	lists := p.Lists()

	var body strings.Builder

	for _, section := range lists.RequiredSections {
		body.WriteString("## " + section + "\n")

		if strings.EqualFold(section, "Acceptance criteria") {
			body.WriteString("1. The image builds.\n")
		} else {
			body.WriteString("Text.\n")
		}
	}

	body.WriteString(extra)

	card := contract.Card{ID: "D1", Project: "alpha", Title: "Add a container image", Type: "feature", Body: body.String()}
	projects := map[string]contract.Project{"alpha": {HasRepo: true, HasVerify: true}}

	var out []string

	for _, f := range contract.Check(contract.Draft{Cards: []contract.Card{card}}, lists, projects) {
		if f.Check == "forbidden_reference" {
			out = append(out, f.Message)
		}
	}

	return out
}

func TestEmbeddedLists_ForbiddenReferences(t *testing.T) {
	inRepo := "Mount the config at /etc/app/config.yaml and keep data in /var/lib/app.\n" +
		"Scratch files go to /tmp/x.\n" +
		"The web card keeps `import { x } from \"../lib/x\"` unchanged.\n"
	assert.Empty(t, forbiddenRefs(t, inRepo), "paths inside an image and quoted relative imports are in-repo content")

	assert.NotEmpty(t, forbiddenRefs(t, "See /home/dev/notes.md for the plan.\n"), "a developer's home directory")
	assert.NotEmpty(t, forbiddenRefs(t, "Copy it from ../other-repo/x.go first.\n"), "a sibling checkout")
}

// packFS is a one-stage pack whose stage carries the given extra yaml lines,
// plus files that replace or add to the defaults; an empty content removes
// a file.
func packFS(stageYAML string, files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{
		"pack.yaml":      {Data: []byte("name: t\nversion: \"1\"\nstages:\n  - id: s1\n    persona: po\n" + stageYAML)},
		"personas/po.md": {Data: []byte("persona")},
		"stages/s1.md":   {Data: []byte("instructions")},
		"lists.yaml":     {Data: []byte("required_sections: [Context]\n")},
		"card.md":        {Data: []byte("## Context\n")},
	}

	for name, content := range files {
		if content == "" {
			delete(fsys, name)

			continue
		}

		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}

	return fsys
}

func TestLoad_Invalid(t *testing.T) {
	tests := []struct {
		name  string
		stage string
		files map[string]string
		want  string
	}{
		{name: "unknown source", stage: "    source: robot\n", want: "unknown source"},
		{name: "unknown gate", stage: "    gate: sometimes\n", want: "unknown gate"},
		{name: "unknown tool", stage: "    tools: [shell]\n", want: "unknown tool group"},
		{name: "unknown check", stage: "    checks: [style]\n", want: "unknown check"},
		{name: "loops_to without max_rounds", stage: "  - id: s2\n    persona: po\n    loops_to: s1\n", files: map[string]string{"stages/s2.md": "x"}, want: "max_rounds"},
		{name: "bad regex", files: map[string]string{"lists.yaml": "forbidden_references:\n  - name: broken\n    pattern: '(['\n"}, want: "forbidden reference"},
		{name: "unknown lists.yaml key", files: map[string]string{"lists.yaml": "required_section: [Context]\n"}, want: "lists.yaml"},
		{name: "card checks without lists.yaml", stage: "    checks: [cards]\n", files: map[string]string{"lists.yaml": ""}, want: "lists.yaml"},
		{name: "card checks without card.md", stage: "    checks: [cards]\n", files: map[string]string{"card.md": ""}, want: "card.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(packFS(tt.stage, tt.files))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoad_ListsAndCardAreOptionalWithoutCardChecks(t *testing.T) {
	p, err := load(packFS("", map[string]string{"lists.yaml": "", "card.md": ""}))
	require.NoError(t, err)
	assert.Empty(t, p.Lists().RequiredSections)
	assert.Empty(t, p.CardTemplate())
}

func TestLoad_EmptyListsYAMLIsEmptyLists(t *testing.T) {
	p, err := load(packFS("    checks: [cards]\n", map[string]string{"lists.yaml": "\n"}))
	require.NoError(t, err)
	assert.Empty(t, p.Lists().RequiredSections)
	assert.NotEmpty(t, p.CardTemplate())
}

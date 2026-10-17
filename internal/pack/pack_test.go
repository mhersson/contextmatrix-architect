package pack

import (
	"slices"
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

		assert.NotEmpty(t, p.Persona(s.Persona), "persona %q of stage %q", s.Persona, s.ID)
		assert.NotEmpty(t, p.Instructions(s.ID), "instructions of stage %q", s.ID)

		if s.Reviewer != nil {
			assert.NotEmpty(t, p.Persona(s.Reviewer.Persona), "reviewer persona %q of stage %q", s.Reviewer.Persona, s.ID)
		}

		for _, c := range s.Consumes {
			assert.True(t, ids[c], "stage %q consumes %q, which must come earlier", s.ID, c)
		}

		if s.LoopsTo != "" {
			assert.True(t, ids[s.LoopsTo], "stage %q loops to %q, which must come earlier", s.ID, s.LoopsTo)
			assert.Positive(t, s.MaxRounds)
		}
	}

	if slices.ContainsFunc(p.Stages, func(s Stage) bool { return s.Reviewer != nil }) {
		assert.NotEmpty(t, p.ReviewInstructions(), "a pack with reviewers carries their instructions")
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

	assert.Equal(t, []string{"intake", "slicing", "solution_design", "grounding", "acceptance", "decomposition", "readiness"}, ids)

	acceptance, _ := p.Stage("acceptance")
	assert.Equal(t, PolicyAuto, acceptance.Policy, "acceptance's gate is auto by default")
	assert.True(t, acceptance.Gate)

	slicing, _ := p.Stage("slicing")
	assert.Equal(t, &Fold{Field: "slices", Max: 1}, slicing.Fold)
	assert.True(t, slicing.Gate)

	design, _ := p.Stage("solution_design")
	assert.True(t, design.Gate)
	assert.Equal(t, PolicyHuman, design.Policy)
	assert.Equal(t, 5, design.MaxQuestions)
	assert.Contains(t, design.Tools, ToolCode)
	assert.Equal(t, []string{CheckAssumptions, CheckDecisions}, design.Checks)
	require.NotNil(t, design.Reviewer)
	assert.Equal(t, "senior_developer", design.Reviewer.Persona)

	intake, _ := p.Stage("intake")
	assert.Equal(t, PolicyHuman, intake.Policy)
	assert.Equal(t, 3, intake.MaxQuestions)

	for _, id := range []string{"intake", "slicing", "solution_design"} {
		s, _ := p.Stage(id)
		assert.Contains(t, s.Tools, ToolResearch, "%s researches its questions when research is on", id)
	}

	grounding, _ := p.Stage("grounding")
	assert.Equal(t, PolicyAuto, grounding.Policy)

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

	tests := []struct {
		name, text, pattern string
	}{
		{"home-relative path", "Keep the notes in ~/notes/plan.md.\n", "home path"},
		{"windows path", "The installer lives at D:\\tools\\setup.exe.\n", "windows path"},
		{"file url", "Open file:///srv/app/config.yaml in a browser.\n", "file url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forbiddenRefs(t, tt.text)
			assert.True(t, slices.ContainsFunc(got, func(m string) bool { return strings.HasPrefix(m, tt.pattern+": ") }),
				"the %s pattern matches; got %v", tt.pattern, got)
		})
	}
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
		{name: "a source key", stage: "    source: human\n", want: "source"},
		{name: "an old gate value", stage: "    gate: questions\n", want: "cannot unmarshal"},
		{name: "unknown policy", stage: "    policy: sometimes\n", want: "unknown policy"},
		{name: "negative max_questions", stage: "    max_questions: -1\n", want: "max_questions"},
		{name: "gate without a reviewer", stage: "    gate: true\n", want: "needs a reviewer"},
		{name: "reviewer without a gate", stage: "    reviewer: {persona: po, threshold: 4}\n", want: "no gate"},
		{name: "reviewer without a persona", stage: "    gate: true\n    reviewer: {threshold: 4}\n", want: "needs a persona"},
		{name: "reviewer persona file missing", stage: "    gate: true\n    reviewer: {persona: ghost, threshold: 4}\n", want: `persona "ghost"`},
		{name: "reviewer threshold out of range", stage: "    gate: true\n    reviewer: {persona: po, threshold: 6}\n", want: "threshold"},
		{name: "fold without a gate", stage: "    fold: {field: slices, max: 1}\n", want: "does not have"},
		{name: "fold without a field", stage: "    gate: true\n    reviewer: {persona: po, threshold: 4}\n    fold: {max: 1}\n", want: "field"},
		{name: "fold with no later gate", stage: "    gate: true\n    reviewer: {persona: po, threshold: 4}\n    fold: {field: slices, max: 1}\n", want: "no later stage"},
		{name: "gate on a loop stage", stage: "  - id: s2\n    persona: po\n    loops_to: s1\n    max_rounds: 2\n    gate: true\n    reviewer: {persona: po, threshold: 4}\n", files: map[string]string{"stages/s2.md": "x"}, want: "takes no gate"},
		{name: "unknown tool", stage: "    tools: [shell]\n", want: "unknown tool group"},
		{name: "unknown check", stage: "    checks: [style]\n", want: "unknown check"},
		{name: "loops_to without max_rounds", stage: "  - id: s2\n    persona: po\n    loops_to: s1\n", files: map[string]string{"stages/s2.md": "x"}, want: "max_rounds"},
		{name: "bad regex", files: map[string]string{"lists.yaml": "forbidden_references:\n  - name: broken\n    pattern: '(['\n"}, want: "forbidden reference"},
		{name: "blank dangling reference", files: map[string]string{"lists.yaml": "dangling_references: [\" \"]\n"}, want: "dangling_references"},
		{name: "unknown lists.yaml key", files: map[string]string{"lists.yaml": "required_section: [Context]\n"}, want: "lists.yaml"},
		{name: "card checks without lists.yaml", stage: "    tools: [code]\n    checks: [cards]\n", files: map[string]string{"lists.yaml": ""}, want: "lists.yaml"},
		{name: "card checks without card.md", stage: "    tools: [code]\n    checks: [cards]\n", files: map[string]string{"card.md": ""}, want: "card.md"},
		{name: "card checks without code tools", stage: "    checks: [cards]\n", want: "tools: [code]"},
		{name: "a reviewer without review instructions", stage: "    gate: true\n    reviewer: {persona: po, threshold: 4}\n", want: "stages/review.md"},
		{name: "a reserved stage id", stage: "  - id: review\n    persona: po\n", files: map[string]string{"stages/review.md": "x"}, want: "reserved"},
		{name: "a dot in a stage id", stage: "  - id: s1.review\n    persona: po\n", files: map[string]string{"stages/s1.review.md": "x"}, want: "dot"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(packFS(tt.stage, tt.files))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestLoad_DialogKeys(t *testing.T) {
	stages := "    policy: human\n    max_questions: 2\n    gate: true\n    reviewer: {persona: rev, threshold: 4}\n    fold: {field: slices, max: 1}\n" +
		"  - id: s2\n    persona: po\n    gate: true\n    reviewer: {persona: po, threshold: 3}\n" +
		"  - id: s3\n    persona: po\n"

	p, err := load(packFS(stages, map[string]string{"personas/rev.md": "the reviewer", "stages/s2.md": "x", "stages/s3.md": "x", "stages/review.md": "judge it"}))
	require.NoError(t, err)

	s1, _ := p.Stage("s1")
	assert.Equal(t, PolicyHuman, s1.Policy)
	assert.Equal(t, 2, s1.MaxQuestions)
	assert.True(t, s1.Gate)
	assert.Equal(t, &Reviewer{Persona: "rev", Threshold: 4}, s1.Reviewer)
	assert.Equal(t, &Fold{Field: "slices", Max: 1}, s1.Fold)
	assert.Equal(t, "the reviewer", p.Persona("rev"), "a reviewer's persona is loaded")

	s3, _ := p.Stage("s3")
	assert.Equal(t, PolicyAuto, s3.Policy, "auto is the default policy")
	assert.False(t, s3.Gate)
	assert.Zero(t, s3.MaxQuestions)

	assert.Equal(t, "judge it", p.ReviewInstructions())
}

func TestLoad_ListsAndCardAreOptionalWithoutCardChecks(t *testing.T) {
	p, err := load(packFS("", map[string]string{"lists.yaml": "", "card.md": ""}))
	require.NoError(t, err)
	assert.Empty(t, p.Lists().RequiredSections)
	assert.Empty(t, p.CardTemplate())
}

func TestLoad_EmptyListsYAMLIsEmptyLists(t *testing.T) {
	p, err := load(packFS("    tools: [code]\n    checks: [cards]\n", map[string]string{"lists.yaml": "\n"}))
	require.NoError(t, err)
	assert.Empty(t, p.Lists().RequiredSections)
	assert.NotEmpty(t, p.CardTemplate())
}

func TestLoad_AcceptsTheCMToolGroup(t *testing.T) {
	p, err := load(packFS("    tools: [code, cm]\n", nil))
	require.NoError(t, err)

	s1, ok := p.Stage("s1")
	require.True(t, ok)
	assert.Equal(t, []string{ToolCode, ToolCM}, s1.Tools)
}

func TestLoad_AcceptsTheResearchToolGroup(t *testing.T) {
	p, err := load(packFS("    tools: [code, research]\n", nil))
	require.NoError(t, err)

	s1, ok := p.Stage("s1")
	require.True(t, ok)
	assert.Equal(t, []string{ToolCode, ToolResearch}, s1.Tools)
}

func TestLoad_AcceptsTheAssumptionsCheck(t *testing.T) {
	p, err := load(packFS("    checks: [assumptions]\n", map[string]string{"lists.yaml": "dangling_references: [see above]\n"}))
	require.NoError(t, err)
	assert.Equal(t, []string{"see above"}, p.Lists().DanglingReferences)
}

func TestLoad_ResponderInstructionsAreOptional(t *testing.T) {
	p, err := load(packFS("", nil))
	require.NoError(t, err)
	assert.Empty(t, p.ResponderInstructions(), "no responder without its instructions")

	p, err = load(packFS("", map[string]string{"stages/responder.md": "answer it"}))
	require.NoError(t, err)
	assert.Equal(t, "answer it", p.ResponderInstructions())
}

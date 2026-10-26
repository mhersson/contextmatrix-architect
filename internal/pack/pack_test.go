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
	assert.NotEmpty(t, p.RetroInstructions())
	assert.NotEmpty(t, p.Persona(RetroPersona))
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

func TestPersonas_NamesEveryPersonaARunCanUse(t *testing.T) {
	p, err := LoadEmbedded()
	require.NoError(t, err)

	want := []string{RetroPersona}

	for _, s := range p.Stages {
		want = append(want, s.Persona)

		if s.Reviewer != nil {
			want = append(want, s.Reviewer.Persona)
		}
	}

	slices.Sort(want)

	assert.Equal(t, slices.Compact(want), p.Personas())
}

func TestLoadEmbedded_DefaultPipeline(t *testing.T) {
	p, err := LoadEmbedded()
	require.NoError(t, err)

	var ids []string
	for _, s := range p.Stages {
		ids = append(ids, s.ID)
	}

	assert.Equal(t, []string{"intake", "mockup", "slicing", "shared_decisions", "solution_design", "grounding", "acceptance", "decomposition", "readiness"}, ids)

	assert.Equal(t, "mockup", p.MockupStage())
	assert.Equal(t, "intake", p.ScreenStage())
	look, ok := p.Stage("mockup")
	require.True(t, ok)
	assert.False(t, look.Gate)
	assert.Nil(t, look.Reviewer)
	assert.Equal(t, "designer", look.Persona)

	for _, id := range []string{"slicing", "shared_decisions", "solution_design", "decomposition", "readiness"} {
		s, _ := p.Stage(id)
		assert.Contains(t, s.Consumes, "mockup", "%s builds on the approved mockup", id)
	}

	acceptance, _ := p.Stage("acceptance")
	assert.Equal(t, PolicyAuto, acceptance.Policy, "acceptance's gate is auto by default")
	assert.Equal(t, PolicyAuto, acceptance.GatePolicy)
	assert.True(t, acceptance.Gate)

	slicing, _ := p.Stage("slicing")
	assert.Equal(t, &Fold{Field: "slices", Max: 1}, slicing.Fold)
	assert.True(t, slicing.Gate)

	design, _ := p.Stage("solution_design")
	assert.True(t, design.Gate)
	assert.Equal(t, PolicyHuman, design.Policy)
	assert.Equal(t, 5, design.MaxQuestions)
	assert.Contains(t, design.Tools, ToolCode)
	assert.Equal(t, []string{CheckAssumptions, CheckDecisions, CheckOutcomes}, design.Checks)
	require.NotNil(t, design.Reviewer)
	assert.Equal(t, "senior_developer", design.Reviewer.Persona)

	intake, _ := p.Stage("intake")
	assert.Equal(t, PolicyHuman, intake.Policy, "a human answers intake's questions")
	assert.True(t, intake.Gate)
	assert.Equal(t, PolicyAuto, intake.GatePolicy, "intake's reviewer's verdict decides its gate")
	assert.Equal(t, 3, intake.MaxQuestions)
	assert.Equal(t, []string{CheckAssumptions, CheckOutcomes, CheckScreen}, intake.Checks, "intake states the need's outcomes and whether it has a screen")

	next := map[string]string{"intake": "system_designer", "slicing": "system_designer", "shared_decisions": "senior_developer", "solution_design": "senior_developer", "acceptance": "tester"}

	for id, persona := range next {
		s, _ := p.Stage(id)
		require.NotNil(t, s.Reviewer, id)
		assert.Equal(t, persona, s.Reviewer.Persona, "%s is reviewed from the next role's seat", id)
		assert.Equal(t, []string{ToolCode, ToolResearch}, s.Reviewer.Tools, "%s's reviewer reads the code and opens cited pages", id)
	}

	for _, id := range []string{"intake", "slicing", "shared_decisions", "solution_design"} {
		s, _ := p.Stage(id)
		assert.Contains(t, s.Tools, ToolResearch, "%s researches its questions when research is on", id)
		assert.Contains(t, s.Tools, ToolCode, "%s reads the code before it asks", id)
	}

	shared, _ := p.Stage("shared_decisions")
	assert.True(t, shared.Shared, "the shared decisions collapse with one slice")
	assert.True(t, shared.Gate)
	assert.Equal(t, PolicyHuman, shared.Policy)
	assert.Equal(t, "system_designer", shared.Persona)
	assert.Equal(t, []string{"intake", "mockup", "slicing"}, shared.Consumes)
	assert.Equal(t, []string{CheckAssumptions, CheckDecisions, CheckOutcomes}, shared.Checks)

	for _, id := range []string{"solution_design", "grounding", "acceptance", "decomposition"} {
		s, _ := p.Stage(id)
		assert.True(t, s.PerSlice, "%s runs once per slice", id)
		assert.Equal(t, []string{"solution_design", "acceptance", "decomposition"}, s.EarlierSlices,
			"%s reads every earlier slice's design, acceptance and cards", id)
		assert.Contains(t, s.Consumes, "shared_decisions", "%s reads the shared decisions", id)
	}

	grounding, _ := p.Stage("grounding")
	assert.Equal(t, PolicyAuto, grounding.Policy)

	readiness, _ := p.Stage("readiness")
	assert.Equal(t, "decomposition", readiness.LoopsTo)
	assert.Equal(t, 3, readiness.MaxRounds)
	assert.Equal(t, []string{CheckReadiness}, readiness.Checks)
	assert.Contains(t, readiness.Tools, ToolCode)
	assert.False(t, readiness.PerSlice, "readiness judges every slice's cards at once")
	assert.Contains(t, readiness.Consumes, "shared_decisions")

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
	projects := map[string]contract.Project{"alpha": {HasRepo: true, VerifyCommand: "make test"}}

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
		{name: "unknown gate_policy", stage: "    gate: true\n    reviewer: {persona: po, threshold: 4}\n    gate_policy: sometimes\n", files: map[string]string{"stages/review.md": "x"}, want: "unknown gate_policy"},
		{name: "gate_policy without a gate", stage: "    gate_policy: auto\n", want: "has a gate_policy but no gate"},
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
		{name: "blank edge case", files: map[string]string{"lists.yaml": "edge_cases: [\" \"]\n"}, want: "edge_cases"},
		{name: "an edge case listed twice", files: map[string]string{"lists.yaml": "edge_cases: [Limits, \" limits\"]\n"}, want: "edge_cases"},
		{name: "unknown lists.yaml key", files: map[string]string{"lists.yaml": "required_section: [Context]\n"}, want: "lists.yaml"},
		{name: "card checks without lists.yaml", stage: "    tools: [code]\n    checks: [cards]\n", files: map[string]string{"lists.yaml": ""}, want: "lists.yaml"},
		{name: "card checks without card.md", stage: "    tools: [code]\n    checks: [cards]\n", files: map[string]string{"card.md": ""}, want: "card.md"},
		{name: "card checks without code tools", stage: "    checks: [cards]\n", want: "tools: [code]"},
		{name: "a reviewer without review instructions", stage: "    gate: true\n    reviewer: {persona: po, threshold: 4}\n", want: "stages/review.md"},
		{name: "a reviewer with the cm tools", stage: "    gate: true\n    reviewer: {persona: po, threshold: 4, tools: [cm]}\n", files: map[string]string{"stages/review.md": "x"}, want: "not code or research"},
		{name: "a reviewer with an unknown tool group", stage: "    gate: true\n    reviewer: {persona: po, threshold: 4, tools: [shell]}\n", files: map[string]string{"stages/review.md": "x"}, want: "not code or research"},
		{name: "a reserved stage id", stage: "  - id: review\n    persona: po\n", files: map[string]string{"stages/review.md": "x"}, want: "reserved"},
		{name: "the reserved plain id", stage: "  - id: plain\n    persona: po\n", files: map[string]string{"stages/plain.md": "x"}, want: "reserved"},
		{name: "the reserved retrospective id", stage: "  - id: retrospective\n    persona: po\n", files: map[string]string{"stages/retrospective.md": "x", "personas/reviewer.md": "x"}, want: "reserved"},
		{name: "retrospective instructions without the reviewer persona", files: map[string]string{"stages/retrospective.md": "x"}, want: `persona "reviewer"`},
		{name: "a dot in a stage id", stage: "  - id: s1.review\n    persona: po\n", files: map[string]string{"stages/s1.review.md": "x"}, want: "dot"},
		{name: "a space in a stage id", stage: "  - id: \"s 1\"\n    persona: po\n", files: map[string]string{"stages/s 1.md": "x"}, want: `"s 1"`},
		{name: "a stage id over 64 characters", stage: "  - id: " + strings.Repeat("a", 65) + "\n    persona: po\n", files: map[string]string{"stages/" + strings.Repeat("a", 65) + ".md": "x"}, want: strings.Repeat("a", 65)},
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

func TestLoad_PlainInstructionsAreOptional(t *testing.T) {
	p, err := load(packFS("", nil))
	require.NoError(t, err)
	assert.Empty(t, p.PlainInstructions())

	p, err = load(packFS("", map[string]string{"stages/plain.md": "word it plainly"}))
	require.NoError(t, err)
	assert.Equal(t, "word it plainly", p.PlainInstructions())
}

func TestLoadEmbedded_HasPlainInstructions(t *testing.T) {
	p, err := LoadEmbedded()
	require.NoError(t, err)
	assert.NotEmpty(t, p.PlainInstructions())
}

func TestPack_UnattendedIsOptionalAndReserved(t *testing.T) {
	p, err := load(packFS("", nil))
	require.NoError(t, err)
	assert.Empty(t, p.UnattendedInstructions())

	p, err = load(packFS("", map[string]string{"stages/unattended.md": "never stop the run"}))
	require.NoError(t, err)
	assert.Equal(t, "never stop the run", p.UnattendedInstructions())

	_, err = load(packFS("  - id: unattended\n    persona: po\n", map[string]string{"stages/unattended.md": "x"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved")

	p, err = LoadEmbedded()
	require.NoError(t, err)
	assert.NotEmpty(t, p.UnattendedInstructions())
}

func TestStageName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		id     string
		sliced bool
		want   string
	}{
		{"solution_design", false, "Solution design"},
		{"intake", false, "Intake"},
		{"a_b_c_d", false, "A b c d"},
		{"", false, ""},
		{"solution_design@SL2", true, "Solution design (SL2)"},
		{"solution_design@SL1", false, "Solution design"},
		{"intake", true, "Intake"},
	} {
		assert.Equal(t, tc.want, StageName(tc.id, tc.sliced), tc.id)
	}
}

func TestLoad_GatePolicy(t *testing.T) {
	stages := "    policy: human\n    gate: true\n    gate_policy: auto\n    reviewer: {persona: po, threshold: 4}\n" +
		"  - id: s2\n    persona: po\n    policy: human\n    gate: true\n    reviewer: {persona: po, threshold: 4}\n" +
		"  - id: s3\n    persona: po\n    policy: human\n"

	p, err := load(packFS(stages, map[string]string{"stages/s2.md": "x", "stages/s3.md": "x", "stages/review.md": "judge it"}))
	require.NoError(t, err)

	s1, _ := p.Stage("s1")
	assert.Equal(t, PolicyHuman, s1.Policy, "a human answers its questions")
	assert.Equal(t, PolicyAuto, s1.GatePolicy, "its reviewer's verdict decides its gate")

	s2, _ := p.Stage("s2")
	assert.Equal(t, PolicyHuman, s2.GatePolicy, "a gate takes the stage's policy by default")

	s3, _ := p.Stage("s3")
	assert.Empty(t, s3.GatePolicy, "a stage without a gate has no gate policy")
}

func TestLoad_ReviewerTools(t *testing.T) {
	p, err := load(packFS("    gate: true\n    reviewer: {persona: po, threshold: 4, tools: [code, research]}\n", map[string]string{"stages/review.md": "judge it"}))
	require.NoError(t, err)

	s1, _ := p.Stage("s1")
	require.NotNil(t, s1.Reviewer)
	assert.Equal(t, []string{ToolCode, ToolResearch}, s1.Reviewer.Tools)
}

// slicedPack is a pack whose stages are the given yaml: a stage list over
// one persona, with instructions for every stage id the tests use, the
// review instructions, and the files a stage that checks cards needs.
func slicedPack(stagesYAML string) fstest.MapFS {
	fsys := fstest.MapFS{
		"pack.yaml":        {Data: []byte("name: t\nversion: \"1\"\nstages:\n" + stagesYAML)},
		"personas/po.md":   {Data: []byte("persona")},
		"stages/review.md": {Data: []byte("judge it")},
		"lists.yaml":       {Data: []byte("required_sections: [Context]\n")},
		"card.md":          {Data: []byte("## Context\n")},
	}

	for _, id := range []string{"cut", "shared", "design", "cards", "check", "middle"} {
		fsys["stages/"+id+".md"] = &fstest.MapFile{Data: []byte("instructions")}
	}

	return fsys
}

const (
	cutYAML    = "  - id: cut\n    persona: po\n    checks: [slices]\n    gate: true\n    fold: {field: slices, max: 1}\n    reviewer: {persona: po, threshold: 4}\n"
	sharedYAML = "  - id: shared\n    persona: po\n    shared: true\n    gate: true\n    reviewer: {persona: po, threshold: 4}\n"
	designYAML = "  - id: design\n    persona: po\n    per_slice: true\n    earlier_slices: [design, cards]\n    gate: true\n    reviewer: {persona: po, threshold: 4}\n"
	cardsYAML  = "  - id: cards\n    persona: po\n    per_slice: true\n    earlier_slices: [design, cards]\n"
	checkYAML  = "  - id: check\n    persona: po\n    consumes: [cards]\n    loops_to: cards\n    max_rounds: 2\n"
	middleYAML = "  - id: middle\n    persona: po\n"

	wholeCardsYAML    = "  - id: cards\n    persona: po\n    tools: [code]\n    checks: [cards]\n"
	wholeCriteriaYAML = "  - id: design\n    persona: po\n    checks: [criteria]\n    gate: true\n    reviewer: {persona: po, threshold: 4}\n"
)

func TestLoad_SliceKeys(t *testing.T) {
	p, err := load(slicedPack(cutYAML + sharedYAML + designYAML + cardsYAML + checkYAML))
	require.NoError(t, err)

	design, _ := p.Stage("design")
	assert.True(t, design.PerSlice)
	assert.Equal(t, []string{"design", "cards"}, design.EarlierSlices)

	shared, _ := p.Stage("shared")
	assert.True(t, shared.Shared)
	assert.False(t, shared.PerSlice)

	check, _ := p.Stage("check")
	assert.False(t, check.PerSlice, "a stage after the per-slice ones runs for the whole blueprint")
	assert.Equal(t, "cut", p.SlicingStage())

	none, err := load(packFS("", nil))
	require.NoError(t, err)
	assert.Empty(t, none.SlicingStage())
}

func TestLoad_SliceKeysInvalid(t *testing.T) {
	tests := []struct {
		name   string
		stages string
		want   string
	}{
		{name: "a per-slice stage without a slicing stage", stages: middleYAML + designYAML, want: "needs an earlier stage that checks slices"},
		{name: "a per-slice stage before slicing", stages: designYAML + cutYAML, want: "needs an earlier stage that checks slices"},
		{
			name:   "the slicing stage itself per-slice",
			stages: strings.Replace(cutYAML, "    fold: {field: slices, max: 1}\n", "    per_slice: true\n", 1) + designYAML + cardsYAML,
			want:   "needs an earlier stage that checks slices",
		},
		{name: "per-slice stages apart", stages: cutYAML + designYAML + middleYAML + cardsYAML, want: "apart from the other per-slice stages"},
		{name: "per_slice and shared", stages: cutYAML + strings.Replace(designYAML, "per_slice: true", "per_slice: true\n    shared: true", 1), want: "both per_slice and shared"},
		{name: "a per-slice stage that folds", stages: cutYAML + strings.Replace(designYAML, "gate: true", "gate: true\n    fold: {field: slices, max: 1}", 1) + cardsYAML, want: "cannot fold"},
		{name: "a per-slice stage that loops", stages: cutYAML + designYAML + strings.Replace(cardsYAML, "earlier_slices", "loops_to: design\n    max_rounds: 2\n    earlier_slices", 1), want: "cannot loop"},
		{name: "earlier_slices on a whole-blueprint stage", stages: cutYAML + designYAML + cardsYAML + "  - id: middle\n    persona: po\n    earlier_slices: [design]\n", want: "is not per_slice"},
		{name: "earlier_slices naming a whole-blueprint stage", stages: cutYAML + strings.Replace(designYAML, "[design, cards]", "[cut]", 1), want: "is not a per-slice stage"},
		{name: "a shared stage without a gate", stages: cutYAML + "  - id: shared\n    persona: po\n    shared: true\n" + designYAML + cardsYAML, want: "needs a gate"},
		{name: "a shared stage before slicing", stages: sharedYAML + cutYAML + designYAML + cardsYAML, want: "sits between"},
		{name: "the slicing stage itself shared", stages: strings.Replace(cutYAML, "checks: [slices]", "checks: [slices]\n    shared: true", 1) + designYAML + cardsYAML, want: "sits between"},
		{name: "a shared stage after the per-slice stages", stages: cutYAML + designYAML + cardsYAML + sharedYAML, want: "sits between"},
		{name: "a shared stage without per-slice stages", stages: cutYAML + sharedYAML, want: "sits between"},
		{name: "a fold whose only later gate is a shared stage's", stages: cutYAML + sharedYAML + "  - id: cards\n    persona: po\n    per_slice: true\n", want: "no later stage has a gate to fold into"},
		{name: "an @ in a stage id", stages: "  - id: plain@x\n    persona: po\n", want: "@"},
		{
			name:   "a whole-blueprint cards stage after a per-slice acceptance",
			stages: cutYAML + strings.Replace(designYAML, "earlier_slices: [design, cards]", "checks: [criteria]", 1) + wholeCardsYAML,
			want:   `stage "cards" checks cards and stage "design" checks criteria`,
		},
		{
			name:   "a per-slice cards stage after a whole-blueprint acceptance",
			stages: cutYAML + wholeCriteriaYAML + strings.Replace(cardsYAML, "earlier_slices: [design, cards]", "tools: [code]\n    checks: [cards]", 1),
			want:   `stage "cards" checks cards and stage "design" checks criteria`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(slicedPack(tt.stages))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestStepKey(t *testing.T) {
	for _, tc := range []struct{ stage, slice, key string }{
		{"solution_design", "SL2", "solution_design@SL2"},
		{"intake", "", "intake"},
	} {
		assert.Equal(t, tc.key, StepKey(tc.stage, tc.slice))

		stage, slice := SplitStep(tc.key)
		assert.Equal(t, tc.stage, stage)
		assert.Equal(t, tc.slice, slice)
	}
}

func TestLoad_AcceptsTheOutcomesCheck(t *testing.T) {
	p, err := load(packFS("    checks: [outcomes]\n", nil))
	require.NoError(t, err)

	s1, _ := p.Stage("s1")
	assert.Equal(t, []string{CheckOutcomes}, s1.Checks)
}

func TestLoad_AcceptsTheReadinessCheck(t *testing.T) {
	p, err := load(packFS("    checks: [readiness]\n", nil))
	require.NoError(t, err)

	s1, _ := p.Stage("s1")
	assert.Equal(t, []string{CheckReadiness}, s1.Checks)
}

func TestLoadEmbedded_ListsTheEdgeCases(t *testing.T) {
	p, err := LoadEmbedded()
	require.NoError(t, err)
	assert.NotEmpty(t, p.Lists().EdgeCases, "acceptance sweeps the default pack's edge cases")
}

const (
	screenYAML = "  - id: need\n    persona: po\n    checks: [screen]\n"
	lookYAML   = "  - id: look\n    persona: po\n    consumes: [need]\n    tools: [code]\n    checks: [mockup]\n    policy: human\n"
)

// mockupPack is slicedPack with the screen and mockup stages' files and the
// mockup responder's instructions.
func mockupPack(stagesYAML string) fstest.MapFS {
	fsys := slicedPack(stagesYAML)
	for _, name := range []string{"stages/need.md", "stages/look.md", "stages/mockup_responder.md"} {
		fsys[name] = &fstest.MapFile{Data: []byte("instructions")}
	}

	return fsys
}

func TestLoad_MockupStage(t *testing.T) {
	p, err := load(mockupPack(screenYAML + lookYAML + cutYAML + designYAML + cardsYAML))
	require.NoError(t, err)
	assert.Equal(t, "look", p.MockupStage())
	assert.Equal(t, "need", p.ScreenStage())
	assert.NotEmpty(t, p.MockupResponderInstructions())

	none, err := load(packFS("", nil))
	require.NoError(t, err)
	assert.Empty(t, none.MockupStage())
	assert.Empty(t, none.ScreenStage())
}

func TestLoad_MockupStageInvalid(t *testing.T) {
	gated := strings.Replace(lookYAML, "    policy: human\n", "    gate: true\n    reviewer: {persona: po, threshold: 4}\n", 1)
	tests := []struct {
		name, stages string
		drop         string
		want         string
	}{
		{name: "no stage states the screen", stages: strings.Replace(lookYAML, "    consumes: [need]\n", "", 1), want: "screen"},
		{name: "the screen stage after it", stages: strings.Replace(lookYAML, "    consumes: [need]\n", "", 1) + screenYAML, want: "screen"},
		{name: "after slicing", stages: screenYAML + cutYAML + lookYAML + designYAML + cardsYAML, want: "before the stage that checks slices"},
		{
			name:   "checking slices itself",
			stages: screenYAML + strings.Replace(lookYAML, "checks: [mockup]", "checks: [mockup, slices]", 1) + designYAML + cardsYAML,
			want:   "before the stage that checks slices",
		},
		{
			name:   "checking the screen itself",
			stages: strings.Replace(strings.Replace(lookYAML, "    consumes: [need]\n", "", 1), "checks: [mockup]", "checks: [screen, mockup]", 1),
			want:   "earlier stage that checks screen",
		},
		{name: "a gate", stages: screenYAML + gated, want: "no gate"},
		{name: "per slice", stages: screenYAML + cutYAML + strings.Replace(lookYAML, "    policy: human\n", "    per_slice: true\n", 1), want: "mockup"},
		{name: "two of them", stages: screenYAML + lookYAML + strings.Replace(lookYAML, "id: look", "id: look2", 1), want: "one"},
		{name: "no responder instructions", stages: screenYAML + lookYAML, drop: "stages/mockup_responder.md", want: "stages/mockup_responder.md"},
		{name: "the reserved id", stages: screenYAML + "  - id: mockup_responder\n    persona: po\n", want: "reserved"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := mockupPack(tt.stages)
			fsys["stages/look2.md"] = &fstest.MapFile{Data: []byte("x")}
			delete(fsys, tt.drop)

			_, err := load(fsys)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func listsPack(t *testing.T) *Pack {
	t.Helper()

	p, err := load(packFS("", map[string]string{"lists.yaml": "edge_cases: [Limits]\ndangling_references: [see above]\nforbidden_references:\n  - {name: home, pattern: '/home/'}\n"}))
	require.NoError(t, err)

	return p
}

func TestMergeLists(t *testing.T) {
	tests := []struct {
		name      string
		overlays  []contract.Overlay
		edges     []string
		dangling  []string
		forbidden []string
		err       string
	}{
		{name: "no overlays", edges: []string{"Limits"}, dangling: []string{"see above"}, forbidden: []string{"home"}},
		{name: "pack first, then each overlay in order", overlays: []contract.Overlay{
			{EdgeCases: []string{"Clock skew"}, DanglingReferences: []string{"as before"}},
			{EdgeCases: []string{"Retries"}, ForbiddenReferences: []contract.Pattern{{Name: "tmp", Pattern: "/tmp/"}}},
		}, edges: []string{"Limits", "Clock skew", "Retries"}, dangling: []string{"see above", "as before"}, forbidden: []string{"home", "tmp"}},
		{name: "an edge case the pack lists, in other case and spacing, adds nothing", overlays: []contract.Overlay{{EdgeCases: []string{"  limits "}}}, edges: []string{"Limits"}, dangling: []string{"see above"}, forbidden: []string{"home"}},
		{name: "an edge case two overlays add is added once", overlays: []contract.Overlay{{EdgeCases: []string{"Retries"}}, {EdgeCases: []string{"retries"}}}, edges: []string{"Limits", "Retries"}, dangling: []string{"see above"}, forbidden: []string{"home"}},
		{name: "a blank edge case", overlays: []contract.Overlay{{EdgeCases: []string{" "}}}, err: "edge_cases"},
		{name: "a blank dangling reference", overlays: []contract.Overlay{{DanglingReferences: []string{" "}}}, err: "dangling_references"},
		{name: "a bad pattern", overlays: []contract.Overlay{{ForbiddenReferences: []contract.Pattern{{Name: "broken", Pattern: "(["}}}}, err: "forbidden reference"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := listsPack(t).MergeLists(tt.overlays)
			if tt.err != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.err)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.edges, got.EdgeCases)
			assert.Equal(t, tt.dangling, got.DanglingReferences)

			names := make([]string, 0, len(got.ForbiddenReferences))
			for _, f := range got.ForbiddenReferences {
				names = append(names, f.Name)
			}

			assert.Equal(t, tt.forbidden, names)
		})
	}
}

func TestMergeLists_LeavesThePacksListsAsTheyAre(t *testing.T) {
	p := listsPack(t)
	before := p.Lists()

	_, err := p.MergeLists([]contract.Overlay{{
		EdgeCases:           []string{"Retries"},
		DanglingReferences:  []string{"as before"},
		ForbiddenReferences: []contract.Pattern{{Name: "tmp", Pattern: "/tmp/"}},
	}})
	require.NoError(t, err)

	assert.Equal(t, before, p.Lists())
}

func TestLoad_RetroInstructionsAreOptional(t *testing.T) {
	p, err := load(packFS("", nil))
	require.NoError(t, err)
	assert.Empty(t, p.RetroInstructions())

	p, err = load(packFS("", map[string]string{"stages/retrospective.md": "judge", "personas/reviewer.md": "rev"}))
	require.NoError(t, err)
	assert.Equal(t, "judge", p.RetroInstructions())
	assert.Equal(t, "rev", p.Persona(RetroPersona))
}

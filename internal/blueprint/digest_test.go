package blueprint

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func storedAs(t *testing.T, data any) store.Artifact {
	t.Helper()

	raw, err := json.Marshal(data)
	require.NoError(t, err)

	return store.Artifact{Stage: "x@SL1", Version: 2, Body: "# body", Data: raw}
}

func hasTool(in stage.Input, name string) bool {
	return slices.ContainsFunc(in.Tools, func(t tools.Tool) bool { return t.Name() == name })
}

func toolNamed(t *testing.T, in stage.Input, name string) tools.Tool {
	t.Helper()

	i := slices.IndexFunc(in.Tools, func(t tools.Tool) bool { return t.Name() == name })
	require.GreaterOrEqual(t, i, 0, "the run has %s", name)

	return in.Tools[i]
}

func TestDigestOf(t *testing.T) {
	design := twoDecisions()
	design["slices"].([]any)[0].(map[string]any)["contracts"] = []any{"GET /reports answers CSV"}

	acceptance := sliceAcceptance("SL1", "AC1", "MC1")
	s := acceptance["slices"].([]any)[0].(map[string]any)
	s["criteria"] = append(s["criteria"].([]any), map[string]any{"id": "AC2", "project": "alpha", "text": "first line\nsecond line"})

	cards := draftOf("Export the reports", cardFor("D1", []any{"AC1"}), cardFor("D2", nil, "D1"))
	cards["manual"] = []any{map[string]any{"id": "M1", "text": "open the file\nin a spreadsheet"}}
	cards["setup"] = map[string]any{"alpha": map[string]any{"verify_command": "make test", "instruction_card": "D1"}}

	t.Run("a design keeps its decisions and contracts", func(t *testing.T) {
		d, ok := digestOf([]string{pack.CheckAssumptions, pack.CheckDecisions}, storedAs(t, design))
		require.True(t, ok)
		assert.Equal(t, []digestDecision{
			{ID: "DEC1", Text: "where the data lives", Chosen: "Postgres"},
			{ID: "DEC2", Text: "how it is deployed", Chosen: "the existing chart"},
		}, d.Decisions)
		assert.Equal(t, []string{"GET /reports answers CSV"}, d.Contracts)
	})

	t.Run("criteria and manual checks carry the outcomes they cite", func(t *testing.T) {
		traced := sliceAcceptance("SL1", "AC1", "MC1")
		sl := traced["slices"].([]any)[0].(map[string]any)
		sl["criteria"].([]any)[0].(map[string]any)["outcomes"] = []any{"O1", " O2 "}
		sl["manual"].([]any)[0].(map[string]any)["outcomes"] = []any{"O3"}

		d, ok := digestOf([]string{pack.CheckCriteria}, storedAs(t, traced))
		require.True(t, ok)
		assert.Equal(t, []string{"O1", "O2"}, d.Criteria[0].Outcomes)
		assert.Equal(t, []string{"O3"}, d.Manual[0].Outcomes)
	})

	t.Run("a malformed trace reads as none and is no misfit", func(t *testing.T) {
		traced := sliceAcceptance("SL1", "AC1", "MC1")
		traced["slices"].([]any)[0].(map[string]any)["criteria"].([]any)[0].(map[string]any)["outcomes"] = "O1"

		d, ok := digestOf([]string{pack.CheckCriteria}, storedAs(t, traced))
		require.True(t, ok)
		assert.Empty(t, d.Criteria[0].Outcomes)
	})

	t.Run("contracts at the top level are kept", func(t *testing.T) {
		d, ok := digestOf([]string{pack.CheckDecisions}, storedAs(t, map[string]any{"contracts": []any{"GET /reports answers CSV"}}))
		require.True(t, ok)
		assert.Equal(t, []string{"GET /reports answers CSV"}, d.Contracts)
	})

	t.Run("decisions at the top level are kept with the contracts", func(t *testing.T) {
		d, ok := digestOf([]string{pack.CheckDecisions}, storedAs(t, map[string]any{
			"decisions": []any{map[string]any{"id": "DEC1", "text": "where the data lives", "chosen": "Postgres"}},
			"contracts": []any{"GET /reports answers CSV"},
		}))
		require.True(t, ok)
		assert.Equal(t, []digestDecision{{ID: "DEC1", Text: "where the data lives", Chosen: "Postgres"}}, d.Decisions)
		assert.Equal(t, []string{"GET /reports answers CSV"}, d.Contracts)
	})

	t.Run("a design's top-level outcomes are kept with their ids and text", func(t *testing.T) {
		withOutcomes := twoDecisions()
		withOutcomes["outcomes"] = []any{outcomeItem("O3", "A page loads within 2 s.\nOn a slow link.")}

		d, ok := digestOf([]string{pack.CheckAssumptions, pack.CheckDecisions, pack.CheckOutcomes}, storedAs(t, withOutcomes))
		require.True(t, ok)
		assert.Equal(t, []digestLine{{ID: "O3", Text: "A page loads within 2 s."}}, d.Outcomes)
	})

	t.Run("an acceptance keeps every id with its first line", func(t *testing.T) {
		d, ok := digestOf([]string{pack.CheckCriteria}, storedAs(t, acceptance))
		require.True(t, ok)
		assert.Equal(t, []digestLine{{ID: "AC1", Text: "the export command writes one file per report"}, {ID: "AC2", Text: "first line"}}, d.Criteria)
		assert.Equal(t, []digestLine{{ID: "MC1", Text: "open an exported file in a spreadsheet"}}, d.Manual)
	})

	t.Run("cards keep what a later slice depends on and must not repeat", func(t *testing.T) {
		art := storedAs(t, cards)
		d, ok := digestOf([]string{pack.CheckCards}, art)
		require.True(t, ok)
		assert.Equal(t, []digestCard{
			{ID: "D1", Title: "Add export step D1", Project: "alpha", Type: "feature", Where: []string{"internal/x.go"}, DependsOn: []string{}, Criteria: []string{"AC1"}},
			{ID: "D2", Title: "Add export step D2", Project: "alpha", Type: "feature", Where: []string{"internal/x.go"}, DependsOn: []string{"D1"}},
		}, d.Cards)
		assert.Equal(t, []digestLine{{ID: "M1", Text: "open the file"}}, d.Manual)
		assert.Equal(t, map[string]contract.Setup{"alpha": {VerifyCommand: "make test", InstructionCard: "D1"}}, d.Setup)

		raw, err := json.Marshal(d)
		require.NoError(t, err)
		assert.Less(t, len(raw), len(art.Data), "the card bodies stay out")
	})

	for _, tt := range []struct {
		name   string
		checks []string
		art    store.Artifact
	}{
		{"no digest shape: grounding reads whole", nil, storedAs(t, map[string]any{"facts": []any{"a"}})},
		{"no data", []string{pack.CheckCards}, store.Artifact{Version: 1, Body: "# Cards"}},
		{"data that does not decode", []string{pack.CheckCards}, store.Artifact{Version: 1, Data: []byte("not json")}},
		{"decisions of another shape", []string{pack.CheckDecisions}, storedAs(t, map[string]any{"slices": []any{map[string]any{"decisions": []any{map[string]any{"question": "x", "answer": "y"}}}}})},
		{"criteria of another shape", []string{pack.CheckCriteria}, storedAs(t, map[string]any{"slices": []any{map[string]any{"criteria": []any{map[string]any{"criterion": "x"}}}}})},
		{"cards of another shape", []string{pack.CheckCards}, storedAs(t, map[string]any{"cards": []any{map[string]any{"name": "x"}}})},
		{"one decision fits and one does not", []string{pack.CheckDecisions}, storedAs(t, map[string]any{"slices": []any{map[string]any{"decisions": []any{map[string]any{"id": "DEC1", "text": "t", "chosen": "c"}, map[string]any{"question": "x"}}}}})},
		{"contracts but decisions of another shape", []string{pack.CheckDecisions}, storedAs(t, map[string]any{"slices": []any{map[string]any{"contracts": []any{"GET /x"}, "decisions": []any{map[string]any{"question": "x"}}}}})},
		{"a decision with an id and nothing else", []string{pack.CheckDecisions}, storedAs(t, map[string]any{"slices": []any{map[string]any{"decisions": []any{map[string]any{"id": "DEC1"}}}}})},
		{"one card fits and one does not", []string{pack.CheckCards}, storedAs(t, map[string]any{"cards": []any{map[string]any{"id": "D1", "title": "t"}, map[string]any{"name": "x"}}})},
		{"another shape leaves nothing to digest", []string{pack.CheckDecisions}, storedAs(t, map[string]any{"slices": []any{}})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := digestOf(tt.checks, tt.art)
			assert.False(t, ok, "the step reads the whole artifact")
		})
	}
}

func TestOneLine(t *testing.T) {
	long := strings.Repeat("a", 250)

	assert.Equal(t, "first", oneLine("  first\nsecond"))
	assert.Equal(t, strings.Repeat("a", 200)+"...", oneLine(long))
}

// designsWithContracts is slicedScript with every slice's design carrying a
// decision and a contract that names the slice.
func designsWithContracts() stageScript {
	script := slicedScript()
	script["solution_design"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		d := sliceDesign(sliceOf(in))
		d["slices"].([]any)[0].(map[string]any)["contracts"] = []any{"the export file of " + sliceOf(in) + " is CSV"}

		return done("# Design "+sliceOf(in), d), nil
	}

	return script
}

func TestSlices_ALaterSliceReadsTheEarlierSlicesAsDigests(t *testing.T) {
	c, _, runner, _ := newSliced(t, designsWithContracts())
	driveIn(t, c, autoStart("BP1"))

	first := func(key string) stage.Input {
		ins := inputsOf(runner, key)
		require.NotEmpty(t, ins, key)

		return ins[0]
	}

	for _, key := range []string{"solution_design@SL2", "solution_design@SL2.review", "grounding@SL2"} {
		in := first(key)
		assert.NotContains(t, in.Task, "# Design SL1", "%s: an earlier slice's body is not given", key)
		assert.Contains(t, in.Task, "the export file of SL1 is CSV", "%s: its digest keeps the contracts", key)
		assert.True(t, hasTool(in, readArtifactName), "%s: and the run can read the whole", key)
	}

	assert.Contains(t, first("grounding@SL2").Task, "# Design SL2", "a slice's own design is given whole")
	assert.False(t, hasTool(first("solution_design@SL1"), readArtifactName), "the first slice reads no digest")

	cards2 := first("decomposition@SL2").Task
	assert.NotContains(t, cards2, "TestThing.", "an earlier slice's card bodies are not given")
	assert.Contains(t, cards2, "Add export step D1", "its digest keeps the card titles")

	ready := first("readiness").Task
	assert.Equal(t, 2, strings.Count(ready, "TestThing."), "readiness reads every slice's cards whole")
	assert.NotContains(t, ready, "# Design SL1", "and the designs as digests")
	assert.True(t, hasTool(first("readiness"), readArtifactName), "and the run can read the whole")
	assert.Contains(t, ready, "the export file of SL1 is CSV")
}

func TestSlices_OneSliceReadsAsBefore(t *testing.T) {
	script := designsWithContracts()
	script["slicing"] = returns("# Slices", oneSlice())

	c, _, runner, _ := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))

	ready := inputsOf(runner, "readiness")
	require.NotEmpty(t, ready)
	assert.Contains(t, ready[0].Task, "# Design SL1", "with one slice every artifact is given whole")
	assert.False(t, hasTool(ready[0], readArtifactName))
}

func TestReadArtifact(t *testing.T) {
	c, _, runner, _ := newSliced(t, designsWithContracts())
	driveIn(t, c, autoStart("BP1"))

	design2 := inputsOf(runner, "solution_design@SL2")
	require.NotEmpty(t, design2)

	reader := toolNamed(t, design2[0], readArtifactName)

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"an earlier slice's latest design, whole", map[string]any{"stage": "solution_design", "slice": "SL1"}, "# Design SL1"},
		{"a slice id in any case", map[string]any{"stage": "solution_design", "slice": "sl1"}, "# Design SL1"},
		{"a whole-blueprint stage it reads", map[string]any{"stage": "slicing"}, "# Slices"},
		{"a stored version", map[string]any{"stage": "solution_design", "slice": "SL1", "version": float64(1)}, "# Design SL1"},
		{"a version not stored", map[string]any{"stage": "solution_design", "slice": "SL1", "version": float64(9)}, ""},
		{"a step the run does not read", map[string]any{"stage": "grounding", "slice": "SL1"}, ""},
		{"a version that is no whole number", map[string]any{"stage": "slicing", "version": 1.5}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := reader.Execute(context.Background(), tt.args)
			if tt.want == "" {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Contains(t, res.Text, tt.want)
		})
	}
}

func TestReadArtifact_TheRecordKeepsTheFirstLine(t *testing.T) {
	var r artifactReader

	head := readMarker + "solution_design@SL1 version 3"
	assert.Equal(t, head, r.Stored(head+"\n\n## solution_design@SL1\n\nthe body"))
	assert.Equal(t, r.Stored(head), r.Stored(r.Stored(head+"\n\nbody")), "storing again changes nothing")
	assert.Equal(t, "an error text", r.Stored("an error text"), "anything else is kept as it is")
}

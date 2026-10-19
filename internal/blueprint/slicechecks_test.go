package blueprint

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// slicedBlueprint starts BP1 on the sliced pack with slicing's two slices
// stored, as the per-slice checks find them.
func slicedBlueprint(t *testing.T) (*Controller, *store.Store, store.Blueprint) {
	t.Helper()

	c, st, _, _ := newSliced(t, slicedScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	putData(t, st, "slicing", twoSlices())

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	return c, st, bp
}

// sliceDesign is goodDesign for slice.
func sliceDesign(slice string) map[string]any {
	d := goodDesign("assumption")
	d["slices"].([]any)[0].(map[string]any)["id"] = slice

	return d
}

// sharedDecisions is a shared stage's one list of decisions.
func sharedDecisions() map[string]any {
	return map[string]any{"decisions": []any{map[string]any{
		"id": "DEC1", "text": "how exported files are named", "chosen": "a dated name",
		"rejected": []any{map[string]any{"option": "a running number", "why": "it says nothing about the content"}}, "basis": "assumption",
	}}}
}

// sliceAcceptance is one slice's acceptance with one criterion and one
// manual check.
func sliceAcceptance(slice, criterion, manual string) map[string]any {
	return map[string]any{"slices": []any{map[string]any{
		"id":       slice,
		"criteria": []any{map[string]any{"id": criterion, "project": "alpha", "text": "the export command writes one file per report"}},
		"manual":   []any{map[string]any{"id": manual, "text": "open an exported file in a spreadsheet"}},
	}}}
}

// refusedFor runs validate on data and checks it is refused by the check
// named want, or stands when want is "".
func refusedFor(t *testing.T, validate func(context.Context, stage.Artifact) error, data map[string]any, want string) {
	t.Helper()

	err := validate(context.Background(), stage.Artifact{Body: "# x", Data: data})
	if want == "" {
		require.NoError(t, err)

		return
	}

	require.Error(t, err)
	assert.Contains(t, err.Error(), ": "+want+": ")
}

func TestDecisions_ASliceDesignCoversItsOwnSliceAlone(t *testing.T) {
	c, _, bp := slicedBlueprint(t)

	both := sliceDesign("SL2")
	both["slices"] = append(both["slices"].([]any), sliceDesign("SL1")["slices"].([]any)[0])

	tests := []struct {
		name string
		data map[string]any
		want string
	}{
		{name: "its own slice", data: sliceDesign("SL2")},
		{name: "another slice instead", data: sliceDesign("SL1"), want: "slice_missing"},
		{name: "another slice as well", data: both, want: "slice_extra"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validate, _ := c.decisionsValidator(bp, "solution_design@SL2")
			refusedFor(t, validate, tt.data, tt.want)
		})
	}
}

func TestDecisions_TheSharedDecisionsAreOneList(t *testing.T) {
	c, _, bp := slicedBlueprint(t)

	unknown := sharedDecisions()
	unknown["decisions"].([]any)[0].(map[string]any)["basis"] = "Q9"

	emptySlices := sharedDecisions()
	emptySlices["slices"] = []any{}

	withContracts := func(v any) map[string]any {
		d := sharedDecisions()
		d["contracts"] = v

		return d
	}

	tests := []struct {
		name string
		data map[string]any
		want string
	}{
		{name: "one list", data: sharedDecisions()},
		{name: "per slice instead", data: sliceDesign("SL1"), want: "shared_slices"},
		{name: "an empty slices list as well", data: emptySlices, want: "shared_slices"},
		{name: "contracts as a list of strings", data: withContracts([]any{"every file is UTF-8"})},
		{name: "contracts as a number", data: withContracts(3), want: "contracts"},
		{name: "a contract that is an object", data: withContracts([]any{map[string]any{"a": 1}}), want: "contracts"},
		{name: "none", data: map[string]any{"decisions": []any{}}, want: "no_decisions"},
		{name: "a basis nobody knows", data: unknown, want: "basis"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validate, _ := c.decisionsValidator(bp, "shared_decisions")
			refusedFor(t, validate, tt.data, tt.want)
		})
	}
}

func TestDecisions_ASliceOwesOnlyItsOwnAnswers(t *testing.T) {
	c, st, bp := slicedBlueprint(t)

	require.NoError(t, st.AddQuestion(context.Background(), store.Question{
		BlueprintID: "BP1", RunID: 99, Stage: "solution_design@SL2", Number: 1, Text: "CSV or JSON?", Answer: "JSON",
		Responder: store.ResponderHuman, AnsweredAt: time.Now().UTC(),
	}))

	first, _ := c.decisionsValidator(bp, "solution_design@SL1")
	refusedFor(t, first, sliceDesign("SL1"), "")

	second, _ := c.decisionsValidator(bp, "solution_design@SL2")
	refusedFor(t, second, sliceDesign("SL2"), "unused_answer")
}

func TestCriteria_ASliceAcceptanceCoversItsSliceWithIDsOfItsOwn(t *testing.T) {
	c, st, bp := slicedBlueprint(t)
	putData(t, st, "acceptance@SL1", sliceAcceptance("SL1", "AC1", "MC1"))

	tests := []struct {
		name string
		data map[string]any
		want string
	}{
		{name: "ids that run on", data: sliceAcceptance("SL2", "AC2", "MC2")},
		{name: "a criterion id SL1 holds", data: sliceAcceptance("SL2", "AC1", "MC2"), want: "criterion_id"},
		{name: "a manual check id SL1 holds", data: sliceAcceptance("SL2", "AC2", "MC1"), want: "manual_id"},
		{name: "another slice", data: sliceAcceptance("SL1", "AC2", "MC2"), want: "slice_extra"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validate, _ := c.criteriaValidator(bp, "acceptance@SL2")
			refusedFor(t, validate, tt.data, tt.want)
		})
	}

	first, _ := c.criteriaValidator(bp, "acceptance@SL1")
	refusedFor(t, first, sliceAcceptance("SL1", "AC1", "MC1"), "")
}

func TestAssumptions_NumberAcrossTheSlicesInPipelineOrder(t *testing.T) {
	c, st, _ := slicedBlueprint(t)

	for _, a := range []struct{ key, text string }{
		{"solution_design@SL2", "Exports run nightly."},
		{"shared_decisions", "Every export is UTF-8."},
		{"solution_design@SL1", "One file per report."},
	} {
		putData(t, st, a.key, map[string]any{"assumptions": []any{a.text}})
	}

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)

	got := make([]string, 0, len(list))
	for _, a := range list {
		got = append(got, a.ID+" "+a.Stage)
	}

	assert.Equal(t, []string{"A1 shared_decisions", "A2 solution_design@SL1", "A3 solution_design@SL2"}, got,
		"numbered in pipeline order, whatever order they were stored in")
}

func TestSlices_EverySliceGetsItsOwnAcceptance(t *testing.T) {
	c, _, runner, _ := newSliced(t, slicedScript())
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, []string{"acceptance@SL1", "acceptance@SL2"}, stepRuns(runner, "acceptance"))

	acc2 := inputsOf(runner, "acceptance@SL2")
	require.Len(t, acc2, 1)
	assert.Contains(t, acc2[0].Task, "# Acceptance SL1", "a later slice reads the earlier slices' acceptance")
}

func TestAssumptions_ACollapsedSharedStageStatesNone(t *testing.T) {
	c, st, _ := slicedBlueprint(t)
	ctx := context.Background()

	putData(t, st, "shared_decisions", map[string]any{"assumptions": []any{"Every export is UTF-8."}})
	putData(t, st, "slicing", oneSlice())
	putData(t, st, "solution_design@SL1", map[string]any{"assumptions": []any{"One file per report."}})

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "A1", list[0].ID)
	assert.Equal(t, "solution_design@SL1", list[0].Stage)
}

func TestSlices_EachRunValidatesAgainstItsOwnStep(t *testing.T) {
	script := slicedScript()
	script["solution_design"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		slice := sliceOf(in)
		other := "SL1"

		if slice == other {
			other = "SL2"
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Design", Data: sliceDesign(slice)}); err != nil {
			return stage.Output{}, err
		}

		if in.Validate(ctx, stage.Artifact{Body: "# Design", Data: sliceDesign(other)}) == nil {
			return stage.Output{}, errors.New("another slice's design was accepted")
		}

		return done("# Design "+slice, nil), nil
	}
	script["acceptance"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		slice := sliceOf(in)
		other := "SL1"

		if slice == other {
			other = "SL2"
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Acceptance", Data: sliceAcceptance(slice, "AC"+slice, "MC"+slice)}); err != nil {
			return stage.Output{}, err
		}

		if in.Validate(ctx, stage.Artifact{Body: "# Acceptance", Data: sliceAcceptance(other, "AC"+other, "MC"+other)}) == nil {
			return stage.Output{}, errors.New("another slice's acceptance was accepted")
		}

		return done("# Acceptance "+slice, nil), nil
	}

	c, _, runner, _ := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, []string{"acceptance@SL1", "acceptance@SL2"}, stepRuns(runner, "acceptance"))
}

// thirdSubmit is validate's answer to data submitted three times; the first
// two must be refused, or the third is no last submit.
func thirdSubmit(t *testing.T, validate func(context.Context, stage.Artifact) error, data map[string]any) error {
	t.Helper()

	for range maxContractRounds - 1 {
		require.Error(t, validate(context.Background(), stage.Artifact{Body: "# x", Data: data}))
	}

	return validate(context.Background(), stage.Artifact{Body: "# x", Data: data})
}

func TestFatalChecks_TheLastSubmitIsRefusedOnlyOverWhatLeavesNothingToBuildOn(t *testing.T) {
	c, _, bp := slicedBlueprint(t)

	slicing := func() func(context.Context, stage.Artifact) error {
		v, _ := c.slicesValidator(bp)

		return v
	}
	design := func() func(context.Context, stage.Artifact) error {
		v, _ := c.decisionsValidator(bp, "solution_design@SL2")

		return v
	}
	criteria := func() func(context.Context, stage.Artifact) error {
		v, _ := c.criteriaValidator(bp, "acceptance@SL2")

		return v
	}

	untitled := twoSlices()
	untitled["slices"].([]any)[1].(map[string]any)["title"] = " "

	both := sliceDesign("SL2")
	both["slices"] = append(both["slices"].([]any), sliceDesign("SL1")["slices"].([]any)[0])

	empty := sliceAcceptance("SL2", "AC2", "MC2")
	empty["slices"].([]any)[0].(map[string]any)["criteria"] = []any{}

	tests := []struct {
		name     string
		validate func(context.Context, stage.Artifact) error
		data     map[string]any
		fatal    string // the check refused for good; "" when the third submit is stored
	}{
		{"slicing with no slices", slicing(), map[string]any{"slices": []any{}}, "no_slices"},
		{"slicing that is no list", slicing(), map[string]any{"slices": "two"}, "shape"},
		{"a slice without a title", slicing(), untitled, ""},
		{"a design without its slice", design(), sliceDesign("SL1"), "slice_missing"},
		{"a design with no slices", design(), map[string]any{"slices": []any{}}, "no_slices"},
		{"a design that also covers another slice", design(), both, ""},
		{"a design that is no list", design(), map[string]any{"slices": "two"}, "shape"},
		{"acceptance that is no list", criteria(), map[string]any{"slices": "two"}, "shape"},
		{"acceptance with no slices", criteria(), map[string]any{"slices": []any{}}, "no_slices"},
		{"acceptance with an empty slice", criteria(), empty, "slice_empty"},
		{"acceptance without its slice", criteria(), sliceAcceptance("SL1", "AC2", "MC2"), "slice_missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := thirdSubmit(t, tt.validate, tt.data)

			if tt.fatal == "" {
				require.NoError(t, err)

				return
			}

			var refused *stage.FatalCheckError
			require.ErrorAs(t, err, &refused)
			assert.True(t, slices.ContainsFunc(refused.Problems, func(p string) bool { return strings.Contains(p, ": "+tt.fatal+": ") }),
				"refused over %s: %v", tt.fatal, refused.Problems)
		})
	}
}

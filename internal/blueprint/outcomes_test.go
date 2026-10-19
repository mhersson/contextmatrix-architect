package blueprint

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func outcomeItem(id, text string) map[string]any { return map[string]any{"id": id, "text": text} }

func TestOutcomeProblems(t *testing.T) {
	earlier := []outcomeEntry{{ID: "O1", Text: "An import of 10,000 rows loses none."}, {ID: "O2", Text: "A search answers within 200 ms."}}

	tests := []struct {
		name    string
		data    map[string]any
		earlier []outcomeEntry
		first   bool
		want    []string // the check names refused, in order
	}{
		{name: "the first states its outcomes", data: map[string]any{"outcomes": []any{outcomeItem("O1", "An upgrade keeps last year's config readable.")}}, first: true},
		{name: "a quality is a word", data: map[string]any{"outcomes": []any{map[string]any{"id": "O3", "text": "Sessions expire after 30 minutes idle.", "quality": "security"}}}, earlier: earlier},
		{name: "the first states none", data: map[string]any{}, first: true, want: []string{"no_outcomes"}},
		{name: "the first states an empty list", data: map[string]any{"outcomes": []any{}}, first: true, want: []string{"no_outcomes"}},
		{name: "a later step may state none", data: map[string]any{}, earlier: earlier},
		{name: "not a list", data: map[string]any{"outcomes": "O1"}, want: []string{"outcome_shape"}},
		{name: "an entry that is no object", data: map[string]any{"outcomes": []any{"O3"}}, earlier: earlier, want: []string{"outcome_shape"}},
		{name: "a quality that is no word", data: map[string]any{"outcomes": []any{map[string]any{"id": "O3", "text": "x", "quality": 3}}}, earlier: earlier, want: []string{"outcome_shape"}},
		{name: "an id of another form", data: map[string]any{"outcomes": []any{outcomeItem("OUT3", "x")}}, earlier: earlier, want: []string{"outcome_id"}},
		{name: "an id an earlier step holds", data: map[string]any{"outcomes": []any{outcomeItem("O2", "x")}}, earlier: earlier, want: []string{"outcome_id"}},
		{name: "an id below the highest", data: map[string]any{"outcomes": []any{outcomeItem("O3", "x")}}, earlier: []outcomeEntry{{ID: "O5", Text: "x"}}, want: []string{"outcome_id"}},
		{name: "an id twice in the submit", data: map[string]any{"outcomes": []any{outcomeItem("O3", "x"), outcomeItem("O3", "y")}}, earlier: earlier, want: []string{"outcome_id"}},
		{name: "a leading zero is another form", data: map[string]any{"outcomes": []any{outcomeItem("O01", "x")}}, earlier: earlier, want: []string{"outcome_id"}},
		{name: "an id too large to number", data: map[string]any{"outcomes": []any{outcomeItem("O99999999999999999999", "x")}}, earlier: earlier, want: []string{"outcome_id"}},
		{name: "an entry with no id", data: map[string]any{"outcomes": []any{map[string]any{"text": "x"}}}, earlier: earlier, want: []string{"outcome_shape"}},
		{name: "a later stage states an empty list", data: map[string]any{"outcomes": []any{}}, earlier: earlier},
		{name: "outcomes inside a slice entry", data: map[string]any{"slices": []any{map[string]any{"id": "SL1", "outcomes": []any{outcomeItem("O3", "x")}}}}, earlier: earlier, want: []string{"outcome_shape"}},
		{name: "no text", data: map[string]any{"outcomes": []any{outcomeItem("O3", " ")}}, earlier: earlier, want: []string{"outcome_text"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := outcomeProblems(tt.data, tt.earlier, tt.first)
			require.Len(t, got, len(tt.want), "%v", got)

			for i, name := range tt.want {
				assert.Contains(t, got[i], ": "+name+": ")
			}
		})
	}
}

func TestOutcomesBefore_ReadsTheStepsBeforeItInPipelineOrder(t *testing.T) {
	c, st, _ := slicedBlueprint(t)
	c.opts.Pack = withOutcomes(c.opts.Pack, "intake", "shared_decisions", "solution_design")

	putData(t, st, "solution_design@SL2", map[string]any{"outcomes": []any{outcomeItem("O4", "SL2's quality.")}})
	putData(t, st, "intake", map[string]any{"outcomes": []any{outcomeItem("O1", "The need's outcome.")}})
	putData(t, st, "solution_design@SL1", map[string]any{"outcomes": []any{outcomeItem("O3", "SL1's quality.")}})
	putData(t, st, "shared_decisions", map[string]any{"outcomes": []any{outcomeItem("O2", "A shared quality.")}})

	ids := func(key string) []string {
		list, err := c.outcomesBefore(context.Background(), "BP1", key)
		require.NoError(t, err)

		out := []string{}
		for _, o := range list {
			out = append(out, o.ID)
		}

		return out
	}

	assert.Equal(t, []string{"O1", "O2", "O3"}, ids("solution_design@SL2"), "every earlier step, never its own or a later one")
	assert.Equal(t, []string{"O1", "O2", "O3", "O4"}, ids("readiness"))
	assert.Equal(t, []string{}, ids("intake"))
}

func TestOutcomesBefore_SkipsAStepThatIsCollapsedOrDoesNotCheckThem(t *testing.T) {
	c, st, _ := slicedBlueprint(t)
	c.opts.Pack = withOutcomes(c.opts.Pack, "intake", "shared_decisions")
	ctx := context.Background()

	putData(t, st, "intake", map[string]any{"outcomes": []any{outcomeItem("O1", "The need's outcome.")}})
	putData(t, st, "shared_decisions", map[string]any{"outcomes": []any{outcomeItem("O2", "A shared quality.")}})
	putData(t, st, "solution_design@SL1", map[string]any{"outcomes": []any{outcomeItem("O3", "A design the pack does not check.")}})

	list, err := c.outcomesBefore(ctx, "BP1", "readiness")
	require.NoError(t, err)
	assert.Equal(t, []outcomeEntry{{ID: "O1", Text: "The need's outcome."}, {ID: "O2", Text: "A shared quality."}}, list, "solution_design does not check outcomes")

	putData(t, st, "slicing", oneSlice())

	list, err = c.outcomesBefore(ctx, "BP1", "readiness")
	require.NoError(t, err)
	assert.Equal(t, []outcomeEntry{{ID: "O1", Text: "The need's outcome."}}, list, "a collapsed shared step does not run")
}

func TestOutcomesOf_AnEntryOfAnotherShapeKeepsItsIdReserved(t *testing.T) {
	art := store.Artifact{Data: []byte(`{"outcomes": [{"id": "O1", "text": "x", "quality": 3}, {"text": "no id"}, {"id": " O2 ", "text": 7}, "O3"]}`)}

	assert.Equal(t, []outcomeEntry{{ID: "O1", Text: "x"}, {ID: "O2"}}, outcomesOf(art))
}

func TestOutcomes_AStageThatChecksThemIsRefusedItsFaults(t *testing.T) {
	script := dialogScript()

	var got, fixed error

	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		got = in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"slices": 1}})
		fixed = in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"slices": 1, "outcomes": []any{outcomeItem("O1", "x")}}})

		return done("# Need", map[string]any{"slices": 1, "outcomes": []any{outcomeItem("O1", "x")}}), nil
	}

	c, st, _, _, _ := newDialog(t, script)
	c.opts.Pack = withOutcomes(c.opts.Pack, "intake")

	drive(t, c, "BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), ": no_outcomes: ")
	require.NoError(t, fixed, "outcomes that stand pass the wired check")

	art, err := st.LatestArtifact(context.Background(), "BP1", "intake")
	require.NoError(t, err)

	var data struct {
		Outcomes []outcomeEntry `json:"outcomes"`
	}

	require.NoError(t, json.Unmarshal(art.Data, &data))
	assert.Equal(t, []outcomeEntry{{ID: "O1", Text: "x"}}, data.Outcomes)
}

// withOutcomes adds the outcomes check to the stages ids name, in memory.
func withOutcomes(pk *pack.Pack, ids ...string) *pack.Pack {
	for i := range pk.Stages {
		for _, id := range ids {
			if pk.Stages[i].ID == id {
				pk.Stages[i].Checks = append(pk.Stages[i].Checks, pack.CheckOutcomes)
			}
		}
	}

	return pk
}

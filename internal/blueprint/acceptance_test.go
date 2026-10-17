package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func goodAcceptance() map[string]any {
	return map[string]any{"slices": []any{map[string]any{
		"id":       "SL1",
		"criteria": []any{map[string]any{"id": "AC1", "project": "alpha", "text": "GET /version answers 200 with the commit."}},
		"manual":   []any{},
	}}}
}

func TestAcceptance_AnAutoGateAndCardsThatCarryItsCriteria(t *testing.T) {
	script := dialogScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()})
		if err == nil || !strings.Contains(err.Error(), "AC1: criterion_unmapped") {
			return stage.Output{}, fmt.Errorf("cards that carry no criterion are refused: %v", err)
		}

		carried := goodCards()
		carried["cards"].([]any)[0].(map[string]any)["criteria"] = []any{"AC1"}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: carried}); err != nil {
			return stage.Output{}, fmt.Errorf("cards that carry every criterion pass: %w", err)
		}

		return done("# Cards", carried), nil
	}

	c, st, runner, rec, _ := newDialog(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "acceptance.review"), 1)

	gates, err := st.ListGates(ctx, "BP1", "acceptance")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder, "acceptance's gate is auto by default")

	cards, err := st.LatestArtifact(ctx, "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(cards))
	assert.True(t, rec.has(EventSignOff))
}

func TestCriteriaValidator(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	slicing, err := json.Marshal(oneSlice())
	require.NoError(t, err)

	_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "slicing", Body: "# Slices", Data: slicing})
	require.NoError(t, err)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	slice := func(d map[string]any) map[string]any { return d["slices"].([]any)[0].(map[string]any) }

	tests := []struct {
		name   string
		change func(d map[string]any)
		want   string
	}{
		{name: "it stands", change: func(map[string]any) {}},
		{name: "no slices", change: func(d map[string]any) { d["slices"] = []any{} }, want: "no_slices"},
		{name: "a slice slicing did not cut", change: func(d map[string]any) { slice(d)["id"] = "SL9" }, want: "slice_missing"},
		{name: "a repeated criterion id", change: func(d map[string]any) {
			slice(d)["criteria"] = append(slice(d)["criteria"].([]any), map[string]any{"id": "AC1", "project": "alpha", "text": "again"})
		}, want: "criterion_id"},
		{name: "a project the blueprint does not target", change: func(d map[string]any) {
			slice(d)["criteria"].([]any)[0].(map[string]any)["project"] = "beta"
		}, want: "criterion_project"},
		{name: "a slice with no criterion", change: func(d map[string]any) { slice(d)["criteria"] = []any{} }, want: "slice_empty"},
		{name: "a slice with only manual checks", change: func(d map[string]any) {
			slice(d)["criteria"] = []any{}
			slice(d)["manual"] = []any{map[string]any{"id": "MC1", "text": "look"}}
		}, want: "slice_empty"},
		{name: "data that is not slices", change: func(d map[string]any) { d["slices"] = "none" }, want: "shape"},
		{name: "a repeated manual id", change: func(d map[string]any) {
			slice(d)["manual"] = []any{map[string]any{"id": "MC1", "text": "a"}, map[string]any{"id": "MC1", "text": "b"}}
		}, want: "manual_id"},
		{name: "an empty manual id", change: func(d map[string]any) {
			slice(d)["manual"] = []any{map[string]any{"id": "", "text": "a"}}
		}, want: "manual_id"},
		{name: "a criterion without text", change: func(d map[string]any) {
			slice(d)["criteria"].([]any)[0].(map[string]any)["text"] = " "
		}, want: "criterion_text"},
		{name: "a manual check without text", change: func(d map[string]any) {
			slice(d)["manual"] = []any{map[string]any{"id": "MC1", "text": ""}}
		}, want: "manual_text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := goodAcceptance()
			tt.change(d)

			validate, _ := c.criteriaValidator(bp, "acceptance")

			err := validate(ctx, stage.Artifact{Body: "# Acceptance", Data: d})
			if tt.want == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), ": "+tt.want+": ")
		})
	}
}

func TestAcceptance_UnreadableDataIsAFindingNotAStall(t *testing.T) {
	script := dialogScript()
	script["acceptance"] = returns("# Acceptance", map[string]any{"slices": "none"})

	var got error

	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		got = in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()})

		return done("# Cards", goodCards()), nil
	}

	c, _, _, _, _ := newDialog(t, script)

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), "acceptance: criteria_unreadable: ")
	assert.NotContains(t, got.Error(), "were not judged")
}

func TestAcceptance_TheSubmitIsRefusedItsFaults(t *testing.T) {
	script := dialogScript()

	var got error

	script["acceptance"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		bad := goodAcceptance()
		bad["slices"].([]any)[0].(map[string]any)["criteria"] = []any{}
		got = in.Validate(ctx, stage.Artifact{Body: "# Acceptance", Data: bad})

		return done("# Acceptance", goodAcceptance()), nil
	}

	c, _, _, _, _ := newDialog(t, script)

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), ": slice_empty: ")
}

func TestAcceptance_AManualCheckNeedsAnEntryThatCarriesIt(t *testing.T) {
	acceptance := goodAcceptance()
	acceptance["slices"].([]any)[0].(map[string]any)["manual"] = []any{map[string]any{"id": "MC1", "text": "check the dashboard"}}

	script := dialogScript()
	script["acceptance"] = returns("# Acceptance", acceptance)

	var got error

	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		carried := goodCards()
		carried["cards"].([]any)[0].(map[string]any)["criteria"] = []any{"AC1"}
		got = in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: carried})

		return done("# Cards", carried), nil
	}

	c, _, _, _, _ := newDialog(t, script)

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), "MC1: manual_unmapped: ")
}

func TestAcceptance_NoCriterionAtAllIsAFinding(t *testing.T) {
	script := dialogScript()
	script["acceptance"] = returns("# Acceptance", map[string]any{"slices": []any{}})

	var got error

	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		got = in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()})

		return done("# Cards", goodCards()), nil
	}

	c, _, _, _, _ := newDialog(t, script)

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), "acceptance: criteria_empty: ")
}

package blueprint

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestArtifact_AStageVersion(t *testing.T) {
	c, st := newViewController(t, "default", 0)
	ctx := context.Background()

	put(t, st, "intake", map[string]any{"slices": 1, "check_problems": []any{"A1: too vague"}})

	art, err := c.Artifact(ctx, viewID, "intake", "", 1)
	require.NoError(t, err)
	assert.Equal(t, "intake", art.Stage)
	assert.Equal(t, 1, art.Version)
	assert.Equal(t, "# intake", art.Body)
	assert.JSONEq(t, `{"slices":1,"check_problems":["A1: too vague"]}`, string(art.Data))
	assert.Equal(t, []string{"A1: too vague"}, art.CheckProblems)
}

func TestArtifact_NotFound(t *testing.T) {
	c, st := newViewController(t, "default", 0)
	ctx := context.Background()

	put(t, st, needStage, nil)
	put(t, st, "intake", nil)
	put(t, st, reviewStage("slicing"), map[string]any{"score": 5})

	for _, tt := range []struct {
		name, id, stage string
		version         int
	}{
		{"an unknown blueprint", "nosuch", "intake", 1},
		{"a version not stored", viewID, "intake", 2},
		{"the need as typed", viewID, needStage, 1},
		{"a review, which travels in the view", viewID, reviewStage("slicing"), 1},
		{"a stage the pack lacks", viewID, "vanished", 1},
		{"a sign-off never shown", viewID, signoffStage, 1},
		{"a board before emission", viewID, emissionStage, 1},
		{"a second board", viewID, emissionStage, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Artifact(ctx, tt.id, tt.stage, "", tt.version)
			require.ErrorIs(t, err, ErrNotFound)
		})
	}
}

func TestArtifact_TheSignoffIsItsLatestPayload(t *testing.T) {
	c, st := newViewController(t, "default", 0)

	appendEvent(t, st, EventSignOff, map[string]any{"version": 1, "order": []any{"D1"}, "warnings": []any{}})
	appendEvent(t, st, EventSignOff, map[string]any{"version": 2, "order": []any{"D1", "D2"}})
	appendEvent(t, st, EventSignOff, map[string]any{"version": 1, "order": []any{"D1"}, "warnings": []any{"reshown"}})

	art, err := c.Artifact(context.Background(), viewID, signoffStage, "", 1)
	require.NoError(t, err)
	assert.Equal(t, signoffStage, art.Stage)
	assert.Equal(t, 1, art.Version)
	assert.JSONEq(t, `{"version":1,"order":["D1"],"warnings":["reshown"]}`, string(art.Data))
}

func TestArtifact_TheBoard(t *testing.T) {
	c, st := newViewController(t, "default", 0)

	through(t, c, st, "readiness") // seven runs at 0.1
	appendEvent(t, st, EventEmitted, map[string]any{
		"cards": []any{
			map[string]any{"draft": "D1", "project": "alpha", "card": "ALPHA-1", "title": "One", "type": "feature", "priority": "high"},
			map[string]any{"draft": "D2", "project": "alpha", "card": "ALPHA-2", "title": "Two", "type": "bug", "priority": ""},
		},
		"playbook": "PB-1", "manual": nil, "steps": []any{"set the verify command"},
	})
	appendEvent(t, st, EventWatchState, map[string]any{
		"cards":    []any{map[string]any{"draft": "D1", "id": "ALPHA-1", "state": "done", "pr": "https://example.com/pr/1"}},
		"playbook": "PB-1",
		"repos":    []any{map[string]any{"project": "alpha", "compare_url": "https://example.com/compare"}},
	})

	art, err := c.Artifact(context.Background(), viewID, emissionStage, "", 1)
	require.NoError(t, err)

	var b struct {
		Cards []struct {
			Draft, Project, Card, Title, Type, Priority, State, PR string
		} `json:"cards"`
		Playbook string `json:"playbook"`
		Repos    []struct {
			Project    string `json:"project"`
			CompareURL string `json:"compare_url"`
		} `json:"repos"`
		Manual  []string `json:"manual"`
		Steps   []string `json:"steps"`
		CostUSD float64  `json:"cost_usd"`
		Priced  bool     `json:"priced"`
	}

	require.NoError(t, json.Unmarshal(art.Data, &b))
	require.Len(t, b.Cards, 2)
	assert.Equal(t, "ALPHA-1", b.Cards[0].Card)
	assert.Equal(t, "feature", b.Cards[0].Type)
	assert.Equal(t, "high", b.Cards[0].Priority)
	assert.Equal(t, "bug", b.Cards[1].Type)

	var raw struct{ Cards []map[string]any }

	require.NoError(t, json.Unmarshal(art.Data, &raw))
	require.Len(t, raw.Cards, 2)
	assert.NotContains(t, raw.Cards[1], "priority", "a draft without a priority has no key, not an empty one")
	assert.Equal(t, "done", b.Cards[0].State)
	assert.Equal(t, "https://example.com/pr/1", b.Cards[0].PR)
	assert.Empty(t, b.Cards[1].State, "a card the watch has not seen has no state yet")
	assert.Equal(t, "PB-1", b.Playbook)
	require.Len(t, b.Repos, 1)
	assert.Equal(t, "https://example.com/compare", b.Repos[0].CompareURL)
	assert.NotNil(t, b.Manual, "lists are [] not null")
	assert.Equal(t, []string{"set the verify command"}, b.Steps)
	assert.InDelta(t, 0.7, b.CostUSD, 1e-9)
	assert.True(t, b.Priced)
	assert.NotContains(t, string(art.Data), "null")
}

func TestArtifact_ABoardFromBeforeCardColoursHasNoTypeOrPriority(t *testing.T) {
	c, st := newViewController(t, "default", 0)

	through(t, c, st, "readiness")
	appendEvent(t, st, EventEmitted, map[string]any{
		"cards":    []any{map[string]any{"draft": "D1", "project": "alpha", "card": "ALPHA-1", "title": "One"}},
		"playbook": "PB-1", "manual": nil, "steps": []any{},
	})

	art, err := c.Artifact(context.Background(), viewID, emissionStage, "", 1)
	require.NoError(t, err)

	var b struct {
		Cards []map[string]any `json:"cards"`
	}

	require.NoError(t, json.Unmarshal(art.Data, &b))
	require.Len(t, b.Cards, 1)
	assert.NotContains(t, b.Cards[0], "type")
	assert.NotContains(t, b.Cards[0], "priority")
}

func TestArtifact_ASignoffShownThenApprovedAutomatically(t *testing.T) {
	c, st := newViewController(t, "default", 0)
	ctx := context.Background()

	through(t, c, st, "readiness")
	appendEvent(t, st, EventSignOff, map[string]any{"version": 1, "warnings": []any{"w1"}, "auto_stopped": true})
	gateRow(t, st, signoffStage, 1, store.GateApprove, store.ResponderAuto, "")
	appendEvent(t, st, EventSignOffAuto, map[string]any{"version": 1, "warnings": []any{}})

	art, err := c.Artifact(ctx, viewID, signoffStage, "", 1)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":1,"warnings":[]}`, string(art.Data), "the later record is the artifact")

	var refs int

	for _, r := range view(t, c).Artifacts {
		if r.Stage == signoffStage {
			refs++

			assert.Equal(t, 1, r.Version)
			assert.Equal(t, artApproved, r.Status)
		}
	}

	assert.Equal(t, 1, refs, "one ref per version")
}

func TestArtifact_ASliceVersion(t *testing.T) {
	c, st := newSlicedView(t)
	ctx := context.Background()

	put(t, st, "solution_design@SL2", map[string]any{"slices": []any{map[string]any{"id": "SL2"}}})

	// The sign-off and the board exist, so only the slice refuses them.
	appendEvent(t, st, EventSignOff, map[string]any{"version": 1})
	appendEvent(t, st, EventEmitted, map[string]any{"cards": []any{}})

	_, err := c.Artifact(ctx, viewID, signoffStage, "", 1)
	require.NoError(t, err, "the sign-off is read without a slice")
	_, err = c.Artifact(ctx, viewID, emissionStage, "", 1)
	require.NoError(t, err, "the board is read without a slice")

	art, err := c.Artifact(ctx, viewID, "solution_design", "SL2", 1)
	require.NoError(t, err)
	assert.Equal(t, "solution_design", art.Stage)
	assert.Equal(t, "SL2", art.Slice)

	for _, tc := range []struct{ name, stage, slice string }{
		{"a slice the record does not hold", "solution_design", "SL3"},
		{"a slice on a whole-blueprint stage", "slicing", "SL1"},
		{"a slice on the sign-off", signoffStage, "SL1"},
		{"a slice on the board", emissionStage, "SL1"},
		{"a per-slice stage asked without its slice", "solution_design", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Artifact(ctx, viewID, tc.stage, tc.slice, 1)
			assert.ErrorIs(t, err, ErrNotFound)
		})
	}
}

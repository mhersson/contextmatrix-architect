package blueprint

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func lessonLists(rec *recorder) []map[string]any {
	var out []map[string]any

	for _, e := range rec.all(EventLessonList) {
		out = append(out, e.Payload)
	}

	return out
}

func TestLessons_ListsEveryLessonOfEveryBlueprint(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, twoCardScript())
	drive(t, c, "BP1")

	ctx := context.Background()
	added, err := st.AddLessons(ctx, []store.Lesson{
		{
			BlueprintID: "BP1", RunID: 3, Scope: store.ScopeProject, Project: "alpha", Text: "Name the key",
			Rationale: "cards parked", Evidence: []string{"ALPHA-001", "ALPHA-002"}, Overlay: json.RawMessage(`{}`),
		},
		{BlueprintID: "BP2", RunID: 8, Scope: store.ScopePack, Text: "Quote the limit", Rationale: "reviews"},
	})
	require.NoError(t, err)

	ok, err := st.DecideLesson(ctx, added[0].ID, store.LessonDecision{
		State: store.LessonActive, Scope: store.ScopePack, Text: "Name the key", By: "human", At: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, c.Lessons(ctx, "BP1", "m-lessons"))

	got := lessonLists(rec)
	require.Len(t, got, 1)
	assert.Equal(t, "m-lessons", got[0]["message_id"])
	assert.Equal(t, false, got[0]["during_run"])

	items, ok := got[0]["lessons"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, items, 2)

	assert.Equal(t, map[string]any{
		"lesson": added[0].ID, "blueprint": "BP1", "state": store.LessonActive, "scope": store.ScopePack, "project": "alpha",
		"text": "Name the key", "rationale": "cards parked", "evidence": []string{"ALPHA-001", "ALPHA-002"}, "decided_by": "human",
	}, items[0])
	assert.Equal(t, added[1].ID, items[1]["lesson"])
	assert.Equal(t, "BP2", items[1]["blueprint"])
	assert.Equal(t, store.LessonProposed, items[1]["state"])
	assert.Empty(t, items[1]["decided_by"])
}

func TestLessons_WithNoneListsNone(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, twoCardScript())
	drive(t, c, "BP1")

	require.NoError(t, c.Lessons(context.Background(), "BP1", "m-lessons"))

	got := lessonLists(rec)
	require.Len(t, got, 1)

	items, ok := got[0]["lessons"].([]map[string]any)
	require.True(t, ok)
	assert.Empty(t, items)
}

func TestLessons_WhileARunIsInFlight(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})

	script := twoCardScript()
	script["intake"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		signal(entered)

		select {
		case <-release:
		case <-ctx.Done():
			return stage.Output{}, ctx.Err()
		}

		return done("x", map[string]any{"slices": 1}), nil
	}

	c, _, _, rec, _ := newPipeline(t, script)
	free := closer(t, release)

	require.NoError(t, c.Start(context.Background(), startInput("BP1")))
	require.NoError(t, c.Message(context.Background(), "BP1", "the need", "m1"))
	receive(t, entered, "the intake run")

	require.NoError(t, c.Lessons(context.Background(), "BP1", "m-lessons"))

	got := lessonLists(rec)
	require.Len(t, got, 1)
	assert.Equal(t, true, got[0]["during_run"])

	free()
	c.Wait("BP1")
}

func TestLessons_WithoutASessionIsNotFound(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, twoCardScript())

	require.ErrorIs(t, c.Lessons(context.Background(), "BP1", "m-lessons"), ErrNotFound)
}

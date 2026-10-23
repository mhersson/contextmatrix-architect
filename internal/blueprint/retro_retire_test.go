package blueprint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func lessonRetires(rec *recorder) []map[string]any {
	var out []map[string]any

	for _, e := range rec.all(EventLessonRetire) {
		out = append(out, e.Payload)
	}

	return out
}

func TestRetire_RetiresForLaterBlueprintsOnly(t *testing.T) {
	c, st, runner, rec, _ := newPipeline(t, happyScript())
	c.opts.Pack = lessonPack(t, "decomposition")
	ctx := context.Background()

	l := activeLesson(t, st, store.ScopePack, "", "keep each card to one deliverable", contract.Overlay{})

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Retire(ctx, "BP1", "l1", "m-retire"))

	got := lessonRetires(rec)
	require.Len(t, got, 1)
	assert.Equal(t, l.ID, got[0]["lesson"])
	assert.Equal(t, store.LessonRetired, got[0]["state"])
	assert.Equal(t, "keep each card to one deliverable", got[0]["text"])
	assert.Equal(t, "m-retire", got[0]["message_id"])
	assert.Equal(t, false, got[0]["during_run"])
	assert.NotContains(t, got[0], "refused")

	require.NoError(t, c.Start(ctx, startInput("BP2")))

	set, err := st.BlueprintLessons(ctx, "BP2")
	require.NoError(t, err)
	assert.Empty(t, set, "a blueprint created after the retire does not get the lesson")

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	instructions := instructionsByStage(runner)
	require.Contains(t, instructions, "decomposition")
	assert.Contains(t, instructions["decomposition"], "L1: keep each card to one deliverable", "a lesson retired after creation stays in the set")
}

func TestRetire_RefusesAnUnknownOrInactiveLesson(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, twoCardScript())
	drive(t, c, "BP1")

	ctx := context.Background()
	added, err := st.AddLessons(ctx, []store.Lesson{{BlueprintID: "BP1", Scope: store.ScopePack, Text: "undecided"}})
	require.NoError(t, err)

	require.NoError(t, c.Retire(ctx, "BP1", "l99", "m1"))
	require.NoError(t, c.Retire(ctx, "BP1", "l1", "m2"))
	require.NoError(t, c.Retire(ctx, "BP1", "l99999999999999999999", "m3"))

	got := lessonRetires(rec)
	require.Len(t, got, 3)
	assert.Equal(t, "There is no lesson L99.", got[0]["refused"])
	assert.Equal(t, "L1 is proposed; only an active lesson can be retired.", got[1]["refused"])
	assert.Equal(t, added[0].ID, got[1]["lesson"])
	assert.Equal(t, "There is no lesson L99999999999999999999.", got[2]["refused"])

	for _, p := range got {
		assert.NotContains(t, p, "state")
	}

	l, err := st.GetLesson(ctx, added[0].ID)
	require.NoError(t, err)
	assert.Equal(t, store.LessonProposed, l.State)
}

func TestRetire_WorksWhileARunIsInFlight(t *testing.T) {
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

	c, st, _, rec, _ := newPipeline(t, script)
	free := closer(t, release)
	l := activeLesson(t, st, store.ScopePack, "", "keep each card to one deliverable", contract.Overlay{})

	require.NoError(t, c.Start(context.Background(), startInput("BP1")))
	require.NoError(t, c.Message(context.Background(), "BP1", "the need", "m1"))
	receive(t, entered, "the intake run")

	require.NoError(t, c.Retire(context.Background(), "BP1", "l1", "m-retire"))

	got := lessonRetires(rec)
	require.Len(t, got, 1)
	assert.Equal(t, true, got[0]["during_run"])

	after, err := st.GetLesson(context.Background(), l.ID)
	require.NoError(t, err)
	assert.Equal(t, store.LessonRetired, after.State)

	free()
	c.Wait("BP1")
}

func TestRetire_WithoutASessionIsNotFound(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, twoCardScript())

	require.ErrorIs(t, c.Retire(context.Background(), "BP1", "l1", "m-retire"), ErrNotFound)
}

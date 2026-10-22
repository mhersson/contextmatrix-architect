package store

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLessons_RoundTripInIDOrder(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Second)

	in := []Lesson{
		{
			BlueprintID: "BP1", RunID: 4, State: LessonActive, Scope: ScopeProject, Project: "alpha",
			Text: "Name the limits", Rationale: "cards parked on them", Evidence: []string{"ALPHA-001"},
			Overlay: json.RawMessage(`{"edge_cases":["Limits"]}`), CreatedAt: created,
		},
		{BlueprintID: "BP1", RunID: 4, Scope: ScopePack, Text: "Second", CreatedAt: created},
		{BlueprintID: "BP2", RunID: 9, Scope: ScopePack, Text: "Third", CreatedAt: created},
	}

	added, err := s.AddLessons(ctx, in)
	require.NoError(t, err)
	require.Len(t, added, 3)

	for i, l := range added {
		assert.Equal(t, LessonProposed, l.State)

		if i > 0 {
			assert.Greater(t, l.ID, added[i-1].ID)
		}
	}

	listed, err := s.ListLessons(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 3)

	for i, l := range listed {
		assert.Equal(t, added[i].ID, l.ID)
		assert.Equal(t, in[i].BlueprintID, l.BlueprintID)
		assert.Equal(t, in[i].RunID, l.RunID)
		assert.Equal(t, LessonProposed, l.State)
		assert.Equal(t, in[i].Scope, l.Scope)
		assert.Equal(t, in[i].Project, l.Project)
		assert.Equal(t, in[i].Text, l.Text)
		assert.Equal(t, in[i].Rationale, l.Rationale)
		assert.Equal(t, created, l.CreatedAt)
		assert.Nil(t, l.DecidedAt)
		assert.Empty(t, l.DecidedBy)
	}

	assert.Equal(t, []string{"ALPHA-001"}, listed[0].Evidence)
	assert.Empty(t, listed[1].Evidence)
	assert.JSONEq(t, `{"edge_cases":["Limits"]}`, string(listed[0].Overlay))
	assert.JSONEq(t, `{}`, string(listed[1].Overlay))
	assert.Equal(t, "L"+strconv.FormatInt(listed[0].ID, 10), listed[0].Tag())

	got, err := s.GetLesson(ctx, listed[1].ID)
	require.NoError(t, err)
	assert.Equal(t, listed[1], got)

	_, err = s.GetLesson(ctx, 999)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestLessons_AnIDIsNeverReused(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	first, err := s.AddLessons(ctx, []Lesson{{BlueprintID: "BP1", Scope: ScopePack, Text: "a"}})
	require.NoError(t, err)

	_, err = s.db.Exec("DELETE FROM lessons")
	require.NoError(t, err)

	second, err := s.AddLessons(ctx, []Lesson{{BlueprintID: "BP1", Scope: ScopePack, Text: "b"}})
	require.NoError(t, err)

	assert.Greater(t, second[0].ID, first[0].ID)
}

func TestDecideLesson_DecidesAProposedLessonOnce(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Second)

	added, err := s.AddLessons(ctx, []Lesson{{BlueprintID: "BP1", Scope: ScopeProject, Project: "alpha", Text: "original"}})
	require.NoError(t, err)

	id := added[0].ID

	ok, err := s.DecideLesson(ctx, id, LessonDecision{State: LessonActive, Scope: ScopePack, Text: "reworded", By: "human", At: t0})
	require.NoError(t, err)
	assert.True(t, ok)

	got, err := s.GetLesson(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, LessonActive, got.State)
	assert.Equal(t, ScopePack, got.Scope)
	assert.Equal(t, "reworded", got.Text)
	require.NotNil(t, got.DecidedAt)
	assert.Equal(t, t0, *got.DecidedAt)
	assert.Equal(t, "human", got.DecidedBy)

	ok, err = s.DecideLesson(ctx, id, LessonDecision{State: LessonRejected, Scope: ScopeProject, Text: "other", By: "someone", At: t0.Add(time.Hour)})
	require.NoError(t, err)
	assert.False(t, ok)

	again, err := s.GetLesson(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, got, again)

	ok, err = s.DecideLesson(ctx, 999, LessonDecision{State: LessonActive, Scope: ScopePack, Text: "x", By: "human", At: t0})
	require.NoError(t, err)
	assert.False(t, ok)
}

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

func TestRetireLesson_RetiresOnlyAnActiveLesson(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	active := addDecided(t, s, ScopePack, "", LessonActive)
	before, err := s.GetLesson(ctx, active.ID)
	require.NoError(t, err)

	ok, err := s.RetireLesson(ctx, active.ID)
	require.NoError(t, err)
	assert.True(t, ok)

	got, err := s.GetLesson(ctx, active.ID)
	require.NoError(t, err)
	assert.Equal(t, LessonRetired, got.State)

	before.State = LessonRetired
	assert.Equal(t, before, got, "only the state changes")

	ok, err = s.RetireLesson(ctx, active.ID)
	require.NoError(t, err)
	assert.False(t, ok)

	proposed := addDecided(t, s, ScopePack, "", LessonProposed)

	ok, err = s.RetireLesson(ctx, proposed.ID)
	require.NoError(t, err)
	assert.False(t, ok)

	got, err = s.GetLesson(ctx, proposed.ID)
	require.NoError(t, err)
	assert.Equal(t, LessonProposed, got.State)
}

// addDecided stores a lesson and decides it into state; proposed leaves it undecided.
func addDecided(t *testing.T, s *Store, scope, project, state string) Lesson {
	t.Helper()

	ctx := context.Background()

	added, err := s.AddLessons(ctx, []Lesson{{BlueprintID: "SRC", Scope: scope, Project: project, Text: scope + project + state}})
	require.NoError(t, err)

	if state != LessonProposed {
		ok, err := s.DecideLesson(ctx, added[0].ID, LessonDecision{State: state, Scope: scope, Text: added[0].Text, By: "op", At: time.Now().UTC()})
		require.NoError(t, err)
		require.True(t, ok)
	}

	return added[0]
}

func lessonIDs(ls []Lesson) []int64 {
	ids := make([]int64, 0, len(ls))
	for _, l := range ls {
		ids = append(ids, l.ID)
	}

	return ids
}

func newBlueprint(id string, projects ...string) Blueprint {
	now := time.Now().UTC().Truncate(time.Second)

	return Blueprint{ID: id, Projects: projects, Mode: "default", State: "new", CreatedAt: now, UpdatedAt: now}
}

func TestCreateBlueprint_TakesTheActiveLessonsItsProjectsCover(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	l1 := addDecided(t, s, ScopePack, "", LessonActive)
	l2 := addDecided(t, s, ScopeProject, "alpha", LessonActive)
	addDecided(t, s, ScopeProject, "beta", LessonActive)
	addDecided(t, s, ScopePack, "", LessonProposed)
	addDecided(t, s, ScopePack, "", LessonRejected)

	require.NoError(t, s.CreateBlueprint(ctx, newBlueprint("BP1", "alpha")))
	require.NoError(t, s.CreateBlueprint(ctx, newBlueprint("BP2")))

	got, err := s.BlueprintLessons(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, []int64{l1.ID, l2.ID}, lessonIDs(got))

	got, err = s.BlueprintLessons(ctx, "BP2")
	require.NoError(t, err)
	assert.Equal(t, []int64{l1.ID}, lessonIDs(got))
}

func TestBlueprintLessons_KeepsALessonInItsNewState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	l1 := addDecided(t, s, ScopePack, "", LessonActive)
	require.NoError(t, s.CreateBlueprint(ctx, newBlueprint("BP1")))

	_, err := s.db.Exec("UPDATE lessons SET state = 'retired' WHERE id = ?", l1.ID)
	require.NoError(t, err)

	got, err := s.BlueprintLessons(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, l1.ID, got[0].ID)
	assert.Equal(t, LessonRetired, got[0].State)
}

func TestCreateBlueprint_ADuplicateIDAddsNoSet(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	l1 := addDecided(t, s, ScopePack, "", LessonActive)
	require.NoError(t, s.CreateBlueprint(ctx, newBlueprint("BP1")))
	addDecided(t, s, ScopePack, "", LessonActive)

	require.Error(t, s.CreateBlueprint(ctx, newBlueprint("BP1")))

	got, err := s.BlueprintLessons(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, []int64{l1.ID}, lessonIDs(got))
}

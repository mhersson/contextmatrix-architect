package blueprint

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// emittedIDs are the ids of the first n alpha cards the fake emits.
func emittedIDs(n int) []string {
	ids := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		ids = append(ids, fmt.Sprintf("ALPHA-%03d", i))
	}

	return ids
}

// atFault is a retro run that finds each of the first n emitted cards at
// fault with a project lesson, quoting the card's id from the body stuck
// gave it.
func atFault(n int) func(context.Context, stage.Input) (stage.Output, error) {
	findings := make([]map[string]any, 0, n)
	for _, id := range emittedIDs(n) {
		findings = append(findings, retroFinding(id, id, lessonFor(store.ScopeProject)))
	}

	return retroRun(findings...)
}

// stuck sets each card ended blocked, its id in its body.
func stuck(fake *fakeCM, ids ...string) {
	for _, id := range ids {
		fake.setEnded(id, cm.EndedCard{
			Card: cm.Card{ID: id, Project: "alpha", Title: "Add a part", State: "blocked"},
			Body: cardBody(id),
		})
	}
}

// roundEvents are BP1's stored round events, oldest first.
func roundEvents(t *testing.T, st *store.Store) []store.Event {
	t.Helper()

	events, err := st.ListEventsOfKind(context.Background(), "BP1", EventRound)
	require.NoError(t, err)

	return events
}

// roundsAfter are BP1's stored round events newer than event id.
func roundsAfter(t *testing.T, st *store.Store, id int64) []store.Event {
	t.Helper()

	var out []store.Event

	for _, e := range roundEvents(t, st) {
		if e.ID > id {
			out = append(out, e)
		}
	}

	return out
}

// newestSnapshot is BP1's newest round as stored.
func newestSnapshot(t *testing.T, c *Controller) roundState {
	t.Helper()

	rounds := mustRounds(t, c, "BP1")
	require.NotEmpty(t, rounds)

	return rounds[len(rounds)-1]
}

// oneLesson emits two cards, the first blocked and at fault with a project
// lesson, the second done, and runs a pass, which posts a lesson round of
// one question.
func oneLesson(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeCM, stageScript) {
	t.Helper()

	script := twoCardScript()
	script[RetroStage] = atFault(1)

	c, st, runner, rec, fake := emitted(t, script)
	stuck(fake, "ALPHA-001")
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	require.Len(t, roundEvents(t, st), 1, "the pass posts its lesson round")

	return c, st, runner, rec, fake, script
}

func TestLessonRound_OneQuestionPerLessonPastTen(t *testing.T) {
	script := manyCardScript(11)
	script[RetroStage] = atFault(11)

	c, st, _, rec, fake := emitted(t, script)
	stuck(fake, emittedIDs(11)...)

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	assert.Empty(t, replies(rec))

	events := roundEvents(t, st)
	require.Len(t, events, 1)

	s := newestSnapshot(t, c)
	assert.Equal(t, RetroStage, s.Frame.Stage)
	assert.Equal(t, RetroStage, s.Frame.Persona)
	assert.True(t, s.Frame.ResearchOff)
	assert.False(t, s.Frame.Closed)
	assert.Empty(t, s.Frame.Assumptions)
	require.Len(t, s.Frame.Questions, 11)

	for i, q := range s.Frame.Questions {
		assert.Equal(t, s.Frame.Questions[0].Number+i, q.Number, "numbered consecutively")
		assert.Len(t, q.Options, 3)
		assert.Equal(t, 1, q.Recommended, "a project lesson recommends its project")
	}

	runs := retroRuns(t, st)
	require.Len(t, runs, 1)
	assert.Equal(t, runs[0].ID, s.PostedBy)
	assert.Equal(t, runs[0].ID, s.RunID)
	assert.Equal(t, runs[0].ID, events[0].RunID)

	byCard := map[string]int64{}

	for _, l := range lessons(t, st) {
		require.Len(t, l.Evidence, 1)
		byCard[l.Evidence[0]] = l.ID
	}

	require.Len(t, s.Lessons, 11)

	for i, id := range emittedIDs(11) {
		assert.Equal(t, byCard[id], s.Lessons[s.Frame.Questions[i].Number], "Q%d asks about %s's lesson", s.Frame.Questions[i].Number, id)
	}

	report, err := st.LatestEvent(context.Background(), "BP1", EventRetroReport)
	require.NoError(t, err)
	assert.Less(t, report.ID, events[0].ID, "the findings message comes before its round")
}

func TestLessonRound_NoLessonPostsNoRound(t *testing.T) {
	script := twoCardScript()
	script[RetroStage] = retroRun(retroFinding("ALPHA-001", "ALPHA-001", nil), retroFinding("ALPHA-002", "ALPHA-002", nil))

	c, st, _, rec, fake := emitted(t, script)
	stuck(fake, emittedIDs(2)...)

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	report, err := st.LatestEvent(context.Background(), "BP1", EventRetroReport)
	require.NoError(t, err)

	assert.Empty(t, roundsAfter(t, st, report.ID))
	assert.Empty(t, replies(rec))
	assert.Empty(t, lessons(t, st))
}

func TestLessonRound_ARoundPastTheFrameLimitIsNotPosted(t *testing.T) {
	lesson := lessonFor(store.ScopeProject)
	lesson["text"] = strings.Repeat("x", 100<<10)

	script := twoCardScript()
	script[RetroStage] = retroRun(retroFinding("ALPHA-001", "ALPHA-001", lesson))

	c, st, _, rec, fake := emitted(t, script)
	stuck(fake, "ALPHA-001")
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	got := replies(rec)
	require.Len(t, got, 1)
	assert.Equal(t, retroTooBig, got[0]["reason"])
	assert.Equal(t, false, got[0]["during_run"])
	assert.Equal(t, "m-retro", got[0]["message_id"])

	ls := lessons(t, st)
	require.Len(t, ls, 1)
	assert.Equal(t, store.LessonProposed, ls[0].State)

	reportOf(t, st)
	assert.Empty(t, roundEvents(t, st))
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
	assert.False(t, holdsSlot(c, "BP1"))
}

func TestRetrospective_AnOpenLessonRoundStartsNoPass(t *testing.T) {
	c, st, runner, rec, _, _ := oneLesson(t)
	open := newestSnapshot(t, c)

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-second"))
	c.Wait("BP1")

	got := replies(rec)
	require.Len(t, got, 1)
	assert.Equal(t, retroRoundOpen, got[0]["reason"])
	assert.Equal(t, false, got[0]["during_run"])
	assert.Equal(t, "m-second", got[0]["message_id"])
	assert.Contains(t, got[0]["message"], strconv.Itoa(open.Number), "the reply names the open round")

	assert.Len(t, inputsOf(runner, RetroStage), 1, "the second reply starts no run")
	assert.Len(t, retroRuns(t, st), 1)
	assert.Len(t, roundEvents(t, st), 1)
	assert.False(t, holdsSlot(c, "BP1"))
}

func TestLessonRound_StaysOpenAcrossAReopenAndARestart(t *testing.T) {
	c, st, _, _, fake, script := oneLesson(t)
	open := newestSnapshot(t, c)

	ctx := context.Background()

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	s := newestSnapshot(t, c)
	assert.Equal(t, open.Number, s.Number)
	assert.False(t, s.Frame.Closed, "a reopen leaves the lesson round open")
	assert.NotEqual(t, ChangeLeft, s.Change)

	c2, _ := reopen(t, c, st, nil, script, fake, startInput("BP1"))

	s = newestSnapshot(t, c2)
	assert.Equal(t, open.Number, s.Number)
	assert.False(t, s.Frame.Closed, "a restart leaves the lesson round open")
	assert.NotEqual(t, ChangeLeft, s.Change)

	view, err := c2.View(ctx, "BP1")
	require.NoError(t, err)
	assert.Nil(t, view.Open, "the panel finds the lesson round in the transcript")
}

func TestLessonRound_IsNeverHandedToTheAutoResponder(t *testing.T) {
	c, _, runner, rec, _, _ := oneLesson(t)
	calls := runner.calls()

	require.NoError(t, c.SetMode(context.Background(), "BP1", "auto", "m-mode"))
	c.Wait("BP1")

	s := newestSnapshot(t, c)
	assert.False(t, s.Frame.Closed)
	assert.False(t, s.Frame.Responder)
	assert.Nil(t, s.HandedOver)
	assert.Equal(t, calls, runner.calls(), "no responder runs")

	modes := rec.all(EventMode)
	require.Len(t, modes, 1)
	assert.NotContains(t, modes[0].Payload, "hands_over")
}

func TestLessonRound_ChatIsANote(t *testing.T) {
	c, _, runner, rec, _, _ := oneLesson(t)
	calls := runner.calls()

	require.NoError(t, c.Message(context.Background(), "BP1", "why L1?", "m-chat"))
	c.Wait("BP1")

	var note *Event

	for _, e := range rec.all(EventNote) {
		if e.Payload["message_id"] == "m-chat" {
			note = &e
		}
	}

	require.NotNil(t, note, "the message is a note")
	assert.Equal(t, "why L1?", note.Payload["text"])
	assert.Equal(t, calls, runner.calls(), "no run follows")
	assert.Empty(t, newestSnapshot(t, c).Frame.Chat)
}

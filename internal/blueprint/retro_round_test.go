package blueprint

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
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

// answerLessons sends answers to lesson round s as reply msgID.
func answerLessons(t *testing.T, c *Controller, s roundState, msgID string, answers ...protocol.RoundAnswer) {
	t.Helper()

	require.NoError(t, c.Reply(context.Background(), "BP1", protocol.RoundReply{PromptID: roundPromptID(s.Number), Answers: answers}, msgID))
}

// decisions returns the lesson_decided payloads, oldest first.
func decisions(rec *recorder) []map[string]any {
	var out []map[string]any

	for _, e := range rec.all(EventLessonDecided) {
		out = append(out, e.Payload)
	}

	return out
}

func lessonOf(t *testing.T, st *store.Store, id int64) store.Lesson {
	t.Helper()

	l, err := st.GetLesson(context.Background(), id)
	require.NoError(t, err)

	return l
}

// lessonIDs are the lesson ids of round s's questions, in question order.
func lessonIDs(s roundState) []int64 {
	out := make([]int64, 0, len(s.Frame.Questions))
	for _, q := range s.Frame.Questions {
		out = append(out, s.Lessons[q.Number])
	}

	return out
}

func TestLessonRound_AnswersDecideEachLessonInQuestionOrder(t *testing.T) {
	script := manyCardScript(4)
	script[RetroStage] = atFault(4)

	c, st, _, rec, fake := emitted(t, script)
	stuck(fake, emittedIDs(4)...)

	ctx := context.Background()

	require.NoError(t, c.Retrospective(ctx, "BP1", "m-retro"))
	c.Wait("BP1")

	open := newestSnapshot(t, c)
	q := open.Frame.Questions
	require.Len(t, q, 4)

	ids := lessonIDs(open)

	answerLessons(t, c, open, "m-answers",
		pick(q[0].Number, 1), pick(q[1].Number, 2), pick(q[2].Number, 3), own(q[3].Number, "Name every key a card needs"))
	c.Wait("BP1")

	first := lessonOf(t, st, ids[0])
	assert.Equal(t, store.LessonActive, first.State)
	assert.Equal(t, store.ScopeProject, first.Scope)
	assert.Equal(t, "alpha", first.Project)
	assert.Equal(t, store.ResponderHuman, first.DecidedBy)
	assert.NotNil(t, first.DecidedAt)

	second := lessonOf(t, st, ids[1])
	assert.Equal(t, store.LessonActive, second.State)
	assert.Equal(t, store.ScopePack, second.Scope)

	third := lessonOf(t, st, ids[2])
	assert.Equal(t, store.LessonRejected, third.State)
	assert.Equal(t, store.ScopeProject, third.Scope)
	assert.Equal(t, "Name where the key comes from", third.Text)

	fourth := lessonOf(t, st, ids[3])
	assert.Equal(t, store.LessonActive, fourth.State)
	assert.Equal(t, store.ScopeProject, fourth.Scope, "own words approve at the proposed scope")
	assert.Equal(t, "Name every key a card needs", fourth.Text)

	got := decisions(rec)
	require.Len(t, got, 4)

	for i, p := range got {
		assert.Equal(t, ids[i], p["lesson"], "decided in question order")
		assert.Equal(t, "m-answers", p["message_id"])
		assert.NotContains(t, p, "error")
	}

	assert.Equal(t, store.LessonActive, got[0]["state"])
	assert.Equal(t, store.ScopeProject, got[0]["scope"])
	assert.Equal(t, "alpha", got[0]["project"])
	assert.Equal(t, store.ScopePack, got[1]["scope"])
	assert.NotContains(t, got[1], "project", "a pack lesson names no project")
	assert.Equal(t, store.LessonRejected, got[2]["state"])
	assert.Equal(t, "Name every key a card needs", got[3]["text"])

	run := retroRuns(t, st)[0]

	rows, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)

	var asked []store.Question

	for _, r := range rows {
		if r.Stage == RetroStage {
			asked = append(asked, r)
		}
	}

	require.Len(t, asked, 4)

	for i, r := range asked {
		assert.Equal(t, q[i].Number, r.Number)
		assert.Equal(t, run.ID, r.RunID)
		assert.Equal(t, store.ResponderHuman, r.Responder)
		assert.Equal(t, "m-answers", r.MessageID)
	}

	s := newestSnapshot(t, c)
	assert.Equal(t, open.Number, s.Number)
	assert.True(t, s.Frame.Closed)
	assert.Equal(t, changeClosed, s.Change)
}

func TestLessonRound_APartialReplyDecidesNothing(t *testing.T) {
	script := twoCardScript()
	script[RetroStage] = atFault(2)

	c, st, _, rec, fake := emitted(t, script)
	stuck(fake, emittedIDs(2)...)

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	open := newestSnapshot(t, c)
	q := open.Frame.Questions
	require.Len(t, q, 2)

	answerLessons(t, c, open, "m-part", pick(q[0].Number, 1), own(q[1].Number, "   "))
	c.Wait("BP1")

	assert.Empty(t, decisions(rec))

	for _, l := range lessons(t, st) {
		assert.Equal(t, store.LessonProposed, l.State)
	}

	s := newestSnapshot(t, c)
	assert.Equal(t, open.Number, s.Number)
	assert.False(t, s.Frame.Closed)
	assert.Equal(t, changeAnswers, s.Change)
	assert.Equal(t, "m-part", s.MessageID)
	assert.Equal(t, 1, s.Dropped, "blank own words are dropped")
	require.Len(t, s.Frame.Answers, 1)
	assert.Equal(t, q[0].Number, s.Frame.Answers[0].Number)
}

func TestLessonRound_AnApprovalTheMergedListsFailIsRejectedWithTheParsersError(t *testing.T) {
	broken := []contract.Pattern{{Name: "broken", Pattern: "(["}}

	lesson := lessonFor(store.ScopeProject)
	lesson["overlay"] = map[string]any{"forbidden_references": []any{map[string]any{"name": broken[0].Name, "pattern": broken[0].Pattern}}}

	script := twoCardScript()
	script[RetroStage] = retroRun(retroFinding("ALPHA-001", "ALPHA-001", lesson))

	c, st, _, rec, fake := emitted(t, script)
	stuck(fake, "ALPHA-001")
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	_, want := c.opts.Pack.MergeLists([]contract.Overlay{{ForbiddenReferences: broken}})
	require.Error(t, want)

	open := newestSnapshot(t, c)
	require.Len(t, open.Frame.Questions, 1)

	answerLessons(t, c, open, "m-approve", pick(open.Frame.Questions[0].Number, 1))
	c.Wait("BP1")

	l := lessonOf(t, st, lessonIDs(open)[0])
	assert.Equal(t, store.LessonRejected, l.State)
	assert.Equal(t, store.ScopeProject, l.Scope)
	assert.Equal(t, "Name where the key comes from", l.Text)

	got := decisions(rec)
	require.Len(t, got, 1)
	assert.Equal(t, store.LessonRejected, got[0]["state"])
	assert.Equal(t, want.Error(), got[0]["error"])
}

func TestLessonRound_AnswersAfterAReopenAndARestartDecide(t *testing.T) {
	t.Run("after the session's end and a start", func(t *testing.T) {
		c, st, _, rec, _, _ := oneLesson(t)
		open := newestSnapshot(t, c)

		ctx := context.Background()

		require.NoError(t, c.End(ctx, "BP1"))
		require.NoError(t, c.Start(ctx, startInput("BP1")))
		c.Wait("BP1")

		answerLessons(t, c, open, "m-answers", pick(open.Frame.Questions[0].Number, 1))
		c.Wait("BP1")

		l := lessonOf(t, st, lessonIDs(open)[0])
		assert.Equal(t, store.LessonActive, l.State)
		assert.Equal(t, store.ScopeProject, l.Scope)
		assert.Len(t, decisions(rec), 1)
	})

	t.Run("after a restart", func(t *testing.T) {
		c, st, _, _, fake, script := oneLesson(t)
		open := newestSnapshot(t, c)

		c2, rec2 := reopen(t, c, st, nil, script, fake, startInput("BP1"))

		answerLessons(t, c2, open, "m-answers", pick(open.Frame.Questions[0].Number, 1))
		c2.Wait("BP1")

		l := lessonOf(t, st, lessonIDs(open)[0])
		assert.Equal(t, store.LessonActive, l.State)
		assert.Equal(t, store.ScopeProject, l.Scope)
		assert.Equal(t, "alpha", l.Project)
		assert.JSONEq(t, `{"edge_cases":["a missing key"]}`, string(l.Overlay))
		assert.Len(t, decisions(rec2), 1)
	})
}

func TestLessonRound_AnAnswerNeedsNoRunSlot(t *testing.T) {
	c, st, _, rec, _, _ := oneLesson(t)
	open := newestSnapshot(t, c)

	c.mu.Lock()
	r, _, claimed := c.claimLocked("BP1")
	c.mu.Unlock()
	require.True(t, claimed)

	answerLessons(t, c, open, "m-answers", pick(open.Frame.Questions[0].Number, 2))

	assert.Equal(t, store.LessonActive, lessonOf(t, st, lessonIDs(open)[0]).State)
	assert.Len(t, decisions(rec), 1)

	c.mu.Lock()
	assert.Same(t, r, c.runs["BP1"], "the answer leaves the slot to its holder")
	c.mu.Unlock()

	c.release("BP1", r)
	c.Wait("BP1")
}

func TestLessonRound_ARepliedRoundTakesNoSecondReply(t *testing.T) {
	c, st, _, rec, _, _ := oneLesson(t)
	open := newestSnapshot(t, c)

	answerLessons(t, c, open, "m-answers", pick(open.Frame.Questions[0].Number, 3))
	c.Wait("BP1")
	require.Len(t, decisions(rec), 1)

	answerLessons(t, c, open, "m-again", pick(open.Frame.Questions[0].Number, 1))
	c.Wait("BP1")

	var stale []map[string]any

	for _, e := range rec.all(EventError) {
		if e.Payload["message_id"] == "m-again" {
			stale = append(stale, e.Payload)
		}
	}

	require.Len(t, stale, 1)
	assert.Equal(t, reasonStaleReply, stale[0]["reason"])
	assert.Equal(t, roundPromptID(open.Number), stale[0]["prompt_id"])
	assert.Len(t, decisions(rec), 1)
	assert.Equal(t, store.LessonRejected, lessonOf(t, st, lessonIDs(open)[0]).State)
}

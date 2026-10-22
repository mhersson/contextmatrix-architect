package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// emitted drives BP1 through the pipeline and approves its sign-off, so its
// cards exist in the fake ContextMatrix it returns.
func emitted(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeCM) {
	t.Helper()

	c, st, runner, rec, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m-approve"))
	c.Wait("BP1")
	require.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))

	return c, st, runner, rec, fake
}

// manyCardScript drafts n alpha cards, D1..Dn, every one ready.
func manyCardScript(n int) stageScript {
	script := twoCardScript()

	cards := make([]any, 0, n)
	ids := make([]string, 0, n)
	order := make([]any, 0, n)

	for i := 1; i <= n; i++ {
		id, title := fmt.Sprintf("D%d", i), fmt.Sprintf("Add part %d", i)
		cards = append(cards, map[string]any{
			"id": id, "project": "alpha", "title": title, "type": "feature", "priority": "medium",
			"depends_on": []any{}, "body": cardBody(title),
			"where": []any{map[string]any{"path": "internal/x.go", "symbol": "Thing"}},
		})
		ids = append(ids, id)
		order = append(order, id)
	}

	script["decomposition"] = returns("# Cards", map[string]any{"title": "Ship the parts", "cards": cards, "order": order})
	script["readiness"] = returns("# Ready", readinessFor(true, ids...))

	return script
}

// retroRun is a retro run that submits findings.
func retroRun(findings ...map[string]any) func(context.Context, stage.Input) (stage.Output, error) {
	list := make([]any, 0, len(findings))
	for _, f := range findings {
		list = append(list, f)
	}

	return returns("# Findings", map[string]any{"findings": list})
}

// retroFinding judges card under rule Where; it is at fault exactly when lesson
// is not nil.
func retroFinding(card, evidence string, lesson map[string]any) map[string]any {
	f := map[string]any{"card": card, "rule": "Where", "evidence": evidence, "at_fault": lesson != nil}
	if lesson != nil {
		f["lesson"] = lesson
	}

	return f
}

func lessonFor(scope string) map[string]any {
	return map[string]any{
		"text": "Name where the key comes from", "rationale": "The agent parked without one", "scope": scope,
		"overlay": map[string]any{"edge_cases": []any{"a missing key"}},
	}
}

// parked is a card its run parked for reason.
func parked(id, body, reason string) cm.EndedCard {
	return cm.EndedCard{
		Card: cm.Card{ID: id, Project: "alpha", Title: "Add the parser", State: "in_progress", WorkerStatus: "parked"},
		Body: body, ActivityLog: []cm.ActivityEntry{{Action: "parked", Message: reason}},
	}
}

// finished is a card that ended done after one review.
func finished(id, body string, costUSD float64) cm.EndedCard {
	return cm.EndedCard{
		Card: cm.Card{ID: id, Project: "alpha", Title: "Add a part", State: "done", ReviewAttempts: 1},
		Body: body, SubtaskCostUSD: costUSD,
	}
}

// replies returns the retro_reply payloads, oldest first.
func replies(rec *recorder) []map[string]any {
	var out []map[string]any

	for _, e := range rec.all(EventRetroReply) {
		out = append(out, e.Payload)
	}

	return out
}

func reasonsOf(rec *recorder) []string {
	var out []string

	for _, p := range replies(rec) {
		out = append(out, p["reason"].(string))
	}

	return out
}

// retroRuns returns the blueprint's runs of kind retro.
func retroRuns(t *testing.T, st *store.Store) []store.Run {
	t.Helper()

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	var out []store.Run

	for _, r := range runs {
		if r.Kind == store.RunKindRetro {
			out = append(out, r)
		}
	}

	return out
}

func lessons(t *testing.T, st *store.Store) []store.Lesson {
	t.Helper()

	ls, err := st.ListLessons(context.Background())
	require.NoError(t, err)

	return ls
}

// stopWatch ends the blueprint's watch, so a test may change the
// controller's options with nothing else reading them.
func stopWatch(t *testing.T, c *Controller, id string) {
	t.Helper()

	if w := watcherOf(c, id); w != nil {
		w.cancel()
		<-w.done
	}
}

// storedReport is a stored retro_report payload.
type storedReport struct {
	Run       int64 `json:"run"`
	Read      int   `json:"read"`
	Struggled int   `json:"struggled"`
	Cards     []struct {
		Card      string   `json:"card"`
		Project   string   `json:"project"`
		Why       []string `json:"why"`
		Evidence  string   `json:"evidence"`
		AtFault   bool     `json:"at_fault"`
		MedianUSD *float64 `json:"median_usd"`
		Lesson    *struct {
			ID      int64  `json:"id"`
			Scope   string `json:"scope"`
			Project string `json:"project"`
		} `json:"lesson"`
	} `json:"cards"`
}

func reportOf(t *testing.T, st *store.Store) storedReport {
	t.Helper()

	ev, err := st.LatestEvent(context.Background(), "BP1", EventRetroReport)
	require.NoError(t, err)

	var r storedReport
	require.NoError(t, json.Unmarshal(ev.Payload, &r))

	return r
}

func TestRetrospective_JudgesEveryStruggledCardAndReportsEachFinding(t *testing.T) {
	script := twoCardScript()

	var seen stage.Input

	script[RetroStage] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		seen = in

		missing := map[string]any{"findings": []any{retroFinding("ALPHA-001", "needs a key", lessonFor("project"))}}
		if err := in.Validate(ctx, stage.Artifact{Body: "x", Data: missing}); err == nil {
			return stage.Output{}, errors.New("a submit missing a struggled card is refused")
		}

		return done("x", map[string]any{"findings": []any{
			retroFinding("ALPHA-001", "needs a key", lessonFor("project")),
			retroFinding("ALPHA-002", "Wire the parser", nil),
		}}), nil
	}

	c, st, runner, rec, fake := emitted(t, script)
	fake.setEnded("ALPHA-001", parked("ALPHA-001", cardBody("needs a key"), "no key"))
	fake.setEnded("ALPHA-002", cm.EndedCard{Card: cm.Card{ID: "ALPHA-002", State: "done", ReviewAttempts: 2}, Body: cardBody("Wire the parser")})

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	require.Len(t, inputsOf(runner, RetroStage), 1)

	runs := retroRuns(t, st)
	require.Len(t, runs, 1)
	assert.Equal(t, RetroStage, runs[0].Stage)
	assert.Equal(t, 1, runs[0].Attempt)
	assert.Equal(t, store.RunDone, runs[0].Status)

	assert.Equal(t, c.opts.Pack.Persona(pack.RetroPersona), seen.Persona)
	assert.Equal(t, c.opts.Pack.RetroInstructions(), seen.Instructions)
	assert.Empty(t, seen.Tools, "a retro run reads no code")
	assert.Nil(t, seen.Interviewer, "a retro run asks no questions")
	assert.Contains(t, seen.Task, cardBody("needs a key"))
	assert.Contains(t, seen.Task, cardBody("Wire the parser"))

	ls := lessons(t, st)
	require.Len(t, ls, 1)
	assert.Equal(t, store.LessonProposed, ls[0].State)
	assert.Equal(t, store.ScopeProject, ls[0].Scope)
	assert.Equal(t, "alpha", ls[0].Project)
	assert.Equal(t, []string{"ALPHA-001"}, ls[0].Evidence)
	assert.Equal(t, runs[0].ID, ls[0].RunID)
	assert.JSONEq(t, `{"edge_cases":["a missing key"]}`, string(ls[0].Overlay))

	report := reportOf(t, st)
	assert.Equal(t, runs[0].ID, report.Run)
	assert.Equal(t, 2, report.Read)
	assert.Equal(t, 2, report.Struggled)
	require.Len(t, report.Cards, 2)

	first, second := report.Cards[0], report.Cards[1]
	assert.Equal(t, "ALPHA-001", first.Card)
	assert.True(t, first.AtFault)
	assert.Equal(t, []string{"parked"}, first.Why)
	assert.Equal(t, "needs a key", first.Evidence)
	require.NotNil(t, first.Lesson)
	assert.Equal(t, ls[0].ID, first.Lesson.ID)
	assert.Equal(t, store.ScopeProject, first.Lesson.Scope)
	assert.Equal(t, "alpha", first.Lesson.Project)

	assert.Equal(t, "ALPHA-002", second.Card)
	assert.False(t, second.AtFault)
	assert.Nil(t, second.Lesson)
	assert.Equal(t, []string{"review_attempts"}, second.Why)

	assert.Empty(t, replies(rec), "a pass that completes says so in its report")
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
	assert.False(t, holdsSlot(c, "BP1"))
}

func TestRetrospective_ACostlyCardCarriesTheMedian(t *testing.T) {
	script := manyCardScript(3)
	script[RetroStage] = retroRun(retroFinding("ALPHA-003", "Add part 3", nil))

	c, st, _, _, fake := emitted(t, script)
	fake.setEnded("ALPHA-001", finished("ALPHA-001", cardBody("Add part 1"), 1))
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Add part 2"), 1))
	fake.setEnded("ALPHA-003", finished("ALPHA-003", cardBody("Add part 3"), 5))

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	report := reportOf(t, st)
	assert.Equal(t, 3, report.Read)
	require.Len(t, report.Cards, 1)
	assert.Equal(t, "ALPHA-003", report.Cards[0].Card)
	assert.Equal(t, []string{"cost"}, report.Cards[0].Why)
	require.NotNil(t, report.Cards[0].MedianUSD)
	assert.InDelta(t, 1, *report.Cards[0].MedianUSD, 1e-9)
	assert.Empty(t, lessons(t, st), "no card at fault, no lesson")
}

func TestRetrospective_WithoutASessionIsNotFound(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, twoCardScript())

	require.ErrorIs(t, c.Retrospective(context.Background(), "BP1", "m-retro"), ErrNotFound)
}

func TestRetrospective_BeforeTheCardsAreEmittedSaysSo(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, twoCardScript())
	drive(t, c, "BP1")

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	got := replies(rec)
	require.Len(t, got, 1)
	assert.Equal(t, retroNotEmitted, got[0]["reason"])
	assert.Equal(t, false, got[0]["during_run"])
	assert.Equal(t, "m-retro", got[0]["message_id"])
	assert.Empty(t, retroRuns(t, st))
	assert.False(t, holdsSlot(c, "BP1"))
}

func TestRetrospective_WhileARunHoldsTheSlotSaysSo(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})

	script := twoCardScript()
	script[RetroStage] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		signal(entered)

		select {
		case <-release:
		case <-ctx.Done():
			return stage.Output{}, ctx.Err()
		}

		return done("x", map[string]any{"findings": []any{retroFinding("ALPHA-001", "needs a key", nil)}}), nil
	}

	c, _, runner, rec, fake := emitted(t, script)
	free := closer(t, release)

	fake.setEnded("ALPHA-001", parked("ALPHA-001", cardBody("needs a key"), "no key"))
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	ctx := context.Background()

	require.NoError(t, c.Retrospective(ctx, "BP1", "m-first"))
	receive(t, entered, "the first pass's run")

	require.NoError(t, c.Retrospective(ctx, "BP1", "m-second"))

	got := replies(rec)
	require.Len(t, got, 1)
	assert.Equal(t, retroRunning, got[0]["reason"])
	assert.Equal(t, true, got[0]["during_run"])
	assert.Equal(t, "m-second", got[0]["message_id"])
	assert.Len(t, inputsOf(runner, RetroStage), 1, "the second reply starts no run")

	free()
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, RetroStage), 1)
	assert.Len(t, replies(rec), 1, "the first pass completes")
}

func TestRetrospective_APackWithoutItsInstructionsSaysSo(t *testing.T) {
	c, st, runner, rec, _ := emitted(t, twoCardScript())

	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS("testdata/pipeline")))
	require.NoError(t, os.Remove(filepath.Join(dir, "stages", "retrospective.md")))

	pk, err := pack.LoadDir(dir)
	require.NoError(t, err)

	stopWatch(t, c, "BP1")
	c.opts.Pack = pk

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	assert.Equal(t, []string{retroNoInstructions}, reasonsOf(rec))
	assert.Empty(t, inputsOf(runner, RetroStage))
	assert.Empty(t, retroRuns(t, st))
	assert.False(t, holdsSlot(c, "BP1"))
}

func TestRetrospective_NoStruggledCardRunsNoModel(t *testing.T) {
	c, st, runner, rec, fake := emitted(t, manyCardScript(3))

	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("ALPHA-%03d", i)
		fake.setEnded(id, finished(id, cardBody(fmt.Sprintf("Add part %d", i)), 1))
	}

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	got := replies(rec)
	require.Len(t, got, 1)
	assert.Equal(t, retroNoStruggle, got[0]["reason"])
	assert.Equal(t, false, got[0]["during_run"])
	assert.Empty(t, inputsOf(runner, RetroStage))
	assert.Empty(t, lessons(t, st))
	assert.Equal(t, 3, fake.endedReads(), "every emitted card is read once")
}

func TestRetrospective_SettlesAStateACrashLeftRunning(t *testing.T) {
	c, st, _, rec, fake := emitted(t, twoCardScript())
	fake.setEnded("ALPHA-001", finished("ALPHA-001", cardBody("Add the parser"), 0))
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	require.NoError(t, st.SetBlueprintState(context.Background(), "BP1", string(StateRunning)))

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	assert.Equal(t, []string{retroNoStruggle}, reasonsOf(rec))
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
}

func TestRetrospective_SetsDoneOnceTheWatchHasEnded(t *testing.T) {
	script := twoCardScript()
	script[RetroStage] = retroRun(retroFinding("ALPHA-001", "needs a key", nil))

	c, st, _, _, fake := emitted(t, script)
	fake.setEnded("ALPHA-001", parked("ALPHA-001", cardBody("needs a key"), "no key"))
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	require.NoError(t, st.AppendEvent(context.Background(), store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventWatchEnded, Payload: json.RawMessage(`{}`),
	}))

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	reportOf(t, st)
	assert.Equal(t, string(StateDone), stateOf(t, st, "BP1"))
}

func TestRetrospective_AFailedRunStoresNothingAndKeepsTheBlueprintUsable(t *testing.T) {
	script := twoCardScript()
	script[RetroStage] = func(context.Context, stage.Input) (stage.Output, error) {
		return stage.Output{}, errors.New("endpoint unreachable")
	}

	c, st, runner, rec, fake := emitted(t, script)
	fake.setEnded("ALPHA-001", parked("ALPHA-001", cardBody("needs a key"), "no key"))
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	ctx := context.Background()

	require.NoError(t, c.Retrospective(ctx, "BP1", "m-retro"))
	c.Wait("BP1")

	got := replies(rec)
	require.Len(t, got, 1)
	assert.Equal(t, retroStopped, got[0]["reason"])
	assert.Equal(t, false, got[0]["during_run"])
	assert.Empty(t, lessons(t, st))
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"), "a retrospective never fails the blueprint")

	runs := retroRuns(t, st)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunFailed, runs[0].Status)

	_, err := st.LatestEvent(ctx, "BP1", EventRetroReport)
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, c.Retrospective(ctx, "BP1", "m-again"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, RetroStage), 2, "a repeated reply runs the model again")

	runs = retroRuns(t, st)
	require.Len(t, runs, 2)
	assert.Equal(t, 2, runs[1].Attempt)
}

func TestRetrospective_AnUnreadableCardStoresNothing(t *testing.T) {
	c, st, runner, rec, fake := emitted(t, twoCardScript())
	fake.setEnded("ALPHA-001", parked("ALPHA-001", cardBody("needs a key"), "no key"))
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	fake.mu.Lock()
	fake.failEnded = 1
	fake.mu.Unlock()

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	got := replies(rec)
	require.Len(t, got, 1)
	assert.Equal(t, retroUnread, got[0]["reason"])
	assert.Contains(t, got[0]["message"], "ALPHA-001", "the reply names the card")
	assert.Empty(t, inputsOf(runner, RetroStage))
	assert.Empty(t, lessons(t, st))
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
}

func TestRetrospective_ACapThatRefusesThePassSaysRaiseItAndReplyAgain(t *testing.T) {
	script := twoCardScript()
	script[RetroStage] = retroRun(retroFinding("ALPHA-001", "needs a key", lessonFor("pack")))

	c, st, runner, rec, fake := emitted(t, script)
	fake.setEnded("ALPHA-001", parked("ALPHA-001", cardBody("needs a key"), "no key"))
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	ctx := context.Background()

	require.NoError(t, c.SetCap(ctx, "BP1", "0.05", "m-cap"))
	c.Wait("BP1")

	require.NoError(t, c.Retrospective(ctx, "BP1", "m-retro"))
	c.Wait("BP1")

	got := replies(rec)
	require.Len(t, got, 1)
	assert.Equal(t, retroCapped, got[0]["reason"])
	assert.Equal(t, false, got[0]["during_run"])
	assert.Empty(t, inputsOf(runner, RetroStage))
	assert.Empty(t, retroRuns(t, st), "a refused run is never started")
	assert.Empty(t, lessons(t, st))
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))

	require.NoError(t, c.Resume(ctx, "BP1", "m-c"))
	c.Wait("BP1")

	resumed := rec.all(EventResume)
	require.Len(t, resumed, 1)
	assert.Equal(t, true, resumed[0].Payload["nothing"], "continue resumes no pass")

	require.NoError(t, c.SetCap(ctx, "BP1", "off", "m-off"))
	c.Wait("BP1")

	caps := rec.all(EventCap)
	require.Len(t, caps, 2)
	assert.NotContains(t, caps[1].Payload, "resumes", "lifting the cap resumes no pass")
	assert.Empty(t, inputsOf(runner, RetroStage))
	assert.Empty(t, retroRuns(t, st))
}

func TestRetrospective_AbortCancelsThePass(t *testing.T) {
	entered := make(chan struct{}, 1)

	script := twoCardScript()
	script[RetroStage] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		signal(entered)
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	}

	c, st, _, rec, fake := emitted(t, script)
	fake.setEnded("ALPHA-001", parked("ALPHA-001", cardBody("needs a key"), "no key"))
	fake.setEnded("ALPHA-002", finished("ALPHA-002", cardBody("Wire the parser"), 0))

	ctx := context.Background()

	require.NoError(t, c.Retrospective(ctx, "BP1", "m-retro"))
	receive(t, entered, "the retro run")

	require.NoError(t, c.Abort(ctx, "BP1", "m-abort"))
	c.Wait("BP1")

	assert.Equal(t, []string{retroStopped}, reasonsOf(rec))
	assert.Empty(t, lessons(t, st))

	aborted, ok := rec.first(EventAborted)
	require.True(t, ok)
	assert.Equal(t, RetroStage, aborted.Payload["stage"])

	runs := retroRuns(t, st)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunCanceled, runs[0].Status)
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
}

func TestRetrospective_AnAbortBeforeThePassStartsNamesIt(t *testing.T) {
	c, st, _, rec, _ := emitted(t, twoCardScript())
	ctx := context.Background()

	// Held, the round lock keeps the reply between its claim and the pass.
	lock := c.roundLock("BP1")
	lock.Lock()

	done := make(chan error, 1)

	go func() { done <- c.Retrospective(ctx, "BP1", "m-retro") }()

	require.Eventually(t, func() bool { return holdsSlot(c, "BP1") }, 5*time.Second, time.Millisecond)
	require.NoError(t, c.Abort(ctx, "BP1", "m-abort"))
	lock.Unlock()
	require.NoError(t, <-done)
	c.Wait("BP1")

	aborted, ok := rec.first(EventAborted)
	require.True(t, ok)
	assert.Equal(t, RetroStage, aborted.Payload["stage"])
	assert.Equal(t, []string{retroStopped}, reasonsOf(rec))
	assert.Empty(t, retroRuns(t, st))
}

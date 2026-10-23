package blueprint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// activeLesson adds a lesson of blueprint BP0 carrying ov and approves it.
func activeLesson(t *testing.T, st *store.Store, scope, project, text string, ov contract.Overlay) store.Lesson {
	t.Helper()

	ctx := context.Background()

	raw, err := json.Marshal(ov)
	require.NoError(t, err)

	added, err := st.AddLessons(ctx, []store.Lesson{{
		BlueprintID: "BP0", Scope: scope, Project: project, Text: text, Overlay: raw, CreatedAt: time.Now().UTC(),
	}})
	require.NoError(t, err)
	require.Len(t, added, 1)

	ok, err := st.DecideLesson(ctx, added[0].ID, store.LessonDecision{State: store.LessonActive, Scope: scope, Text: text, By: "tester", At: time.Now().UTC()})
	require.NoError(t, err)
	require.True(t, ok)

	l, err := st.GetLesson(ctx, added[0].ID)
	require.NoError(t, err)

	return l
}

func TestListsFor_MergesTheSetsOverlaysInLessonOrder(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP-OLD")))

	activeLesson(t, st, store.ScopePack, "", "first", contract.Overlay{DanglingReferences: []string{"as before"}})
	activeLesson(t, st, store.ScopePack, "", "second", contract.Overlay{DanglingReferences: []string{"as said"}})

	require.NoError(t, c.Start(ctx, startInput("BP-NEW")))

	pk := c.opts.Pack.Lists().DanglingReferences

	for id, want := range map[string][]string{
		"BP-NEW": append(append([]string{}, pk...), "as before", "as said"),
		"BP-OLD": pk,
	} {
		bp, err := st.GetBlueprint(ctx, id)
		require.NoError(t, err)

		lists, err := c.listsFor(ctx, bp)
		require.NoError(t, err)
		assert.Equal(t, want, lists.DanglingReferences, id)
	}
}

// cardsRefusal drives decomposition's submit of goodCards with a marked card
// on a blueprint started after a lesson forbidding the mark when lessonFirst,
// before it otherwise.
func cardsRefusal(t *testing.T, lessonFirst bool) error {
	t.Helper()

	var got error

	script := dialogScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		cards := goodCards()
		card := cards["cards"].([]any)[0].(map[string]any)
		card["body"] = card["body"].(string) + "LESSONMARK\n"

		got = in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: cards})

		return done("# Cards", goodCards()), nil
	}

	c, st, _, _, _ := newDialog(t, script)

	lesson := func() {
		activeLesson(t, st, store.ScopePack, "", "mark", contract.Overlay{ForbiddenReferences: []contract.Pattern{{Name: "lesson mark", Pattern: "LESSONMARK"}}})
	}

	if lessonFirst {
		lesson()
	}

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	if !lessonFirst {
		lesson()
	}

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	return got
}

func TestCards_ALessonForbiddenReferenceRefusesACard(t *testing.T) {
	err := cardsRefusal(t, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden_reference")

	// The dialog pack's other checks may still refuse the draft.
	if err := cardsRefusal(t, false); err != nil {
		assert.NotContains(t, err.Error(), "forbidden_reference", "a blueprint started before the lesson is not held to it")
	}
}

// lessonTraced starts BP1 on the traced pack after an active lesson with
// edge case, with intake stating outcomes and one slice.
func lessonTraced(t *testing.T, edge string) (*Controller, store.Blueprint) {
	t.Helper()

	c, st, _, _, _ := newDialog(t, dialogScript())
	c.opts.Pack = tracedPack(t)
	ctx := context.Background()

	activeLesson(t, st, store.ScopePack, "", "clock", contract.Overlay{EdgeCases: []string{edge}})
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	putData(t, st, "intake", map[string]any{"outcomes": []any{outcomeItem("O1", "Callers read the version."), outcomeItem("O2", "It answers within 100 ms.")}})
	putData(t, st, "slicing", oneSlice())

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	return c, bp
}

func acceptanceTask(t *testing.T, c *Controller, bp store.Blueprint) string {
	t.Helper()

	step, ok := c.stepAt("acceptance")
	require.True(t, ok)

	var b strings.Builder

	require.NoError(t, c.writeEdgeCases(context.Background(), &b, bp, step))

	return b.String()
}

func TestAcceptance_ALessonEdgeCaseIsSweptAndListed(t *testing.T) {
	c, bp := lessonTraced(t, "Clock skew")

	assert.Contains(t, acceptanceTask(t, c, bp), "- Clock skew\n")

	validate, _ := c.criteriaValidator(bp, "acceptance", nil)
	refusedFor(t, validate, tracedAcceptance(), "sweep_missing")

	d := tracedAcceptance()
	s := d["slices"].([]any)[0].(map[string]any)
	s["sweep"] = append(s["sweep"].([]any), map[string]any{"condition": "clock SKEW", "settled_by": []any{"AC1"}})

	validate, _ = c.criteriaValidator(bp, "acceptance", nil)
	refusedFor(t, validate, d, "")
}

func TestAcceptance_ALessonEdgeCaseThePackNowListsIsSweptOnce(t *testing.T) {
	c, bp := lessonTraced(t, " limits")

	assert.Equal(t, 1, strings.Count(acceptanceTask(t, c, bp), "- Limits\n"))

	validate, _ := c.criteriaValidator(bp, "acceptance", nil)
	refusedFor(t, validate, tracedAcceptance(), "")
}

func TestAssumptions_ALessonDanglingReferenceRefusesAnAssumption(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP-OLD")))
	activeLesson(t, st, store.ScopePack, "", "agreed", contract.Overlay{DanglingReferences: []string{"as agreed"}})
	require.NoError(t, c.Start(ctx, startInput("BP-NEW")))

	submit := func(id string) error {
		bp, err := st.GetBlueprint(ctx, id)
		require.NoError(t, err)

		validate, _ := c.assumptionsValidator(bp, "intake")

		return validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"assumptions": []any{"We ship it as agreed."}}})
	}

	err := submit("BP-NEW")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "as agreed")

	assert.NoError(t, submit("BP-OLD"))
}

func TestRetrospective_ItsTaskGivesTheBlueprintsLists(t *testing.T) {
	script := twoCardScript()
	script[RetroStage] = retroRun(retroFinding("ALPHA-001", "ALPHA-001", nil), retroFinding("ALPHA-002", "ALPHA-002", nil))

	c, st, runner, _, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	activeLesson(t, st, store.ScopePack, "", "agreed", contract.Overlay{DanglingReferences: []string{"as agreed upon"}})

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m-approve"))
	c.Wait("BP1")

	fake.setEnded("ALPHA-001", parked("ALPHA-001", cardBody("needs a key"), "no key"))
	fake.setEnded("ALPHA-002", cm.EndedCard{Card: cm.Card{ID: "ALPHA-002", State: "done", ReviewAttempts: 2}, Body: cardBody("Wire the parser")})

	require.NoError(t, c.Retrospective(context.Background(), "BP1", "m-retro"))
	c.Wait("BP1")

	runs := inputsOf(runner, RetroStage)
	require.Len(t, runs, 1)
	assert.Contains(t, runs[0].Task, "as agreed upon")
}

// lessonPack is the pipeline pack, copied to a temporary directory, with
// readiness checking readiness and, when cardStage is not decomposition, the
// stage that checks cards named cardStage.
func lessonPack(t *testing.T, cardStage string) *pack.Pack {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.CopyFS(dir, os.DirFS("testdata/pipeline")))

	packYAML := filepath.Join(dir, "pack.yaml")
	raw, err := os.ReadFile(packYAML) //nolint:gosec // the test's own temp dir
	require.NoError(t, err)

	text := strings.Replace(string(raw), "    loops_to: decomposition\n", "    checks: [readiness]\n    loops_to: decomposition\n", 1)
	require.NotEqual(t, string(raw), text)

	if cardStage != "decomposition" {
		text = strings.ReplaceAll(text, "decomposition", cardStage)
		require.NoError(t, os.Rename(filepath.Join(dir, "stages", "decomposition.md"), filepath.Join(dir, "stages", cardStage+".md")))
	}

	require.NoError(t, os.WriteFile(packYAML, []byte(text), 0o600)) //nolint:gosec // the test's own temp dir

	pk, err := pack.LoadDir(dir)
	require.NoError(t, err)

	return pk
}

// instructionsByStage is the instructions of the first run of each stage.
func instructionsByStage(runner *fakeRunner) map[string]string {
	runner.mu.Lock()
	defer runner.mu.Unlock()

	out := map[string]string{}

	for _, in := range runner.inputs {
		if _, ok := out[in.Stage]; !ok {
			out[in.Stage] = in.Instructions
		}
	}

	return out
}

func TestWithLessons(t *testing.T) {
	assert.Equal(t, "base", withLessons("base", nil))

	got := withLessons("base", []store.Lesson{{ID: 1, Text: "a"}, {ID: 2, Text: "b"}})

	assert.True(t, strings.HasPrefix(got, "base\n\n## Lessons from earlier blueprints\n\n"))
	assert.Contains(t, got, "- L1: a\n- L2: b\n")
}

func TestStageInput_LessonsReachDecompositionAndReadinessOnly(t *testing.T) {
	c, st, runner, _, _ := newPipeline(t, happyScript())
	c.opts.Pack = lessonPack(t, "decomposition")
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP-OLD")))
	activeLesson(t, st, store.ScopePack, "", "keep each card to one deliverable", contract.Overlay{})

	drive(t, c, "BP-NEW")

	got := instructionsByStage(runner)
	require.Contains(t, got, "decomposition")
	require.Contains(t, got, "readiness")

	for _, id := range []string{"decomposition", "readiness"} {
		assert.Contains(t, got[id], "L1: keep each card to one deliverable", id)
	}

	for _, id := range []string{"intake", "grounding"} {
		require.Contains(t, got, id)
		assert.NotContains(t, got[id], "L1", id)
	}
}

func TestStageInput_LessonsFollowTheCardsCheckNotTheStageID(t *testing.T) {
	script := happyScript()
	script["cards"] = script["decomposition"]
	delete(script, "decomposition")

	c, st, runner, _, _ := newPipeline(t, script)
	c.opts.Pack = lessonPack(t, "cards")
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP-OLD")))
	activeLesson(t, st, store.ScopePack, "", "keep each card to one deliverable", contract.Overlay{})

	drive(t, c, "BP-NEW")

	got := instructionsByStage(runner)
	require.Contains(t, got, "cards")
	assert.Contains(t, got["cards"], "L1: keep each card to one deliverable")
}

func TestStageInput_ABlueprintCreatedBeforeTheApprovalGetsNone(t *testing.T) {
	c, st, runner, _, _ := newPipeline(t, happyScript())
	c.opts.Pack = lessonPack(t, "decomposition")
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP-OLD")))
	activeLesson(t, st, store.ScopePack, "", "keep each card to one deliverable", contract.Overlay{})

	require.NoError(t, c.Message(ctx, "BP-OLD", "the need", "m1"))
	c.Wait("BP-OLD")

	got := instructionsByStage(runner)
	require.Contains(t, got, "decomposition")
	assert.NotContains(t, got["decomposition"], "L1")
	assert.NotContains(t, got["readiness"], "L1")
}

func TestStageInput_TheReviewerOfDecompositionGetsNoLesson(t *testing.T) {
	script := happyScript()
	script["decomposition.review"] = passing()

	c, st, runner, _, _ := newPipeline(t, script)
	c.opts.Pack = gatedCards(lessonPack(t, "decomposition"))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP-OLD")))
	activeLesson(t, st, store.ScopePack, "", "keep each card to one deliverable", contract.Overlay{})

	drive(t, c, "BP-NEW")

	got := instructionsByStage(runner)
	require.Contains(t, got, "decomposition.review")
	assert.Contains(t, got["decomposition"], "L1")
	assert.NotContains(t, got["decomposition.review"], "keep each card to one deliverable")
}

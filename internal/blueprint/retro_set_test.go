package blueprint

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
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

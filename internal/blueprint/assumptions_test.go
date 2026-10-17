package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// lineWith returns the first line of body that carries every s, or "".
func lineWith(body string, s ...string) string {
	for _, l := range strings.Split(body, "\n") {
		if !slices.ContainsFunc(s, func(part string) bool { return !strings.Contains(l, part) }) {
			return l
		}
	}

	return ""
}

func TestAssumptions_AVerdictReachesTheSignoffAndTheCard(t *testing.T) {
	script := happyScript()
	script["intake"] = returns(needBody, map[string]any{"slices": 1, "assumptions": []any{"The parser stays in Go.", "The API keeps its routes."}})
	script["grounding"] = returns("# Grounding", map[string]any{"verdicts": []any{
		map[string]any{"id": "A1", "status": "Verified", "evidence": "[S1] internal/x.go is Go"},
		map[string]any{"id": "A9", "status": "verified", "evidence": "no such assumption"},
	}})

	c, st, runner, rec, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")

	grounded := false

	for i := range runner.calls() {
		if in := runner.input(i); in.Stage == "grounding" {
			grounded = true

			assert.NotEmpty(t, lineWith(in.Task, "A1", "The parser stays in Go."), "grounding is told each assumption's id")
		}
	}

	require.True(t, grounded, "grounding ran")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []any{
		map[string]any{"id": "A2", "stage": "intake", "text": "The API keeps its routes.", "status": "unverified", "evidence": ""},
		map[string]any{"id": "A1", "stage": "intake", "text": "The parser stays in Go.", "status": "verified", "evidence": "[S1] internal/x.go is Go"},
	}, so.Payload["assumptions"], "unverified first")

	emitNow(t, c, st, "BP1")

	cards, _ := fake.inputs()
	require.Len(t, cards, 1)
	assert.Contains(t, lineWith(cards[0].Body, "The parser stays in Go."), "(verified: [S1] internal/x.go is Go)")
	assert.Contains(t, lineWith(cards[0].Body, "The API keeps its routes."), "(unverified)")
	assert.Contains(t, cards[0].Body, "- S1: ", "the evidence's tag is mapped in the Sources section")
}

func TestAssumptions_AnIntakeWithoutDataAssumptionsKeepsItsSection(t *testing.T) {
	script := happyScript()
	script["intake"] = returns(needBody+"## Assumptions\n- The parser stays in Go.\n", map[string]any{"slices": 1})

	c, st, _, rec, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ctx := context.Background()

	drive(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, "- The parser stays in Go.", so.Payload["assumptions"], "the section's text, without ids or statuses")

	sum, err := c.summary(ctx, "BP1")
	require.NoError(t, err)
	assert.Contains(t, sum.description("BP1"), "- The parser stays in Go.", "the playbook description")

	emitNow(t, c, st, "BP1")

	cards, _ := fake.inputs()
	require.Len(t, cards, 1)
	assert.Contains(t, cards[0].Body, "- The parser stays in Go.", "a single card's Context")
}

func TestAssumptions_IntakesSubmitIsChecked(t *testing.T) {
	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.Validate == nil {
			return stage.Output{}, errors.New("intake's submit has no validator")
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"assumptions": []any{"See above."}}}); err == nil {
			return stage.Output{}, errors.New("an assumption pointing above is refused")
		}

		return done("# Need", nil), nil
	}

	c, st, _, _, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	_, err := st.LatestArtifact(context.Background(), "BP1", "intake")
	require.NoError(t, err)
}

func TestAssumptionsValidator(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	submit := func(v func(context.Context, stage.Artifact) error, assumptions ...any) error {
		return v(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"assumptions": assumptions}})
	}

	v := c.assumptionsValidator()
	require.Error(t, submit(v, "The limits stay as noted above."), "a sentence that points above")
	require.NoError(t, submit(v, "Requests stay under 100 per second."), "fixed")

	v = c.assumptionsValidator()
	require.Error(t, submit(v, "", 3), "not sentences")
	require.Error(t, submit(v, "As noted above, the cache stays."))
	require.NoError(t, submit(v, "As noted above, the cache stays."), "the third failing submit is accepted, so the stage never loops")

	require.NoError(t, c.assumptionsValidator()(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"slices": 1}}), "no assumptions is fine")
}

// putData stores the next version of a stage's artifact, after every
// artifact stored so far.
func putData(t *testing.T, st *store.Store, stageID string, data map[string]any) {
	t.Helper()

	raw, err := json.Marshal(data)
	require.NoError(t, err)

	_, err = st.PutArtifact(context.Background(), store.Artifact{BlueprintID: "BP1", Stage: stageID, Body: "# " + stageID, Data: raw})
	require.NoError(t, err)
}

func statuses(list []assumption) map[string]string {
	out := map[string]string{}
	for _, a := range list {
		out[a.ID] = a.Status
	}

	return out
}

func TestAssumptions_LatestVerdictWinsAndSignoffOrdersByStatus(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	putData(t, st, "intake", map[string]any{"assumptions": []any{"One.", "Two.", "Three.", "Four."}})
	putData(t, st, "grounding", map[string]any{"verdicts": []any{
		map[string]any{"id": "A1", "status": "verified", "evidence": "[S1] first"},
		map[string]any{"id": "A2", "status": "verified", "evidence": "[S1] second"},
		map[string]any{"id": "A4", "status": "contradicted", "evidence": "[S1] fourth"},
	}})
	putData(t, st, "decomposition", map[string]any{"verdicts": []any{
		map[string]any{"id": "A1", "status": "contradicted", "evidence": "[S1] a later stage found otherwise"},
	}})

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": "contradicted", "A2": "verified", "A3": "unverified", "A4": "contradicted"}, statuses(list))
	assert.Equal(t, "[S1] a later stage found otherwise", list[0].Evidence, "a later stage's verdict wins")

	var order []string
	for _, a := range signoffOrder(list) {
		order = append(order, a.ID)
	}

	assert.Equal(t, []string{"A3", "A1", "A4", "A2"}, order, "unverified, contradicted, then verified, each in id order")
}

func TestAssumptions_AVerdictOlderThanTheAssumptionsDoesNotApply(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	putData(t, st, "intake", map[string]any{"assumptions": []any{"One."}})
	putData(t, st, "grounding", map[string]any{"verdicts": []any{map[string]any{"id": "A1", "status": "verified", "evidence": "[S1] x"}}})

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": "verified"}, statuses(list))

	putData(t, st, "intake", map[string]any{"assumptions": []any{"Something else entirely."}})

	list, err = c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": "unverified"}, statuses(list), "the verdict judged the old sentence")
	assert.Empty(t, list[0].Evidence)
}

func TestAssumptions_ARerunOfAnEarlierStatingStageInvalidatesVerdictsOnTheLaterOnesIds(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	// A second stating stage, after intake in pack order.
	later := slices.IndexFunc(c.opts.Pack.Stages, func(s pack.Stage) bool { return s.ID == "decomposition" })
	require.Positive(t, later)
	c.opts.Pack.Stages[later].Checks = append(slices.Clone(c.opts.Pack.Stages[later].Checks), pack.CheckAssumptions)

	putData(t, st, "intake", map[string]any{"assumptions": []any{"One."}})
	putData(t, st, "decomposition", map[string]any{"assumptions": []any{"Two."}})
	putData(t, st, "readiness", map[string]any{"verdicts": []any{map[string]any{"id": "A2", "status": "verified", "evidence": "[S1] x"}}})

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": "unverified", "A2": "verified"}, statuses(list))

	putData(t, st, "intake", map[string]any{"assumptions": []any{"One.", "One and a half."}})

	list, err = c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": "unverified", "A2": "unverified", "A3": "unverified"}, statuses(list),
		"the verdict on A2 judged a sentence that is A3 now")
}

func TestAssumptions_AMalformedVerdictCostsOnlyItself(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	putData(t, st, "intake", map[string]any{"assumptions": []any{"One.", "Two."}})
	putData(t, st, "grounding", map[string]any{"verdicts": []any{
		map[string]any{"id": "A1", "status": "verified", "evidence": []any{"[S1] a", "[S2] b"}},
		map[string]any{"id": "A2", "status": "verified", "evidence": "[S1] fine"},
	}})

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": "unverified", "A2": "verified"}, statuses(list))
}

func TestAllOf(t *testing.T) {
	ctx := context.Background()

	counting := func(name string, refuse int) (func(context.Context, stage.Artifact) error, *int) {
		calls := new(int)

		return func(context.Context, stage.Artifact) error {
			*calls++

			if *calls <= refuse {
				return errors.New(name + " refuses")
			}

			return nil
		}, calls
	}

	assert.Nil(t, allOf(nil))

	a, aCalls := counting("a", 2)
	b, bCalls := counting("b", 2)
	v := allOf([]func(context.Context, stage.Artifact) error{a, b})

	for range 2 {
		err := v(ctx, stage.Artifact{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "a refuses")
		assert.Contains(t, err.Error(), "b refuses", "every refusal is reported")
	}

	require.NoError(t, v(ctx, stage.Artifact{}), "the third submit is accepted")
	assert.Equal(t, 3, *aCalls)
	assert.Equal(t, 3, *bCalls, "the second validator sees every submit")
}

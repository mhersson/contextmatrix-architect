package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
		map[string]any{"id": "A2", "stage": "intake", "text": "The API keeps its routes.", "status": "unverified", "basis": "", "answer": "", "evidence": ""},
		map[string]any{"id": "A1", "stage": "intake", "text": "The parser stays in Go.", "status": "verified", "basis": "", "answer": "", "evidence": "[S1] internal/x.go is Go"},
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
	bp := store.Blueprint{ID: "BP1"}

	submit := func(v func(context.Context, stage.Artifact) error, assumptions ...any) error {
		return v(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"assumptions": assumptions}})
	}

	v, state := c.assumptionsValidator(bp, "intake")
	require.Error(t, submit(v, "The limits stay as noted above."), "a sentence that points above")
	require.NoError(t, submit(v, "Requests stay under 100 per second."), "fixed")
	assert.Empty(t, state.last(), "an accepted submit that passes keeps no problems")

	v, state = c.assumptionsValidator(bp, "intake")
	require.Error(t, submit(v, "", 3), "not sentences")
	require.Error(t, submit(v, "As noted above, the cache stays."))
	require.NoError(t, submit(v, "As noted above, the cache stays."), "the third failing submit is accepted, so the stage never loops")
	assert.Len(t, state.last(), 1, "with the problem it still has")

	v, _ = c.assumptionsValidator(bp, "intake")
	require.NoError(t, v(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"slices": 1}}), "no assumptions is fine")
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

// seedAnswer stores answered question number of BP1, answered "answer <n>":
// by the user, or by the responder with the evidence its own tool calls
// returned, "" for none.
func seedAnswer(t *testing.T, st *store.Store, number int, responder, evidence string) {
	t.Helper()

	ctx := context.Background()
	q := store.Question{
		BlueprintID: "BP1", Stage: "intake", Number: number, Text: fmt.Sprintf("Question %d?", number), Answer: fmt.Sprintf("answer %d", number),
		Responder: responder, Assumption: responder == store.ResponderAuto, Grounded: evidence != "",
	}

	if q.Assumption {
		payload, err := json.Marshal(map[string]any{"number": number, "text": q.Answer, "responder": responder, "grounded": q.Grounded, "evidence": evidence})
		require.NoError(t, err)
		require.NoError(t, st.AppendEvent(ctx, store.Event{Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventAnswer, Payload: payload}))
	}

	require.NoError(t, st.AddQuestion(ctx, q))
}

func byID(list []assumption) map[string]assumption {
	out := map[string]assumption{}
	for _, a := range list {
		out[a.ID] = a
	}

	return out
}

func TestAssumptions_ABasisOnAHumanAnswerIsConfirmed(t *testing.T) {
	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Routes", "Keep the routes?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		return done(needBody, map[string]any{"slices": 1, "assumptions": []any{
			map[string]any{"text": "The API keeps its routes.", "basis": " [q1] "},
		}}), nil
	}

	c, _, _, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "keep them"))
	c.Wait("BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []any{
		map[string]any{"id": "A1", "stage": "intake", "text": "The API keeps its routes.", "status": "confirmed", "basis": "Q1", "answer": "keep them", "evidence": ""},
	}, so.Payload["assumptions"], "a human answer has no entry of its own, and the one resting on it carries it")
}

func TestAssumptions_ABasisOnAnUnverifiedAutoAnswerMergesItsEntry(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	seedAnswer(t, st, 1, store.ResponderAuto, "")
	seedAnswer(t, st, 2, store.ResponderAuto, "")
	putData(t, st, "intake", map[string]any{"assumptions": []any{
		map[string]any{"text": "Charts ship with helm.", "basis": "Q1"},
		map[string]any{"text": "Helm 3 is installed.", "basis": "Q1"},
	}})

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": "assumed", "A2": "assumed", "Q2": "assumed"}, statuses(list), "Q1 is listed once, as what rests on it")

	got := byID(list)
	assert.Equal(t, "Q1", got["A1"].Basis)
	assert.Equal(t, "Q1", got["A2"].Basis)
	assert.Equal(t, "Q2", got["Q2"].Basis, "an auto answer no assumption names keeps its own entry")
	assert.Equal(t, "answer 1", got["A1"].Answer, "the merged entries carry the answer they rest on")
	assert.Equal(t, "answer 1", got["A2"].Answer)
	assert.Empty(t, got["Q2"].Answer, "its own entry's text holds it already")
}

func TestAssumptions_AnEntryRestingOnAnAutoAnswerCarriesItToTheSignoff(t *testing.T) {
	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or kustomize?", "helm", "kustomize")}}); err != nil {
			return stage.Output{}, err
		}

		return done(needBody, map[string]any{"slices": 1, "assumptions": []any{
			map[string]any{"text": "Charts live under charts/.", "basis": "Q1"},
		}}), nil
	}
	script["intake.responder"] = returns("# Answer", map[string]any{"answer": "helm", "reasoning": "a guess", "grounded": false})

	c, _, _, rec, _ := newPipeline(t, script)

	driveIn(t, c, autoInput())

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []any{
		map[string]any{"id": "A1", "stage": "intake", "text": "Charts live under charts/.", "status": "assumed", "basis": "Q1", "answer": "helm", "evidence": ""},
	}, so.Payload["assumptions"], "the answer the AI gave stays in view on the one entry")
}

func TestAssumptions_ABasisOnAVerifiedAutoAnswerIsVerified(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	seedAnswer(t, st, 1, store.ResponderAuto, "[S1] charts/app")
	putData(t, st, "intake", map[string]any{"assumptions": []any{map[string]any{"text": "Charts ship with helm.", "basis": "Q1"}}})

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, assumption{ID: "A1", Stage: "intake", Text: "Charts ship with helm.", Basis: "Q1", Answer: "answer 1", Status: "verified", Evidence: "[S1] charts/app"}, list[0])
}

func TestAssumptions_ABasisNamingNoAnsweredQuestionReadsAsNone(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	seedAnswer(t, st, 1, store.ResponderAuto, "")
	putData(t, st, "intake", map[string]any{"assumptions": []any{
		map[string]any{"text": "One.", "basis": "Q9"},
		map[string]any{"text": "Two.", "basis": "S1"},
		map[string]any{"text": "Three."},
	}})

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": "unverified", "A2": "unverified", "A3": "unverified", "Q1": "assumed"}, statuses(list))
	require.Len(t, list, 4)

	for _, a := range list[:3] {
		assert.Empty(t, a.Basis, a.ID)
		assert.Empty(t, a.Answer, a.ID)
	}
}

func TestAssumptions_AGroundingVerdictOverridesTheAnswersStatusUnlessUnverified(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	seedAnswer(t, st, 1, store.ResponderHuman, "")
	seedAnswer(t, st, 2, store.ResponderAuto, "[S1] charts/app")
	putData(t, st, "intake", map[string]any{"assumptions": []any{
		map[string]any{"text": "One.", "basis": "Q1"},
		map[string]any{"text": "Two.", "basis": "Q1"},
		map[string]any{"text": "Three.", "basis": "Q1"},
		map[string]any{"text": "Four.", "basis": "Q2"},
		"Five.",
		map[string]any{"text": "Six.", "basis": "Q1"},
		map[string]any{"text": "Seven.", "basis": "Q2"},
	}})
	putData(t, st, "grounding", map[string]any{"verdicts": []any{
		map[string]any{"id": "A1", "status": "verified", "evidence": "[S2] one"},
		map[string]any{"id": "A2", "status": "contradicted", "evidence": "[S2] two"},
		map[string]any{"id": "A3", "status": "unverified", "evidence": "not found"},
		map[string]any{"id": "A4", "status": "unverified", "evidence": "not found"},
		map[string]any{"id": "A5", "status": "unverified", "evidence": "not found"},
		map[string]any{"id": "A6", "status": "verified", "evidence": "[S2] six"},
		map[string]any{"id": "A7", "status": "contradicted", "evidence": "[S2] seven"},
	}})
	// A later stage that cannot find them in the code.
	putData(t, st, "decomposition", map[string]any{"verdicts": []any{
		map[string]any{"id": "A6", "status": "unverified", "evidence": "not found"},
		map[string]any{"id": "A7", "status": "unverified", "evidence": "not found"},
	}})

	list, err := c.assumptions(context.Background(), "BP1")
	require.NoError(t, err)

	got := byID(list)
	assert.Equal(t, map[string]string{
		"A1": "verified", "A2": "contradicted", "A3": "confirmed", "A4": "verified", "A5": "unverified", "A6": "confirmed", "A7": "verified",
	}, statuses(list))
	assert.Equal(t, "[S2] one", got["A1"].Evidence)
	assert.Equal(t, "[S2] two", got["A2"].Evidence)
	assert.Empty(t, got["A3"].Evidence, "an unverified verdict resets to the answer's status")
	assert.Equal(t, "[S1] charts/app", got["A4"].Evidence, "and to the responder's evidence")
	assert.Empty(t, got["A5"].Evidence, "and without a basis to unverified with none")
	assert.Empty(t, got["A6"].Evidence, "a later unverified verdict resets an earlier verified one")
	assert.Equal(t, "[S1] charts/app", got["A7"].Evidence, "and an earlier contradicted one")
}

func TestSignoffOrder_RiskiestFirst(t *testing.T) {
	list := []assumption{
		{ID: "A1", Status: assumptionVerified},
		{ID: "A2", Status: assumptionConfirmed},
		{ID: "A3", Status: assumptionAssumed},
		{ID: "A4", Status: assumptionContradicted},
		{ID: "A5", Status: assumptionUnverified},
		{ID: "Q1", Status: assumptionAssumed},
		{ID: "A6", Status: assumptionUnverified},
	}

	var order []string
	for _, a := range signoffOrder(list) {
		order = append(order, a.ID)
	}

	assert.Equal(t, []string{"A5", "A6", "A4", "A3", "Q1", "A2", "A1"}, order)
}

func TestAssumptionsValidator_Basis(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	seedAnswer(t, st, 1, store.ResponderHuman, "")
	seedAnswer(t, st, 2, store.ResponderAuto, "")
	seedAnswer(t, st, 3, store.ResponderHuman, "")

	tests := []struct {
		name  string
		entry any
		ok    bool
	}{
		{"a sentence", "Requests stay under 100 per second.", true},
		{"a basis on an answered question", map[string]any{"text": "The API keeps its routes.", "basis": "Q1"}, true},
		{"a bracketed lower-case basis", map[string]any{"text": "Charts ship with helm.", "basis": "[q2]"}, true},
		{"no basis", map[string]any{"text": "The cache stays."}, true},
		{"a number no question has", "The release ships in Q4 2027.", true},
		{"an unknown basis", map[string]any{"text": "The cache stays.", "basis": "Q9"}, false},
		{"a source basis", map[string]any{"text": "The cache stays.", "basis": "S1"}, false},
		{"a basis that is not text", map[string]any{"text": "The cache stays.", "basis": 1}, false},
		{"a text naming an answered question", "As the user chose in Q1, the API keeps its routes.", false},
		{"a basis entry whose text names one", map[string]any{"text": "Per Q3, the cache stays.", "basis": "Q3"}, false},
		{"a basis entry without text", map[string]any{"basis": "Q1"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _ := c.assumptionsValidator(store.Blueprint{ID: "BP1"}, "intake")
			err := v(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"assumptions": []any{tt.entry}}})

			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestAssumptions_TheCardAndThePlaybookCarryTheBasisAndItsAnswer(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ctx := context.Background()

	drive(t, c, "BP1")

	seedAnswer(t, st, 1, store.ResponderHuman, "")
	seedAnswer(t, st, 2, store.ResponderAuto, "")
	seedAnswer(t, st, 3, store.ResponderAuto, "")
	seedAnswer(t, st, 4, store.ResponderAuto, "[S1] charts/app")
	putData(t, st, "intake", map[string]any{"slices": 1, "assumptions": []any{
		map[string]any{"text": "The API keeps its routes.", "basis": "Q1"},
		map[string]any{"text": "Charts ship with helm.", "basis": "Q2"},
		map[string]any{"text": "Helm 3 is installed.", "basis": "Q4"},
	}})

	check := func(what, body string) {
		t.Helper()

		confirmed := lineWith(body, "The API keeps its routes.")
		assert.Contains(t, confirmed, assumptionConfirmed, what)
		assert.Contains(t, confirmed, "Q1", what)
		assert.Equal(t, 1, strings.Count(confirmed, "answer 1"), "%s: the human answer it rests on", what)

		assumed := lineWith(body, "Charts ship with helm.")
		assert.Contains(t, assumed, assumptionAssumed, what)
		assert.Contains(t, assumed, "Q2", what)
		assert.Equal(t, 1, strings.Count(assumed, "answer 2"), "%s: the auto answer it rests on", what)

		verified := lineWith(body, "Helm 3 is installed.")
		assert.Contains(t, verified, assumptionVerified, what)
		assert.Contains(t, verified, "[S1] charts/app", what)
		assert.Equal(t, 1, strings.Count(verified, "Q4"), what)
		assert.Equal(t, 1, strings.Count(verified, "answer 4"), "%s: a verified entry still names the answer it rests on", what)

		own := lineWith(body, "- Q3 ")
		assert.Contains(t, own, assumptionAssumed, what)
		assert.Equal(t, 1, strings.Count(own, "Q3"), "%s: a basis equal to the id is not repeated", what)
		assert.Equal(t, 1, strings.Count(own, "answer 3"), "%s: nor is the answer its text holds", what)
	}

	sum, err := c.summary(ctx, "BP1")
	require.NoError(t, err)
	check("the playbook description", sum.description("BP1"))

	emitNow(t, c, st, "BP1")

	cards, _ := fake.inputs()
	require.Len(t, cards, 1)
	check("a single card's Context", cards[0].Body)
}

func TestAssumptions_AnAcceptedAssumptionReadsAsAcceptedAtItsStage(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	drive(t, c, "BP1")

	au := c.newAudit()

	intakeRun := doneRun(t, st, "intake")
	c.putRound(ctx, au, "BP1", intakeRun, tabRound(2, "intake", intakeRun, []string{"One region."}, accept("A1")))

	// An older close wrote the acceptance as the answered row Q5.
	designRun := doneRun(t, st, "solution_design")
	older := tabRound(3, "solution_design", designRun, []string{"Two zones."}, accept("A1"))
	older.Accepted = map[string]int{"A1": 5}
	c.putRound(ctx, au, "BP1", designRun, older)
	require.NoError(t, st.AddQuestion(ctx, store.Question{
		BlueprintID: "BP1", RunID: designRun, Stage: "solution_design", Number: 5, Text: "Two zones.", Options: []string{"Accept", "Turn into a question"},
		Answer: "Accepted", Responder: store.ResponderHuman, AnsweredAt: time.Now().UTC(),
	}))

	seedAnswer(t, st, 1, store.ResponderHuman, "")
	putData(t, st, "intake", map[string]any{"slices": 1, "assumptions": []any{
		map[string]any{"text": "One region.", "basis": "round2.A1"},
		map[string]any{"text": "Two zones.", "basis": "Q5"},
		map[string]any{"text": "The API keeps its routes.", "basis": "Q1"},
	}})

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)

	got := byID(list)
	assert.Equal(t, "intake", got["A1"].AcceptedAt)
	assert.Equal(t, "solution_design", got["A2"].AcceptedAt)
	assert.Empty(t, got["A2"].Answer)
	assert.Empty(t, got["A3"].AcceptedAt)
	assert.Equal(t, "answer 1", got["A3"].Answer)

	payload := map[string]map[string]any{}

	for _, e := range assumptionsPayload(list) {
		entry, ok := e.(map[string]any)
		require.True(t, ok)

		id, _ := entry["id"].(string)
		payload[id] = entry
	}

	assert.Equal(t, "solution_design", payload["A2"]["accepted"])
	assert.NotContains(t, payload["A3"], "accepted")

	lines := assumptionLines(list, false)

	for text, stage := range map[string]string{"One region.": "Intake", "Two zones.": "Solution design"} {
		status, _, _ := strings.Cut(lineWith(lines, text), text)
		assert.Contains(t, status, "accepted at "+stage)

		for _, hidden := range []string{"Q5", "round2.A1", "Accepted"} {
			assert.NotContains(t, status, hidden)
		}
	}

	answered, _, _ := strings.Cut(lineWith(lines, "The API keeps its routes."), "The API")
	assert.Contains(t, answered, "Q1")
	assert.Contains(t, answered, "answer 1")
	assert.NotContains(t, answered, "accepted at")
}

func TestAssumptions_AStageRunStoresTheIDs(t *testing.T) {
	script := happyScript()
	script["intake"] = returns(needBody, map[string]any{"slices": 1, "assumptions": []any{"The parser stays in Go.", "The API keeps its routes."}})

	c, st, _, _, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	art, err := st.LatestArtifact(context.Background(), "BP1", "intake")
	require.NoError(t, err)

	var data struct {
		Assumptions []map[string]any `json:"assumptions"`
	}

	require.NoError(t, json.Unmarshal(art.Data, &data))
	require.Len(t, data.Assumptions, 2)
	assert.Equal(t, "A1", data.Assumptions[0]["id"])
	assert.Equal(t, "A2", data.Assumptions[1]["id"])
}

func TestAssumptions_SoFarListsIDStatusAndText(t *testing.T) {
	list := []assumption{{ID: "A1", Text: "The API keeps its routes.", Basis: "Q1", Answer: "keep them", Status: assumptionConfirmed}}

	line := briefAssumptionLines(list)
	for _, part := range []string{"A1", assumptionConfirmed, "The API keeps its routes."} {
		assert.Contains(t, line, part)
	}

	assert.NotContains(t, line, "keep them", "the line carries no answer")
	assert.NotContains(t, line, "Q1")
}

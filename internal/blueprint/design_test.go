package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// newDialog is newPipeline on the dialog test pack: intake, solution design
// behind a reviewed gate, grounding, decomposition and readiness.
func newDialog(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeWorkspace) {
	t.Helper()

	c, st, runner, rec, ws := newPipeline(t, script)
	c.opts.Pack = dialogPack(t)

	return c, st, runner, rec, ws
}

// newDialogOn is newDialog on an existing store and workspace, as a restart
// finds them.
func newDialogOn(t *testing.T, st *store.Store, script stageScript, ws *fakeWorkspace) (*Controller, *fakeRunner, *recorder) {
	t.Helper()

	c, _, runner, rec, _ := newPipelineOn(t, st, script, ws)
	c.opts.Pack = dialogPack(t)

	return c, runner, rec
}

func dialogPack(t *testing.T) *pack.Pack {
	t.Helper()

	pk, err := pack.LoadDir("testdata/dialog")
	require.NoError(t, err)

	return pk
}

func passing() func(context.Context, stage.Input) (stage.Output, error) {
	return returns("# Review", map[string]any{"score": float64(5), "findings": []any{}})
}

func dialogScript() stageScript {
	s := happyScript()
	s["slicing"] = returns("# Slices", oneSlice())
	s["slicing.review"] = passing()
	s["solution_design"] = returns("# Design", goodDesign("assumption"))
	s["solution_design.review"] = passing()
	s["acceptance"] = returns("# Acceptance", goodAcceptance())
	s["acceptance.review"] = passing()

	return s
}

// goodDesign is one slice with one decision resting on basis.
func goodDesign(basis string) map[string]any {
	return map[string]any{"slices": []any{map[string]any{
		"id": "SL1", "projects": []any{"alpha"},
		"decisions": []any{map[string]any{
			"id": "DEC1", "text": "where the data lives", "chosen": "Postgres",
			"rejected": []any{map[string]any{"option": "SQLite", "why": "one writer"}}, "basis": basis,
		}},
		"contracts": []any{},
	}}}
}

// twoDecisions is one slice whose decisions rest on Q1 and on source S1.
func twoDecisions() map[string]any {
	d := goodDesign("Q1")
	decisions := d["slices"].([]any)[0].(map[string]any)["decisions"].([]any)
	d["slices"].([]any)[0].(map[string]any)["decisions"] = append(decisions, map[string]any{
		"id": "DEC2", "text": "how it is deployed", "chosen": "the existing chart",
		"rejected": []any{map[string]any{"option": "kustomize", "why": "not used here"}}, "basis": "S1",
	})

	return d
}

// decisionOf is decision i of the first slice.
func decisionOf(d map[string]any, i int) map[string]any {
	return d["slices"].([]any)[0].(map[string]any)["decisions"].([]any)[i].(map[string]any)
}

func TestSolutionDesign_AConversationThenItsGate(t *testing.T) {
	type result struct{ task, refusal string }

	seen := make(chan result, 1)

	const choice = "Postgres, but managed"

	script := dialogScript()
	// Intake asks first, so solution design's question is Q2, not Q1.
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Operator", "Who runs it?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		return done("# Need", nil), nil
	}
	script["solution_design"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			q := roundQ("Data", "Where does the data live?", "Postgres", "SQLite")
			q.Recommended = 1

			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{q}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		// The answer is not the basis of any decision yet.
		d := goodDesign("assumption")
		decisionOf(d, 0)["chosen"] = choice

		refusal := in.Validate(ctx, stage.Artifact{Body: "# Design", Data: d})
		if refusal == nil {
			return stage.Output{}, errors.New("a design that drops an answer was accepted")
		}

		d = goodDesign("Q2")
		decisionOf(d, 0)["chosen"] = choice

		if err := in.Validate(ctx, stage.Artifact{Body: "# Design", Data: d}); err != nil {
			return stage.Output{}, err
		}

		seen <- result{task: in.Task, refusal: refusal.Error()}

		return done("# Design", d), nil
	}

	c, _, runner, rec, ws := newDialog(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "ops"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 2, true, own(2, choice))
	c.Wait("BP1")

	var got result

	select {
	case got = <-seen:
	default:
		require.Fail(t, "solution design did not submit")
	}

	assert.Contains(t, got.task, "[Q2]", "the continuation carries the question's number with the answer")
	assert.Contains(t, got.task, choice)
	assert.Contains(t, got.refusal, ": unused_answer: ", "the decisions check refuses a dropped answer")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	assert.Equal(t, "solution_design", gate.Payload["stage"])

	designs := inputsOf(runner, "solution_design")
	require.Len(t, designs, 2, "the run that asked and its continuation")
	assert.NotEmpty(t, designs[1].Tools, "solution design reads the code")

	grounded, ok := rec.first(EventGrounded)
	require.True(t, ok)
	assert.Equal(t, "solution_design", grounded.Payload["stage"], "the checkout moves to solution design")

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m4"))
	c.Wait("BP1")

	assert.Equal(t, []string{"intake", "intake", "slicing", "solution_design", "solution_design", "grounding", "acceptance", "decomposition", "readiness"}, stagesRun(runner))
	assert.Equal(t, 1, ws.checkouts(), "grounding reads the tree solution design checked out")
	assert.True(t, rec.has(EventSignOff))
}

func TestSolutionDesign_ThePanelIsToldWhatTheCheckoutFoundBeforeTheGate(t *testing.T) {
	c, _, _, rec, _ := newDialog(t, dialogScript())
	c.opts.Projects = listAlphaUnset

	drive(t, c, "BP1")

	rec.mu.Lock()
	events := slices.Clone(rec.recorded)
	rec.mu.Unlock()

	at := func(kind string) int {
		return slices.IndexFunc(events, func(e Event) bool {
			return e.Kind == kind && (kind != EventStageEnded || e.Payload["stage"] == "solution_design")
		})
	}

	require.GreaterOrEqual(t, at(EventSetupGaps), 0, "the gaps are reported")
	assert.Less(t, at(EventStageEnded), at(EventSetupGaps), "after solution design ends")
	assert.Less(t, at(EventSetupGaps), at(EventGate), "before its gate is shown")
}

func TestSolutionDesign_ARejectedDesignReportsTheSameGapsOnce(t *testing.T) {
	tests := []struct {
		name    string
		changes bool
		want    int
	}{
		{name: "the same gaps", want: 1},
		{name: "a changed set", changes: true, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _, rec, ws := newDialog(t, dialogScript())
			ws.noAgents = true

			var unset atomic.Bool

			c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
				if unset.Load() {
					return listAlphaUnset(ctx, key)
				}

				return listAlpha(ctx, key)
			}
			ctx := context.Background()

			require.NoError(t, c.Start(ctx, startInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")
			require.Equal(t, 1, rec.count(EventSetupGaps))

			unset.Store(tt.changes)

			require.NoError(t, c.Message(ctx, "BP1", "reject choose again", "m2"))
			c.Wait("BP1")

			assert.Equal(t, 2, rec.count(EventGate), "the stage ran again to a new gate")
			assert.Equal(t, tt.want, rec.count(EventSetupGaps))
		})
	}
}

func TestDecisionsValidator(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	now := time.Now().UTC()

	for _, q := range []store.Question{
		{BlueprintID: "BP1", RunID: 1, Stage: "solution_design", Number: 1, Text: "Where?", Answer: "Postgres", Responder: store.ResponderHuman, AnsweredAt: now},
		{BlueprintID: "BP1", RunID: 1, Stage: "intake", Number: 2, Text: "Who runs it?", Answer: "ops", Responder: store.ResponderHuman, AnsweredAt: now},
	} {
		require.NoError(t, st.AddQuestion(ctx, q))
	}

	_, err := st.AddSource(ctx, store.Source{BlueprintID: "BP1", Kind: "repo", Identity: "https://example.com/alpha.git", Version: "abc", FetchedAt: now})
	require.NoError(t, err)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	tests := []struct {
		name   string
		change func(d map[string]any)
		want   string // the check that fails; "" when the design stands
	}{
		{name: "it stands, intake's answer unused", change: func(map[string]any) {}},
		{name: "a bracketed tag", change: func(d map[string]any) { decisionOf(d, 0)["basis"] = "[Q1]" }},
		{name: "a lowercase tag", change: func(d map[string]any) { decisionOf(d, 0)["basis"] = "q1" }},
		{name: "a padded source tag", change: func(d map[string]any) { decisionOf(d, 1)["basis"] = " S1 " }},
		{name: "no slices", change: func(d map[string]any) { d["slices"] = []any{} }, want: "no_slices"},
		{name: "no chosen option", change: func(d map[string]any) { decisionOf(d, 0)["chosen"] = " " }, want: "chosen"},
		{name: "no rejected option", change: func(d map[string]any) { decisionOf(d, 0)["rejected"] = []any{} }, want: "rejected"},
		{name: "a basis nobody knows", change: func(d map[string]any) { decisionOf(d, 1)["basis"] = "Q9" }, want: "basis"},
		{name: "an answer no decision rests on", change: func(d map[string]any) { decisionOf(d, 0)["basis"] = "assumption" }, want: "unused_answer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := twoDecisions()
			tt.change(d)

			validate, _ := c.decisionsValidator(bp, "solution_design")

			err := validate(ctx, stage.Artifact{Body: "# Design", Data: d})
			if tt.want == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), ": "+tt.want+": ")
		})
	}
}

func TestSolutionDesign_AThirdFailingSubmitKeepsItsProblems(t *testing.T) {
	script := dialogScript()
	script["solution_design"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Data", "Where does the data live?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		}

		// Every submit drops the answer; the third is accepted as it is.
		d := goodDesign("assumption")

		var err error

		for range maxContractRounds {
			err = in.Validate(ctx, stage.Artifact{Body: "# Design", Data: d})
		}

		if err != nil {
			return stage.Output{}, err
		}

		return done("# Design", d), nil
	}

	c, st, runner, rec, _ := newDialog(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "Postgres"))
	c.Wait("BP1")

	design, err := st.LatestArtifact(ctx, "BP1", "solution_design")
	require.NoError(t, err)
	assert.Contains(t, strings.Join(problemsOf(design), "\n"), "Q1: unused_answer", "kept with the artifact")

	gate, ok := rec.first(EventGate)
	require.True(t, ok)
	require.Equal(t, "solution_design", gate.Payload["stage"])
	assert.Equal(t, problemsOf(design), gate.Payload["problems"], "shown at its gate")

	reviews := inputsOf(runner, "solution_design.review")
	require.Len(t, reviews, 1)
	assert.Contains(t, reviews[0].Task, "Q1: unused_answer", "given to its reviewer")
}

func TestDecisions_EverySliceIsDesigned(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	slicing, err := json.Marshal(twoSlices())
	require.NoError(t, err)

	_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "slicing", Body: "# Slices", Data: slicing})
	require.NoError(t, err)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	validate, _ := c.decisionsValidator(bp, "solution_design")

	err = validate(ctx, stage.Artifact{Body: "# Design", Data: goodDesign("assumption")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SL2: slice_missing: ")
}

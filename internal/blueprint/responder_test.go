package blueprint

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// askingIntake scripts an intake that asks one question and puts the
// answer, or the error it got, in its artifact.
func askingIntake(got chan<- error) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		a, err := in.Asker.Ask(ctx, stage.Question{Text: "Helm or kustomize?", Options: []string{"helm", "kustomize"}})
		got <- err

		if ctx.Err() != nil {
			return stage.Output{}, ctx.Err()
		}

		return done("# Need\n\n"+a, map[string]any{"slices": 1}), nil
	}
}

func autoInput() StartInput {
	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto

	return in
}

func TestResponder_AnswersAnAutoQuestionAsAnAssumption(t *testing.T) {
	got := make(chan error, 1)

	script := happyScript()
	script["intake"] = askingIntake(got)
	script["intake.responder"] = returns("# Answer", map[string]any{"answer": "helm", "reasoning": "the repository ships a chart", "grounded": true})

	c, st, runner, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	driveIn(t, c, autoInput())

	require.NoError(t, <-got)

	need, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.Equal(t, "# Need\n\nQ1: helm", need.Body, "the stage goes on with the answer")

	responders := inputsOf(runner, "intake.responder")
	require.Len(t, responders, 1)
	assert.Equal(t, c.opts.Pack.Persona("product_owner"), responders[0].Persona, "the asking stage's persona")
	assert.Equal(t, c.opts.Pack.ResponderInstructions(), responders[0].Instructions)
	assert.Contains(t, responders[0].Task, "Helm or kustomize?")
	assert.Contains(t, responders[0].Task, "the need")
	assert.Nil(t, responders[0].History, "never the asker's transcript")
	assert.Nil(t, responders[0].Asker)

	recorded, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, recorded, 1)
	assert.Equal(t, store.ResponderAuto, recorded[0].Responder)
	assert.True(t, recorded[0].Assumption)
	assert.True(t, recorded[0].Grounded)
	assert.Equal(t, "helm", recorded[0].Answer)

	ans, ok := rec.first(EventAnswer)
	require.True(t, ok)
	assert.Equal(t, store.ResponderAuto, ans.Payload["responder"])
	assert.Equal(t, 1, ans.Payload["number"])

	so, ok := rec.first(EventSignOff)
	require.True(t, ok, "the auto mode stops at the sign-off")
	require.Len(t, so.Payload["assumptions"], 1)

	entry, ok := so.Payload["assumptions"].([]any)[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Q1", entry["id"])
	assert.Equal(t, "intake", entry["stage"])
	assert.Equal(t, "verified", entry["status"])
	assert.Empty(t, entry["evidence"])
	assert.Contains(t, entry["text"], "Helm or kustomize?")
	assert.Contains(t, entry["text"], "helm")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, store.RunKindResponder, runs[1].Kind, "the responder's row follows the asking stage's")
	assert.Equal(t, "intake", runs[1].Stage)
}

func TestResponder_AVerdictOnAnAutoAnswerApplies(t *testing.T) {
	got := make(chan error, 1)

	script := happyScript()
	script["intake"] = askingIntake(got)
	script["intake.responder"] = returns("# Answer", map[string]any{"answer": "helm", "reasoning": "a guess", "grounded": false})
	script["grounding"] = returns("# Grounding", map[string]any{"verdicts": []any{
		map[string]any{"id": "Q1", "status": "contradicted", "evidence": "[S1] the repository has no chart"},
	}})

	c, _, _, rec, _ := newPipeline(t, script)

	driveIn(t, c, autoInput())

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)

	entry, ok := so.Payload["assumptions"].([]any)[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Q1", entry["id"])
	assert.Equal(t, "contradicted", entry["status"])
	assert.Equal(t, "[S1] the repository has no chart", entry["evidence"])
}

func TestResponder_AFailureLeavesTheStageToDecide(t *testing.T) {
	got := make(chan error, 1)

	script := happyScript()
	script["intake"] = askingIntake(got)
	script["intake.responder"] = func(context.Context, stage.Input) (stage.Output, error) {
		return stage.Output{}, errors.New("provider down")
	}

	c, st, runner, _, _ := newPipeline(t, script)
	ctx := context.Background()

	driveIn(t, c, autoInput())

	require.Error(t, <-got, "ask_human returns a tool error")
	require.Len(t, inputsOf(runner, "intake.responder"), 1, "the responder ran")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(runs), 2)
	assert.Equal(t, store.RunKindResponder, runs[1].Kind)
	assert.Equal(t, store.RunFailed, runs[1].Status)

	_, err = st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err, "the stage decides and goes on")

	recorded, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Empty(t, recorded)
}

func TestResponder_SeesTheNotesAndEarlierAnswers(t *testing.T) {
	asked := make(chan struct{}, 1)
	cont := make(chan struct{})

	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if _, err := in.Asker.Ask(ctx, stage.Question{Text: "Helm or kustomize?"}); err != nil {
			return stage.Output{}, err
		}

		signal(asked)
		<-cont

		if _, err := in.Asker.Ask(ctx, stage.Question{Text: "Which cluster?"}); err != nil {
			return stage.Output{}, err
		}

		return done("# Need", nil), nil
	}

	var answers atomic.Int32

	script["intake.responder"] = func(context.Context, stage.Input) (stage.Output, error) {
		if answers.Add(1) == 1 {
			return done("# Answer", map[string]any{"answer": "helm", "reasoning": "a chart exists", "grounded": true}), nil
		}

		return done("# Answer", map[string]any{"answer": "prod", "reasoning": "the need says so", "grounded": false}), nil
	}

	c, _, runner, _, _ := newPipeline(t, script)
	release := closer(t, cont)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoInput()))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, asked, "the first answer")
	require.NoError(t, c.Message(ctx, "BP1", "use the managed chart", "m2"))
	release()
	c.Wait("BP1")

	responders := inputsOf(runner, "intake.responder")
	require.Len(t, responders, 2)
	assert.Contains(t, responders[1].Task, "helm", "the earlier answer")
	assert.Contains(t, responders[1].Task, "use the managed chart", "the user's note")
}

func TestResponder_AnAbortEndsTheAskingRunToo(t *testing.T) {
	got := make(chan error, 1)
	entered := make(chan struct{}, 1)

	script := happyScript()
	script["intake"] = askingIntake(got)
	script["intake.responder"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		signal(entered)
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	}

	c, st, _, _, _ := newPipeline(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoInput()))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, entered, "the responder")
	require.NoError(t, c.Abort(ctx, "BP1", "m2"))
	c.Wait("BP1")

	require.ErrorIs(t, <-got, context.Canceled, "the stage gets the cancel, not an answer")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunCanceled, runs[0].Status, "the asking run")
	assert.Equal(t, store.RunCanceled, runs[1].Status, "the responder run")

	_, err = st.LatestArtifact(ctx, "BP1", "intake")
	require.ErrorIs(t, err, store.ErrNotFound)
}

// spendLine is a usage line the sink prices at 1.5 USD under testPrice.
const spendLine = `{"seq":1,"kind":"usage","data":{"prompt_tokens":1000000,"completion_tokens":50000,"cost_usd":0}}` + "\n"

func TestResponder_SpendsFromTheAskingRunsCap(t *testing.T) {
	got := make(chan error, 1)

	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		_, _ = in.Events.Write([]byte(spendLine))

		return askingIntake(got)(ctx, in)
	}
	// 0.5 + 0.4 USD at testPrice: over what the asking run has left.
	script["intake.responder"] = func(context.Context, stage.Input) (stage.Output, error) {
		out := done("# Answer", map[string]any{"answer": "helm", "reasoning": "r", "grounded": true})
		out.Result = harness.Result{Completed: true, Reason: "done", PromptTokens: 500_000, CompletionTokens: 40_000}

		return out, nil
	}

	c, st, runner, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	in := autoInput()
	in.Price = testPrice()
	in.CostCapUSD = 10

	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	responders := inputsOf(runner, "intake.responder")
	require.Len(t, responders, 1)
	assert.InDelta(t, 0.5, responders[0].MaxCostUSD, 1e-9, "what the asking run's 2 USD cap has left after its 1.5")

	require.ErrorIs(t, <-got, context.Canceled, "the responder's cost passed the asking run's cap")

	stopped, ok := rec.first(EventError)
	require.True(t, ok)
	assert.InDelta(t, 2.4, stopped.Payload["spent_usd"], 1e-6, "what the run and its responder spent, not the run's own 1.5")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunFailed, runs[0].Status)
	assert.Equal(t, "max_cost", runs[0].Reason)
}

func TestResponder_NoBudgetLeftLeavesTheStageToDecide(t *testing.T) {
	got := make(chan error, 1)

	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		// Exactly the 2 USD run cap at testPrice: nothing left, not passed.
		_, _ = in.Events.Write([]byte(`{"seq":1,"kind":"usage","data":{"prompt_tokens":1000000,"completion_tokens":100000,"cost_usd":0}}` + "\n"))

		return askingIntake(got)(ctx, in)
	}
	script["intake.responder"] = returns("# Answer", map[string]any{"answer": "helm", "reasoning": "r", "grounded": true})

	c, st, runner, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	in := autoInput()
	in.Price = testPrice()
	in.CostCapUSD = 10

	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.Error(t, <-got, "ask_human returns a tool error")
	assert.Empty(t, inputsOf(runner, "intake.responder"), "no responder run without budget")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	for _, r := range runs {
		assert.NotEqual(t, store.RunKindResponder, r.Kind)
	}

	refused, ok := rec.first(EventError)
	require.True(t, ok, "the refusal is reported")
	assert.Equal(t, "responder_refused", refused.Payload["reason"])

	_, err = st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err, "the stage decides and goes on")
}

func TestResponder_GetsTheGateFeedbackItsAttemptAddresses(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			if _, err := in.Asker.Ask(ctx, stage.Question{Text: "Which store?"}); err != nil {
				return stage.Output{}, err
			}
		}

		return done("# Shape", nil), nil
	}
	script["shape.responder"] = returns("# Answer", map[string]any{"answer": "Postgres", "reasoning": "r", "grounded": false})

	c, _, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.True(t, rec.has(EventGate))

	// The open gate stays the human's; the next attempt's questions are auto.
	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", "auto", "m2"))
	require.NoError(t, c.Message(ctx, "BP1", "reject name the owner", "m3"))
	c.Wait("BP1")

	responders := inputsOf(runner, "shape.responder")
	require.Len(t, responders, 1)
	assert.Contains(t, responders[0].Task, "name the owner", "the feedback the asking attempt addresses")
}

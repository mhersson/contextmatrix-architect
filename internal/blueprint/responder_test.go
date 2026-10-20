package blueprint

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// askingIntake scripts an intake that asks one question round and reports
// the error it got.
func askingIntake(got chan<- error) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or kustomize?", "helm", "kustomize")}})
		got <- err

		if ctx.Err() != nil {
			return stage.Output{}, ctx.Err()
		}

		return done("# Need", map[string]any{"slices": 1}), nil
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
	script["intake.responder"] = reading("[S1] charts/app", map[string]any{"answer": "helm", "reasoning": "the repository ships a chart", "grounded": true, "evidence": "[S1] charts/app"})

	c, st, runner, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	driveIn(t, c, autoInput())

	require.NoError(t, <-got)

	round := latestRound(t, c, "BP1", 1)
	assert.True(t, round.Frame.Responder)
	assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Option: 1}}, round.Frame.Answers, "the answer names the first option")

	responders := inputsOf(runner, "intake.responder")
	require.Len(t, responders, 1)
	assert.Equal(t, c.opts.Pack.Persona("product_owner"), responders[0].Persona, "the asking stage's persona")
	assert.Equal(t, c.opts.Pack.ResponderInstructions(), responders[0].Instructions)
	assert.Contains(t, responders[0].Task, "Helm or kustomize?")
	assert.Contains(t, responders[0].Task, "the need")
	assert.Nil(t, responders[0].History, "never the asker's transcript")
	assert.Nil(t, responders[0].Interviewer)

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
	assert.Equal(t, "[S1] charts/app", entry["evidence"])
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

	require.NoError(t, <-got, "the round is posted and the stage decides the question")
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

	answers := latestRound(t, c, "BP1", 1).Frame.Answers
	require.Len(t, answers, 1)
	assert.NotEmpty(t, answers[0].Text, "the stage is told it has no answer")
}

func TestResponder_SeesTheNotesAndEarlierAnswers(t *testing.T) {
	asked := make(chan struct{}, 1)
	cont := make(chan struct{})

	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or kustomize?")}}); err != nil {
			return stage.Output{}, err
		}

		signal(asked)
		<-cont

		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Cluster", "Which cluster?")}}); err != nil {
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

	require.NoError(t, <-got, "the stage decides the question")
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

func TestResponder_BelowItsFloorTheStageDecides(t *testing.T) {
	got := make(chan error, 1)

	script := happyScript()
	script["intake"] = askingIntake(got)
	script["intake.responder"] = returns("# Answer", map[string]any{"answer": "helm", "reasoning": "r", "grounded": true})

	c, st, runner, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	in := autoInput()
	in.Price = testPrice()
	in.CostCapUSD = 0.9

	require.NoError(t, c.Start(ctx, in))

	// An earlier responder run puts the responder's floor at 0.5, above the
	// 0.4 left, while intake's own floor stays at the minimum.
	ranBefore(t, st, store.RunKindResponder, "intake", store.RunDone, 0.5)

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.NoError(t, <-got, "the stage decides the question")
	assert.Empty(t, inputsOf(runner, "intake.responder"), "no responder run below its floor")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var responders int

	for _, r := range runs {
		if r.Kind == store.RunKindResponder {
			responders++
		}
	}

	assert.Equal(t, 1, responders, "only the earlier responder run")

	refused, ok := rec.first(EventError)
	require.True(t, ok, "the refusal is reported")
	assert.Equal(t, "responder_refused", refused.Payload["reason"])
	assert.Empty(t, capStops(t, st), "a responder refused inside its run stops no stage")

	_, err = st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err, "the stage decides and goes on")
}

func TestResponder_GetsTheGateFeedbackItsAttemptAddresses(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if shapes.Add(1) == 2 {
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Store", "Which store?")}}); err != nil {
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

// reading scripts a responder whose run carried the messages also and then
// read a tool result with the given content before submitting.
func reading(toolResult string, data map[string]any, also ...llm.Message) func(context.Context, stage.Input) (stage.Output, error) {
	return func(context.Context, stage.Input) (stage.Output, error) {
		out := done("# Answer", data)
		out.Result.Messages = append(out.Result.Messages, also...)
		out.Result.Messages = append(out.Result.Messages, llm.Message{Role: "tool", Content: toolResult})

		return out, nil
	}
}

func TestResponder_EvidenceVerifiesOnlyWithItsOwnToolResults(t *testing.T) {
	answerWith := func(grounded bool, evidence string) map[string]any {
		return map[string]any{"answer": "helm", "reasoning": "r", "grounded": grounded, "evidence": evidence}
	}

	tests := []struct {
		name      string
		responder func(context.Context, stage.Input) (stage.Output, error)
		verified  bool
	}{
		{"cites a tag its tool result carried", reading("[S1] charts/ exists", answerWith(true, "[S1] the chart is in charts/")), true},
		{"grounded with no tool call", returns("# Answer", answerWith(true, "[S1] the chart")), false},
		{"cites a tag only the task carries", reading("[S1] charts/ exists", answerWith(true, "[S2] the task said so"),
			llm.Message{Role: "user", Content: "[S2] charts/ holds the chart"}), false},
		{"not grounded despite matching evidence", reading("[S1] charts/ exists", answerWith(false, "[S1] the chart")), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make(chan error, 1)

			script := happyScript()
			script["intake"] = askingIntake(got)
			script["intake.responder"] = tt.responder

			c, st, _, rec, _ := newPipeline(t, script)

			driveIn(t, c, autoInput())
			require.NoError(t, <-got)

			recorded, err := st.ListQuestions(context.Background(), "BP1")
			require.NoError(t, err)
			require.Len(t, recorded, 1)
			assert.Equal(t, tt.verified, recorded[0].Grounded)

			ans, ok := rec.first(EventAnswer)
			require.True(t, ok)
			assert.Equal(t, tt.verified, ans.Payload["grounded"])

			if tt.verified {
				assert.NotEmpty(t, ans.Payload["evidence"])
			} else {
				assert.Empty(t, ans.Payload["evidence"])
			}

			list, err := c.assumptions(context.Background(), "BP1")
			require.NoError(t, err)
			require.Len(t, list, 1)
			assert.Equal(t, "Q1", list[0].ID)

			if tt.verified {
				assert.Equal(t, assumptionVerified, list[0].Status)
				assert.Equal(t, "[S1] the chart is in charts/", list[0].Evidence)
			} else {
				assert.Equal(t, assumptionAssumed, list[0].Status)
				assert.Empty(t, list[0].Evidence)
			}
		})
	}
}

func TestAssumptions_AGroundedRowWithoutEvidenceIsNotVerified(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	require.NoError(t, st.AddQuestion(ctx, store.Question{
		BlueprintID: "BP1", Stage: "intake", Number: 1, Text: "q?", Answer: "a",
		Responder: store.ResponderAuto, Assumption: true, Grounded: true,
	}))
	// The answer event as it was stored before evidence was recorded.
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventAnswer,
		Payload: []byte(`{"number":1,"text":"a","responder":"auto","reasoning":"r","grounded":true}`),
	}))

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, assumptionAssumed, list[0].Status)
}

func TestCitesToolResult(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "[S9] in the task"},
		{Role: "assistant", Content: "[S8] my own prose"},
		{Role: "tool", Content: "[S1] one"},
		{Role: "tool", Content: "[S2,S3] two and three"},
		{Role: "tool", Content: "no tag [S7] here"},
		{Role: "tool", Content: "[S40] forty"},
	}

	tests := []struct {
		evidence string
		want     bool
	}{
		{"[S1]", true},
		{"[S3]", true},
		{"[S1, S4]", true},
		{"[S4, S5]", false},
		{"[S9]", false},
		{"[S8]", false},
		{"[S7]", false},
		{"[S40]", true},
		{"[S4]", false},
		{"S1 without brackets", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.evidence, func(t *testing.T) {
			assert.Equal(t, tt.want, citesToolResult(tt.evidence, msgs))
		})
	}
}

// blockOf is the entry writeQuestions rendered for question tag.
func blockOf(text, tag string) string {
	_, rest, _ := strings.Cut(text, "- ["+tag+"]")
	block, _, _ := strings.Cut(rest, "\n- [")

	return block
}

func seedMixedAnswers(t *testing.T, st *store.Store) {
	t.Helper()

	ctx := context.Background()

	for _, q := range []store.Question{
		{BlueprintID: "BP1", RunID: 1, Stage: "intake", Number: 1, Text: "Where?", Answer: "helm", Responder: store.ResponderHuman},
		{BlueprintID: "BP1", RunID: 1, Stage: "intake", Number: 2, Text: "Which cluster?", Answer: "prod", Responder: store.ResponderAuto, Assumption: true},
	} {
		require.NoError(t, st.AddQuestion(ctx, q))
	}
}

func assertAttributed(t *testing.T, task string) {
	t.Helper()

	human, auto := blockOf(task, "Q1"), blockOf(task, "Q2")
	require.NotEmpty(t, human)
	require.NotEmpty(t, auto)
	assert.Contains(t, human, answeredByUser)
	assert.NotContains(t, human, answeredByResponder)
	assert.Contains(t, auto, answeredByResponder)
	assert.NotContains(t, auto, answeredByUser)
}

func TestQuestions_ARerunIsToldWhoAnsweredEach(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	driveIn(t, c, startInput("BP1"))
	seedMixedAnswers(t, st)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	task, err := c.taskFor(ctx, bp, intake, nil)
	require.NoError(t, err)

	assertAttributed(t, task)
}

func TestQuestions_TheResponderIsToldWhoAnsweredEarlierOnes(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	driveIn(t, c, startInput("BP1"))
	seedMixedAnswers(t, st)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	task, err := c.responderTask(ctx, bp, intake, 1, nil, protocol.RoundQuestion{Number: 3, Text: "Next?"}, "")
	require.NoError(t, err)

	assertAttributed(t, task)
}

func TestRound_TheContinuationListsTheResponderAnswers(t *testing.T) {
	c, _, runner, _, _ := newDialog(t, stageScript{
		"intake.responder": respondsWith("helm"),
		"intake": func(ctx context.Context, in stage.Input) (stage.Output, error) {
			if in.History != nil {
				return stage.Output{}, errors.New("stop after intake")
			}

			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		},
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, autoInput()))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.Equal(t, 3, runner.calls())

	task := runner.input(2).Task
	assert.Contains(t, task, "[Q1]")
	assert.Contains(t, task, "helm")
	assert.Contains(t, task, answeredByResponder)
	assert.NotContains(t, task, answeredByUser)
}

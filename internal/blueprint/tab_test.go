package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// tabbed is an intake fake: a fresh run posts qs with the assumptions tab, a
// chat run replies with chat, and a continuation submits data.
func tabbed(qs []stage.RoundQuestion, tab []string, chat stage.ChatAnswer, data map[string]any) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch {
		case in.Chat:
			if _, err := in.Interviewer.Answer(ctx, chat); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		case in.History == nil:
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: qs, Assumptions: tab}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		default:
			return done("# Need", data), nil
		}
	}
}

func accept(id string) protocol.AssumptionDecision {
	return protocol.AssumptionDecision{ID: id, Accept: true}
}
func convert(id string) protocol.AssumptionDecision { return protocol.AssumptionDecision{ID: id} }

// decide sends a reply to round n with answers and decisions.
func decide(t *testing.T, c *Controller, id string, n int, answers []protocol.RoundAnswer, decisions ...protocol.AssumptionDecision) {
	t.Helper()

	msgID := fmt.Sprintf("d%d-%d-%d", n, len(answers), len(decisions))
	require.NoError(t, c.Reply(context.Background(), id, protocol.RoundReply{PromptID: roundPromptID(n), Answers: answers, Decisions: decisions, Final: true}, msgID))
}

// submitAssumptions runs validator v on a submit stating assumptions.
func submitAssumptions(v func(context.Context, stage.Artifact) error, assumptions ...any) error {
	return v(context.Background(), stage.Artifact{Body: "# Need", Data: map[string]any{"assumptions": assumptions}})
}

// tabRound is round n of stageID as run posted it, a tab of texts alone (A1,
// A2, ...), closed with decisions as a human's close stores it.
func tabRound(n int, stageID string, run int64, texts []string, decisions ...protocol.AssumptionDecision) roundState {
	s := roundState{
		Number: n, Attempt: 1, BuilderRound: 1, PostedBy: run, RunID: run, Change: changeClosed, TabIssued: len(texts),
		Frame: protocol.LogRound{Stage: stageID, Questions: []protocol.RoundQuestion{}, Decisions: decisions, Closed: true},
	}

	for i, text := range texts {
		s.Frame.Assumptions = append(s.Frame.Assumptions, protocol.RoundAssumption{ID: fmt.Sprintf("A%d", i+1), Text: text})
	}

	for _, d := range decisions {
		if !d.Accept {
			s.Converted = append(s.Converted, d.ID)
		}
	}

	return s
}

// doneRun records a stage run of stageID that ended done.
func doneRun(t *testing.T, st *store.Store, stageID string) int64 {
	t.Helper()

	ctx := context.Background()
	runID, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: stageID, Kind: store.RunKindStage, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: time.Now().UTC()})
	require.NoError(t, err)
	require.NoError(t, st.EndRun(ctx, runID, store.RunDone, "done", store.RunUsage{}, time.Now().UTC()))

	return runID
}

// snapshots counts the snapshots of round n stored with change.
func snapshots(t *testing.T, st *store.Store, n int, change string) int {
	t.Helper()

	events, err := st.ListEventsOfKind(context.Background(), "BP1", EventRound)
	require.NoError(t, err)

	count := 0

	for _, e := range events {
		var s roundState
		require.NoError(t, json.Unmarshal(e.Payload, &s))

		if s.Number == n && s.Change == change {
			count++
		}
	}

	return count
}

func questionNumbers(qs []store.Question) []int {
	out := make([]int, 0, len(qs))
	for _, q := range qs {
		out = append(out, q.Number)
	}

	return out
}

func TestTab_AcceptedAssumptionsTakeNoQuestionNumber(t *testing.T) {
	var (
		runs      atomic.Int32
		submitted error
	)

	// Round 1 asks two questions, round 2 is a tab alone, round 3 asks one,
	// and the last continuation submits the accepted ones by their tags.
	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		var rd stage.Round

		switch runs.Add(1) {
		case 1:
			rd.Questions = []stage.RoundQuestion{roundQ("Users", "Who uses it?"), roundQ("Region", "Which region?")}
		case 2:
			rd.Assumptions = []string{"One region.", "Postgres stays.", "Two zones."}
		case 3:
			rd.Questions = []stage.RoundQuestion{roundQ("Zones", "How many zones?")}
		default:
			data := map[string]any{"slices": 1, "assumptions": []any{
				map[string]any{"text": "One region.", "basis": "round2.A1"},
				map[string]any{"text": "Postgres stays.", "basis": "Round2.a2"},
			}}
			submitted = in.Validate(ctx, stage.Artifact{Body: "# Need", Data: data})

			return done("# Need", data), nil
		}

		if _, err := in.Interviewer.Ask(ctx, rd); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}

	c, st, runner, _, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	answerRound(t, c, "BP1", 1, true, own(1, "ops"), own(2, "eu"))
	c.Wait("BP1")
	decide(t, c, "BP1", 2, nil, accept("A1"), accept("A2"), convert("A3"))
	c.Wait("BP1")

	third := latestRound(t, c, "BP1", 3)
	require.Len(t, third.Frame.Questions, 1)
	assert.Equal(t, 3, third.Frame.Questions[0].Number, "the next asked question follows Q1-Q2")

	answerRound(t, c, "BP1", 3, true, own(3, "two"))
	c.Wait("BP1")
	require.NoError(t, submitted, "acceptances stated by their tags pass the submit checks under human")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, questionNumbers(qs), "only asked questions are rows")

	tab := latestRound(t, c, "BP1", 2)
	assert.True(t, tab.Frame.Closed)
	assert.Nil(t, tab.Accepted)
	assert.Equal(t, []string{"A3"}, tab.Converted)
	assert.Zero(t, snapshots(t, st, 2, ChangeClosing), "a close reserves no number")

	intakes := inputsOf(runner, "intake")
	require.Len(t, intakes, 4)
	assert.NotNil(t, intakes[2].History)
	assert.Contains(t, intakes[2].Task, "round2.A1", "the continuation is told the acceptance's tag")
	assert.Contains(t, intakes[2].Task, "round2.A2")
	assert.NotContains(t, intakes[2].Task, "round2.A3", "one turned into a question has no tag")

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": assumptionConfirmed, "A2": assumptionConfirmed}, statuses(list))
	assert.Equal(t, "round2.A2", byID(list)["A2"].Basis)
}

func TestTab_AnAcceptanceTagIsABasisTheUserConfirmed(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.opts.Pack.Stage("intake")
	require.True(t, ok)

	au := c.newAudit()

	autoRun := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	auto := tabRound(1, "intake", autoRun, []string{"Auto region."}, accept("A1"))
	auto.Frame.Responder = true
	c.putRound(ctx, au, "BP1", autoRun, auto)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	c.putRound(ctx, au, "BP1", poster, tabRound(2, "intake", poster, []string{"One region.", "Postgres stays."}, accept("A1"), convert("A2")))

	// A later round of the attempt asks the one turned into a question.
	follow := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	c.putRound(ctx, au, "BP1", follow, twoQuestionRound(3, 1, follow))

	// A round the pipeline went past never closed, so it accepted nothing.
	leftRun := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	left := tabRound(4, "intake", leftRun, []string{"Left region."}, accept("A1"))
	left.Change = ChangeLeft
	c.putRound(ctx, au, "BP1", leftRun, left)

	tests := []struct {
		name  string
		entry any
		ok    bool
	}{
		{"the acceptance's tag", map[string]any{"text": "One region.", "basis": "round2.A1"}, true},
		{"a bracketed tag in another case", map[string]any{"text": "One region.", "basis": "[ROUND2.a1]"}, true},
		{"the round as R", map[string]any{"text": "One region.", "basis": "R2.A1"}, false},
		{"one turned into a question", map[string]any{"text": "Postgres stays.", "basis": "round2.A2"}, false},
		{"an unknown round", map[string]any{"text": "One region.", "basis": "round9.A1"}, false},
		{"one accepted on an auto round", map[string]any{"text": "Auto region.", "basis": "round1.A1"}, false},
		{"one decided on a round the pipeline left", map[string]any{"text": "Left region.", "basis": "round4.A1"}, false},
		{"a sentence naming the tag", "One region, as round2.A1 records.", false},
		{"a sentence naming the tag in another case", "One region, as Round2.a1 records.", false},
		{"a sentence naming an unknown round", "One region, unlike round9.A1.", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _ := c.assumptionsValidator(bp)
			err := submitAssumptions(v, tt.entry)

			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}

	v, state := c.tabValidator(bp, intake, 1)
	require.NoError(t, submitAssumptions(v, map[string]any{"text": "One region.", "basis": "round2.A1"}), "the tab check under human")
	assert.Empty(t, state.last())

	putData(t, st, "intake", map[string]any{"assumptions": []any{
		map[string]any{"text": "One region.", "basis": "[ROUND2.a1]"},
		map[string]any{"text": "Postgres stays.", "basis": "round2.A2"},
		map[string]any{"text": "Far region.", "basis": "round9.A1"},
		map[string]any{"text": "Auto region.", "basis": "round1.A1"},
	}})

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A1": assumptionConfirmed, "A2": assumptionUnverified, "A3": assumptionUnverified, "A4": assumptionUnverified}, statuses(list))
	assert.Equal(t, "round2.A1", byID(list)["A1"].Basis)

	for _, id := range []string{"A2", "A3", "A4"} {
		assert.Empty(t, byID(list)[id].Basis, "%s reads as no basis", id)
	}

	putData(t, st, "grounding", map[string]any{"verdicts": []any{
		map[string]any{"id": "A1", "status": "unverified", "evidence": "not found"},
	}})

	list, err = c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, assumptionConfirmed, byID(list)["A1"].Status, "not finding it in the code never hides the user's acceptance")
}

func TestNormalTag(t *testing.T) {
	tests := []struct{ in, want string }{
		{"round2.A1", "round2.A1"},
		{"[ROUND2.a1]", "round2.A1"},
		{" Round12.a3 ", "round12.A3"},
		{"[q1]", "Q1"},
		{"s2", "S2"},
		{"assumption", "ASSUMPTION"},
		{"round2", "ROUND2"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, normalTag(tt.in), tt.in)
	}
}

func TestTab_ARerunOfTheStageIsToldItsAcceptances(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.opts.Pack.Stage("intake")
	require.True(t, ok)

	grounding, ok := c.opts.Pack.Stage("grounding")
	require.True(t, ok)

	// A chat run answered last; the run that posted the round accepted it.
	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	chat := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	s := tabRound(1, "intake", poster, []string{"One region."}, accept("A1"))
	s.RunID = chat
	c.putRound(ctx, c.newAudit(), "BP1", chat, s)

	var b strings.Builder
	require.NoError(t, c.writeAnswered(ctx, &b, bp, intake))
	assert.Contains(t, b.String(), "round1.A1", "the stage run again is told its acceptance's tag")

	b.Reset()
	require.NoError(t, c.writeAnsweredAfter(ctx, &b, bp, intake, poster))
	assert.Empty(t, b.String(), "only the acceptances of runs after the one resumed")

	b.Reset()
	require.NoError(t, c.writeAnswered(ctx, &b, bp, grounding))
	assert.Empty(t, b.String(), "another stage's acceptance")
}

func TestTab_AnOlderAcceptanceRowStaysItsBasis(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.opts.Pack.Stage("intake")
	require.True(t, ok)

	// An older close recorded A1 as the answered question Q5.
	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	s := tabRound(1, "intake", poster, []string{"One region."}, accept("A1"))
	s.Accepted = map[string]int{"A1": 5}
	c.putRound(ctx, c.newAudit(), "BP1", poster, s)
	require.NoError(t, st.AddQuestion(ctx, store.Question{
		BlueprintID: "BP1", RunID: poster, Stage: "intake", Number: 5, Text: "One region.", Options: []string{"Accept", "Turn into a question"},
		Context: "Round 1's assumptions tab, A1", Answer: "Accepted", Responder: store.ResponderHuman, AnsweredAt: time.Now().UTC(),
	}))

	putData(t, st, "intake", map[string]any{"assumptions": []any{map[string]any{"text": "One region.", "basis": "Q5"}}})

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "Q5", list[0].Basis)
	assert.Equal(t, assumptionConfirmed, list[0].Status)

	next, err := c.nextQuestionNumber(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, 6, next)

	var b strings.Builder
	require.NoError(t, c.writeAnswered(ctx, &b, bp, intake))
	assert.Equal(t, 1, strings.Count(b.String(), "[Q5]"), "its row stays its one citation")
	assert.NotContains(t, b.String(), "round1.A1")
}

func TestTab_AResumedConversationIsToldItsOwnRoundsTags(t *testing.T) {
	s := tabRound(2, "intake", 7, []string{"One region.", "Postgres stays."}, accept("A1"), convert("A2"))
	accepted := []acceptedAssumption{
		{Tag: "round1.A2", Round: 1, ID: "A2", Stage: "intake", RunID: 5, Text: "Two zones."},
		{Tag: "round2.A1", Round: 2, ID: "A1", Stage: "intake", RunID: 7, Text: "One region."},
	}

	var b strings.Builder

	writeRoundAssumptions(&b, s, accepted)
	assert.Contains(t, b.String(), "round2.A1")
	assert.NotContains(t, b.String(), "round1.A2", "another round's acceptance under the same id")
}

func TestTab_AnOlderClosingSnapshotClosesWithoutARow(t *testing.T) {
	c, st, runner, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	au := c.newAudit()
	bp := seeded(t, st)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", asking)
	s := twoQuestionRound(1, 1, poster)
	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}}
	c.putRound(ctx, au, "BP1", poster, s)

	// An older close reserved Q5 for A1 and stopped before any row.
	closing := s.clone()
	closing.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "the ops team"}}
	closing.Frame.Decisions = []protocol.AssumptionDecision{accept("A1")}
	closing.Change, closing.MessageID, closing.Accepted = ChangeClosing, "m9", map[string]int{"A1": 5}
	c.putRound(ctx, au, "BP1", 0, closing)

	next, err := c.nextQuestionNumber(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, 6, next, "a new question numbers after the reserved one")

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2}, questionNumbers(qs), "no row for the reserved number")

	closed := latestRound(t, c, "BP1", 1)
	assert.True(t, closed.Frame.Closed)
	assert.Equal(t, 1, closedSnapshots(t, st, 1))
	require.Equal(t, 1, runner.calls(), "one continuation")
	assert.Contains(t, runner.input(0).Task, "round1.A1", "the stage is told the acceptance's tag")

	_, err = c.closeRound(ctx, au, bp, closing)
	require.NoError(t, err, "a second close writes no row")

	qs, err = st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 2)

	next, err = c.nextQuestionNumber(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, 6, next, "the reserved number stays reserved after the close")
	assert.Equal(t, map[string]int{"A1": 5}, latestRound(t, c, "BP1", 1).Accepted)
}

func TestTab_ClosingAResponderRoundConfirmsNothing(t *testing.T) {
	c, st, au := newRoundController(t)
	ctx := context.Background()

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	s := twoQuestionRound(1, 1, 7)
	s.Frame.Responder = true
	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}, {ID: "A2", Text: "Postgres stays."}}
	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "the ops team"}}
	s.Frame.Decisions = []protocol.AssumptionDecision{accept("A1"), convert("A2")}

	closed, err := c.closeRound(ctx, au, bp, s)
	require.NoError(t, err)
	assert.Empty(t, closed.Converted, "only a human turns an assumption into a question")

	accepted, err := c.acceptedAssumptions(ctx, "BP1")
	require.NoError(t, err)
	assert.Empty(t, accepted, "under auto no assumption is confirmed")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 2, "the answers only")
}

func TestTab_UnderHumanEveryStatedAssumptionNeedsABasis(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.opts.Pack.Stage("intake")
	require.True(t, ok)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	c.putRound(ctx, c.newAudit(), "BP1", poster, tabRound(1, "intake", poster, []string{"One region."}, accept("A1")))

	v, _ := c.tabValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v, "One region."), "no basis under the human policy")

	v, _ = c.tabValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v, map[string]any{"text": "One region.", "basis": " "}), "a blank basis is none")

	v, state := c.tabValidator(bp, intake, 1)
	require.NoError(t, submitAssumptions(v, map[string]any{"text": "One region.", "basis": "round1.A1"}), "stated with its acceptance as the basis")
	assert.Empty(t, state.last())

	v, _ = c.tabValidator(bp, intake, 1)
	require.NoError(t, c.SetPolicy(ctx, "BP1", "intake", pack.PolicyAuto, "m2"))
	require.NoError(t, submitAssumptions(v, "One region."), "under auto, read at submit, an unbased one stands")
}

func TestTab_AConvertedAssumptionMustBeAskedFirst(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.opts.Pack.Stage("intake")
	require.True(t, ok)

	au := c.newAudit()
	stated := map[string]any{"text": "One region.", "basis": "round1.A1"}

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	s := twoQuestionRound(1, 1, poster)
	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}, {ID: "A2", Text: "Postgres stays."}}
	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 1}, {Number: 2, Text: "ops"}}
	s.Frame.Decisions = []protocol.AssumptionDecision{accept("A1"), convert("A2")}
	s.Change, s.Frame.Closed, s.Converted = changeClosed, true, []string{"A2"}
	c.putRound(ctx, au, "BP1", poster, s)

	v, _ := c.tabValidator(bp, intake, 1)
	err = submitAssumptions(v, stated)
	require.Error(t, err, "A2 was not asked yet")
	assert.Contains(t, err.Error(), "Postgres stays.", "the refusal names the assumption to ask")

	v, _ = c.tabValidator(bp, intake, 2)
	require.NoError(t, submitAssumptions(v, stated), "another attempt's round binds only that attempt")

	other := postedRun(t, st, "BP1", 2, 1, store.RunAsked, "round", nil)
	c.putRound(ctx, au, "BP1", other, twoQuestionRound(2, 4, other))

	v, _ = c.tabValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v, stated), "a later round of another attempt does not ask it")

	follow := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	c.putRound(ctx, au, "BP1", follow, twoQuestionRound(3, 6, follow))

	v, _ = c.tabValidator(bp, intake, 1)
	require.NoError(t, submitAssumptions(v, stated), "a later round of the attempt")
}

func TestTab_AutoAcceptsEveryAssumptionAndConfirmsNone(t *testing.T) {
	script := happyScript()
	script["intake"] = tabbed([]stage.RoundQuestion{roundQ("Users", "Who uses it?")}, []string{"One region."}, stage.ChatAnswer{},
		map[string]any{"slices": 1, "assumptions": []any{"One region."}})
	script["intake.responder"] = returns("# Answer", map[string]any{"answer": "ops", "reasoning": "a guess", "grounded": false})

	c, st, runner, _, _ := newPipeline(t, script)
	ctx := context.Background()

	driveIn(t, c, autoInput())

	s := latestRound(t, c, "BP1", 1)
	assert.True(t, s.Frame.Responder)
	assert.True(t, s.Frame.Closed)
	assert.Equal(t, []protocol.AssumptionDecision{accept("A1")}, s.Frame.Decisions)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 1, "no acceptance row")
	assert.Equal(t, store.ResponderAuto, qs[0].Responder)

	intakes := inputsOf(runner, "intake")
	require.Len(t, intakes, 2)
	assert.Contains(t, intakes[1].Task, "A1", "the continuation is told how A1 was decided")

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)

	i := slices.IndexFunc(list, func(a assumption) bool { return a.Text == "One region." })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, assumptionUnverified, list[i].Status, "auto never confirms")
}

func TestTab_AStageWithoutTheAssumptionsCheckPostsNoTab(t *testing.T) {
	var refused error

	script := dialogScript()
	script["slicing"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		_, refused = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Cut", "One slice or two?")}, Assumptions: []string{"One region."}})

		return done("# Slices", oneSlice()), nil
	}

	c, _, _, _, _ := newDialog(t, script)
	drive(t, c, "BP1")

	require.Error(t, refused)

	rounds, err := c.rounds(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Empty(t, rounds, "nothing was posted")
}

func TestTab_AChatReplacesTheTabAndKeepsUnchangedDecisions(t *testing.T) {
	script := happyScript()
	script["intake"] = tabbed([]stage.RoundQuestion{roundQ("Users", "Who uses it?")}, []string{"Postgres stays.", "One region."},
		stage.ChatAnswer{Reply: "Two zones as well.", Assumptions: []string{"One region.", "Two zones."}}, nil)

	c, _, runner, _, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	decide(t, c, "BP1", 1, []protocol.RoundAnswer{own(1, "ops")}, accept("A2"))
	c.Wait("BP1")
	require.Equal(t, 1, runner.calls(), "A1 is undecided, so the round waits")

	require.NoError(t, c.Message(ctx, "BP1", "What about zones?", "m3"))
	c.Wait("BP1")

	s := latestRound(t, c, "BP1", 1)
	assert.Equal(t, []protocol.RoundAssumption{{ID: "A2", Text: "One region."}, {ID: "A3", Text: "Two zones."}}, s.Frame.Assumptions,
		"an unchanged assumption keeps its id; a new one takes an id the round never issued")
	assert.Equal(t, []protocol.AssumptionDecision{accept("A2")}, s.Frame.Decisions, "A2's decision stays; nothing decides A3")
	assert.False(t, s.Frame.Closed, "A3 is undecided")
	assert.Equal(t, 2, runner.calls(), "the chat run, and no continuation")

	// CM still holds the user's pick for the dropped A1 and sends it again:
	// it lands on no assumption.
	decide(t, c, "BP1", 1, []protocol.RoundAnswer{own(1, "ops")}, accept("A1"), accept("A2"))
	c.Wait("BP1")

	s = latestRound(t, c, "BP1", 1)
	assert.Equal(t, 1, s.Dropped)
	assert.Equal(t, []protocol.AssumptionDecision{accept("A2")}, s.Frame.Decisions)
	assert.False(t, s.Frame.Closed)

	decide(t, c, "BP1", 1, []protocol.RoundAnswer{own(1, "ops")}, accept("A2"), convert("A3"))
	c.Wait("BP1")

	s = latestRound(t, c, "BP1", 1)
	assert.True(t, s.Frame.Closed)
	assert.Equal(t, []string{"A3"}, s.Converted)

	accepted, err := c.acceptedAssumptions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, accepted, 1, "only what the user accepted")
	assert.Equal(t, "round1.A2", accepted[0].Tag, "under the id it was shown with")
	assert.Equal(t, "One region.", accepted[0].Text)
}

func TestRetabbed_NeverReusesAnID(t *testing.T) {
	s := twoQuestionRound(1, 1, 7)
	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "x"}, {ID: "A2", Text: "y"}}
	s.Frame.Decisions = []protocol.AssumptionDecision{convert("A1"), accept("A2")}
	s.TabIssued = 2

	once := retabbed(s, []string{"y", "z"})
	assert.Equal(t, []protocol.RoundAssumption{{ID: "A2", Text: "y"}, {ID: "A3", Text: "z"}}, once.Frame.Assumptions)
	assert.Equal(t, []protocol.AssumptionDecision{accept("A2")}, once.Frame.Decisions)

	twice := retabbed(once, []string{"x", "y", "y"})
	assert.Equal(t, []protocol.RoundAssumption{{ID: "A4", Text: "x"}, {ID: "A2", Text: "y"}, {ID: "A5", Text: "y"}}, twice.Frame.Assumptions,
		"a dropped id is never issued again, and a repeated text is a new assumption")
	assert.Equal(t, []protocol.AssumptionDecision{accept("A2")}, twice.Frame.Decisions)

	_, dropped := mergeReply(twice, nil, []protocol.AssumptionDecision{accept("A1"), accept("A3")})
	assert.Equal(t, 2, dropped, "decisions for ids the tab no longer has")

	assert.Equal(t, []protocol.RoundAssumption{{ID: "A1", Text: "x"}, {ID: "A2", Text: "y"}}, s.Frame.Assumptions, "the snapshot read is untouched")
}

func TestTab_ATabCountsTowardTheRoundsSize(t *testing.T) {
	s := twoQuestionRound(1, 1, 7)
	require.NoError(t, fits(s))

	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: strings.Repeat("x", maxRoundBytes)}}
	require.Error(t, fits(s))
}

func TestTab_ATabOnlyRoundWritesNoAnswers(t *testing.T) {
	s := twoQuestionRound(1, 1, 7)
	s.Frame.Questions = nil
	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}}

	var b strings.Builder

	writeRoundAnswers(&b, s)
	assert.Empty(t, b.String())
}

func TestTab_IntakesSubmitNeedsABasisUnderHuman(t *testing.T) {
	tests := []struct {
		name    string
		in      StartInput
		refused bool
	}{
		{"human", startInput("BP1"), true},
		{"auto", autoInput(), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got error

			script := happyScript()
			script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				got = in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"assumptions": []any{"One region."}}})

				return done("# Need", map[string]any{"slices": 1}), nil
			}

			c, _, _, _, _ := newPipeline(t, script)
			driveIn(t, c, tt.in)

			if tt.refused {
				require.Error(t, got)

				return
			}

			require.NoError(t, got)
		})
	}
}

func TestTab_AChatOnAStageWithoutTheCheckPostsNoTab(t *testing.T) {
	var refused error

	c, _, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch {
		case in.Chat:
			_, refused = in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Noted.", Assumptions: []string{"One region."}})

			if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Noted."}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		case in.History == nil:
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Users", "Who uses it?")}}); err != nil {
				return stage.Output{}, err
			}

			return askedOutput(in), nil
		default:
			return artifactOutput("# Need", in), nil
		}
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "Why?", "m2"))
	c.Wait("BP1")

	require.Error(t, refused)
	assert.Empty(t, latestRound(t, c, "BP1", 1).Frame.Assumptions)
}

func TestTab_SignoffConfirmsAnAcceptedAssumption(t *testing.T) {
	script := happyScript()
	script["intake"] = tabbed([]stage.RoundQuestion{roundQ("Users", "Who uses it?")}, []string{"One region."}, stage.ChatAnswer{},
		map[string]any{"slices": 1, "assumptions": []any{map[string]any{"text": "One region.", "basis": "round1.A1"}}})

	c, _, _, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	decide(t, c, "BP1", 1, []protocol.RoundAnswer{own(1, "ops")}, accept("A1"))
	c.Wait("BP1")

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "One region.", list[0].Text)
	assert.Equal(t, "round1.A1", list[0].Basis)
	assert.Equal(t, assumptionConfirmed, list[0].Status)

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)

	entries, ok := so.Payload["assumptions"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 1)

	entry, ok := entries[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, assumptionConfirmed, entry["status"])
}

func TestTab_SolutionDesignOwesItsOwnAcceptances(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	au := c.newAudit()

	intakeRun := doneRun(t, st, "intake")
	c.putRound(ctx, au, "BP1", intakeRun, tabRound(1, "intake", intakeRun, []string{"Two zones."}, accept("A1")))

	designRun := doneRun(t, st, "solution_design")
	c.putRound(ctx, au, "BP1", designRun, tabRound(2, "solution_design", designRun, []string{"One region."}, accept("A1")))

	stating := func(d map[string]any, basis string) map[string]any {
		d["assumptions"] = []any{map[string]any{"text": "One region.", "basis": basis}}

		return d
	}

	owed := func(problems []string, tag string) bool {
		return slices.ContainsFunc(problems, func(p string) bool { return strings.HasPrefix(p, tag+": unused_answer: ") })
	}

	tests := []struct {
		name    string
		design  map[string]any
		owes    bool
		refused bool
	}{
		{"unused by any decision or assumption", goodDesign("assumption"), true, true},
		{"stated with it as the basis", stating(goodDesign("assumption"), "[ROUND2.a1]"), false, false},
		{"the basis of a decision", goodDesign("ROUND2.A1"), false, false},
		{"another stage's acceptance as a decision's basis", stating(goodDesign("round1.A1"), "round2.A1"), false, false},
		{"an unknown tag as a decision's basis", stating(goodDesign("round9.A1"), "round2.A1"), false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems, err := c.decisionProblems(ctx, bp, "solution_design", tt.design)
			require.NoError(t, err)
			assert.Equal(t, tt.owes, owed(problems, "round2.A1"), problems)
			assert.False(t, owed(problems, "round1.A1"), "intake's acceptance is not the design's to use")
			assert.Equal(t, tt.refused, len(problems) > 0, problems)
		})
	}

	// A re-slice after the acceptance: it is no longer owed, and stays citable.
	doneRun(t, st, "slicing")

	problems, err := c.decisionProblems(ctx, bp, "solution_design", goodDesign("assumption"))
	require.NoError(t, err)
	assert.False(t, owed(problems, "round2.A1"), problems)

	problems, err = c.decisionProblems(ctx, bp, "solution_design", goodDesign("round2.A1"))
	require.NoError(t, err)
	assert.Empty(t, problems)

	// An older close wrote a later acceptance as Q7: it is owed once, as Q7.
	olderRun := doneRun(t, st, "solution_design")
	older := tabRound(3, "solution_design", olderRun, []string{"Two regions."}, accept("A1"))
	older.Accepted = map[string]int{"A1": 7}
	c.putRound(ctx, au, "BP1", olderRun, older)
	require.NoError(t, st.AddQuestion(ctx, store.Question{
		BlueprintID: "BP1", RunID: olderRun, Stage: "solution_design", Number: 7, Text: "Two regions.", Options: []string{"Accept", "Turn into a question"},
		Answer: "Accepted", Responder: store.ResponderHuman, AnsweredAt: time.Now().UTC(),
	}))

	problems, err = c.decisionProblems(ctx, bp, "solution_design", goodDesign("round2.A1"))
	require.NoError(t, err)
	assert.Len(t, problems, 1, problems)
	assert.True(t, owed(problems, "Q7"), problems)
}

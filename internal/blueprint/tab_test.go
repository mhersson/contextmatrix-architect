package blueprint

import (
	"context"
	"fmt"
	"slices"
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

func TestTab_AcceptedAssumptionsAreRecordedAsAnsweredQuestions(t *testing.T) {
	script := happyScript()
	script["intake"] = tabbed([]stage.RoundQuestion{roundQ("Users", "Who uses it?")}, []string{"One region.", "Postgres stays."}, stage.ChatAnswer{}, nil)

	c, st, runner, _, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	decide(t, c, "BP1", 1, []protocol.RoundAnswer{own(1, "ops")}, accept("A1"), convert("A2"))
	c.Wait("BP1")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 2)
	assert.Equal(t, "ops", qs[0].Answer)

	acceptance := qs[1]
	assert.Equal(t, 2, acceptance.Number)
	assert.Equal(t, "One region.", acceptance.Text)
	assert.Equal(t, "Accepted", acceptance.Answer)
	assert.Equal(t, store.ResponderHuman, acceptance.Responder)
	assert.False(t, acceptance.Assumption, "a human accepted it")
	assert.Equal(t, "intake", acceptance.Stage)
	assert.Equal(t, qs[0].RunID, acceptance.RunID, "recorded under the run that posted the round")
	assert.Equal(t, qs[0].MessageID, acceptance.MessageID, "and the answering message")

	s := latestRound(t, c, "BP1", 1)
	assert.True(t, s.Frame.Closed)
	assert.Equal(t, map[string]int{"A1": 2}, s.Accepted)
	assert.Equal(t, []string{"A2"}, s.Converted)

	intakes := inputsOf(runner, "intake")
	require.Len(t, intakes, 2)

	cont := intakes[1]
	assert.NotNil(t, cont.History)
	assert.Contains(t, cont.Task, "[Q2]", "the accepted assumption's tag")
	assert.Contains(t, cont.Task, "A2", "the one turned into a question")
}

func TestTab_ACloseCutShortReusesTheAcceptanceNumbers(t *testing.T) {
	c, st, runner, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	au := c.newAudit()
	bp := seeded(t, st)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", asking)
	s := twoQuestionRound(1, 1, poster)
	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}}
	c.putRound(ctx, au, "BP1", poster, s)

	// The close reserved Q5 for A1 and stopped before any row.
	closing := s.clone()
	closing.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "the ops team"}}
	closing.Frame.Decisions = []protocol.AssumptionDecision{accept("A1")}
	closing.Change, closing.MessageID, closing.Accepted = ChangeClosing, "m9", map[string]int{"A1": 5}
	c.putRound(ctx, au, "BP1", 0, closing)

	next, err := c.nextQuestionNumber(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, 6, next, "a new question numbers after the reserved acceptance")

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 3)
	assert.Equal(t, []int{1, 2, 5}, []int{qs[0].Number, qs[1].Number, qs[2].Number})
	assert.Equal(t, "One region.", qs[2].Text)
	assert.Equal(t, "Accepted", qs[2].Answer)
	assert.Equal(t, "m9", qs[2].MessageID)

	closed := latestRound(t, c, "BP1", 1)
	assert.True(t, closed.Frame.Closed)
	assert.Equal(t, map[string]int{"A1": 5}, closed.Accepted)
	assert.Equal(t, 1, closedSnapshots(t, st, 1))
	require.Equal(t, 1, runner.calls(), "one continuation")

	_, err = c.closeRound(ctx, au, bp, closing)
	require.NoError(t, err, "a second close writes no row twice")

	qs, err = st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 3)
}

func TestTab_ClosingAResponderRoundConfirmsNothing(t *testing.T) {
	c, st, au := newRoundController(t)
	ctx := context.Background()

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	s := twoQuestionRound(1, 1, 7)
	s.Frame.Responder = true
	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}}
	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 2}, {Number: 2, Text: "the ops team"}}
	s.Frame.Decisions = []protocol.AssumptionDecision{accept("A1")}

	closed, err := c.closeRound(ctx, au, bp, s)
	require.NoError(t, err)
	assert.Nil(t, closed.Accepted)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, qs, 2, "the answers only: under auto no assumption is confirmed")
}

func TestTab_UnderHumanEveryStatedAssumptionNeedsABasis(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.opts.Pack.Stage("intake")
	require.True(t, ok)

	require.NoError(t, st.AddQuestion(ctx, store.Question{
		BlueprintID: "BP1", Stage: "intake", Number: 2, Text: "One region.", Options: []string{"Accept", "Turn into a question"},
		Answer: "Accepted", Responder: store.ResponderHuman, AnsweredAt: time.Now().UTC(),
	}))

	v, _ := c.tabValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v, "One region."), "no basis under the human policy")

	v, _ = c.tabValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v, map[string]any{"text": "One region.", "basis": " "}), "a blank basis is none")

	v, state := c.tabValidator(bp, intake, 1)
	require.NoError(t, submitAssumptions(v, map[string]any{"text": "One region.", "basis": "Q2"}), "stated with its acceptance as the basis")
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
	stated := map[string]any{"text": "One region.", "basis": "Q3"}

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	s := twoQuestionRound(1, 1, poster)
	s.Frame.Assumptions = []protocol.RoundAssumption{{ID: "A1", Text: "One region."}, {ID: "A2", Text: "Postgres stays."}}
	s.Frame.Answers = []protocol.RoundAnswer{{Number: 1, Option: 1}, {Number: 2, Text: "ops"}}
	s.Frame.Decisions = []protocol.AssumptionDecision{accept("A1"), convert("A2")}
	s.Change, s.Frame.Closed, s.Accepted, s.Converted = changeClosed, true, map[string]int{"A1": 3}, []string{"A2"}
	c.putRound(ctx, au, "BP1", poster, s)

	v, _ := c.tabValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v, stated), "A2 was not asked yet")

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
	assert.Equal(t, []protocol.RoundAssumption{{ID: "A1", Text: "One region."}, {ID: "A2", Text: "Two zones."}}, s.Frame.Assumptions)
	assert.Equal(t, []protocol.AssumptionDecision{accept("A1")}, s.Frame.Decisions,
		"One region.'s decision moves with it to A1; nothing decides the new A2")
	assert.False(t, s.Frame.Closed, "the new A2 is undecided")
	assert.Equal(t, 2, runner.calls(), "the chat run, and no continuation")

	decide(t, c, "BP1", 1, []protocol.RoundAnswer{own(1, "ops")}, accept("A1"), accept("A2"))
	c.Wait("BP1")
	assert.Equal(t, map[string]int{"A1": 2, "A2": 3}, latestRound(t, c, "BP1", 1).Accepted)
}

func TestTab_SignoffConfirmsAnAcceptedAssumption(t *testing.T) {
	script := happyScript()
	script["intake"] = tabbed([]stage.RoundQuestion{roundQ("Users", "Who uses it?")}, []string{"One region."}, stage.ChatAnswer{},
		map[string]any{"slices": 1, "assumptions": []any{map[string]any{"text": "One region.", "basis": "Q2"}}})

	c, _, _, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	decide(t, c, "BP1", 1, []protocol.RoundAnswer{own(1, "ops")}, accept("A1"))
	c.Wait("BP1")

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "One region.", list[0].Text)
	assert.Equal(t, "Q2", list[0].Basis)
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

func TestTab_AnAcceptedAssumptionStatedWithItsBasisIsNoUnusedAnswer(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, st.AddQuestion(ctx, store.Question{
		BlueprintID: "BP1", RunID: 1, Stage: "solution_design", Number: 1, Text: "One region.", Options: []string{"Accept", "Turn into a question"},
		Answer: "Accepted", Responder: store.ResponderHuman, AnsweredAt: time.Now().UTC(),
	}))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	d := goodDesign("assumption")

	validate, _ := c.decisionsValidator(bp, "solution_design")
	err = validate(ctx, stage.Artifact{Body: "# Design", Data: d})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Q1: unused_answer: ", "an acceptance the design never states is owed")

	d["assumptions"] = []any{map[string]any{"text": "One region.", "basis": "[q1]"}}

	validate, _ = c.decisionsValidator(bp, "solution_design")
	require.NoError(t, validate(ctx, stage.Artifact{Body: "# Design", Data: d}), "an assumption resting on it uses it")
}

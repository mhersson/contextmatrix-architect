package blueprint

import (
	"context"
	"errors"
	"fmt"
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

func plainStart(id string) StartInput {
	in := startInput(id)
	in.PlainLanguage = true

	return in
}

func TestPlain_TheFlagIsKeptFromCreationAndServed(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, plainStart("BP1")))

	created, ok := rec.first(EventCreated)
	require.True(t, ok)
	assert.Equal(t, true, created.Payload["plain_language"])
	assert.True(t, created.Plain, "the sink is told")

	view, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, view.PlainLanguage)

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, bp.PlainLanguage, "a later start never changes it")
}

func TestPlain_OffAddsNothing(t *testing.T) {
	c, _, _, rec := newController(t, artifactRun("# Need"))
	require.NoError(t, c.Start(context.Background(), startInput("BP1")))

	created, ok := rec.first(EventCreated)
	require.True(t, ok)
	assert.NotContains(t, created.Payload, "plain_language")
	assert.False(t, created.Plain)
}

func TestPlain_EveryRunKindGetsThePlainInstructions(t *testing.T) {
	// Auto mode, so every run kind runs without a person: the responder for
	// intake's untagged question, and the design's reviewer for its gate.
	run := func(t *testing.T, plain bool) (*fakeRunner, *pack.Pack) {
		t.Helper()

		script := dialogScript()
		script["intake.responder"] = respondsWith("one shared list")
		script["intake"] = func(ctx context.Context, si stage.Input) (stage.Output, error) {
			if si.History == nil {
				if _, err := si.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Lists", "How many lists does a household keep?", "One", "One per person")}}); err != nil {
					return stage.Output{}, err
				}

				return askedOutput(si), nil
			}

			return done("# Need", nil), nil
		}

		c, _, runner, _, _ := newDialog(t, script)
		in := startInput("BP1")
		in.Mode, in.PlainLanguage = protocol.ArchitectModeAuto, plain
		driveIn(t, c, in)

		return runner, c.opts.Pack
	}

	plainRunner, pk := run(t, true)
	defaultRunner, _ := run(t, false)

	require.NotEmpty(t, pk.PlainInstructions(), "the dialog test pack ships plain.md")

	for _, stageID := range []string{"intake", "intake.responder", "solution_design.review"} {
		p, d := inputsOf(plainRunner, stageID), inputsOf(defaultRunner, stageID)
		require.NotEmpty(t, p, stageID)
		require.NotEmpty(t, d, stageID)
		assert.NotEqual(t, d[0].Instructions, p[0].Instructions, stageID)
	}

	assert.Equal(t, pk.Instructions("intake"), inputsOf(defaultRunner, "intake")[0].Instructions, "off, the instructions are the pack's")
}

func TestPlain_AGateIsDecidedByTheAutoPolicy(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	driveIn(t, c, plainStart("BP1"))

	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner))

	gates, err := st.ListGates(context.Background(), "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestPlain_TheViewAndTheGatePlanShowGatesAsAuto(t *testing.T) {
	c, _, _, rec := newGated(t, gatedScript())
	driveIn(t, c, plainStart("BP1"))

	view, err := c.View(context.Background(), "BP1")
	require.NoError(t, err)

	var seen bool

	for _, step := range view.Pipeline {
		if step.Stage == "shape" {
			seen = true

			assert.Equal(t, pack.PolicyAuto, step.Policy)
		}
	}

	require.True(t, seen, "the view lists the shape stage")

	plan, ok := rec.first(EventGatePlan)
	require.True(t, ok)

	var planned bool

	for _, entry := range plan.Payload["stages"].([]any) {
		if m := entry.(map[string]any); m["stage"] == "shape" {
			planned = true

			assert.Equal(t, pack.PolicyAuto, m["gate_policy"])
		}
	}

	require.True(t, planned, "the plan lists the shape stage")
}

func TestPlain_AGateReplyStillMakesAGateHuman(t *testing.T) {
	c, _, _, rec := newGated(t, gatedScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, plainStart("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", pack.PolicyHuman, "m0"))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	assert.True(t, rec.has(EventGate))
}

func TestPlain_QuestionsKeepThePacksPolicy(t *testing.T) {
	c, st, _, _ := newGated(t, gatedScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, plainStart("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	got, err := c.policy(ctx, bp, "intake")
	require.NoError(t, err)
	assert.Equal(t, pack.PolicyHuman, got)

	got, err = c.gatePolicy(ctx, bp, "shape")
	require.NoError(t, err)
	assert.Equal(t, pack.PolicyAuto, got)
}

func experienceQ(title, text string, options ...string) stage.RoundQuestion {
	q := roundQ(title, text, options...)
	q.Audience = stage.AudienceExperience

	return q
}

func technicalQ(title, text string, options ...string) stage.RoundQuestion {
	q := roundQ(title, text, options...)
	q.Audience = stage.AudienceTechnical

	return q
}

func TestAudience_RecordedInEveryMode(t *testing.T) {
	script := dialogScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
			experienceQ("Offline", "Should it work without internet?", "Yes", "No"),
			technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server"),
		}}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}

	c, _, runner, _, _ := newDialog(t, script)
	drive(t, c, "BP1")

	s := latestRound(t, c, "BP1", 1)
	require.Len(t, s.Frame.Questions, 2, "off, one round holds both")
	assert.Equal(t, map[int]string{1: stage.AudienceExperience, 2: stage.AudienceTechnical}, s.Audiences)
	assert.Empty(t, inputsOf(runner, "intake.responder"))
}

// The dialog pack's intake is under the human policy.
func TestAudience_AnUntaggedQuestionIsRefusedOnlyInPlainLanguage(t *testing.T) {
	for _, plain := range []bool{false, true} {
		t.Run(fmt.Sprint(plain), func(t *testing.T) {
			var asked error

			script := dialogScript()
			script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				_, asked = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Users", "Who uses it?")}})

				return done("# Need", nil), nil
			}

			c, _, _, rec, _ := newDialog(t, script)
			in := startInput("BP1")
			in.PlainLanguage = plain
			driveIn(t, c, in)

			if plain {
				require.Error(t, asked)
				assert.False(t, rec.has(EventRound), "no round was posted")
			} else {
				assert.NoError(t, asked)
			}
		})
	}
}

func TestAudience_AChatRevisionKeepsItsTag(t *testing.T) {
	first := []stage.RoundQuestion{experienceQ("Offline", "Should it work without internet?", "Yes", "No")}

	revise := stage.ChatAnswer{Reply: "Noted.", Questions: []stage.RoundQuestion{
		{Number: 1, Title: "Offline", Text: "Should it work offline?", Options: []stage.RoundOption{{Title: "Yes"}, {Title: "No"}}, Audience: stage.AudienceTechnical},
		technicalQ("Storage", "Where is the list kept?"),
	}}

	c, _, _, _ := newController(t, tabbed(first, nil, revise, nil))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.Equal(t, map[int]string{1: stage.AudienceExperience}, latestRound(t, c, "BP1", 1).Audiences)

	require.NoError(t, c.Message(ctx, "BP1", "hm", "m2"))
	c.Wait("BP1")

	assert.Equal(t, map[int]string{1: stage.AudienceTechnical, 2: stage.AudienceTechnical}, latestRound(t, c, "BP1", 1).Audiences)
}

func TestAudience_AChatRevisionThatGivesNoAudienceDropsTheTag(t *testing.T) {
	first := []stage.RoundQuestion{experienceQ("Offline", "Should it work without internet?", "Yes", "No")}

	revise := stage.ChatAnswer{Reply: "Noted.", Questions: []stage.RoundQuestion{
		{Number: 1, Title: "Offline", Text: "Should it work offline?", Options: []stage.RoundOption{{Title: "Yes"}, {Title: "No"}}},
	}}

	c, _, _, _ := newController(t, tabbed(first, nil, revise, nil))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.Equal(t, map[int]string{1: stage.AudienceExperience}, latestRound(t, c, "BP1", 1).Audiences)

	require.NoError(t, c.Message(ctx, "BP1", "hm", "m2"))
	c.Wait("BP1")

	assert.Empty(t, latestRound(t, c, "BP1", 1).Audiences)
}

func answeringWith(answer, consequence string, in stage.Input) stage.Output {
	out := answering(answer, in)
	if consequence != "" {
		out.Artifact.Data["consequence"] = consequence
	}

	return out
}

func respondsWithConsequence(answer, consequence string) func(context.Context, stage.Input) (stage.Output, error) {
	return func(_ context.Context, in stage.Input) (stage.Output, error) {
		return answeringWith(answer, consequence, in), nil
	}
}

// receiveText waits for one text on ch, failing the test at a deadline.
func receiveText(t *testing.T, ch <-chan string) string {
	t.Helper()

	select {
	case text := <-ch:
		return text
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a text")
	}

	return ""
}

const listConsequence = "Both phones see changes within seconds, but adding items needs internet."

// splitIntake asks one experience and one technical question on its first
// run and submits on its continuation, recording the continuation's task.
func splitIntake(tasks chan<- string) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			tasks <- in.Task

			return done("# Need", nil), nil
		}

		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
			experienceQ("Offline", "Should it work without internet?", "Yes", "No"),
			technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server"),
		}}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}
}

// asksThenStops is an intake fake: a fresh run posts rd, and its
// continuation stops the pipeline, which these tests are not about.
func asksThenStops(rd stage.Round) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			return stage.Output{}, errors.New("stop after intake")
		}

		if _, err := in.Interviewer.Ask(ctx, rd); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}
}

func TestPlain_ATechnicalQuestionGoesToTheResponderAndAnExperienceQuestionToThePerson(t *testing.T) {
	tasks := make(chan string, 1)

	script := dialogScript()
	script["intake"] = splitIntake(tasks)
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, _, runner, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))

	tech := latestRound(t, c, "BP1", 1)
	assert.True(t, tech.Frame.Responder)
	assert.True(t, tech.Frame.Closed)
	require.Len(t, tech.Frame.Questions, 1)
	assert.Equal(t, "Where should the list be kept?", tech.Frame.Questions[0].Text)
	assert.Len(t, inputsOf(runner, "intake.responder"), 1)

	person := latestRound(t, c, "BP1", 2)
	assert.True(t, person.Plain)
	assert.False(t, person.Frame.Responder)
	assert.False(t, person.Frame.Closed)
	assert.Equal(t, 1, person.Technical)
	require.Len(t, person.Frame.Questions, 1)
	assert.Equal(t, "Should it work without internet?", person.Frame.Questions[0].Text)

	require.Len(t, person.Frame.Assumptions, 1)
	entry := person.Frame.Assumptions[0]
	assert.Equal(t, listConsequence, entry.Text)
	assert.NotEmpty(t, entry.Detail)
	assert.Equal(t, map[string]int{entry.ID: tech.Frame.Questions[0].Number}, person.Consequences)

	decide(t, c, "BP1", 2, []protocol.RoundAnswer{pick(person.Frame.Questions[0].Number, 2)}, accept(entry.ID))
	c.Wait("BP1")

	task := receiveText(t, tasks)
	assert.Contains(t, task, "On a shared server", "the responder's answer reaches the stage")
	assert.Contains(t, task, "Should it work without internet?")
}

func TestPlain_OnlyTechnicalQuestionsWithoutAConsequenceNeedNobody(t *testing.T) {
	script := dialogScript()
	script["intake"] = asksThenStops(stage.Round{Questions: []stage.RoundQuestion{
		technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server"),
		technicalQ("Sync", "How do the phones exchange changes?", "Through the server", "Directly"),
	}})
	script["intake.responder"] = respondsWith("On a shared server")

	c, st, runner, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))

	rounds, err := c.rounds(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, rounds, 1, "nothing is left for the person")
	assert.True(t, rounds[0].Frame.Responder)
	assert.True(t, rounds[0].Frame.Closed)
	assert.Len(t, rounds[0].Frame.Answers, 2)

	runs := inputsOf(runner, "intake")
	require.Len(t, runs, 2, "the stage continues at once")
	assert.NotNil(t, runs[1].History)
	assert.NotEqual(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

func TestPlain_OnlyTechnicalQuestionsWithAConsequencePostAConsequencesRound(t *testing.T) {
	script := dialogScript()
	script["intake"] = asksThenStops(stage.Round{Questions: []stage.RoundQuestion{
		technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server"),
	}})
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, st, runner, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))

	tech := latestRound(t, c, "BP1", 1)
	assert.True(t, tech.Frame.Responder)

	person := latestRound(t, c, "BP1", 2)
	assert.True(t, person.Plain)
	assert.Equal(t, 1, person.Technical)
	assert.Empty(t, person.Frame.Questions)
	require.Len(t, person.Frame.Assumptions, 1)
	assert.Equal(t, listConsequence, person.Frame.Assumptions[0].Text)
	assert.Equal(t, map[string]int{person.Frame.Assumptions[0].ID: tech.Frame.Questions[0].Number}, person.Consequences)
	assert.False(t, person.Frame.Closed)

	assert.Len(t, inputsOf(runner, "intake"), 1, "the stage waits for the person")
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

func TestPlain_AnAssumptionWithoutAConsequenceIsAcceptedAutomatically(t *testing.T) {
	const (
		hidden      = "Booking ids are random strings."
		slots       = "Bookings start on the quarter hour."
		consequence = "Customers can only book at :00, :15, :30 or :45."
	)

	script := dialogScript()
	script["intake"] = asksThenStops(stage.Round{
		Questions:    []stage.RoundQuestion{experienceQ("Reminders", "Should customers get a reminder the day before?", "Yes", "No")},
		Assumptions:  []string{hidden, slots},
		Consequences: []string{"", consequence},
	})

	c, _, runner, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))

	tech := latestRound(t, c, "BP1", 1)
	assert.True(t, tech.Frame.Responder)
	assert.True(t, tech.Frame.Closed)
	assert.Empty(t, tech.Frame.Questions)
	require.Len(t, tech.Frame.Assumptions, 1)
	assert.Equal(t, hidden, tech.Frame.Assumptions[0].Text)
	assert.Empty(t, tech.Frame.Assumptions[0].Detail)
	assert.Equal(t, []protocol.AssumptionDecision{accept(tech.Frame.Assumptions[0].ID)}, tech.Frame.Decisions)
	assert.Empty(t, inputsOf(runner, "intake.responder"), "no question needed the responder")

	person := latestRound(t, c, "BP1", 2)
	assert.True(t, person.Plain)
	assert.Equal(t, 1, person.Technical)
	require.Len(t, person.Frame.Questions, 1)
	require.Len(t, person.Frame.Assumptions, 1)
	assert.Equal(t, consequence, person.Frame.Assumptions[0].Text)
	assert.Equal(t, slots, person.Frame.Assumptions[0].Detail)
	assert.Empty(t, person.Consequences, "a stage's entry describes no answer")
}

func TestPlain_ARoundUnderTheAutoPolicyRecordsTheConsequenceOnly(t *testing.T) {
	for _, plain := range []bool{true, false} {
		t.Run(fmt.Sprint(plain), func(t *testing.T) {
			script := dialogScript()
			script["intake"] = splitIntake(make(chan string, 1))
			script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

			c, _, runner, rec, _ := newDialog(t, script)
			in := autoStart("BP1")
			in.PlainLanguage = plain
			driveIn(t, c, in)

			rounds, err := c.rounds(context.Background(), "BP1")
			require.NoError(t, err)
			require.Len(t, rounds, 1, "no round is put to a person")
			assert.True(t, rounds[0].Frame.Responder)
			assert.Equal(t, plain, inputsOf(runner, "intake")[0].PlainLanguage, "the round tools offer a consequence only on plain")

			answers := rec.all(EventAnswer)
			require.Len(t, answers, 2)

			for _, e := range answers {
				if plain {
					assert.Equal(t, listConsequence, e.Payload["consequence"])
				} else {
					assert.NotContains(t, e.Payload, "consequence")
				}
			}
		})
	}
}

func TestPlain_TheConsequenceIsNotRecordedOffPlain(t *testing.T) {
	script := dialogScript()
	script["intake"] = splitIntake(make(chan string, 1))
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, _, _, rec, _ := newDialog(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "intake", pack.PolicyAuto, "m0"))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	answers := rec.all(EventAnswer)
	require.Len(t, answers, 2)

	for _, e := range answers {
		assert.NotContains(t, e.Payload, "consequence")
	}
}

func TestPlain_AFailingResponderLeavesTheTechnicalAnswerToTheStage(t *testing.T) {
	script := dialogScript()
	script["intake"] = splitIntake(make(chan string, 1))
	script["intake.responder"] = func(context.Context, stage.Input) (stage.Output, error) {
		return stage.Output{}, errors.New("the endpoint is unreachable")
	}

	c, _, _, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))

	tech := latestRound(t, c, "BP1", 1)
	assert.True(t, tech.Frame.Responder)
	assert.True(t, tech.Frame.Closed)
	require.Len(t, tech.Frame.Answers, 1)
	assert.Zero(t, tech.Frame.Answers[0].Option)
	assert.NotEmpty(t, tech.Frame.Answers[0].Text, "the stage is told it decides, as on an auto round")

	person := latestRound(t, c, "BP1", 2)
	require.Len(t, person.Frame.Questions, 1)
	assert.Equal(t, "Should it work without internet?", person.Frame.Questions[0].Text)
	assert.Empty(t, person.Frame.Assumptions)
	assert.Empty(t, person.Consequences)
}

func TestPlain_AChatKeepsTheAnswersConsequencesAndRefusesTechnicalQuestions(t *testing.T) {
	const (
		shared      = "Each household shares one list."
		consequence = "Everyone at home sees and edits the same list."
		ticked      = "An item stays on the list until it is ticked off."
		stays       = "Nothing disappears until someone ticks it off."
		reworded    = "An item stays until a person ticks it off."
		history     = "The list keeps no record of past trips."
		noHistory   = "You cannot look back at what you bought before."
	)

	// The last call re-tabs from the tab it sees, so it is set once that tab
	// stands.
	chats := []stage.ChatAnswer{
		{Reply: "Noted.", Questions: []stage.RoundQuestion{technicalQ("Sync", "How do the phones exchange changes?")}},
		{Reply: "Noted.", Questions: []stage.RoundQuestion{roundQ("Shops", "Which shops do you use?")}},
		{Reply: "Noted.", Assumptions: []string{shared}},
		{Reply: "Noted.", Assumptions: []string{shared, ticked, history}, Consequences: []string{consequence, stays, noHistory}},
		{},
	}

	answered := make(chan error, len(chats))

	var n atomic.Int32

	ask := splitIntake(make(chan string, 1))

	script := dialogScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if !in.Chat {
			return ask(ctx, in)
		}

		_, err := in.Interviewer.Answer(ctx, chats[n.Add(1)-1])
		answered <- err

		// A refused call goes back to the model, which replies without it.
		if err != nil {
			if _, err := in.Interviewer.Answer(ctx, stage.ChatAnswer{Reply: "Noted."}); err != nil {
				return stage.Output{}, err
			}
		}

		return askedOutput(in), nil
	}
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, _, _, _, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))

	posted := latestRound(t, c, "BP1", 2)
	require.Len(t, posted.Frame.Assumptions, 1)

	entry := posted.Frame.Assumptions[0]
	require.NoError(t, c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: roundPromptID(2), Decisions: []protocol.AssumptionDecision{accept(entry.ID)}}, "m2"))
	c.Wait("BP1")

	before := latestRound(t, c, "BP1", 2)
	require.Equal(t, []protocol.AssumptionDecision{accept(entry.ID)}, before.Frame.Decisions)

	chat := func(msgID string) error {
		t.Helper()

		require.NoError(t, c.Message(ctx, "BP1", "about the list", msgID))
		c.Wait("BP1")

		select {
		case err := <-answered:
			return err
		default:
			t.Fatal("no chat run answered")
		}

		return nil
	}

	for i, what := range []string{"a technical question", "an untagged question", "an entry without a consequence"} {
		require.Error(t, chat(fmt.Sprintf("c%d", i)), what)

		after := latestRound(t, c, "BP1", 2)
		assert.Equal(t, before.Frame.Questions, after.Frame.Questions, what)
		assert.Equal(t, before.Audiences, after.Audiences, what)
		assert.Equal(t, before.Frame.Assumptions, after.Frame.Assumptions, what)
		assert.Equal(t, before.Frame.Decisions, after.Frame.Decisions, what)
	}

	require.NoError(t, chat("c3"))

	after := latestRound(t, c, "BP1", 2)
	require.Len(t, after.Frame.Assumptions, 4)

	for i, want := range []protocol.RoundAssumption{{Text: consequence, Detail: shared}, {Text: stays, Detail: ticked}, {Text: noHistory, Detail: history}} {
		got := after.Frame.Assumptions[i]
		assert.Equal(t, want.Text, got.Text)
		assert.Equal(t, want.Detail, got.Detail)
		assert.NotEqual(t, entry.ID, got.ID)
	}

	assert.Equal(t, entry, after.Frame.Assumptions[3], "the answer's entry is kept as it was")
	assert.Equal(t, before.Consequences, after.Consequences)
	assert.Equal(t, []protocol.AssumptionDecision{accept(entry.ID)}, after.Frame.Decisions)

	tab := after.Frame.Assumptions
	require.NoError(t, c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: roundPromptID(2), Decisions: []protocol.AssumptionDecision{
		accept(tab[0].ID), accept(tab[1].ID), convert(tab[2].ID),
	}}, "m3"))
	c.Wait("BP1")

	// The first entry unchanged, the second's consequence reworded, the third
	// dropped, and the answer's entry sent back as the tab showed it.
	chats[4] = stage.ChatAnswer{Reply: "Noted.", Assumptions: []string{shared, ticked, entry.Detail}, Consequences: []string{consequence, reworded, entry.Text}}

	require.NoError(t, chat("c4"))

	final := latestRound(t, c, "BP1", 2)
	require.Len(t, final.Frame.Assumptions, 3, "the answer's entry is not added again")
	assert.Equal(t, tab[0], final.Frame.Assumptions[0], "an unchanged entry keeps its id")

	changed := final.Frame.Assumptions[1]
	assert.Equal(t, reworded, changed.Text)
	assert.Equal(t, ticked, changed.Detail)
	assert.NotContains(t, []string{entry.ID, tab[0].ID, tab[1].ID, tab[2].ID}, changed.ID, "a reworded consequence takes a new id")

	assert.Equal(t, entry, final.Frame.Assumptions[2])
	assert.ElementsMatch(t, []protocol.AssumptionDecision{accept(entry.ID), accept(tab[0].ID)}, final.Frame.Decisions,
		"the unchanged entry keeps its decision; the reworded and the dropped ones lose theirs")
}

func TestPlain_AReopenedSplitRoundStillTellsTheStageBothHalves(t *testing.T) {
	const consequence = "Everyone at home sees and edits the same list."

	tasks := make(chan string, 1)

	// The answer carries no consequence, so it reaches the continuation only
	// through the responder's round; the tab holds the stage's entry alone.
	script := dialogScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			tasks <- in.Task

			return done("# Need", nil), nil
		}

		if _, err := in.Interviewer.Ask(ctx, stage.Round{
			Questions: []stage.RoundQuestion{
				experienceQ("Offline", "Should it work without internet?", "Yes", "No"),
				technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server"),
			},
			Assumptions:  []string{"Each household shares one list."},
			Consequences: []string{consequence},
		}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}
	script["intake.responder"] = respondsWith("On a shared server")

	c, st, _, _, ws := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))
	require.False(t, latestRound(t, c, "BP1", 2).Frame.Closed)
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Shutdown(ctx))

	c2, runner2, _ := newDialogOn(t, st, script, ws)
	require.NoError(t, c2.Start(ctx, plainStart("BP1")))
	c2.Wait("BP1")

	person := latestRound(t, c2, "BP1", 2)
	assert.Equal(t, changeShown, person.Change)
	require.Len(t, person.Frame.Questions, 1)
	require.Len(t, person.Frame.Assumptions, 1)
	assert.Equal(t, consequence, person.Frame.Assumptions[0].Text, "shown with its consequence")
	assert.Empty(t, person.Consequences, "no answer's entry is on the tab")
	assert.Empty(t, inputsOf(runner2, "intake"), "a reopen runs nothing while the round waits")

	decide(t, c2, "BP1", 2, []protocol.RoundAnswer{pick(person.Frame.Questions[0].Number, 1)}, accept(person.Frame.Assumptions[0].ID))
	c2.Wait("BP1")

	task := receiveText(t, tasks)
	assert.Contains(t, task, "Where should the list be kept?", "the responder's round is read from the record")
	assert.Contains(t, task, "On a shared server")
	assert.Contains(t, task, "Should it work without internet?")
}

func TestPlain_APackWithoutResponderRefusesTechnicalQuestions(t *testing.T) {
	var refused error

	// The single pack carries no stages/responder.md.
	c, st, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		_, refused = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
			experienceQ("Offline", "Should it work without internet?", "Yes", "No"),
			technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server"),
		}})

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, plainStart("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.Error(t, refused)

	posted, err := st.ListEventsOfKind(ctx, "BP1", EventRound)
	require.NoError(t, err)
	assert.Empty(t, posted, "nothing is posted")
}

func TestPlain_AnAskTooBigForThePersonIsRefusedBeforeTheResponderRuns(t *testing.T) {
	big := strings.Repeat("Some households shop together every week. ", 3<<10)
	storage := technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server")

	tests := []struct {
		name  string
		round stage.Round
	}{
		{"an experience question", stage.Round{Questions: []stage.RoundQuestion{
			{Title: "Offline", Text: "Should it work without internet?", Why: big, Audience: stage.AudienceExperience}, storage,
		}}},
		{"a consequence", stage.Round{
			Questions:    []stage.RoundQuestion{storage},
			Assumptions:  []string{"Each household shares one list."},
			Consequences: []string{big},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var refused error

			script := dialogScript()
			script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				_, refused = in.Interviewer.Ask(ctx, tt.round)

				return done("# Need", nil), nil
			}
			script["intake.responder"] = respondsWith("On a shared server")

			c, st, runner, _, _ := newDialog(t, script)
			driveIn(t, c, plainStart("BP1"))

			require.Error(t, refused)

			posted, err := st.ListEventsOfKind(context.Background(), "BP1", EventRound)
			require.NoError(t, err)
			assert.Empty(t, posted, "nothing is posted")
			assert.Empty(t, inputsOf(runner, "intake.responder"), "the responder never runs")
		})
	}
}

func TestPlain_TheSplitAsksResultNamesTheRespondersAnswers(t *testing.T) {
	results := make(chan string, 1)

	script := dialogScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		text, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
			experienceQ("Offline", "Should it work without internet?", "Yes", "No"),
			technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server"),
		}})
		if err != nil {
			return stage.Output{}, err
		}

		results <- text

		return askedOutput(in), nil
	}
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, _, _, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))

	text := receiveText(t, results)
	assert.Contains(t, text, "On a shared server", "a chat that resumes the conversation knows the answer")
	assert.Contains(t, text, listConsequence)
}

// The dialog pack's intake is under the human policy; the cap is set on it.
func TestPlain_TheQuestionCapCountsTheTechnicalQuestions(t *testing.T) {
	storage := technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server")
	sync := technicalQ("Sync", "How do the phones exchange changes?", "Through the server", "Directly")
	backup := technicalQ("Backup", "How often is the list copied for safekeeping?", "On every change", "Once a day")
	offline := experienceQ("Offline", "Should it work without internet?", "Yes", "No")
	shops := experienceQ("Shops", "Which shops do you shop at?", "One", "Several")

	t.Run("two technical questions past a cap of one", func(t *testing.T) {
		var refused error

		script := dialogScript()
		script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
			_, refused = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{offline, storage, sync}})

			return done("# Need", nil), nil
		}
		script["intake.responder"] = respondsWith("On a shared server")

		c, st, runner, _, _ := newDialog(t, script)
		c.opts.Pack.Stages[0].MaxQuestions = 1
		driveIn(t, c, plainStart("BP1"))

		require.Error(t, refused)

		posted, err := st.ListEventsOfKind(context.Background(), "BP1", EventRound)
		require.NoError(t, err)
		assert.Empty(t, posted, "nothing is posted")
		assert.Empty(t, inputsOf(runner, "intake.responder"), "the responder never runs")
	})

	t.Run("experience questions spend none of it", func(t *testing.T) {
		asked := make(chan error, 2)

		var runs atomic.Int32

		script := dialogScript()
		script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
			switch runs.Add(1) {
			case 1:
				_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{offline, shops, storage}})

				return askedOutput(in), err
			case 2:
				_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{sync}})
				asked <- err

				return askedOutput(in), err
			default:
				_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{backup}})
				asked <- err

				return stage.Output{}, errors.New("stop after intake")
			}
		}
		script["intake.responder"] = respondsWith("On a shared server")

		c, _, _, _, _ := newDialog(t, script)
		c.opts.Pack.Stages[0].MaxQuestions = 2
		driveIn(t, c, plainStart("BP1"))

		person := latestRound(t, c, "BP1", 2)
		require.Len(t, person.Frame.Questions, 2, "two experience questions and one technical fit a cap of two")

		var answers []protocol.RoundAnswer
		for _, q := range person.Frame.Questions {
			answers = append(answers, pick(q.Number, 1))
		}

		decide(t, c, "BP1", 2, answers)
		c.Wait("BP1")

		require.Len(t, asked, 2, "the stage asked twice more")
		require.NoError(t, <-asked, "the second technical question is within the cap")
		require.Error(t, <-asked, "a third is past it")
	})
}

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

const slotConsequence = "Customers can only book at :00, :15, :30 or :45."

// salonIntake posts one experience question and one assumption with a
// consequence; its continuation states it with the acceptance's tag.
func salonIntake() func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			return done("# Need", map[string]any{"assumptions": []any{
				map[string]any{"text": "Bookings start on the quarter hour.", "basis": "round1.A1", "consequence": slotConsequence},
			}}), nil
		}

		if _, err := in.Interviewer.Ask(ctx, stage.Round{
			Questions:    []stage.RoundQuestion{experienceQ("Reminders", "Should customers get a reminder the day before?", "Yes", "No")},
			Assumptions:  []string{"Bookings start on the quarter hour."},
			Consequences: []string{slotConsequence},
		}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}
}

func TestPlain_AnAcceptedConsequenceIsMarkedNotConfirmed(t *testing.T) {
	script := dialogScript()
	script["intake"] = salonIntake()

	c, _, _, _, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))

	person := latestRound(t, c, "BP1", 1)
	decide(t, c, "BP1", 1, []protocol.RoundAnswer{pick(person.Frame.Questions[0].Number, 1)}, accept(person.Frame.Assumptions[0].ID))
	c.Wait("BP1")

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)

	a := byID(list)["A1"]
	assert.Equal(t, assumptionUnverified, a.Status, "accepting a consequence confirms nothing")
	assert.Equal(t, 1, a.ConsequenceAccepted)
	assert.Equal(t, slotConsequence, a.Consequence)
	assert.Equal(t, "Bookings start on the quarter hour.", a.Text, "the technical text stays the record")

	entry := assumptionsPayload(list)[0].(map[string]any)
	assert.Equal(t, slotConsequence, entry["consequence"])
	assert.Equal(t, map[string]any{"by": store.ResponderHuman, "round": 1}, entry["consequence_accepted"])
}

func TestPlain_AnAcceptedAnswerConsequenceMarksItsQuestion(t *testing.T) {
	script := dialogScript()
	script["intake"] = splitIntake(make(chan string, 1))
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, _, _, _, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))

	tech := latestRound(t, c, "BP1", 1)
	require.Len(t, tech.Frame.Questions, 1)

	person := latestRound(t, c, "BP1", 2)
	require.Len(t, person.Frame.Assumptions, 1)
	decide(t, c, "BP1", 2, []protocol.RoundAnswer{pick(person.Frame.Questions[0].Number, 1)}, accept(person.Frame.Assumptions[0].ID))
	c.Wait("BP1")

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)

	q, ok := byID(list)[fmt.Sprintf("Q%d", tech.Frame.Questions[0].Number)]
	require.True(t, ok, "the responder's answer is listed")
	assert.Equal(t, assumptionAssumed, q.Status, "the answer keeps the status the responder's answer gives it")
	assert.Equal(t, 2, q.ConsequenceAccepted)
	assert.Equal(t, listConsequence, q.Consequence)
}

func TestPlain_TheBasisRuleAsksOnlyForConsequences(t *testing.T) {
	const slots = "Bookings start on the quarter hour."

	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, plainStart("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	s := tabRound(1, "intake", poster, []string{slotConsequence}, accept("A1"))
	s.Plain, s.Frame.Assumptions[0].Detail = true, slots
	c.putRound(ctx, c.newAudit(), "BP1", poster, s)

	tagged := map[string]any{"text": slots, "consequence": slotConsequence, "basis": "round1.A1"}

	tests := []struct {
		name  string
		entry any
		ok    bool
	}{
		{"no consequence and no basis", map[string]any{"text": "Booking ids are random strings."}, true},
		{"a consequence and no basis", map[string]any{"text": slots, "consequence": slotConsequence}, false},
		{"a consequence with its acceptance's tag", tagged, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _ := c.tabValidator(bp, intake, 1)
			err := submitAssumptions(v, tt.entry)

			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}

	v, _ := c.assumptionsValidator(bp, "intake")
	require.NoError(t, submitAssumptions(v, tagged), "a plain acceptance's tag is a basis")
	require.Error(t, submitAssumptions(v, map[string]any{"text": slots, "consequence": 15}), "a consequence is a sentence")
}

func TestPlain_AnAcceptanceCannotBeRepostedInOtherWords(t *testing.T) {
	tests := []struct{ name, consequence string }{
		{"the same consequence", slotConsequence},
		{"another consequence", "Appointments begin only at set times."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asked := make(chan error, 1)
			first := salonIntake()

			script := dialogScript()
			script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				if in.History == nil {
					return first(ctx, in)
				}

				_, err := in.Interviewer.Ask(ctx, stage.Round{
					Assumptions:  []string{"Bookings start on the quarter hour."},
					Consequences: []string{tt.consequence},
				})
				asked <- err

				return done("# Need", nil), nil
			}

			c, _, _, _, _ := newDialog(t, script)
			driveIn(t, c, plainStart("BP1"))

			person := latestRound(t, c, "BP1", 1)
			decide(t, c, "BP1", 1, []protocol.RoundAnswer{pick(person.Frame.Questions[0].Number, 1)}, accept(person.Frame.Assumptions[0].ID))
			c.Wait("BP1")

			select {
			case err := <-asked:
				require.ErrorContains(t, err, acceptanceTag(1, person.Frame.Assumptions[0].ID), "the refusal names the tag to cite")
			default:
				t.Fatal("the continuation never asked")
			}

			rounds, err := c.rounds(context.Background(), "BP1")
			require.NoError(t, err)
			assert.Len(t, rounds, 1, "nothing is posted again")
		})
	}
}

func TestPlain_AHandedOverRoundMarksOnlyThePersonsAcceptances(t *testing.T) {
	const (
		slots       = "Bookings start on the quarter hour."
		moves       = "A booking can be moved until the day before."
		movesShown  = "Customers cannot move a booking on the day itself."
		movesStated = "Customers cannot move a booking once its day has come."
	)

	script := dialogScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			return done("# Need", map[string]any{"assumptions": []any{
				map[string]any{"text": slots, "basis": "round1.A1"},
				map[string]any{"text": moves, "consequence": movesStated},
			}}), nil
		}

		if _, err := in.Interviewer.Ask(ctx, stage.Round{
			Questions:    []stage.RoundQuestion{experienceQ("Reminders", "Should customers get a reminder the day before?", "Yes", "No")},
			Assumptions:  []string{slots, moves},
			Consequences: []string{slotConsequence, movesShown},
		}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}
	script["intake.responder"] = respondsWith("Yes")

	c, _, _, _, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))

	person := latestRound(t, c, "BP1", 1)
	require.True(t, person.Plain)
	require.Len(t, person.Frame.Assumptions, 2)

	first, second := person.Frame.Assumptions[0].ID, person.Frame.Assumptions[1].ID
	require.NoError(t, c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: roundPromptID(1), Decisions: []protocol.AssumptionDecision{accept(first)}}, "m-tab"))
	c.Wait("BP1")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	round := latestRound(t, c, "BP1", 1)
	require.True(t, round.Frame.Closed)
	require.ElementsMatch(t, []protocol.AssumptionDecision{accept(first), accept(second)}, round.Frame.Decisions)

	accepted, err := c.acceptedAssumptions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, map[string]int{acceptanceTag(1, first): 1}, plainTags(accepted), "the responder's acceptance is no person's")

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)

	a1, a2 := byID(list)["A1"], byID(list)["A2"]
	assert.Equal(t, slots, a1.Text)
	assert.Equal(t, acceptanceTag(1, first), a1.Basis)
	assert.Equal(t, 1, a1.ConsequenceAccepted, "the person accepted it before the switch")
	assert.Equal(t, slotConsequence, a1.Consequence, "the consequence the person accepted")
	assert.Equal(t, assumptionUnverified, a1.Status)

	assert.Equal(t, moves, a2.Text)
	assert.Zero(t, a2.ConsequenceAccepted, "the responder's acceptance marks nothing")
	assert.Equal(t, movesStated, a2.Consequence)
	assert.Equal(t, assumptionUnverified, a2.Status)
}

// tabDecisionLine is what text, a resumed conversation's task, says of the
// decision on tab entry id of a plain round: the line after the entry and
// its consequence.
func tabDecisionLine(text, id string) string {
	lines := strings.Split(text, "\n")

	for i, line := range lines {
		if strings.HasPrefix(line, "- "+id+": ") && i+2 < len(lines) {
			return lines[i+2]
		}
	}

	return ""
}

func TestPlain_AHandedOverRoundMarksOnlyTheAnswersThePersonAccepted(t *testing.T) {
	tasks := make(chan string, 1)

	script := dialogScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			tasks <- in.Task

			return done("# Need", nil), nil
		}

		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
			experienceQ("Offline", "Should it work without internet?", "Yes", "No"),
			technicalQ("Storage", "Where should the list be kept?", "On each phone", "On a shared server"),
			technicalQ("Sync", "How do the phones exchange changes?", "Through the server", "Directly"),
		}}); err != nil {
			return stage.Output{}, err
		}

		return askedOutput(in), nil
	}
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, _, _, _, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))

	person := latestRound(t, c, "BP1", 2)
	require.True(t, person.Plain)
	require.Len(t, person.Frame.Assumptions, 2, "one entry per answer with a consequence")

	first, second := person.Frame.Assumptions[0].ID, person.Frame.Assumptions[1].ID
	require.NoError(t, c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: roundPromptID(2), Decisions: []protocol.AssumptionDecision{accept(first)}}, "m-tab"))
	c.Wait("BP1")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	round := latestRound(t, c, "BP1", 2)
	require.True(t, round.Frame.Closed)
	require.ElementsMatch(t, []protocol.AssumptionDecision{accept(first), accept(second)}, round.Frame.Decisions)

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)

	kept, handed := byID(list)[fmt.Sprintf("Q%d", person.Consequences[first])], byID(list)[fmt.Sprintf("Q%d", person.Consequences[second])]
	assert.Equal(t, 2, kept.ConsequenceAccepted, "the person accepted it before the switch")
	assert.Zero(t, handed.ConsequenceAccepted, "the responder's acceptance marks nothing")
	assert.Equal(t, listConsequence, handed.Consequence, "its consequence is still recorded")

	for _, q := range []assumption{kept, handed} {
		assert.Equal(t, assumptionAssumed, q.Status, q.ID)
	}

	// The handed-over answer stands as recorded: the continuation does not
	// tell the stage to state it, as it tells it of a stage's entry.
	accepted, err := c.acceptedAssumptions(ctx, "BP1")
	require.NoError(t, err)

	asStage := round.clone()
	delete(asStage.Consequences, second)

	var b strings.Builder
	writeRoundAssumptions(&b, asStage, accepted)

	told := tabDecisionLine(receiveText(t, tasks), second)
	require.NotEmpty(t, told)
	assert.NotEqual(t, tabDecisionLine(b.String(), second), told)
}

func TestPlain_ARerunOfTheStageIsToldItsPlainAcceptances(t *testing.T) {
	const slots = "Bookings start on the quarter hour."

	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, plainStart("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	s := tabRound(1, "intake", poster, []string{slotConsequence}, accept("A1"))
	s.Plain, s.Frame.Assumptions[0].Detail = true, slots
	c.putRound(ctx, c.newAudit(), "BP1", poster, s)

	var b strings.Builder
	require.NoError(t, c.writeAnswered(ctx, &b, bp, intake))
	assert.Contains(t, b.String(), "round1.A1", "the stage run again is told the acceptance's tag")
	assert.Contains(t, b.String(), slots)
	assert.Contains(t, b.String(), slotConsequence)
}

func TestPlain_OffTheEntriesCarryNoConsequence(t *testing.T) {
	const consequence = "Every member needs an email address to join."

	for _, plain := range []bool{false, true} {
		t.Run(fmt.Sprint(plain), func(t *testing.T) {
			script := happyScript()
			script["intake"] = returns("# Need", map[string]any{"slices": 1, "assumptions": []any{
				map[string]any{"text": "Members sign in with their email address.", "consequence": consequence},
			}})

			c, _, _, _, _ := newPipeline(t, script)
			in := startInput("BP1")
			in.PlainLanguage = plain
			driveIn(t, c, in)

			list, err := c.assumptions(context.Background(), "BP1")
			require.NoError(t, err)
			require.Len(t, list, 1)

			entry := assumptionsPayload(list)[0].(map[string]any)
			assert.NotContains(t, entry, "consequence_accepted")

			if plain {
				assert.Equal(t, consequence, entry["consequence"])
			} else {
				assert.NotContains(t, entry, "consequence")
				assert.Empty(t, list[0].Consequence)
			}
		})
	}
}

const (
	recipesShared  = "Everyone in a household sees the same recipes."
	recipesPrivate = "Each person keeps a private recipe box."
)

// acceptedThenRestated drives a plain intake whose person accepts
// recipesShared as the consequence of a stage's assumption or, with
// viaAnswer, of the responder's answer, and whose continuation states the
// decision resting on that acceptance with restated as its consequence.
func acceptedThenRestated(t *testing.T, viaAnswer bool, restated string) *Controller {
	t.Helper()

	const technical = "Recipes are saved per household."

	rd := stage.Round{
		Questions:    []stage.RoundQuestion{experienceQ("Sharing", "Should two people in one household share their recipes?", "Yes", "No")},
		Assumptions:  []string{technical},
		Consequences: []string{recipesShared},
	}
	basis, person := "round1.A1", 1

	if viaAnswer {
		rd = stage.Round{Questions: []stage.RoundQuestion{technicalQ("Storage", "Where are the recipes kept?", "On each device", "In one shared place")}}
		basis, person = "Q1", 2
	}

	script := dialogScript()
	script["intake.responder"] = respondsWithConsequence("In one shared place", recipesShared)
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			return done("# Need", map[string]any{"assumptions": []any{
				map[string]any{"text": technical, "basis": basis, "consequence": restated},
			}}), nil
		}

		_, err := in.Interviewer.Ask(ctx, rd)

		return askedOutput(in), err
	}

	c, _, _, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))

	round := latestRound(t, c, "BP1", person)
	require.True(t, round.Plain)
	require.Len(t, round.Frame.Assumptions, 1)
	require.Equal(t, recipesShared, round.Frame.Assumptions[0].Text)

	var answers []protocol.RoundAnswer
	for _, q := range round.Frame.Questions {
		answers = append(answers, pick(q.Number, 1))
	}

	decide(t, c, "BP1", person, answers, accept(round.Frame.Assumptions[0].ID))
	c.Wait("BP1")

	return c
}

func TestPlain_AnAcceptedConsequenceStaysTheSentenceShown(t *testing.T) {
	for name, viaAnswer := range map[string]bool{"a stage's assumption": false, "an answer": true} {
		t.Run(name, func(t *testing.T) {
			c := acceptedThenRestated(t, viaAnswer, recipesPrivate)

			list, err := c.assumptions(context.Background(), "BP1")
			require.NoError(t, err)

			a, ok := byID(list)["A1"]
			require.True(t, ok)
			assert.Positive(t, a.ConsequenceAccepted)
			assert.Equal(t, recipesShared, a.Consequence, "the sentence the person accepted, not the restated one")

			for _, e := range assumptionsPayload(list) {
				if entry := e.(map[string]any); entry["id"] == "A1" {
					assert.Equal(t, recipesShared, entry["consequence"])
				}
			}
		})
	}
}

const (
	copyDate  = "Photos copied from a friend may get the copy date, not the day they were taken."
	takenDate = "Photos keep the day they were taken."
)

func TestPlain_ATurnedDownAnswerIsAskedAgainAndSupersedesTheOld(t *testing.T) {
	var runs, answered atomic.Int32

	responder := func(_ context.Context, in stage.Input) (stage.Output, error) {
		if answered.Add(1) == 1 {
			return answeringWith("Use the file's change date", copyDate, in), nil
		}

		return answeringWith("Read the camera's date", takenDate, in), nil
	}

	dates := technicalQ("Dates", "How should each photo's date be found?", "Read the camera's date", "Use the file's change date")
	tasks := make(chan string, 4)

	script := dialogScript()
	script["intake.responder"] = responder
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch runs.Add(1) {
		case 1:
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{dates}})

			return askedOutput(in), err
		case 2:
			tasks <- in.Task

			again := dates
			again.Reasks = 1
			again.Why = copyDate
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{again}})

			return askedOutput(in), err
		default:
			return done("# Need", nil), nil
		}
	}

	c, _, runner, rec, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))

	person := latestRound(t, c, "BP1", 2)
	decide(t, c, "BP1", 2, nil, convert(person.Frame.Assumptions[0].ID))
	c.Wait("BP1")

	assert.Contains(t, receiveText(t, tasks), copyDate, "the stage is told the person's concern")

	reasked := inputsOf(runner, "intake.responder")
	require.Len(t, reasked, 2)
	assert.Contains(t, reasked[1].Task, copyDate, "the responder answers with the concern")

	answers := rec.all(EventAnswer)
	require.Len(t, answers, 2)
	assert.Equal(t, 1, answers[1].Payload["supersedes"])

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Q%d", answers[1].Payload["number"]), byID(list)["Q1"].SupersededBy)
}

// photoDates is the technical question the photo-renaming tool asks, and asks
// again.
func photoDates() stage.RoundQuestion {
	return technicalQ("Dates", "How should each photo's date be found?", "Read the camera's date", "Use the file's change date")
}

// reaskingIntake asks photoDates on its first run and, on its second, asks it
// again with reasks 1 and no why, so only the controller tells its responder
// the concern; any later run is then's.
func reaskingIntake(then func(context.Context, stage.Input) (stage.Output, error)) func(context.Context, stage.Input) (stage.Output, error) {
	var runs atomic.Int32

	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch runs.Add(1) {
		case 1:
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{photoDates()}})

			return askedOutput(in), err
		case 2:
			again := photoDates()
			again.Reasks = 1
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{again}})

			return askedOutput(in), err
		default:
			return then(ctx, in)
		}
	}
}

// turnDownTheFirstAnswer turns the first entry on the person's round 2, the
// consequence of the responder's answer to Q1, into a question.
func turnDownTheFirstAnswer(t *testing.T, c *Controller) {
	t.Helper()

	person := latestRound(t, c, "BP1", 2)
	require.Equal(t, map[string]int{person.Frame.Assumptions[0].ID: 1}, person.Consequences)

	decide(t, c, "BP1", 2, nil, convert(person.Frame.Assumptions[0].ID))
	c.Wait("BP1")
}

func stopAfterIntake(context.Context, stage.Input) (stage.Output, error) {
	return stage.Output{}, errors.New("stop after intake")
}

func TestPlain_ASubmitWaitsForTheReAsk(t *testing.T) {
	const detail = "How should each photo's date be found? Use the file's change date"

	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, plainStart("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	au := c.newAudit()

	responderRound := func(n, question int) roundState {
		return roundState{
			Number: n, Attempt: 1, BuilderRound: 1, PostedBy: poster, RunID: poster, Change: changeClosed,
			Frame: protocol.LogRound{
				Stage: "intake", Responder: true, Closed: true,
				Questions: []protocol.RoundQuestion{{Number: question, Text: "How should each photo's date be found?"}},
				Answers:   []protocol.RoundAnswer{own(question, "Use the file's change date")},
			},
		}
	}

	c.putRound(ctx, au, "BP1", poster, responderRound(1, 1))

	person := tabRound(2, "intake", poster, []string{copyDate}, convert("A1"))
	person.Plain, person.Technical, person.Consequences = true, 1, map[string]int{"A1": 1}
	person.Frame.Assumptions[0].Detail = detail
	c.putRound(ctx, au, "BP1", poster, person)

	// Intake runs both checks; the turned-down answer is refused once, by
	// the re-ask check.
	tab, tabState := c.tabValidator(bp, intake, 1)
	require.NoError(t, submitAssumptions(tab))
	assert.Empty(t, tabState.last())

	v, state := c.reaskValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v), "Q1 is not asked again yet")
	assert.Len(t, state.last(), 1, "the turned-down answer is refused once")

	// A later round that does not ask Q1 again is not enough.
	c.putRound(ctx, au, "BP1", poster, responderRound(3, 2))

	v, _ = c.reaskValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v), "round 3 did not ask Q1 again")

	reask := responderRound(4, 3)
	reask.Reasks = map[int]int{3: 1}
	c.putRound(ctx, au, "BP1", poster, reask)

	v, _ = c.reaskValidator(bp, intake, 1)
	assert.NoError(t, submitAssumptions(v))
}

func TestPlain_AnUnaskedConversionIsNamedByItsTechnicalText(t *testing.T) {
	const slots = "Bookings start on the quarter hour."

	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, plainStart("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	s := tabRound(1, "intake", poster, []string{slotConsequence}, convert("A1"))
	s.Plain, s.Frame.Assumptions[0].Detail = true, slots
	c.putRound(ctx, c.newAudit(), "BP1", poster, s)

	v, _ := c.tabValidator(bp, intake, 1)
	err = submitAssumptions(v)
	require.Error(t, err)
	assert.Contains(t, err.Error(), slots, "the stage is shown the technical text")
	assert.NotContains(t, err.Error(), slotConsequence)
}

func TestPlain_ATurnedDownAssumptionComesBackOnlyOnAnAcceptance(t *testing.T) {
	const (
		slots  = "Bookings start on the quarter hour."
		hidden = "Booking ids are random strings."
	)

	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, plainStart("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	poster := postedRun(t, st, "BP1", 1, 1, store.RunAsked, "round", nil)
	au := c.newAudit()

	plainTab := func(n int, d protocol.AssumptionDecision) roundState {
		s := tabRound(n, "intake", poster, []string{slotConsequence}, d)
		s.Plain, s.Frame.Assumptions[0].Detail = true, slots

		return s
	}

	c.putRound(ctx, au, "BP1", poster, plainTab(1, convert("A1")))

	// A later round, so the turn-down asks for no more rounds; the
	// responder's round accepts what no person would notice.
	later := tabRound(2, "intake", poster, []string{hidden}, accept("A1"))
	later.Frame.Responder = true
	c.putRound(ctx, au, "BP1", poster, later)

	v, _ := c.tabValidator(bp, intake, 1)
	require.Error(t, submitAssumptions(v, map[string]any{"text": slots}), "the decision the person turned down, without its consequence or a basis")

	v, _ = c.tabValidator(bp, intake, 1)
	require.NoError(t, submitAssumptions(v, map[string]any{"text": hidden}), "one no person would notice needs no basis")

	c.putRound(ctx, au, "BP1", poster, plainTab(3, accept("A1")))

	v, _ = c.tabValidator(bp, intake, 1)
	assert.NoError(t, submitAssumptions(v, map[string]any{"text": slots, "basis": "round3.A1", "consequence": slotConsequence}), "a later acceptance is its basis")
}

func TestPlain_ReasksMustNameATurnedDownAnswer(t *testing.T) {
	const (
		folders     = "Should renamed photos stay in their folders?"
		namesShown  = "Renamed photos sort by the day they were taken."
		namesAnswer = "By date"
	)

	t.Run("plain", func(t *testing.T) {
		var answered atomic.Int32

		names := technicalQ("Names", "How should renamed files be named?", namesAnswer, "By date and place")

		// Q1 and Q2 go to the responder, Q3 to the person, who turns Q1's
		// consequence down and accepts Q2's.
		responder := func(_ context.Context, in stage.Input) (stage.Output, error) {
			switch answered.Add(1) {
			case 1:
				return answeringWith("Use the file's change date", copyDate, in), nil
			case 2:
				return answeringWith(namesAnswer, namesShown, in), nil
			default:
				return answeringWith("Read the camera's date", "", in), nil
			}
		}

		again := photoDates()
		again.Reasks, again.Why = 1, copyDate

		accepted := names
		accepted.Reasks = 2

		experience := photoDates()
		experience.Reasks = 3

		onExperience := experienceQ("Folders", folders, "Yes", "No")
		onExperience.Reasks = 1

		nothing := photoDates()
		nothing.Reasks = 9

		refusals := make(chan error, 8)

		var runs atomic.Int32

		script := dialogScript()
		script["intake.responder"] = responder
		script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
			switch runs.Add(1) {
			case 1:
				_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
					photoDates(), names, experienceQ("Folders", folders, "Yes", "No"),
				}})

				return askedOutput(in), err
			case 2:
				for _, q := range []stage.RoundQuestion{accepted, experience, onExperience, nothing} {
					_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{q}})
					refusals <- err
				}

				_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{again}})

				return askedOutput(in), err
			default:
				// Asked again already.
				_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{again}})
				refusals <- err

				return stopAfterIntake(ctx, in)
			}
		}

		c, _, runner, _, _ := newDialog(t, script)
		driveIn(t, c, plainStart("BP1"))

		person := latestRound(t, c, "BP1", 2)
		require.Len(t, person.Frame.Questions, 1)
		require.Len(t, person.Frame.Assumptions, 2)
		require.Equal(t, map[string]int{person.Frame.Assumptions[0].ID: 1, person.Frame.Assumptions[1].ID: 2}, person.Consequences)

		decide(t, c, "BP1", 2, []protocol.RoundAnswer{pick(person.Frame.Questions[0].Number, 1)},
			convert(person.Frame.Assumptions[0].ID), accept(person.Frame.Assumptions[1].ID))
		c.Wait("BP1")

		close(refusals)

		var n int

		for err := range refusals {
			n++

			require.Error(t, err, "refusal %d", n)
		}

		assert.Equal(t, 5, n)

		rounds, err := c.rounds(context.Background(), "BP1")
		require.NoError(t, err)
		require.Len(t, rounds, 3, "only the re-ask of Q1 is posted")
		assert.Equal(t, map[int]int{4: 1}, rounds[2].Reasks)
		assert.Len(t, inputsOf(runner, "intake.responder"), 3)
	})

	t.Run("off plain", func(t *testing.T) {
		var refused error

		q := roundQ("Dates", "How should each photo's date be found?", "Read the camera's date", "Use the file's change date")
		q.Reasks = 1

		script := dialogScript()
		script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
			_, refused = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{q}})

			return stopAfterIntake(ctx, in)
		}

		c, _, _, rec, _ := newDialog(t, script)
		drive(t, c, "BP1")

		require.Error(t, refused)
		assert.False(t, rec.has(EventRound), "no round was posted")
	})
}

// The dialog pack's intake is under the human policy; the cap is set on it.
func TestPlain_AReAskGoesThroughAtTheQuestionCap(t *testing.T) {
	var answered atomic.Int32

	asked := make(chan error, 1)

	script := dialogScript()
	script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		if answered.Add(1) == 1 {
			return answeringWith("Use the file's change date", copyDate, in), nil
		}

		return answeringWith("Read the camera's date", "", in), nil
	}
	script["intake"] = reaskingIntake(func(ctx context.Context, in stage.Input) (stage.Output, error) {
		_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
			technicalQ("Names", "How should renamed files be named?", "By date", "By date and place"),
		}})
		asked <- err

		return stopAfterIntake(ctx, in)
	})

	c, _, _, _, _ := newDialog(t, script)
	c.opts.Pack.Stages[0].MaxQuestions = 1
	driveIn(t, c, plainStart("BP1"))
	turnDownTheFirstAnswer(t, c)

	rounds, err := c.rounds(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, rounds, 3, "the re-ask is posted at the cap")
	assert.Equal(t, map[int]int{2: 1}, rounds[2].Reasks)

	require.Len(t, asked, 1)
	require.Error(t, <-asked, "the first answer spent the cap, and the re-ask spent none of it")
}

func TestPlain_AFailedReAskSupersedesNothing(t *testing.T) {
	var answered atomic.Int32

	script := dialogScript()
	script["intake"] = reaskingIntake(stopAfterIntake)
	script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		if answered.Add(1) == 1 {
			return answeringWith("Use the file's change date", copyDate, in), nil
		}

		return stage.Output{}, errors.New("the endpoint is unreachable")
	}

	c, st, runner, rec, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))
	turnDownTheFirstAnswer(t, c)

	responders := inputsOf(runner, "intake.responder")
	require.Len(t, responders, 2, "the re-ask went to the responder")
	assert.Contains(t, responders[1].Task, copyDate, "the responder is told the concern")
	assert.NotContains(t, responders[0].Task, copyDate)
	assert.Equal(t, map[int]int{2: 1}, latestRound(t, c, "BP1", 3).Reasks)
	assert.Len(t, rec.all(EventAnswer), 1, "the failed re-ask recorded no answer")

	list, err := c.assumptions(ctx, "BP1")
	require.NoError(t, err)
	assert.Empty(t, byID(list)["Q1"].SupersededBy)

	for _, entry := range assumptionsPayload(list) {
		assert.NotContains(t, entry, "superseded_by")
	}

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	v, _ := c.reaskValidator(bp, intake, 1)
	assert.NoError(t, submitAssumptions(v), "the re-ask was asked")
}

func TestPlain_TheSupersededAnswerIsListedAsSuperseded(t *testing.T) {
	tests := []struct {
		name   string
		stated bool
		id     string
	}{
		{"the answer's own entry", false, "Q1"},
		{"a stated assumption resting on it", true, "A1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var answered atomic.Int32

			var data map[string]any
			if tt.stated {
				data = map[string]any{"assumptions": []any{
					map[string]any{"text": "Each photo is dated by its file's change date.", "basis": "Q1"},
				}}
			}

			script := dialogScript()
			script["intake"] = reaskingIntake(func(context.Context, stage.Input) (stage.Output, error) {
				return done("# Need", data), nil
			})
			script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
				if answered.Add(1) == 1 {
					return answeringWith("Use the file's change date", copyDate, in), nil
				}

				return answeringWith("Read the camera's date", takenDate, in), nil
			}
			script["slicing"] = stopAfterIntake

			c, _, _, rec, _ := newDialog(t, script)
			driveIn(t, c, plainStart("BP1"))
			turnDownTheFirstAnswer(t, c)

			answers := rec.all(EventAnswer)
			require.Len(t, answers, 2)

			reask := fmt.Sprintf("Q%d", answers[1].Payload["number"])

			person := latestRound(t, c, "BP1", 4)
			require.Len(t, person.Frame.Assumptions, 1)
			assert.Equal(t, takenDate, person.Frame.Assumptions[0].Text)
			decide(t, c, "BP1", 4, nil, accept(person.Frame.Assumptions[0].ID))
			c.Wait("BP1")

			list, err := c.assumptions(context.Background(), "BP1")
			require.NoError(t, err)

			entries := map[string]map[string]any{}

			for _, e := range assumptionsPayload(list) {
				entry := e.(map[string]any)
				entries[entry["id"].(string)] = entry
			}

			require.Contains(t, entries, tt.id)
			assert.Equal(t, reask, entries[tt.id]["superseded_by"])

			require.Contains(t, entries, reask)
			assert.NotContains(t, entries[reask], "superseded_by", "the new answer stands")

			// The cards and the playbook description say which answer stands.
			lines := assumptionLines(list, false)
			assert.Contains(t, lineOf(lines, "- "+tt.id+" ("), reask)
			assert.NotContains(t, lineOf(lines, "- "+reask+" ("), tt.id)
		})
	}
}

func TestPlain_AStageWithoutTheAssumptionsCheckWaitsForTheReAsk(t *testing.T) {
	var runs, answered atomic.Int32

	// Slicing checks slices, not assumptions; its person's round still shows
	// the responder's consequence.
	submits := make(chan error, 2)
	sizes := technicalQ("Size", "How many photos does one run rename?", "One folder", "Every folder below it")

	script := dialogScript()
	script["slicing.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		if answered.Add(1) == 1 {
			return answeringWith("Every folder below it", "A run may rename photos you did not mean to touch.", in), nil
		}

		return answeringWith("One folder", "", in), nil
	}
	script["slicing"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		slices := stage.Artifact{Body: "# Slices", Data: oneSlice()}

		switch runs.Add(1) {
		case 1:
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{sizes}})

			return askedOutput(in), err
		case 2:
			submits <- in.Validate(ctx, slices)

			again := sizes
			again.Reasks = 1
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{again}})

			return askedOutput(in), err
		default:
			submits <- in.Validate(ctx, slices)

			return returns("# Slices", oneSlice())(ctx, in)
		}
	}
	script["solution_design"] = stopAfterIntake

	c, _, _, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))
	turnDownTheFirstAnswer(t, c)

	require.Error(t, receiveErr(t, submits), "Q1 is not asked again yet")
	require.NoError(t, receiveErr(t, submits), "the re-ask was asked")
	assert.Equal(t, map[int]int{2: 1}, latestRound(t, c, "BP1", 3).Reasks)
}

// receiveErr waits for one result on ch, failing the test at a deadline.
func receiveErr(t *testing.T, ch <-chan error) error {
	t.Helper()

	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a result")
	}

	return nil
}

// datesDesign is one slice whose decisions, each on how a photo is dated,
// rest on bases in turn.
func datesDesign(bases ...string) map[string]any {
	decisions := make([]any, 0, len(bases))

	for i, basis := range bases {
		decisions = append(decisions, map[string]any{
			"id": fmt.Sprintf("DEC%d", i+1), "text": "how each photo is dated", "chosen": "the camera's date",
			"rejected": []any{map[string]any{"option": "the file's change date", "why": "copying a photo changes it"}}, "basis": basis,
		})
	}

	return map[string]any{"slices": []any{map[string]any{
		"id": "SL1", "projects": []any{"alpha"}, "decisions": decisions, "contracts": []any{},
	}}}
}

func TestPlain_ADesignRestsOnTheAnswerThatStands(t *testing.T) {
	var answered atomic.Int32

	script := dialogScript()
	script["solution_design.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		if answered.Add(1) == 1 {
			return answeringWith("Use the file's change date", copyDate, in), nil
		}

		return answeringWith("Read the camera's date", "", in), nil
	}
	script["solution_design"] = reaskingIntake(func(context.Context, stage.Input) (stage.Output, error) {
		return done("# Design", datesDesign("Q2")), nil
	})
	script["grounding"] = stopAfterIntake

	c, st, _, _, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))
	turnDownTheFirstAnswer(t, c)
	require.Equal(t, map[int]int{2: 1}, latestRound(t, c, "BP1", 3).Reasks, "Q2 asked Q1 again")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	submit := func(data map[string]any) error {
		v, _ := c.decisionsValidator(bp, "solution_design")

		return v(ctx, stage.Artifact{Body: "# Design", Data: data})
	}

	statedOnTheOld := datesDesign("Q2")
	statedOnTheOld["assumptions"] = []any{map[string]any{"text": "Photos without a camera date keep their old names.", "basis": "Q1"}}

	require.NoError(t, submit(datesDesign("Q2")), "the decisions rest on the answer that stands, which is the one owed")
	require.Error(t, submit(datesDesign("Q1", "Q2")), "no decision rests on the answer the person turned down")
	assert.NoError(t, submit(statedOnTheOld), "a stated assumption may rest on it, and the sign-off lists it superseded")
}

// supersededIntake asks photoDates, asks it again once the person turned the
// first answer's consequence down, then asks it a third time without reasks,
// sending what that ask returned on refused, and asks names; it then submits.
func supersededIntake(refused chan<- error, names stage.RoundQuestion) func(context.Context, stage.Input) (stage.Output, error) {
	var later atomic.Int32

	return reaskingIntake(func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if later.Add(1) > 1 {
			return done("# Need", nil), nil
		}

		_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{photoDates()}})
		refused <- err

		_, err = in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{names}})

		return askedOutput(in), err
	})
}

func TestPlain_LaterRunsAreToldWhichAnswerStands(t *testing.T) {
	var answered atomic.Int32

	refused := make(chan error, 1)
	names := technicalQ("Names", "How should renamed files be named?", "By date", "By date and place")

	script := dialogScript()
	script["intake"] = supersededIntake(refused, names)
	script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		switch answered.Add(1) {
		case 1:
			return answeringWith("Use the file's change date", copyDate, in), nil
		case 2:
			return answeringWith("Read the camera's date", "", in), nil
		default:
			return answeringWith("By date", "", in), nil
		}
	}
	script["slicing"] = stopAfterIntake

	c, st, runner, _, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))
	turnDownTheFirstAnswer(t, c)

	var asked error

	select {
	case asked = <-refused:
	default:
		t.Fatal("the third run never asked")
	}

	require.Error(t, asked, "the answered question is not asked a third time")
	assert.Contains(t, asked.Error(), "Q2", "the newest answer is cited")
	assert.Contains(t, asked.Error(), "Read the camera's date")
	assert.NotContains(t, asked.Error(), "Use the file's change date", "not the superseded one")

	responders := inputsOf(runner, "intake.responder")
	require.Len(t, responders, 3)
	assert.Contains(t, blockOf(responders[2].Task, "Q1"), "Q2", "the responder is told Q2 supersedes Q1")
	assert.NotContains(t, blockOf(responders[2].Task, "Q2"), "Q1")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	intake, ok := c.stepAt("intake")
	require.True(t, ok)

	var b strings.Builder
	require.NoError(t, c.writeAnswered(ctx, &b, bp, intake))
	assert.Contains(t, blockOf(b.String(), "Q1"), "Q2", "a re-run of the stage is told Q2 supersedes Q1")

	later := inputsOf(runner, "slicing")
	require.NotEmpty(t, later)
	assert.Contains(t, lineOf(later[0].Task, "- Q1 ("), "Q2", "a later stage is told Q2 supersedes Q1")
	assert.NotContains(t, lineOf(later[0].Task, "- Q2 ("), "Q1")
}

// lineOf is the line of text that starts with prefix, "" for none.
func lineOf(text, prefix string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}

	return ""
}

func TestPlain_AReAskTurnedDownAgainIsAskedAgainItself(t *testing.T) {
	var runs, answered atomic.Int32

	refused := make(chan error, 1)

	script := dialogScript()
	script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		switch answered.Add(1) {
		case 1:
			return answeringWith("Use the file's change date", copyDate, in), nil
		case 2:
			return answeringWith("Read the camera's date", "Photos without a camera date are left unnamed.", in), nil
		default:
			return answeringWith("Read the camera's date, else the change date", "", in), nil
		}
	}
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		ask := func(reasks int) error {
			q := photoDates()
			q.Reasks = reasks
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{q}})

			return err
		}

		switch runs.Add(1) {
		case 1:
			return askedOutput(in), ask(0)
		case 2:
			return askedOutput(in), ask(1)
		case 3:
			refused <- ask(1)

			return askedOutput(in), ask(2)
		default:
			return stopAfterIntake(ctx, in)
		}
	}

	c, _, runner, rec, _ := newDialog(t, script)

	driveIn(t, c, plainStart("BP1"))
	turnDownTheFirstAnswer(t, c)

	person := latestRound(t, c, "BP1", 4)
	require.Equal(t, map[string]int{person.Frame.Assumptions[0].ID: 2}, person.Consequences)
	decide(t, c, "BP1", 4, nil, convert(person.Frame.Assumptions[0].ID))
	c.Wait("BP1")

	select {
	case err := <-refused:
		require.ErrorContains(t, err, "reasks 2", "the refusal points at the newest answer")
	default:
		t.Fatal("the third run never asked")
	}

	assert.Equal(t, map[int]int{3: 2}, latestRound(t, c, "BP1", 5).Reasks)
	assert.Len(t, inputsOf(runner, "intake.responder"), 3)

	answers := rec.all(EventAnswer)
	require.Len(t, answers, 3)
	assert.Equal(t, 2, answers[2].Payload["supersedes"])
}

func TestPlain_AReAskUnderTheAutoPolicyCarriesTheConcern(t *testing.T) {
	var runs, answered atomic.Int32

	script := dialogScript()
	script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		switch answered.Add(1) {
		case 1:
			return answeringWith("Use the file's change date", copyDate, in), nil
		case 2:
			return answeringWith("Yes", "", in), nil
		default:
			return answeringWith("Read the camera's date", "", in), nil
		}
	}
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch runs.Add(1) {
		case 1:
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{
				photoDates(), experienceQ("Folders", "Should renamed photos stay in their folders?", "Yes", "No"),
			}})

			return askedOutput(in), err
		case 2:
			// No why: only the controller can carry the concern.
			again := photoDates()
			again.Reasks = 1
			_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{again}})

			return askedOutput(in), err
		default:
			return stopAfterIntake(ctx, in)
		}
	}

	c, _, runner, rec, _ := newDialog(t, script)
	ctx := context.Background()

	driveIn(t, c, plainStart("BP1"))

	// The person turns the answer's consequence down and leaves the
	// experience question open; the switch hands that to the responder.
	person := latestRound(t, c, "BP1", 2)
	require.Equal(t, map[string]int{person.Frame.Assumptions[0].ID: 1}, person.Consequences)
	require.NoError(t, c.Reply(ctx, "BP1", protocol.RoundReply{PromptID: roundPromptID(2), Decisions: []protocol.AssumptionDecision{convert(person.Frame.Assumptions[0].ID)}}, "m-tab"))
	c.Wait("BP1")

	require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
	c.Wait("BP1")

	reask := latestRound(t, c, "BP1", 3)
	assert.False(t, reask.Plain, "asked under the auto policy")
	assert.True(t, reask.Frame.Responder)
	assert.Equal(t, map[int]int{3: 1}, reask.Reasks)

	responders := inputsOf(runner, "intake.responder")
	require.Len(t, responders, 3)
	assert.Contains(t, responders[2].Task, copyDate, "the responder is told the concern")

	answers := rec.all(EventAnswer)
	require.Len(t, answers, 3)
	assert.Equal(t, 1, answers[2].Payload["supersedes"])
}

// consequenceDesign is a solution design fake: a fresh run asks question as
// an experience question with technical on its tab, carrying consequence
// when it is set; its continuation states technical with the acceptance's
// tag (the person's round's), carrying consequence when it is set.
func consequenceDesign(question, technical, consequence, tag string) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History == nil {
			rd := stage.Round{
				Questions:   []stage.RoundQuestion{experienceQ("Choice", question, "Yes", "No")},
				Assumptions: []string{technical},
			}
			if consequence != "" {
				rd.Consequences = []string{consequence}
			}

			_, err := in.Interviewer.Ask(ctx, rd)

			return askedOutput(in), err
		}

		entry := map[string]any{"text": technical, "basis": tag}
		if consequence != "" {
			entry["consequence"] = consequence
		}

		data := goodDesign("assumption")
		data["assumptions"] = []any{entry}

		return done("# Design", data), nil
	}
}

// answerFirstOpenRound decides the round the view shows open: option 1 for
// every question and accept for every entry. It then waits.
func answerFirstOpenRound(t *testing.T, c *Controller, id string) {
	t.Helper()

	view, err := c.View(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, view.Open)

	var n int

	_, err = fmt.Sscanf(view.Open.PromptID, "round:%d", &n)
	require.NoError(t, err)

	round := latestRound(t, c, id, n)

	var answers []protocol.RoundAnswer
	for _, q := range round.Frame.Questions {
		answers = append(answers, pick(q.Number, 1))
	}

	var decisions []protocol.AssumptionDecision
	for _, a := range round.Frame.Assumptions {
		decisions = append(decisions, accept(a.ID))
	}

	decide(t, c, id, n, answers, decisions...)
	c.Wait(id)
}

func TestPlain_TheReviewerGetsTheAnswersConsequences(t *testing.T) {
	const (
		question    = "Should every member see the others' phone numbers?"
		technical   = "Phone numbers are readable by the board role alone."
		consequence = "Only the club's board can see members' phone numbers."
		answered    = "Two board members can change the list at the same time."
		unstated    = "Members are listed in the order they joined."
		unrestated  = "New members always appear at the bottom of the list."
	)

	// The responder round is round 1, so the person's round is round 2.
	design := consequenceDesign(question, technical, consequence, "round2.A1")
	script := dialogScript()
	script["solution_design.responder"] = respondsWithConsequence("A database", answered)
	script["solution_design"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if in.History != nil {
			return design(ctx, in)
		}

		_, err := in.Interviewer.Ask(ctx, stage.Round{
			Questions: []stage.RoundQuestion{
				experienceQ("Privacy", question, "Every member", "The board only"),
				technicalQ("Records", "Where are the member records kept?", "One shared file", "A database"),
			},
			Assumptions: []string{technical, unstated}, Consequences: []string{consequence, unrestated},
		})

		return askedOutput(in), err
	}

	c, _, runner, _, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))

	// The person turns the second entry down, so the design does not state it.
	person := latestRound(t, c, "BP1", 2)
	require.Len(t, person.Frame.Questions, 1)
	require.Len(t, person.Frame.Assumptions, 3)
	require.Equal(t, unrestated, person.Frame.Assumptions[1].Text)

	tab := person.Frame.Assumptions
	decide(t, c, "BP1", 2, []protocol.RoundAnswer{pick(person.Frame.Questions[0].Number, 1)}, accept(tab[0].ID), convert(tab[1].ID), accept(tab[2].ID))
	c.Wait("BP1")

	reviews := inputsOf(runner, "solution_design.review")
	require.NotEmpty(t, reviews)
	assert.Contains(t, reviews[0].Task, answered, "an answer's consequence reaches the reviewer only through the plain section")
	assert.Contains(t, reviews[0].Task, unrestated, "so does a consequence the person was shown that the design does not state")
}

func TestPlain_TheSignoffCarriesAPlainSummary(t *testing.T) {
	script := dialogScript()
	script["intake"] = returns("# Need", map[string]any{"outcomes": []any{
		map[string]any{"id": "O1", "text": "Plan a week of dinners in under five minutes."},
		map[string]any{"id": "O2", "text": "Get one shopping list for the whole week."},
	}})
	script["slicing"] = returns("# Slices", map[string]any{"slices": []any{
		map[string]any{"id": "SL1", "title": "The weekly planner", "value": "pick seven dinners from saved recipes", "projects": []any{"alpha"}},
	}})
	script["solution_design"] = consequenceDesign("Should two people in one household share their recipes?", "Recipes are saved per household.", "Everyone in a household sees the same recipes.", "round1.A1")

	c, _, _, rec, _ := newDialog(t, script)
	driveIn(t, c, plainStart("BP1"))
	answerFirstOpenRound(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)

	sum, ok := so.Payload["plain_summary"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"The weekly planner: pick seven dinners from saved recipes"}, anyList(sum["build"]))
	assert.Equal(t, []any{"Plan a week of dinners in under five minutes.", "Get one shopping list for the whole week."}, anyList(sum["can_do"]))
	require.NotEmpty(t, anyList(sum["accepted"]))

	ids := map[any]bool{}

	for _, a := range anyList(so.Payload["assumptions"]) {
		if entry := a.(map[string]any); entry["consequence_accepted"] != nil {
			ids[entry["id"]] = true
		}
	}

	for _, a := range anyList(sum["accepted"]) {
		assert.True(t, ids[a.(map[string]any)["id"]], "every accepted entry links to an assumption entry")
	}
}

func TestPlain_AChangedDecisionIsFlagged(t *testing.T) {
	const (
		shown = "Everyone in a household sees the same recipes."
		first = "Recipes are saved per household."
	)

	for name, tc := range map[string]struct {
		restated    string
		wantChanged bool
	}{
		"restated":  {"Recipes are saved per person.", true},
		"unchanged": {"recipes are saved per household", false},
	} {
		t.Run(name, func(t *testing.T) {
			script := dialogScript()
			script["solution_design"] = consequenceDesign("Should two people in one household share their recipes?", first, shown, "round1.A1")

			c, st, _, _, _ := newDialog(t, script)
			driveIn(t, c, plainStart("BP1"))
			answerFirstOpenRound(t, c, "BP1")

			putData(t, st, "solution_design", map[string]any{"assumptions": []any{
				map[string]any{"text": tc.restated, "basis": "round1.A1", "consequence": shown},
			}})

			ctx := context.Background()
			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			list, err := c.assumptions(ctx, "BP1")
			require.NoError(t, err)

			sum := c.plainSummary(ctx, bp, list, "Title")
			accepted := anyList(sum["accepted"])
			require.Len(t, accepted, 1)

			entry := accepted[0].(map[string]any)
			assert.Equal(t, shown, entry["text"])

			if tc.wantChanged {
				assert.Equal(t, true, entry["changed"])
			} else {
				assert.NotContains(t, entry, "changed")
			}
		})
	}
}

func TestPlain_AnAcceptedConsequenceIsFlaggedWhenALaterVersionRevisesIt(t *testing.T) {
	for name, tc := range map[string]struct {
		viaAnswer   bool
		restated    string
		revised     bool
		later       string
		wantChanged bool
	}{
		"its first statement words it otherwise":               {restated: recipesPrivate},
		"an answer's, first stated otherwise":                  {viaAnswer: true, restated: recipesPrivate},
		"a later version revises the consequence":              {restated: recipesShared, revised: true, later: recipesPrivate, wantChanged: true},
		"a later version revises the consequence on an answer": {viaAnswer: true, restated: recipesShared, revised: true, later: recipesPrivate, wantChanged: true},
		"a later version restates it unchanged":                {restated: recipesShared, revised: true, later: "everyone in a household sees the same recipes"},
		"a later version drops the consequence":                {restated: recipesShared, revised: true},
		"a later version adds a consequence":                   {revised: true, later: recipesPrivate},
	} {
		t.Run(name, func(t *testing.T) {
			c := acceptedThenRestated(t, tc.viaAnswer, tc.restated)
			ctx := context.Background()

			if tc.revised {
				list, err := c.assumptions(ctx, "BP1")
				require.NoError(t, err)

				first := byID(list)["A1"]
				putData(t, c.opts.Store, "intake", map[string]any{"assumptions": []any{
					map[string]any{"text": first.Text, "basis": first.Basis, "consequence": tc.later},
				}})
			}

			bp, err := c.opts.Store.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			list, err := c.assumptions(ctx, "BP1")
			require.NoError(t, err)

			accepted := anyList(c.plainSummary(ctx, bp, list, "Title")["accepted"])
			require.Len(t, accepted, 1)

			entry := accepted[0].(map[string]any)
			assert.Equal(t, recipesShared, entry["text"], "the sentence the person accepted")

			if tc.wantChanged {
				assert.Equal(t, true, entry["changed"])
			} else {
				assert.NotContains(t, entry, "changed")
			}
		})
	}
}

func TestPlain_TheSummaryFollowsTheRecordsRules(t *testing.T) {
	ctx := context.Background()

	t.Run("a superseded answer is changed and the order is the list's", func(t *testing.T) {
		c, _, _, _, _ := newDialog(t, dialogScript())

		sum := c.plainSummary(ctx, store.Blueprint{ID: "BP9"}, []assumption{
			{ID: "A2", Consequence: "Second.", ConsequenceAccepted: 1},
			{ID: "A1", Consequence: "First.", ConsequenceAccepted: 1, SupersededBy: "Q4"},
			{ID: "A3", Consequence: "Not accepted."},
		}, "Title")

		accepted := anyList(sum["accepted"])
		require.Len(t, accepted, 2)
		assert.Equal(t, "A2", accepted[0].(map[string]any)["id"])
		assert.NotContains(t, accepted[0], "changed")
		assert.Equal(t, "A1", accepted[1].(map[string]any)["id"])
		assert.Equal(t, true, accepted[1].(map[string]any)["changed"])
	})

	t.Run("an accepted sentence is listed once, changed when any entry carrying it is", func(t *testing.T) {
		c, _, _, _, _ := newDialog(t, dialogScript())

		sum := c.plainSummary(ctx, store.Blueprint{ID: "BP9"}, []assumption{
			{ID: "A1", Basis: "Q2", Consequence: "Everyone at home edits one list.", ConsequenceAccepted: 3},
			{ID: "A2", Basis: "Q2", Consequence: "everyone at home edits one list", ConsequenceAccepted: 3, SupersededBy: "Q5"},
			{ID: "A3", Basis: "Q4", Consequence: "Adding items needs internet.", ConsequenceAccepted: 3},
		}, "Title")

		accepted := anyList(sum["accepted"])
		require.Len(t, accepted, 2)
		assert.Equal(t, "A1", accepted[0].(map[string]any)["id"])
		assert.Equal(t, true, accepted[0].(map[string]any)["changed"])
		assert.Equal(t, "A3", accepted[1].(map[string]any)["id"])
		assert.NotContains(t, accepted[1], "changed")
	})

	t.Run("without slicing the build is the title", func(t *testing.T) {
		c, _, _, _, _ := newDialog(t, dialogScript())

		sum := c.plainSummary(ctx, store.Blueprint{ID: "BP9"}, nil, "Recipe planner")
		assert.Equal(t, []any{"Recipe planner"}, anyList(sum["build"]))
		assert.Empty(t, anyList(sum["can_do"]))
		assert.Empty(t, anyList(sum["accepted"]))
	})

	t.Run("a slice without a value gives its title and one without a title is skipped", func(t *testing.T) {
		c, st, _, _, _ := newDialog(t, dialogScript())
		driveIn(t, c, plainStart("BP1"))

		putData(t, st, "slicing", map[string]any{"slices": []any{
			map[string]any{"id": "SL1", "title": "Weekly planner", "value": ""},
			map[string]any{"id": "SL2", "title": "", "value": "orphan value"},
			map[string]any{"id": "SL3", "title": "Shopping list", "value": "one list for the week"},
		}})

		bp, err := st.GetBlueprint(ctx, "BP1")
		require.NoError(t, err)

		sum := c.plainSummary(ctx, bp, nil, "Title")
		assert.Equal(t, []any{"Weekly planner", "Shopping list: one list for the week"}, anyList(sum["build"]))
	})

	t.Run("an accepted answer that is not superseded reads unchanged", func(t *testing.T) {
		c, _, _, _, _ := newDialog(t, dialogScript())

		sum := c.plainSummary(ctx, store.Blueprint{ID: "BP9"}, []assumption{
			{ID: "Q1", Stage: "intake", Basis: "Q1", Consequence: "Kept.", ConsequenceAccepted: 1},
		}, "Title")

		accepted := anyList(sum["accepted"])
		require.Len(t, accepted, 1)
		assert.NotContains(t, accepted[0], "changed")
	})

	for name, tc := range map[string]struct {
		versions    [][]any
		text        string
		wantChanged bool
	}{
		"the first version stating the basis decides, not the latest": {
			versions:    [][]any{{"Other."}, {statedUnderTag("Recipes are saved per household.")}, {statedUnderTag("Recipes are saved per person.")}},
			text:        "Recipes are saved per person.",
			wantChanged: true,
		},
		"a basis first stated in a later version, equal to the text, is unchanged": {
			versions: [][]any{{"Other."}, {statedUnderTag("Recipes are saved per household.")}},
			text:     "Recipes are saved per household.",
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, st, _, _, _ := newDialog(t, dialogScript())
			driveIn(t, c, plainStart("BP1"))

			for _, v := range tc.versions {
				putData(t, st, "solution_design", map[string]any{"assumptions": v})
			}

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			sum := c.plainSummary(ctx, bp, []assumption{
				{ID: "A1", Stage: "solution_design", Text: tc.text, Basis: "round1.A1", Consequence: "Shared.", ConsequenceAccepted: 1},
			}, "Title")

			accepted := anyList(sum["accepted"])
			require.Len(t, accepted, 1)

			if tc.wantChanged {
				assert.Equal(t, true, accepted[0].(map[string]any)["changed"])
			} else {
				assert.NotContains(t, accepted[0], "changed")
			}
		})
	}
}

func statedUnderTag(text string) map[string]any {
	return map[string]any{"text": text, "basis": "round1.A1"}
}

func TestPlain_AnAutoModeHasConsequencesButNothingAccepted(t *testing.T) {
	script := dialogScript()
	script["intake"] = splitIntake(make(chan string, 1))
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, _, _, rec, _ := newDialog(t, script)
	in := plainStart("BP1")
	in.Mode = "auto"
	driveIn(t, c, in)

	for _, r := range mustRounds(t, c, "BP1") {
		assert.False(t, r.Plain, "no round is put to a person")
	}

	assert.False(t, rec.has(EventGate))

	so, ok := rec.first(EventSignOff)
	if !ok {
		so, ok = rec.first(EventSignOffAuto)
	}

	require.True(t, ok)

	var consequences int

	for _, a := range anyList(so.Payload["assumptions"]) {
		entry := a.(map[string]any)

		assert.NotContains(t, entry, "consequence_accepted")

		if entry["consequence"] != nil {
			consequences++
		}
	}

	assert.Positive(t, consequences)

	sum, ok := so.Payload["plain_summary"].(map[string]any)
	require.True(t, ok)
	assert.Empty(t, anyList(sum["accepted"]))
}

func TestPlain_OffTheSignoffHasNoSummary(t *testing.T) {
	script := dialogScript()
	script["intake"] = splitIntake(make(chan string, 1))
	script["intake.responder"] = respondsWithConsequence("On a shared server", listConsequence)

	c, _, _, rec, _ := newDialog(t, script)
	driveIn(t, c, autoStart("BP1"))

	so, ok := rec.first(EventSignOff)
	if !ok {
		so, ok = rec.first(EventSignOffAuto)
	}

	require.True(t, ok, "%v", rec.kinds())
	assert.NotContains(t, so.Payload, "plain_summary")
}

func mustRounds(t *testing.T, c *Controller, id string) []roundState {
	t.Helper()

	rounds, err := c.rounds(context.Background(), id)
	require.NoError(t, err)

	return rounds
}

func TestPlain_AnEndedPipelineFoldsItsTechnicalReason(t *testing.T) {
	ended := func(t *testing.T, plain bool) protocol.BlueprintView {
		t.Helper()

		script := happyScript()
		script["readiness"] = returns("# Not ready", readiness(false))

		c, _, _, _, _ := newPipeline(t, script)
		// Readiness's spent rounds end only a pack with no stage that checks cards.
		withoutCardsCheck(c.opts.Pack)

		in := startInput("BP1")
		in.PlainLanguage = plain
		driveIn(t, c, in)

		view, err := c.View(context.Background(), "BP1")
		require.NoError(t, err)
		require.Equal(t, viewEnded, view.State)

		return view
	}

	technical, plain := ended(t, false), ended(t, true)

	require.NotEmpty(t, technical.EndReason)
	assert.Empty(t, technical.EndDetail, "off plain nothing is folded")
	assert.Equal(t, technical.EndReason, plain.EndDetail, "the technical reason is folded")
	assert.NotEmpty(t, plain.EndReason)
	assert.NotEqual(t, plain.EndDetail, plain.EndReason)
}

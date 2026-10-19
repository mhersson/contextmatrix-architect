package blueprint

import (
	"context"
	"fmt"
	"testing"

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

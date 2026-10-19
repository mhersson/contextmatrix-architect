package blueprint

import (
	"context"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
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

package blueprint

import (
	"context"
	"fmt"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		text string
		name string
		args []string
	}{
		{text: "abort", name: CommandAbort},
		{text: "  Export ", name: CommandExport},
		{text: "gate intake auto", name: CommandGate, args: []string{"intake", "auto"}},
		{text: "Gate Intake HUMAN", name: CommandGate, args: []string{"intake", "human"}},
		{text: "gate intake"},
		{text: "gate intake sometimes"},
		{text: "Gate it behind a feature flag"},
		{text: "Abort the old plan"},
		{text: "export the data as csv"},
		{text: "the design"},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			name, args := ParseCommand(tt.text)
			assert.Equal(t, tt.name, name)
			assert.Equal(t, tt.args, args)
		})
	}
}

func TestPolicy_FirstMatchWins(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		replies [][2]string // gate replies in order: stage, policy
		want    string
	}{
		{name: "the pack", mode: protocol.ArchitectModeDefault, want: pack.PolicyHuman},
		{name: "the auto mode", mode: protocol.ArchitectModeAuto, want: pack.PolicyAuto},
		{name: "yolo", mode: protocol.ArchitectModeYolo, want: pack.PolicyAuto},
		{name: "a reply beats the pack", mode: protocol.ArchitectModeDefault, replies: [][2]string{{"intake", "auto"}}, want: pack.PolicyAuto},
		{name: "a reply beats the mode", mode: protocol.ArchitectModeAuto, replies: [][2]string{{"intake", "human"}}, want: pack.PolicyHuman},
		{name: "the latest reply wins", mode: protocol.ArchitectModeDefault, replies: [][2]string{{"intake", "auto"}, {"intake", "human"}}, want: pack.PolicyHuman},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _ := newController(t, artifactRun("# Need"))
			ctx := context.Background()

			in := startInput("BP1")
			in.Mode = tt.mode
			require.NoError(t, c.Start(ctx, in))

			for i, r := range tt.replies {
				require.NoError(t, c.SetPolicy(ctx, "BP1", r[0], r[1], fmt.Sprintf("m%d", i)))
			}

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			got, err := c.policy(ctx, bp, "intake")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSetPolicy_RefusesTheSignoffAndUnknownStages(t *testing.T) {
	c, _, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, c.SetPolicy(ctx, "BP1", "signoff", "auto", "m1"))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "nosuch", "auto", "m2"))

	events := rec.all(EventPolicy)
	require.Len(t, events, 2)

	for _, e := range events {
		refused, _ := e.Payload["refused"].(string)
		assert.NotEmpty(t, refused)
		assert.Equal(t, false, e.Payload["during_run"])
	}

	unknown, _ := events[1].Payload["refused"].(string)
	assert.Contains(t, unknown, "intake", "an unknown stage's refusal names the stages a reply may name")

	require.ErrorIs(t, c.SetPolicy(ctx, "BP2", "intake", "auto", "m3"), ErrNotFound, "a session that is not attached")
}

func TestSetPolicy_ARefusedReplyNeverBecomesAPolicy(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, c.SetPolicy(ctx, "BP1", "design", "human", "m1"))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "nosuch", "human", "m2"))

	events := rec.all(EventPolicy)
	require.Len(t, events, 2)

	humanSourced, _ := events[0].Payload["refused"].(string)
	unknown, _ := events[1].Payload["refused"].(string)

	assert.NotEmpty(t, humanSourced)
	assert.NotEqual(t, humanSourced, unknown, "a stage that exists is not reported as unknown")

	for _, text := range []string{humanSourced, unknown} {
		for _, id := range c.modelStages() {
			assert.Contains(t, text, id, "a refusal names the stages a reply may name")
		}
	}

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	got, err := c.policy(ctx, bp, "design")
	require.NoError(t, err)
	assert.Equal(t, pack.PolicyAuto, got, "nothing was stored for it")
}

func TestAsk_ACancelledRunRecordsNoQuestion(t *testing.T) {
	c, _, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	require.NoError(t, c.Start(ctx, in))

	bp, err := c.opts.Store.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	a := &asker{c: c, bp: bp, stage: "intake", run: &run{}}
	_, err = a.Ask(cancelled, stage.Question{Text: "Which port?"})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, rec.has(EventQuestion))
}

func TestSetPolicy_AnOpenQuestionKeepsItsResponder(t *testing.T) {
	second := make(chan error, 1)

	c, _, _, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		first, err := in.Asker.Ask(ctx, stage.Question{Text: "Helm or kustomize?"})
		if err != nil {
			return stage.Output{}, err
		}

		_, err = in.Asker.Ask(ctx, stage.Question{Text: "Which cluster?"})
		second <- err

		return artifactOutput("# Need\n\n"+first, in), nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	rec.waitFor(t, EventQuestion)

	require.NoError(t, c.SetPolicy(ctx, "BP1", "intake", "auto", "m2"))

	change, ok := rec.first(EventPolicy)
	require.True(t, ok)
	assert.Equal(t, "question", change.Payload["open"])
	assert.Equal(t, true, change.Payload["during_run"])

	require.NoError(t, c.Message(ctx, "BP1", "helm", "m3"))

	select {
	case err := <-second:
		require.ErrorIs(t, err, errNoResponder, "the next question follows the new policy")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the stage never asked its second question")
	}

	c.Wait("BP1")

	questions := rec.all(EventQuestion)
	require.Len(t, questions, 2)
	assert.Equal(t, store.ResponderHuman, questions[0].Payload["responder"])
	assert.Equal(t, store.ResponderAuto, questions[1].Payload["responder"])
}

func TestAsk_AnAutoPolicyTellsTheStageToDecide(t *testing.T) {
	asked := make(chan error, 1)

	c, st, _, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		_, err := in.Asker.Ask(ctx, stage.Question{Text: "Helm or kustomize?"})
		asked <- err

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.ErrorIs(t, <-asked, errNoResponder)
	assert.False(t, rec.has(EventAnswer))

	q, ok := rec.first(EventQuestion)
	require.True(t, ok)
	assert.Equal(t, store.ResponderAuto, q.Payload["responder"])
	assert.Equal(t, "intake", q.Payload["stage"])

	_, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err, "the stage goes on and submits")
}

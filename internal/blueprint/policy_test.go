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

	require.NoError(t, c.SetPolicy(ctx, "BP1", "nosuch", "human", "m1"))

	events := rec.all(EventPolicy)
	require.Len(t, events, 1)

	unknown, _ := events[0].Payload["refused"].(string)
	assert.NotEmpty(t, unknown)

	for _, id := range c.stageIDs() {
		assert.Contains(t, unknown, id, "a refusal names the stages a reply may name")
	}

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	got, err := c.policy(ctx, bp, "nosuch")
	require.NoError(t, err)
	assert.Equal(t, pack.PolicyAuto, got, "nothing was stored for it")
}

func TestAsk_ACancelledRunRecordsNoRound(t *testing.T) {
	c, _, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	require.NoError(t, c.Start(ctx, in))

	bp, err := c.opts.Store.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	intake, ok := c.opts.Pack.Stage("intake")
	require.True(t, ok)

	iv := &interviewer{c: c, bp: bp, st: intake, r: &run{}}
	_, err = iv.Ask(cancelled, stage.Round{Questions: []stage.RoundQuestion{roundQ("Port", "Which port?")}})
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, rec.has(EventRound))
}

func TestSetPolicy_AnOpenRoundKeepsItsResponder(t *testing.T) {
	c, st, runner, rec := newController(t, asks("# Need", roundQ("Deploy", "Helm or Kustomize?", "Helm", "Kustomize")))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.NoError(t, c.SetPolicy(ctx, "BP1", "intake", "auto", "m2"))

	change, ok := rec.first(EventPolicy)
	require.True(t, ok)
	assert.Equal(t, "round", change.Payload["open"])
	assert.Equal(t, false, change.Payload["during_run"])

	answerRound(t, c, "BP1", 1, true, pick(1, 1))
	c.Wait("BP1")

	require.Equal(t, 2, runner.calls(), "the round closes and its stage goes on")
	assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)

	qs, err := st.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, qs, 1)
	assert.Equal(t, store.ResponderHuman, qs[0].Responder, "the human the round was put to answered it")
}

func TestGatePlan_NamesWhereAGateFolds(t *testing.T) {
	c, _, _, rec, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	rec.waitFor(t, EventGatePlan)

	plan, ok := rec.first(EventGatePlan)
	require.True(t, ok)

	var slicing map[string]any

	for _, s := range plan.Payload["stages"].([]any) {
		if m := s.(map[string]any); m["stage"] == "slicing" {
			slicing = m
		}
	}

	require.NotNil(t, slicing)
	assert.Equal(t, "solution_design", slicing["folds_into"])
	assert.Equal(t, "slices", slicing["fold_field"])
	assert.EqualValues(t, 1, slicing["fold_max"])
}

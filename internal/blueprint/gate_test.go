package blueprint

import (
	"context"
	"fmt"
	"path/filepath"
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

// newGated builds a controller on the gated test pack: intake, then shape
// behind a gate, then plan, with no stage that checks cards.
func newGated(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	return newGatedOn(t, st, script)
}

// newGatedOn is newGated on an existing store, as a restart is.
func newGatedOn(t *testing.T, st *store.Store, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	pk, err := pack.LoadDir("testdata/gated")
	require.NoError(t, err)

	runner := &fakeRunner{fn: scripted(script)}
	rec := newRecorder()
	c := New(Options{
		Store: st, Pack: pk, Runner: runner, MaxRunCostUSD: 2, MaxTurns: 10,
		Sink: rec.sink, HarnessLine: rec.line,
	})

	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	return c, st, runner, rec
}

func gatedScript() stageScript {
	return stageScript{
		"intake":       returns("# Need", map[string]any{"slices": 1}),
		"shape":        returns("# Shape", nil),
		"shape.review": returns("# Review", map[string]any{"score": float64(5), "findings": []any{}}),
		"plan":         returns("# Plan", nil),
	}
}

// startGated starts BP1 in mode, states the need and waits until nothing
// runs.
func startGated(t *testing.T, c *Controller, mode string) {
	t.Helper()

	ctx := context.Background()
	in := startInput("BP1")
	in.Mode = mode
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
}

// stagesRun lists the stage of every stage run the fake runner was asked
// for; reviewer and responder runs, whose stage names hold a dot, are left
// out.
func stagesRun(r *fakeRunner) []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.inputs))

	for _, in := range r.inputs {
		if !strings.Contains(in.Stage, ".") {
			out = append(out, in.Stage)
		}
	}

	return out
}

// inputsOf returns the input of every run of stageID, oldest first.
func inputsOf(r *fakeRunner, stageID string) []stage.Input {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []stage.Input

	for _, in := range r.inputs {
		if in.Stage == stageID {
			out = append(out, in)
		}
	}

	return out
}

func TestMatchAction(t *testing.T) {
	actions := append(stageGateActions("solution_design"),
		gateAction{Label: "Reject slicing", Reply: "reject slicing", Feedback: true, Decision: store.GateReject, Target: "slicing"})

	tests := []struct {
		text     string
		reply    string // "" when no action matches
		feedback string
	}{
		{text: "approve", reply: "approve"},
		{text: "  APPROVE ", reply: "approve"},
		{text: "approve it"},
		{text: "approved"},
		{text: "reject"},
		{text: "reject   "},
		{text: "rejected the plan"},
		{text: "reject make it smaller", reply: "reject", feedback: "make it smaller"},
		{text: "Reject\tmake it smaller", reply: "reject", feedback: "make it smaller"},
		{text: "reject\n- split D1\n- add tests", reply: "reject", feedback: "- split D1\n- add tests"},
		{text: "reject slicing", reply: "reject", feedback: "slicing"},
		{text: "reject slicing merge them", reply: "reject slicing", feedback: "merge them"},
		{text: "REJECT SLICING  merge them ", reply: "reject slicing", feedback: "merge them"},
		{text: "reject slicingmerge", reply: "reject", feedback: "slicingmerge"},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			a, feedback, ok := matchAction(tt.text, actions)
			if tt.reply == "" {
				assert.False(t, ok, "no action")

				return
			}

			require.True(t, ok)
			assert.Equal(t, tt.reply, a.Reply)
			assert.Equal(t, tt.feedback, feedback)
		})
	}
}

func TestGate_AHumanGateWaitsThenApproveGoesOn(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	gate, ok := rec.first(EventGate)
	require.True(t, ok, "the gate is shown")
	assert.Equal(t, "shape", gate.Payload["stage"])
	assert.Equal(t, 1, gate.Payload["version"])
	assert.Equal(t, []string{"intake", "shape"}, stagesRun(runner), "nothing runs past the gate")
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))

	require.NoError(t, c.Message(ctx, "BP1", "Approve", "m2"))
	c.Wait("BP1")

	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner))

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, store.ResponderHuman, gates[0].Responder)
	assert.Equal(t, 1, gates[0].Version)

	decided, ok := rec.first(EventDecided)
	require.True(t, ok)
	assert.Equal(t, "shape", decided.Payload["stage"])
	assert.True(t, rec.has(EventReport))
}

func TestGate_RejectRunsTheStageAgainWithItsArtifactAndEveryFeedback(t *testing.T) {
	var shapes atomic.Int32

	script := gatedScript()
	script["shape"] = func(context.Context, stage.Input) (stage.Output, error) {
		return done(fmt.Sprintf("# Shape %d", shapes.Add(1)), nil), nil
	}

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.NoError(t, c.Message(ctx, "BP1", "reject make it smaller", "m2"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "REJECT\tsplit the second part", "m3"))
	c.Wait("BP1")

	assert.Equal(t, []string{"intake", "shape", "shape", "shape"}, stagesRun(runner))

	third := inputsOf(runner, "shape")[2]
	assert.Contains(t, third.Task, "# Shape 2", "the rejected artifact")
	assert.Contains(t, third.Task, "make it smaller")
	assert.Contains(t, third.Task, "split the second part")

	gates := rec.all(EventGate)
	require.Len(t, gates, 3, "each new version waits at the gate")
	assert.Equal(t, 3, gates[2].Payload["version"])

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, 3, runs[len(runs)-1].Attempt, "a rejection starts a new attempt")
}

func TestGate_OtherTextIsANoteAndTheGateStaysOpen(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	for i, text := range []string{"looks fine to me", "reject", "approve it"} {
		require.NoError(t, c.Message(ctx, "BP1", text, fmt.Sprintf("n%d", i)))
		c.Wait("BP1")
	}

	assert.Equal(t, []string{"intake", "shape"}, stagesRun(runner), "nothing ran")

	notes := rec.all(EventNote)
	require.Len(t, notes, 3)

	for _, n := range notes {
		assert.NotEmpty(t, n.Payload["hint"])
		assert.Equal(t, "shape", n.Payload["stage"], "the gated stage")
	}

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates)
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

func TestGate_AnAutoPolicyApprovesOnItsOwn(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeAuto)

	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner))
	assert.False(t, rec.has(EventGate), "an auto gate is never shown")

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestGate_AGateReplyChangesAGateNotYetOpen(t *testing.T) {
	c, _, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", "auto", "m0"))

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	assert.False(t, rec.has(EventGate))
	assert.Equal(t, []string{"intake", "shape", "plan"}, stagesRun(runner))
}

func TestGate_AGateReplyLeavesTheOpenGateOpen(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)
	require.NoError(t, c.SetPolicy(ctx, "BP1", "shape", "auto", "m2"))
	c.Wait("BP1")

	change, ok := rec.first(EventPolicy)
	require.True(t, ok)
	assert.Equal(t, "gate", change.Payload["open"])
	assert.Equal(t, []string{"intake", "shape"}, stagesRun(runner), "the change does not decide the open gate")

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates)

	// A shown gate stays with the human across a reopen, whatever the
	// policy says now.
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Shutdown(ctx))

	c2, _, runner2, rec2 := newGatedOn(t, st, gatedScript())
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	assert.True(t, rec2.has(EventGate))
	assert.Zero(t, runner2.calls())
}

func TestGate_AReopenShowsTheOpenGateAgain(t *testing.T) {
	c, st, _, rec := newGated(t, gatedScript())
	ctx := context.Background()

	startGated(t, c, protocol.ArchitectModeDefault)

	shown, ok := rec.first(EventGate)
	require.True(t, ok)

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Shutdown(ctx))

	c2, _, runner2, rec2 := newGatedOn(t, st, gatedScript())
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	again, ok := rec2.first(EventGate)
	require.True(t, ok, "the gate is shown again")
	assert.JSONEq(t, payloadJSON(t, shown), payloadJSON(t, again))
	assert.Zero(t, runner2.calls(), "nothing runs")
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))

	require.NoError(t, c2.Message(ctx, "BP1", "approve", "m2"))
	c2.Wait("BP1")
	assert.Equal(t, []string{"plan"}, stagesRun(runner2))
}

// seedGated stores a blueprint on the gated pack with intake and shape done
// and their artifacts stored, as a crash after the last artifact leaves it.
func seedGated(t *testing.T, st *store.Store, mode string) {
	t.Helper()

	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{
		ID: "BP1", Projects: []string{"alpha"}, Mode: mode, Model: "m",
		State: string(StateRunning), CreatedAt: now, UpdatedAt: now,
	}))

	_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: needStage, Body: "the need"})
	require.NoError(t, err)

	for _, id := range []string{"intake", "shape"} {
		_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: id, Body: "# " + id})
		require.NoError(t, err)

		run, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: id, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
		require.NoError(t, err)
		require.NoError(t, st.EndRun(ctx, run, store.RunDone, "", store.RunUsage{}, now))
	}
}

func TestGate_AGateNeverShownOpensUnderThePolicyOnReopen(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		shown      bool
		responder  string
		stagesRun  []string
		wantWaited bool
	}{
		{name: "human", mode: protocol.ArchitectModeDefault, shown: true, wantWaited: true},
		{name: "auto", mode: protocol.ArchitectModeAuto, responder: store.ResponderAuto, stagesRun: []string{"plan"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := newGated(t, gatedScript())
			ctx := context.Background()

			seedGated(t, st, tt.mode)

			in := startInput("BP1")
			in.Mode = tt.mode
			require.NoError(t, c.Start(ctx, in))
			c.Wait("BP1")

			assert.Equal(t, tt.shown, rec.has(EventGate))
			assert.ElementsMatch(t, tt.stagesRun, stagesRun(runner))

			gates, err := st.ListGates(ctx, "BP1", "shape")
			require.NoError(t, err)

			if tt.wantWaited {
				assert.Empty(t, gates)
				assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))

				return
			}

			require.Len(t, gates, 1)
			assert.Equal(t, store.GateApprove, gates[0].Decision)
			assert.Equal(t, tt.responder, gates[0].Responder)
		})
	}
}

func TestGate_AShownGateThatCannotBeReadIsReportedNotOpened(t *testing.T) {
	c, st, runner, rec := newGated(t, gatedScript())
	ctx := context.Background()

	seedGated(t, st, protocol.ArchitectModeAuto)
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventGate, Payload: []byte(`{"stage":"shape","version":"one"}`),
	}))

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	require.NoError(t, c.Start(ctx, in))
	c.Wait("BP1")

	assert.True(t, rec.has(EventError), "the unreadable record is reported")
	assert.Empty(t, stagesRun(runner), "nothing runs")

	gates, err := st.ListGates(ctx, "BP1", "shape")
	require.NoError(t, err)
	assert.Empty(t, gates, "the auto policy does not decide a gate that may have been shown")
}

func TestGate_AnApprovedLaterVersionIsNotCalledRejected(t *testing.T) {
	tests := []struct {
		name     string
		decision string
		carried  bool
	}{
		{name: "latest rejected", decision: store.GateReject, carried: true},
		{name: "latest approved", decision: store.GateApprove},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _ := newGated(t, gatedScript())
			ctx := context.Background()

			seedGated(t, st, protocol.ArchitectModeDefault)

			now := time.Now().UTC()
			_, err := st.AddGate(ctx, store.Gate{BlueprintID: "BP1", Stage: "shape", Version: 1, Decision: store.GateReject, Feedback: "earlier feedback", Responder: store.ResponderHuman, DecidedAt: now})
			require.NoError(t, err)

			_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "shape", Body: "# shape two"})
			require.NoError(t, err)
			_, err = st.AddGate(ctx, store.Gate{BlueprintID: "BP1", Stage: "shape", Version: 2, Decision: tt.decision, Feedback: "later feedback", Responder: store.ResponderHuman, DecidedAt: now})
			require.NoError(t, err)

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			shape, ok := c.opts.Pack.Stage("shape")
			require.True(t, ok)

			task, err := c.taskFor(ctx, bp, shape)
			require.NoError(t, err)

			if tt.carried {
				assert.Contains(t, task, "earlier feedback")
				assert.Contains(t, task, "# shape two")

				return
			}

			assert.NotContains(t, task, "earlier feedback")
			assert.NotContains(t, task, "# shape two")
		})
	}
}

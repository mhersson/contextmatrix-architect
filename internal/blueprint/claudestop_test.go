package blueprint

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestClaudeStop(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 9, 0, 0, 0, time.FixedZone("x", 2*3600))

	tests := []struct {
		name       string
		err        error
		reason     string
		ok         bool
		resetsAt   string
		hasResetAt bool
	}{
		{"a usage limit with a reset", fmt.Errorf("stage x: %w", &claudecode.UsageLimitError{Window: "seven_day", ResetsAt: t0}), "usage_limit", true, t0.UTC().Format(time.RFC3339), true},
		{"a usage limit without a reset", fmt.Errorf("stage x: %w", &claudecode.UsageLimitError{Window: "five_hour"}), "usage_limit", true, "", false},
		{"a failed login", fmt.Errorf("x: %w", claudecode.ErrNotLoggedIn), "not_logged_in", true, "", false},
		{"another error", errors.New("other"), "", false, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, payload, ok := claudeStop(tt.err)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.reason, reason)

			if !tt.ok {
				return
			}

			assert.Equal(t, tt.reason, payload["reason"])
			assert.NotEmpty(t, payload["message"])

			got, has := payload["resets_at"]
			assert.Equal(t, tt.hasResetAt, has)

			if tt.hasResetAt {
				assert.Equal(t, tt.resetsAt, got)
			}
		})
	}
}

func TestStageUsageLimitStops(t *testing.T) {
	t0 := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	limited := true
	script := gatedScript()
	inner := script["intake"]
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if limited {
			return stage.Output{}, fmt.Errorf("stage %s: %w", in.Stage, &claudecode.UsageLimitError{Window: "seven_day", ResetsAt: t0})
		}

		return inner(ctx, in)
	}

	c, st, runner, rec := newGated(t, script)
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeDefault
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	stop := lastError(t, rec)
	assert.Equal(t, "usage_limit", stop.Payload["reason"])
	assert.Equal(t, "seven_day", stop.Payload["window"])
	assert.Equal(t, t0.Format(time.RFC3339), stop.Payload["resets_at"])

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.NotEmpty(t, runs)
	assert.Equal(t, store.RunFailed, runs[len(runs)-1].Status)
	assert.Equal(t, "usage_limit", runs[len(runs)-1].Reason)

	before := runner.calls()
	limited = false

	require.NoError(t, c.Message(ctx, "BP1", "continue", "m2"))
	c.Wait("BP1")
	assert.Greater(t, runner.calls(), before, "continue runs the stage again")
}

func TestSideRunUsageLimitReason(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = refusedWith("error", &claudecode.UsageLimitError{Window: "five_hour"})

	c, st, _, _ := newGated(t, script)
	startGated(t, c, protocol.ArchitectModeDefault)

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	var reviews int

	for _, r := range runs {
		if r.Kind != store.RunKindReviewer {
			continue
		}

		reviews++

		assert.Equal(t, store.RunFailed, r.Status)
		assert.Equal(t, "usage_limit", r.Reason)
	}

	assert.Positive(t, reviews)
}

func TestReviewerClaudeStopLeavesTheBlueprintIdle(t *testing.T) {
	var limited atomic.Bool

	limited.Store(true)

	script := gatedScript()
	review := script["shape.review"]
	script["shape.review"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if limited.Load() {
			return stage.Output{}, fmt.Errorf("stage %s: %w", in.Stage, &claudecode.UsageLimitError{Window: "five_hour"})
		}

		return review(ctx, in)
	}

	c, st, runner, rec := newGated(t, script)
	startGated(t, c, protocol.ArchitectModeDefault)

	stop := lastError(t, rec)
	assert.Equal(t, "usage_limit", stop.Payload["reason"])
	assert.Equal(t, "five_hour", stop.Payload["window"])
	assert.Equal(t, "shape", stop.Payload["stage"])

	ctx := context.Background()
	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	limited.Store(false)

	require.NoError(t, c.Message(ctx, "BP1", "continue", "m2"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "shape.review"), 2, "continue runs the review again")

	_, err = st.LatestArtifact(ctx, "BP1", "shape.review")
	require.NoError(t, err, "the review is stored after the reset")
}

func TestResponderClaudeStopLeavesTheRoundOpen(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"a usage limit", &claudecode.UsageLimitError{Window: "five_hour"}},
		{"a failed login", claudecode.ErrNotLoggedIn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := make(chan error, 1)

			script := happyScript()
			script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				_, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Deploy", "Helm or kustomize?", "helm", "kustomize")}})
				got <- err

				// The asker's next request meets the same stop.
				return stage.Output{}, fmt.Errorf("stage %s: %w", in.Stage, tt.err)
			}
			script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
				return stage.Output{}, fmt.Errorf("stage %s: %w", in.Stage, tt.err)
			}

			c, st, _, rec, _ := newPipeline(t, script)
			ctx := context.Background()

			driveIn(t, c, autoInput())

			require.ErrorIs(t, <-got, tt.err, "the asker gets the stop, not an answer")

			rounds, err := c.rounds(ctx, "BP1")
			require.NoError(t, err)
			assert.Empty(t, rounds, "no round is stored with a degraded answer")

			wantReason, _, _ := claudeStop(tt.err)
			assert.Equal(t, wantReason, lastError(t, rec).Payload["reason"])

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)
			assert.Equal(t, string(StateIdle), bp.State)
		})
	}
}

func TestHandoverClaudeStopStopsAndContinueRetries(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"a usage limit", &claudecode.UsageLimitError{Window: "five_hour"}},
		{"a failed login", claudecode.ErrNotLoggedIn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var limited atomic.Bool

			runs := atomic.Int32{}
			script := handoverScript()
			script["intake.responder"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
				if runs.Add(1) == 2 && limited.Load() {
					return stage.Output{}, fmt.Errorf("stage %s: %w", in.Stage, tt.err)
				}

				return answering("operators", in), nil
			}

			limited.Store(true)

			c, st, _, rec, _ := newPipeline(t, script)
			ctx := context.Background()

			drive(t, c, "BP1")

			require.NoError(t, c.SetMode(ctx, "BP1", protocol.ArchitectModeAuto, "m-mode"))
			c.Wait("BP1")

			round := latestRound(t, c, "BP1", 1)
			require.False(t, round.Frame.Closed, "the stop closes nothing")
			assert.Equal(t, []protocol.RoundAnswer{{Number: 1, Text: "operators"}}, round.Frame.Answers,
				"the answer given before the stop is kept and no degraded one is stored")

			wantReason, _, _ := claudeStop(tt.err)
			stop := lastError(t, rec)
			assert.Equal(t, wantReason, stop.Payload["reason"])
			assert.Equal(t, "intake", stop.Payload["stage"])

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)
			assert.Equal(t, string(StateIdle), bp.State)

			limited.Store(false)

			require.NoError(t, c.Resume(ctx, "BP1", "m-continue"))
			c.Wait("BP1")

			assert.Equal(t, []int{1, 2, 2}, responderRounds(t, st), "continue answers only the open question")
			assert.True(t, latestRound(t, c, "BP1", 1).Frame.Closed)
		})
	}
}

func TestMockupTurnClaudeStopLeavesTheRevisionUndecided(t *testing.T) {
	var limited atomic.Bool

	limited.Store(true)

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(bookingRevision())
	use := reacting(map[string]any{"use": true})
	script["mockup.responder"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if limited.Load() {
			return stage.Output{}, fmt.Errorf("stage %s: %w", in.Stage, &claudecode.UsageLimitError{Window: "five_hour"})
		}

		return use(ctx, in)
	}

	c, st, runner, rec, _ := newMockup(t, script)
	driveIn(t, c, autoStart("BP1"))

	s := onlyMockupRound(t, c)
	assert.Empty(t, s.Mockup.Action, "the revision is not used as it is set")
	assert.False(t, s.Mockup.Failed)
	assert.Empty(t, mockupGates(t, st), "nothing is decided")
	assert.NotContains(t, stagesRun(runner), "slicing")

	for _, e := range rec.all(EventError) {
		assert.NotEqual(t, reasonResponderFailed, e.Payload["reason"])
	}

	stop := lastError(t, rec)
	assert.Equal(t, "usage_limit", stop.Payload["reason"])
	assert.Equal(t, "mockup", stop.Payload["stage"])

	ctx := context.Background()
	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	limited.Store(false)

	require.NoError(t, c.Resume(ctx, "BP1", "m-continue"))
	c.Wait("BP1")

	gates := mockupGates(t, st)
	require.Len(t, gates, 1, "continue runs the turn again")
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
	assert.False(t, onlyMockupRound(t, c).Mockup.Failed, "the responder's own use")
	assert.Len(t, inputsOf(runner, "mockup.responder"), 2)
	assert.Contains(t, stagesRun(runner), "slicing")
}

func TestSinkForwardsUsageLimit(t *testing.T) {
	c, _, _, rec := newGated(t, gatedScript())
	s := &harnessSink{audit: c.newAudit(), id: "BP1", runID: 1}

	_, err := s.Write([]byte(`{"kind":"usage_limit","data":{"window":"seven_day"}}` + "\n"))
	require.NoError(t, err)

	shown := rec.shown()
	require.Len(t, shown, 1, "the panel bridges the usage limit")
	assert.Contains(t, shown[0], `"kind":"usage_limit"`)
}

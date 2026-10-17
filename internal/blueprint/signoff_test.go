package blueprint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestSignoff_DefaultModeWaitsForApproveThenEmits(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ctx := context.Background()

	drive(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok, "the sign-off is shown")
	assert.Empty(t, so.Payload["blocking"])
	assert.Empty(t, fake.titles(), "nothing is emitted before the approval")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateWaiting), bp.State)

	require.NoError(t, c.Message(ctx, "BP1", "Approve", "m3"))
	c.Wait("BP1")

	assert.Equal(t, []string{"Add the thing"}, fake.titles())
	assert.True(t, rec.has(EventEmitted))

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateApprove, gates[0].Decision)
	assert.Equal(t, store.ResponderHuman, gates[0].Responder)
}

func TestSignoff_RejectRerunsDecompositionWithTheFeedbackAndAFreshBudget(t *testing.T) {
	var readinessRuns atomic.Int32

	script := happyScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		if readinessRuns.Add(1) == 1 {
			return done("# Ready", readiness(true)), nil
		}

		return done("# Not ready", readiness(false)), nil
	}

	c, st, runner, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	require.True(t, rec.has(EventSignOff))

	require.NoError(t, c.Message(ctx, "BP1", "reject split the card in two", "m3"))
	c.Wait("BP1")

	var last stage.Input

	for i := range runner.calls() {
		if in := runner.input(i); in.Stage == "decomposition" {
			last = in
		}
	}

	assert.Contains(t, last.Task, "split the card in two", "decomposition sees the feedback")
	assert.Equal(t, int32(4), readinessRuns.Load(), "three readiness rounds after the rejection")

	reports := rec.all(EventReport)
	require.Len(t, reports, 2, "the ready report, then the not-ready one")
	assert.Equal(t, true, reports[0].Payload["signoff"], "the sign-off follows a ready end")
	assert.Equal(t, false, reports[1].Payload["signoff"])

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.GateReject, gates[0].Decision)
	assert.Equal(t, "split the card in two", gates[0].Feedback)
}

func TestSignoff_RejectWithoutFeedbackIsANote(t *testing.T) {
	c, _, runner, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	drive(t, c, "BP1")

	calls := runner.calls()

	require.NoError(t, c.Message(ctx, "BP1", "reject", "m3"))
	c.Wait("BP1")

	assert.Equal(t, calls, runner.calls(), "nothing re-runs")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Contains(t, note.Payload["hint"], "reject <feedback>")
}

func TestSignoff_OtherTextIsANoteWithTheHint(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "looks good to me", "m3"))
	c.Wait("BP1")

	assert.Empty(t, fake.titles())

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Contains(t, note.Payload["hint"], "approve")
	assert.Equal(t, signoffStage, note.Payload["stage"])
}

func TestSignoff_ANoteWhileTheSignoffHoldsTheSlotRecordsTheSignoff(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		approve bool
	}{
		{name: "the emission an approve starts", mode: protocol.ArchitectModeDefault, approve: true},
		{name: "the sign-off that follows readiness", mode: protocol.ArchitectModeAutoEmit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _, rec, _ := newPipeline(t, happyScript())
			hold := newHoldCM(t, newFakeCM())
			hold.title = "Add the thing"
			c.opts.ConnectCM = hold.connect
			ctx := context.Background()

			in := startInput("BP1")
			in.Mode = tt.mode
			require.NoError(t, c.Start(ctx, in))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")
			require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))

			if tt.approve {
				c.Wait("BP1")
				require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
			}

			receive(t, hold.entered, "the held card")
			require.NoError(t, c.Message(ctx, "BP1", "keep the API stable", "m4"))
			hold.free()
			c.Wait("BP1")

			note, ok := rec.first(EventNote)
			require.True(t, ok)
			assert.Equal(t, signoffStage, note.Payload["stage"])
		})
	}
}

func TestSignoff_ApproveIsTheDesignWhileTheDesignStageWaits(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	design, err := st.LatestArtifact(ctx, "BP1", "design")
	require.NoError(t, err)
	assert.Equal(t, "approve", design.Body)

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	assert.Empty(t, gates)
}

func TestSignoff_AutoEmitApprovesWhenPreflightIsClean(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit
	driveIn(t, c, in)

	assert.Equal(t, []string{"Add the thing"}, fake.titles())
	assert.False(t, rec.has(EventSignOff))

	gates, err := st.ListGates(context.Background(), "BP1", signoffStage)
	require.NoError(t, err)
	require.Len(t, gates, 1)
	assert.Equal(t, store.ResponderAuto, gates[0].Responder)
}

func TestSignoff_AutoEmitEmitsAfterAFirstCommitToAnEmptyRepository(t *testing.T) {
	c, _, _, rec, ws := newPipeline(t, happyScript())
	ws.empty = map[string]bool{"alpha": true}
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ws.heads = map[string]string{"https://example.com/alpha.git": "sha-first"}

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit
	driveIn(t, c, in)

	assert.Equal(t, []string{"Add the thing"}, fake.titles())
	assert.False(t, rec.has(EventSignOff))
}

func TestSignoff_AutoEmitStopsForAHumanOnAWarning(t *testing.T) {
	c, _, _, rec, ws := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ws.heads = map[string]string{"https://example.com/alpha.git": "sha-new"}

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit
	driveIn(t, c, in)

	assert.Empty(t, fake.titles())

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, true, so.Payload["auto_stopped"])
	assert.NotEmpty(t, so.Payload["warnings"])

	actions := anyList(so.Payload["actions"])
	require.Len(t, actions, 2)
	assert.Equal(t, "Approve and override warnings", actions[0].(map[string]any)["label"])
	assert.Equal(t, "approve", actions[0].(map[string]any)["reply"])
}

func TestSignoff_TheAutoModeStillWaitsForAHuman(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAuto
	driveIn(t, c, in)

	assert.Empty(t, fake.titles())
	assert.True(t, rec.has(EventSignOff))
}

func TestSignoff_ShowsBlockingProblemsAndApproveIsRefused(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	fake.projects = []cm.Project{{Name: "alpha", Verify: &cm.Verify{Command: "make test"}}}
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.NotEmpty(t, so.Payload["blocking"])

	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m3"))
	c.Wait("BP1")

	assert.Empty(t, fake.titles())

	var blocked bool

	for _, e := range rec.all(EventError) {
		if strings.Contains(e.Payload["message"].(string), "blocked") {
			blocked = true
		}
	}

	assert.True(t, blocked)
}

func TestSignoff_ApproveAnswersAPendingQuestion(t *testing.T) {
	asked := make(chan struct{}, 1)

	script := happyScript()
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		signal(asked)

		answer, err := in.Asker.Ask(ctx, stage.Question{Text: "Go ahead?"})
		if err != nil {
			return stage.Output{}, err
		}

		return done("# Need\n\n"+answer, nil), nil
	}

	c, st, _, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, asked, "the question")
	rec.waitFor(t, EventQuestion)

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	assert.True(t, rec.has(EventAnswer))

	art, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.Equal(t, "# Need\n\napprove", art.Body)

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	assert.Empty(t, gates)
}

func TestSignoff_ApproveWhileAStageRunsIsANote(t *testing.T) {
	started := make(chan struct{}, 1)
	finish := make(chan struct{})

	script := happyScript()
	script["intake"] = func(context.Context, stage.Input) (stage.Output, error) {
		signal(started)

		<-finish

		return done("# Need", nil), nil
	}

	c, st, _, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	// Intake ignores its context, and a failing test must still let it end.
	release := closer(t, finish)

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "intake")

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	release()
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, true, note.Payload["during_run"])

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	assert.Empty(t, gates)
}

func TestSignoff_ShowsIntakesAssumptions(t *testing.T) {
	script := happyScript()
	script["intake"] = returns(needBody, map[string]any{"assumptions": []any{"The parser stays in Go."}})

	c, _, _, rec, _ := newPipeline(t, script)

	drive(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []any{
		map[string]any{"id": "A1", "stage": "intake", "text": "The parser stays in Go.", "status": "unverified", "evidence": ""},
	}, so.Payload["assumptions"])
}

func TestSignoff_ASecondApproveAfterEmissionIsANote(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")
	require.Equal(t, []string{"Add the thing"}, fake.titles())

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m4"))
	c.Wait("BP1")

	assert.Equal(t, []string{"Add the thing"}, fake.titles(), "nothing is created twice")
	assert.True(t, rec.has(EventNote))
}

func TestSignoff_AnAbortBeforeThePreflightStillShowsTheSignOff(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	entered := make(chan struct{}, 1)
	c.opts.ConnectCM = func(ctx context.Context, _ string) (CMClient, error) {
		signal(entered)

		<-ctx.Done()

		return nil, ctx.Err()
	}
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
	receive(t, entered, "the preflight")

	require.NoError(t, c.Abort(ctx, "BP1", "m-abort"))
	c.Wait("BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.NotEmpty(t, so.Payload["preflight_error"])

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	assert.Empty(t, gates)
}

// slowCM holds ListPlaybooks, the last call of the preflight, until release, so a test can abort while the
// preflight is in flight and still let it finish cleanly.
type slowCM struct {
	CMClient

	entered chan struct{}
	release chan struct{}
	free    func()
}

// newSlowCM wraps inner; the hold ignores the call's context, so it is freed
// when the test ends at the latest.
func newSlowCM(t *testing.T, inner CMClient) *slowCM {
	t.Helper()

	s := &slowCM{CMClient: inner, entered: make(chan struct{}, 1), release: make(chan struct{})}
	s.free = closer(t, s.release)

	return s
}

func (s *slowCM) ListPlaybooks(ctx context.Context) ([]cm.PlaybookSummary, error) {
	signal(s.entered)

	<-s.release

	return s.CMClient.ListPlaybooks(ctx)
}

func TestSignoff_AnAbortDuringACleanPreflightDoesNotApprove(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	slow := newSlowCM(t, fake)
	c.opts.ConnectCM = func(context.Context, string) (CMClient, error) { return slow, nil }
	ctx := context.Background()

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit

	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
	receive(t, slow.entered, "the preflight")

	require.NoError(t, c.Abort(ctx, "BP1", "m-abort"))
	slow.free()
	c.Wait("BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, true, so.Payload["auto_stopped"])
	assert.Empty(t, fake.titles())

	gates, err := st.ListGates(ctx, "BP1", signoffStage)
	require.NoError(t, err)
	assert.Empty(t, gates)
}

// restart attaches the session to a fresh controller on the same store and
// workspace, as CM's start does after the architect restarted. It does not
// wait for what the start resumes.
func restart(t *testing.T, st *store.Store, ws *fakeWorkspace, script stageScript, connect func(context.Context, string) (CMClient, error), in StartInput) (*Controller, *fakeRunner, *recorder) {
	t.Helper()

	c2, _, runner, rec, _ := newPipelineOn(t, st, script, ws)
	c2.opts.ConnectCM = connect
	c2.opts.WatchInterval = 5 * time.Millisecond

	require.NoError(t, c2.Start(context.Background(), in))

	return c2, runner, rec
}

// shutdownHeld shuts c down during a held ContextMatrix write. Shutdown
// waits for the write, so the hold is freed once Shutdown has begun.
func shutdownHeld(t *testing.T, c *Controller, hold *holdCM) {
	t.Helper()

	var err error

	done := make(chan struct{})

	go func() {
		defer close(done)

		err = c.Shutdown(context.Background())
	}()

	require.Eventually(t, c.closingDown, 5*time.Second, time.Millisecond, "Shutdown begins")
	hold.free()
	receive(t, done, "the shutdown")
	require.NoError(t, err)
}

// reopen ends the session and shuts c down, then restarts and waits for
// what the start resumed.
func reopen(t *testing.T, c *Controller, st *store.Store, ws *fakeWorkspace, script stageScript, fake *fakeCM, in StartInput) (*Controller, *recorder) {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, c.End(ctx, in.ID))
	require.NoError(t, c.Shutdown(ctx))

	c2, _, rec := restart(t, st, ws, script, fake.connect, in)
	c2.Wait(in.ID)

	return c2, rec
}

func stateOf(t *testing.T, st *store.Store, id string) string {
	t.Helper()

	bp, err := st.GetBlueprint(context.Background(), id)
	require.NoError(t, err)

	return bp.State
}

// holdsSlot reports whether a run or a caller holds id's run slot.
func holdsSlot(c *Controller, id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, held := c.runs[id]

	return held
}

func payloadJSON(t *testing.T, e Event) string {
	t.Helper()

	b, err := json.Marshal(e.Payload)
	require.NoError(t, err)

	return string(b)
}

func TestReopen_ShowsAnOpenSignoffAgainAsItWas(t *testing.T) {
	c, st, _, rec, ws := newPipeline(t, happyScript())
	drive(t, c, "BP1")

	shown, ok := rec.first(EventSignOff)
	require.True(t, ok)

	second := newFakeCM()
	_, rec2 := reopen(t, c, st, ws, happyScript(), second, startInput("BP1"))

	again, ok := rec2.first(EventSignOff)
	require.True(t, ok, "the sign-off is shown again")
	assert.JSONEq(t, payloadJSON(t, shown), payloadJSON(t, again))
	assert.Zero(t, second.connectCount(), "no preflight runs again")
	assert.Equal(t, string(StateWaiting), stateOf(t, st, "BP1"))
}

// A restarted architect gets CM's start, then at once the message that found
// the session unknown: the start must leave the run slot free for it.
func TestReopen_AMessageReSentAfterARestartDecidesTheSignoff(t *testing.T) {
	cases := []struct {
		name, text         string
		decision, feedback string
		titles             []string
		reruns             bool
	}{
		{name: "approve", text: "approve", decision: store.GateApprove, titles: []string{"Add the thing"}},
		{name: "reject", text: "reject split the card in two", decision: store.GateReject, feedback: "split the card in two", titles: []string{}, reruns: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _, _, ws := newPipeline(t, happyScript())
			ctx := context.Background()

			drive(t, c, "BP1")
			require.NoError(t, c.End(ctx, "BP1"))
			require.NoError(t, c.Shutdown(ctx))

			// ContextMatrix is held until the message is in, so a sign-off that
			// ran again on the start would still hold the run slot then.
			second := newFakeCM()
			held := make(chan struct{})

			var once sync.Once

			open := func() { once.Do(func() { close(held) }) }
			t.Cleanup(open)

			connect := func(ctx context.Context, key string) (CMClient, error) {
				select {
				case <-held:
				case <-ctx.Done():
					return nil, ctx.Err()
				}

				return second.connect(ctx, key)
			}

			c2, runner2, rec2 := restart(t, st, ws, happyScript(), connect, startInput("BP1"))
			assert.False(t, holdsSlot(c2, "BP1"), "the run slot is free when the start returns")

			require.NoError(t, c2.Message(ctx, "BP1", tc.text, "m3"))
			open()
			c2.Wait("BP1")

			gates, err := st.ListGates(ctx, "BP1", signoffStage)
			require.NoError(t, err)
			require.Len(t, gates, 1, "the message is the decision")
			assert.Equal(t, tc.decision, gates[0].Decision)
			assert.Equal(t, tc.feedback, gates[0].Feedback)
			assert.Equal(t, store.ResponderHuman, gates[0].Responder)
			assert.False(t, rec2.has(EventNote))
			assert.Equal(t, tc.titles, second.titles())

			reran := false

			for i := range runner2.calls() {
				if runner2.input(i).Stage == "decomposition" {
					reran = true
				}
			}

			assert.Equal(t, tc.reruns, reran, "decomposition re-runs on a reject")
		})
	}
}

func TestReopen_AStoppedEmissionWaitsForApprove(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		// stop approves and stops the emission partway; it returns the titles
		// created by then.
		stop    func(t *testing.T, c *Controller) []string
		missing []string
	}{
		{
			name: "a ContextMatrix error",
			stop: func(t *testing.T, c *Controller) []string {
				t.Helper()

				fake := newFakeCM()
				fake.failCreate["Wire the parser"] = 1
				c.opts.ConnectCM = fake.connect

				require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
				c.Wait("BP1")

				return fake.titles()
			},
			missing: []string{"Wire the parser"},
		},
		{
			name: "an abort",
			stop: func(t *testing.T, c *Controller) []string {
				t.Helper()

				fake := newFakeCM()
				hold := newHoldCM(t, fake)
				hold.title = "Add the parser"
				c.opts.ConnectCM = hold.connect

				require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
				receive(t, hold.entered, "the held card")
				require.NoError(t, c.Abort(ctx, "BP1", "m4"))
				hold.free()
				c.Wait("BP1")

				return fake.titles()
			},
			missing: []string{"Wire the parser"},
		},
		{
			name: "a blocked preflight",
			stop: func(t *testing.T, c *Controller) []string {
				t.Helper()

				fake := newFakeCM()
				fake.projects = []cm.Project{{Name: "alpha", Verify: &cm.Verify{Command: "make test"}}}
				c.opts.ConnectCM = fake.connect

				require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
				c.Wait("BP1")

				return fake.titles()
			},
			missing: []string{"Add the parser", "Wire the parser"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _, _, ws := newPipeline(t, twoCardScript())

			drive(t, c, "BP1")

			created := tc.stop(t, c)
			require.Len(t, created, 2-len(tc.missing))
			require.Equal(t, string(StateIdle), stateOf(t, st, "BP1"))

			second := newFakeCM()
			c2, rec2 := reopen(t, c, st, ws, twoCardScript(), second, startInput("BP1"))

			assert.Empty(t, second.titles(), "nothing resumes without approve")
			assert.Equal(t, string(StateIdle), stateOf(t, st, "BP1"))

			reminder, ok := rec2.first(EventAwaitInput)
			require.True(t, ok, "the reminder is posted")
			assert.Equal(t, signoffStage, reminder.Payload["stage"])
			assert.Contains(t, reminder.Payload["prompt"], "reply approve to resume")

			require.NoError(t, c2.Message(ctx, "BP1", "approve", "m5"))
			c2.Wait("BP1")

			assert.Equal(t, tc.missing, second.titles(), "only what is missing")

			_, books := second.inputs()
			assert.Len(t, books, 1)
			assert.Equal(t, 1, rec2.count(EventEmitted))
			assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
		})
	}
}

func TestReopen_AnEmissionARestartCutShortResumes(t *testing.T) {
	c, st, _, rec, ws := newPipeline(t, twoCardScript())
	first := newFakeCM()
	hold := newHoldCM(t, first)
	hold.title = "Add the parser"
	c.opts.ConnectCM = hold.connect
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	receive(t, hold.entered, "the held card")
	shutdownHeld(t, c, hold)

	require.Equal(t, []string{"Add the parser"}, first.titles())
	assert.Equal(t, string(StateEmitting), stateOf(t, st, "BP1"), "a restart is not a stop")
	assert.False(t, rec.has(EventError), "no stop is reported")

	second := newFakeCM()
	c2, _, rec2 := restart(t, st, ws, twoCardScript(), second.connect, startInput("BP1"))
	c2.Wait("BP1")

	assert.Equal(t, []string{"Wire the parser"}, second.titles(), "only the missing card")

	_, books := second.inputs()
	assert.Len(t, books, 1)
	assert.True(t, rec2.has(EventEmitted))
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
}

// A crash after the last emission record, before the state and the
// announcement: the reopen announces, sets emitted and watches, creating and
// starting nothing.
func TestReopen_ACompleteEmissionARestartCutBeforeItsAnnouncementAnnounces(t *testing.T) {
	cases := []struct {
		name string
		mode string
		run  bool
	}{
		{name: "default mode", mode: "default"},
		{name: "yolo with the run start recorded", mode: protocol.ArchitectModeYolo, run: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _, _, ws := newPipeline(t, happyScript())
			// A moved head stops yolo at the sign-off instead of emitting.
			ws.heads = map[string]string{"https://example.com/alpha.git": "sha-new"}
			ctx := context.Background()
			now := time.Now().UTC()

			in := startInput("BP1")
			in.Mode = tc.mode
			driveIn(t, c, in)

			verdict, err := st.LatestArtifact(ctx, "BP1", "readiness")
			require.NoError(t, err)

			_, err = st.AddGate(ctx, store.Gate{
				BlueprintID: "BP1", Stage: signoffStage, Version: verdict.Version,
				Decision: store.GateApprove, Responder: store.ResponderHuman, DecidedAt: now,
			})
			require.NoError(t, err)
			require.NoError(t, st.AddEmission(ctx, store.Emission{
				BlueprintID: "BP1", Kind: store.EmitCard, DraftID: "D1", Project: "alpha", CMID: "ALPHA-001", CreatedAt: now,
			}))

			if tc.run {
				require.NoError(t, st.AddEmission(ctx, store.Emission{
					BlueprintID: "BP1", Kind: store.EmitRun, Project: "alpha", CMID: "ALPHA-001", CreatedAt: now,
				}))
			}

			require.NoError(t, st.SetBlueprintState(ctx, "BP1", string(StateEmitting)))

			second := newFakeCM()
			second.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "todo"})
			_, rec2 := reopen(t, c, st, ws, happyScript(), second, in)

			assert.Equal(t, 1, rec2.count(EventEmitted), "the cut announcement is made")
			assert.Empty(t, second.titles())
			assert.Empty(t, second.cardRuns())
			assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
			require.Eventually(t, func() bool { return second.getCount() > 0 }, 5*time.Second, 5*time.Millisecond, "the watch polls")

			ev, ok := rec2.first(EventEmitted)
			require.True(t, ok)
			assert.Equal(t, false, ev.Payload["run_follows"], "no run start follows")
		})
	}
}

// CM re-sends the start when its log stream ends; the session is still held,
// so the emission in flight is the only one.
func TestStart_AReSentStartDuringEmissionStartsNothingMore(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	hold := newHoldCM(t, fake)
	hold.title = "Add the thing"
	c.opts.ConnectCM = hold.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()
	in := startInput("BP1")

	driveIn(t, c, in)
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	receive(t, hold.entered, "the held card")

	require.NoError(t, c.Start(ctx, in))
	hold.free()
	c.Wait("BP1")

	assert.Equal(t, []string{"Add the thing"}, fake.titles())
	assert.Equal(t, 1, rec.count(EventEmitted))

	w := watcherOf(c, "BP1")
	require.NotNil(t, w, "the watch runs")

	require.NoError(t, c.Start(ctx, in))
	assert.Same(t, w, watcherOf(c, "BP1"), "no second watch")
}

func TestReopen_RestartsTheWatchWithoutEmittingAgain(t *testing.T) {
	c, st, _, _, ws := newPipeline(t, happyScript())
	c.opts.ConnectCM = newFakeCM().connect

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m3"))
	c.Wait("BP1")

	second := newFakeCM()
	second.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "todo"})
	_, rec2 := reopen(t, c, st, ws, happyScript(), second, startInput("BP1"))

	require.Eventually(t, func() bool { return second.getCount() > 0 }, time.Second, 5*time.Millisecond, "the watch polls")
	assert.Empty(t, second.titles())
	assert.False(t, rec2.has(EventEmitted))
	assert.False(t, rec2.has(EventSignOff))
}

func TestReopen_NeverStartsAYoloRunTwice(t *testing.T) {
	c, st, _, _, ws := newPipeline(t, happyScript())
	first := newFakeCM()
	c.opts.ConnectCM = first.connect

	in := yoloInput("BP1")
	driveIn(t, c, in)
	require.Len(t, first.cardRuns(), 1, "the first controller started the run")

	second := newFakeCM()
	second.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "in_progress"})
	reopen(t, c, st, ws, happyScript(), second, in)

	assert.Empty(t, second.cardRuns())
	assert.Empty(t, second.titles())
}

// A restart during the card writes of a yolo emission stops it before the
// run start, which the resumed emission makes once.
func TestReopen_AYoloRunStartARestartPreventedHappensOnce(t *testing.T) {
	c, st, _, rec, ws := newPipeline(t, happyScript())
	first := newFakeCM()
	hold := newHoldCM(t, first)
	hold.title = "Add the thing"
	c.opts.ConnectCM = hold.connect
	ctx := context.Background()
	in := yoloInput("BP1")

	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
	receive(t, hold.entered, "the held card")
	// Shutdown waits for the card, then stops before the run start.
	shutdownHeld(t, c, hold)

	require.Equal(t, []string{"Add the thing"}, first.titles())
	assert.Empty(t, first.cardRuns())
	assert.Equal(t, string(StateEmitting), stateOf(t, st, "BP1"), "the run start was not attempted")
	assert.False(t, rec.has(EventError), "no stop is reported")

	second := newFakeCM()
	second.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "todo"})
	c2, _, rec2 := restart(t, st, ws, happyScript(), second.connect, in)
	c2.Wait("BP1")

	assert.Equal(t, []string{"alpha/ALPHA-001|human:architect"}, second.cardRuns(), "the start happens once")
	assert.Empty(t, second.titles())
	assert.False(t, rec2.has(EventEmitted), "the cards were announced before the restart")
	assert.True(t, rec2.has(EventRunStarted))
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
}

// Shutdown waits for a run start in flight, as ContextMatrix may finish it:
// the start is recorded and announced once, no watch starts while closing,
// and the restart starts nothing more.
func TestReopen_ARunStartShutdownWaitedForIsRecordedOnce(t *testing.T) {
	cases := []struct {
		name   string
		script stageScript
		target string
		inCM   func(f *fakeCM)
	}{
		{
			name: "run_card", script: happyScript(), target: "ALPHA-001",
			inCM: func(f *fakeCM) {
				f.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "todo", WorkerStatus: "queued"})
			},
		},
		{
			name: "run_playbook", script: twoCardScript(), target: "ship-the-parser",
			inCM: func(f *fakeCM) {
				f.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "todo"})
				f.setCards("ALPHA-002", cm.Card{ID: "ALPHA-002", State: "todo"})
				f.setBooks("ship-the-parser", cm.Playbook{ID: "ship-the-parser", Total: 3, Run: &cm.PlaybookRun{Status: "running"}})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _, rec, ws := newPipeline(t, tc.script)
			first := newFakeCM()
			hold := newHoldCM(t, first)
			hold.run = true
			c.opts.ConnectCM = hold.connect
			ctx := context.Background()
			in := yoloInput("BP1")

			require.NoError(t, c.Start(ctx, in))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")
			require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
			receive(t, hold.entered, "the held run start")
			shutdownHeld(t, c, hold)

			assert.Len(t, slices.Concat(first.runCards, first.runBooks), 1, "the held write completes")

			starts := rec.all(EventRunStarted)
			require.Len(t, starts, 1, "the start is announced")
			assert.Equal(t, tc.target, starts[0].Payload["target"])
			assert.False(t, rec.has(EventError), "no stop is reported")
			assert.Nil(t, watcherOf(c, "BP1"), "no watch starts while closing")
			assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))

			second := newFakeCM()
			tc.inCM(second)
			c2, _, rec2 := restart(t, st, ws, tc.script, second.connect, in)
			c2.Wait("BP1")

			assert.Empty(t, slices.Concat(second.runCards, second.runBooks), "the restart starts nothing")
			assert.Empty(t, second.titles())
			assert.False(t, rec2.has(EventRunStarted))
			assert.NotNil(t, watcherOf(c2, "BP1"), "the restart watches the run")

			recs, err := st.ListEmissions(ctx, "BP1")
			require.NoError(t, err)

			runs := slices.DeleteFunc(recs, func(e store.Emission) bool { return e.Kind != store.EmitRun })
			require.Len(t, runs, 1, "the start is recorded once")
			assert.Equal(t, tc.target, runs[0].CMID)
		})
	}
}

func TestReopen_ARefusedYoloStartWaitsForApprove(t *testing.T) {
	c, st, _, _, ws := newPipeline(t, happyScript())
	first := newFakeCM()
	first.runErr = errors.New("cm: run_card: no execution backend is configured")
	c.opts.ConnectCM = first.connect
	ctx := context.Background()
	in := yoloInput("BP1")

	driveIn(t, c, in)
	require.Len(t, first.cardRuns(), 1)
	require.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))

	second := newFakeCM()
	second.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "todo"})
	c2, rec2 := reopen(t, c, st, ws, happyScript(), second, in)

	assert.Empty(t, second.cardRuns(), "a refused start is not retried on its own")
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))

	reminder, ok := rec2.first(EventAwaitInput)
	require.True(t, ok, "the reminder is posted")
	assert.Equal(t, signoffStage, reminder.Payload["stage"])
	assert.Contains(t, reminder.Payload["prompt"], "start it")

	require.NoError(t, c2.Message(ctx, "BP1", "approve", "m3"))
	c2.Wait("BP1")

	assert.Len(t, second.cardRuns(), 1, "approve retries the start")
	assert.Empty(t, second.titles())
	assert.False(t, rec2.has(EventEmitted))

	require.NoError(t, c2.Message(ctx, "BP1", "approve", "m4"))
	c2.Wait("BP1")

	assert.Len(t, second.cardRuns(), 1, "a started run is never started again")
}

func TestReopen_AnAutoStoppedSignoffIsNotApprovedOnReopen(t *testing.T) {
	c, st, _, rec, ws := newPipeline(t, happyScript())
	ws.heads = map[string]string{"https://example.com/alpha.git": "sha-new"}

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit
	driveIn(t, c, in)

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	require.Equal(t, true, so.Payload["auto_stopped"])

	// Preflight would be clean now; a human was asked, so only one decides.
	ws.mu.Lock()
	ws.heads = nil
	ws.mu.Unlock()

	second := newFakeCM()
	_, rec2 := reopen(t, c, st, ws, happyScript(), second, in)

	again, ok := rec2.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, true, again.Payload["auto_stopped"])
	assert.Empty(t, second.titles())
	assert.Zero(t, second.connectCount())

	gates, err := st.ListGates(context.Background(), "BP1", signoffStage)
	require.NoError(t, err)
	assert.Empty(t, gates)
}

func TestReopen_ADoneBlueprintStaysDone(t *testing.T) {
	c, st, _, _, ws := newPipeline(t, happyScript())
	c.opts.ConnectCM = newFakeCM().connect

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m3"))
	c.Wait("BP1")
	require.NoError(t, st.SetBlueprintState(context.Background(), "BP1", string(StateDone)))

	second := newFakeCM()
	c2, _ := reopen(t, c, st, ws, happyScript(), second, startInput("BP1"))

	assert.Nil(t, watcherOf(c2, "BP1"), "no watch for a finished run")
	assert.Zero(t, second.getCount())
	assert.Equal(t, string(StateDone), stateOf(t, st, "BP1"))
}

// Text other than approve while an approved emission is unfinished says how
// to finish it.
func TestSignoff_TextWhileEmissionIsUnfinishedHintsAtApprove(t *testing.T) {
	cases := []struct {
		name   string
		script stageScript
		in     StartInput
		inCM   func(f *fakeCM)
		hint   string
	}{
		{
			name: "a stopped emission", script: twoCardScript(), in: startInput("BP1"),
			inCM: func(f *fakeCM) { f.failCreate["Wire the parser"] = 1 },
			hint: "reply approve to resume emission",
		},
		{
			name: "a refused run start", script: happyScript(), in: yoloInput("BP1"),
			inCM: func(f *fakeCM) { f.runErr = errors.New("cm: run_card: no execution backend is configured") },
			hint: "reply approve to start the run",
		},
		{name: "a finished emission", script: happyScript(), in: startInput("BP1"), inCM: func(*fakeCM) {}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _, rec, _ := newPipeline(t, tc.script)
			fake := newFakeCM()
			tc.inCM(fake)
			c.opts.ConnectCM = fake.connect
			ctx := context.Background()

			driveIn(t, c, tc.in)

			if tc.in.Mode != protocol.ArchitectModeYolo {
				require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
				c.Wait("BP1")
			}

			titles := fake.titles()

			require.NoError(t, c.Message(ctx, "BP1", "what now?", "m4"))
			c.Wait("BP1")

			assert.Equal(t, titles, fake.titles(), "a note resumes nothing")

			note, ok := rec.first(EventNote)
			require.True(t, ok)

			if tc.hint == "" {
				assert.NotContains(t, note.Payload, "hint")

				return
			}

			assert.Contains(t, note.Payload["hint"], tc.hint)
		})
	}
}

// When what emission recorded cannot be read, text after the approval is a
// plain note: a hint could point the wrong way, and the error is logged.
func TestSignoff_TextWhileTheEmissionCannotBeReadIsAPlainNote(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "architect.db")

	st, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	c, _, _, rec, _ := newPipelineOn(t, st, twoCardScript(), nil)
	fake := newFakeCM()
	fake.failCreate["Wire the parser"] = 1
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")
	require.Equal(t, string(StateIdle), stateOf(t, st, "BP1"), "the emission stopped")

	// A second connection hides the emissions table, so reading what
	// emission recorded fails.
	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	_, err = raw.ExecContext(ctx, `ALTER TABLE emissions RENAME TO emissions_hidden`)
	require.NoError(t, err)

	require.NoError(t, c.Message(ctx, "BP1", "what now?", "m4"))
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.NotContains(t, note.Payload, "hint")
	assert.Equal(t, []string{"Add the parser"}, fake.titles(), "a note resumes nothing")
}

// newSetupPipeline is a pipeline whose project has no verify command in
// ContextMatrix and whose tree has no AGENTS.md, with cards that carry the
// setup.
func newSetupPipeline(t *testing.T) (*Controller, *store.Store, *recorder, *fakeCM) {
	t.Helper()

	script := happyScript()
	script["decomposition"] = returns("# Cards", setupCards())

	c, st, _, rec, ws := newPipeline(t, script)
	c.opts.Projects = listAlphaUnset
	ws.noAgents = true

	fake := newFakeCM()
	fake.projects = []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git"}}
	c.opts.ConnectCM = fake.connect

	return c, st, rec, fake
}

const verifyStep = "set verify command `go test ./...` on project alpha in ContextMatrix"

func TestSignoff_ListsTheVerifyStepAndMarksTheInstructionCard(t *testing.T) {
	c, _, rec, _ := newSetupPipeline(t)

	drive(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Empty(t, so.Payload["blocking"], "neither gap blocks")
	assert.Empty(t, so.Payload["warnings"])
	assert.Equal(t, []string{verifyStep}, so.Payload["steps"])

	cards, _ := so.Payload["cards"].([]any)
	require.Len(t, cards, 1)
	assert.Equal(t, "AGENTS.md", cards[0].(map[string]any)["writes"])
}

func TestSignoff_AutoEmitEmitsDespiteASetupStep(t *testing.T) {
	c, _, rec, fake := newSetupPipeline(t)

	in := startInput("BP1")
	in.Mode = protocol.ArchitectModeAutoEmit
	driveIn(t, c, in)

	assert.Equal(t, []string{"Add the thing"}, fake.titles(), "a step does not stop an emitting mode")
	assert.False(t, rec.has(EventSignOff))

	em, ok := rec.first(EventEmitted)
	require.True(t, ok)
	assert.Equal(t, []string{verifyStep}, em.Payload["steps"])
}

func TestSignoff_AVerifyCommandSetBeforeApproveDropsTheStep(t *testing.T) {
	c, _, rec, fake := newSetupPipeline(t)
	ctx := context.Background()

	drive(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []string{verifyStep}, so.Payload["steps"])

	// The user sets the verify command in ContextMatrix, then approves.
	fake.mu.Lock()
	fake.projects = []cm.Project{alphaProject()}
	fake.mu.Unlock()

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	em, ok := rec.first(EventEmitted)
	require.True(t, ok)
	assert.Empty(t, em.Payload["steps"], "emission reads ContextMatrix again")
}

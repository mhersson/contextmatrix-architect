package blueprint

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

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
	fake.projects = []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git"}}
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
	script["intake"] = returns(needBody, nil)

	c, _, _, rec, _ := newPipeline(t, script)

	drive(t, c, "BP1")

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, "- The parser stays in Go.", so.Payload["assumptions"])
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

func TestParseReject(t *testing.T) {
	cases := []struct {
		name, text, feedback string
		ok                   bool
	}{
		{"plain", "reject split it", "split it", true},
		{"multi-line", "reject\n- split D1\n- add tests", "- split D1\n- add tests", true},
		{"tab", "reject\tsplit it", "split it", true},
		{"bare", "reject", "", true},
		{"capitalised", "Reject split it", "split it", true},
		{"longer word", "rejected foo", "", false},
		{"other", "approve", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feedback, ok := parseReject(tc.text)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.feedback, feedback)
		})
	}
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

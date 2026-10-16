package blueprint

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// The session's secrets: the endpoint API key, the MCP key and the
// git-credentials token. Each must be masked everywhere the record and the
// panel carry text.
const (
	testKey      = "sk-secret-0123456789"
	testMCPKey   = "mcp-key-0123456789"
	testGitToken = "BP.mac"
)

type fakeRunner struct {
	mu     sync.Mutex
	inputs []stage.Input
	fn     func(ctx context.Context, in stage.Input) (stage.Output, error)
}

func (f *fakeRunner) Run(ctx context.Context, in stage.Input) (stage.Output, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, in)
	f.mu.Unlock()

	return f.fn(ctx, in)
}

func (f *fakeRunner) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.inputs)
}

func (f *fakeRunner) input(i int) stage.Input {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.inputs[i]
}

type recorder struct {
	mu      sync.Mutex
	events  []Event
	lines   [][]byte
	changed chan struct{}
}

func newRecorder() *recorder {
	return &recorder{changed: make(chan struct{})}
}

func (r *recorder) sink(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, e)
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *recorder) line(_ string, p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines = append(r.lines, bytes.Clone(p))
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}

	return out
}

func (r *recorder) has(kind string) bool {
	return r.count(kind) > 0
}

func (r *recorder) count(kind string) int {
	n := 0

	for _, k := range r.kinds() {
		if k == kind {
			n++
		}
	}

	return n
}

func (r *recorder) first(kind string) (Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, e := range r.events {
		if e.Kind == kind {
			return e, true
		}
	}

	return Event{}, false
}

// all returns every recorded event of kind, oldest first.
func (r *recorder) all(kind string) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []Event

	for _, e := range r.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}

	return out
}

// waitFor blocks until an event of kind has reached the sink.
func (r *recorder) waitFor(t *testing.T, kind string) {
	t.Helper()

	timeout := time.After(5 * time.Second)

	for {
		r.mu.Lock()
		changed := r.changed
		found := slices.ContainsFunc(r.events, func(e Event) bool { return e.Kind == kind })
		r.mu.Unlock()

		if found {
			return
		}

		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("no %s event; saw %v", kind, r.kinds())
		}
	}
}

func artifactOutput(body string, in stage.Input) stage.Output {
	return stage.Output{
		Artifact: stage.Artifact{Body: body, Data: map[string]any{"slices": 1}},
		Result: harness.Result{
			Completed: true, Reason: "done", TotalCostUSD: 0.5,
			Messages: []llm.Message{{Role: "user", Content: in.Task}},
		},
	}
}

func artifactRun(body string) func(context.Context, stage.Input) (stage.Output, error) {
	return func(_ context.Context, in stage.Input) (stage.Output, error) {
		// The real runner streams harness JSONL here; one line is enough to
		// prove it lands in the audit log.
		_, _ = in.Events.Write([]byte(`{"seq":1,"kind":"model_response","data":{"content":"hi"}}` + "\n"))

		return artifactOutput(body, in), nil
	}
}

// receive waits for one signal on ch, failing the test at a deadline.
func receive(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// signal is a non-blocking send, so a fake that runs more often than a test
// expects reports it through assertions instead of blocking.
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// closer returns a func that closes ch once, and runs it when the test ends,
// so a fake that waits on ch never outlives a failing test. Called after the
// controller is built, it runs before the controller's Shutdown waits.
func closer(t *testing.T, ch chan struct{}) func() {
	t.Helper()

	var once sync.Once

	closeCh := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(closeCh)

	return closeCh
}

func newController(t *testing.T, fn func(context.Context, stage.Input) (stage.Output, error)) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	return newControllerOn(t, st, fn)
}

func newControllerOn(t *testing.T, st *store.Store, fn func(context.Context, stage.Input) (stage.Output, error)) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	// One stage: nothing follows intake, so these tests see a single run.
	pk, err := pack.LoadDir("testdata/single")
	require.NoError(t, err)

	runner := &fakeRunner{fn: fn}
	rec := newRecorder()
	c := New(Options{
		Store: st, Pack: pk, Runner: runner, MaxRunCostUSD: 2, MaxTurns: 10,
		Sink: rec.sink, HarnessLine: rec.line,
	})

	return c, st, runner, rec
}

func testPrice() *protocol.ModelPrice {
	// 1 USD per million prompt tokens and 10 per million completion tokens:
	// 100k prompt + 10k completion tokens cost exactly 0.20 USD.
	return &protocol.ModelPrice{PromptUSDPerMTok: 1, CompletionUSDPerMTok: 10, CacheReadUSDPerMTok: 0.1, CacheWriteUSDPerMTok: 1.25}
}

func newControllerWithRunCap(t *testing.T, runCap float64, fn func(context.Context, stage.Input) (stage.Output, error)) (*Controller, *store.Store, *fakeRunner, *recorder) {
	t.Helper()

	c, st, runner, rec := newController(t, fn)
	c.opts.MaxRunCostUSD = runCap

	return c, st, runner, rec
}

func startInput(id string) StartInput {
	return StartInput{
		ID: id, Projects: []string{"alpha"}, Mode: "default", CostCapUSD: 0, Model: "session/model",
		Endpoint:  protocol.LLMEndpoint{Type: "openrouter", APIKey: testKey},
		MCPAPIKey: testMCPKey, GitCredentialsToken: testGitToken,
	}
}

func TestStart_CreatesOnceThenReopens(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha"}, bp.Projects)
	assert.Equal(t, string(StateNew), bp.State)
	assert.Equal(t, []string{EventCreated, EventEnded, EventReopened}, rec.kinds())
}

func TestStart_OnAHeldSessionIsSilent(t *testing.T) {
	c, st, runner, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))

	before, err := st.ListEvents(ctx, "BP1")
	require.NoError(t, err)

	runsBefore, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	published := len(rec.kinds())

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	after, err := st.ListEvents(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, after, len(before), "no event is stored")
	assert.Len(t, rec.kinds(), published, "nothing reaches the panel")

	runsAfter, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, runsBefore, runsAfter, "no run is started")
	assert.Zero(t, runner.calls(), "the model is never called")
}

func TestMessage_FirstMessageIsTheNeedAndRunsIntake(t *testing.T) {
	c, st, runner, rec := newController(t, artifactRun("# Need\n\nk8s"))
	c.opts.Models = map[string]string{"product_owner": "po/model"}
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.NoError(t, c.Message(ctx, "BP1", "containerise this project", "m1"))
	c.Wait("BP1")

	require.Equal(t, 1, runner.calls())
	in := runner.input(0)
	assert.Equal(t, "intake", in.Stage)
	assert.Contains(t, in.Task, "containerise this project")
	assert.Equal(t, "po/model", in.Model, "persona override wins over the session model")
	assert.Equal(t, testKey, in.Endpoint.APIKey)
	assert.Subset(t, in.Secrets, []string{testKey, testMCPKey, testGitToken}, "the harness masks every session secret in tool output")
	assert.InDelta(t, 2.0, in.MaxCostUSD, 1e-9)
	assert.Equal(t, 10, in.MaxTurns)

	need, err := st.LatestArtifact(ctx, "BP1", "need")
	require.NoError(t, err)
	assert.Equal(t, "containerise this project", need.Body)

	art, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.Equal(t, "# Need\n\nk8s", art.Body)
	assert.JSONEq(t, `{"slices":1}`, string(art.Data))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunDone, runs[0].Status)
	assert.InDelta(t, 0.5, runs[0].Usage.CostUSD, 1e-9)

	tr, err := st.GetTranscript(ctx, runs[0].ID)
	require.NoError(t, err)
	assert.Contains(t, string(tr), "containerise this project")

	assert.Equal(t, []string{EventCreated, EventStageStarted, EventArtifact, EventStageEnded, EventReport}, rec.kinds(),
		"nothing follows intake in a one-stage pack, so the pipeline reports")

	started, ok := rec.first(EventStageStarted)
	require.True(t, ok)
	assert.Equal(t, c.opts.Pack.Name, started.Payload["pack"], "every run logs the pack name")
	assert.Equal(t, c.opts.Pack.Version, started.Payload["pack_version"], "and the pack version")

	stored, err := st.ListEvents(ctx, "BP1")
	require.NoError(t, err)

	var harnessKinds []string

	for _, e := range stored {
		if e.Kind == EventHarness {
			harnessKinds = append(harnessKinds, e.Kind)
		}
	}

	assert.Len(t, harnessKinds, 1, "harness JSONL lines are audited")
	assert.Len(t, rec.lines, 1, "and forwarded to the bridge")
}

func TestMessage_OnIdleBlueprintIsANote(t *testing.T) {
	c, _, runner, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.NoError(t, c.Message(ctx, "BP1", "also use helm", "m2"))
	c.Wait("BP1")
	assert.Equal(t, 1, runner.calls())

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, false, note.Payload["during_run"], "no run is in flight")
}

func TestMessage_NoteWhileAStageRunsSaysSo(t *testing.T) {
	started := make(chan struct{}, 4)
	finish := make(chan struct{})
	c, _, _, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		signal(started)
		<-finish

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "the intake run")

	require.NoError(t, c.Message(ctx, "BP1", "also use helm", "m2"))
	close(finish)
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, true, note.Payload["during_run"], "the run continues after the note")
}

func TestMessage_AfterAFailedIntakeIsTheNeed(t *testing.T) {
	c, st, runner, _ := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		if strings.Contains(in.Task, "first need") {
			return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("llm: 502 from provider")
		}

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "first need", "m1"))
	c.Wait("BP1")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	require.Equal(t, string(StateFailed), bp.State)

	require.NoError(t, c.Message(ctx, "BP1", "second need", "m2"))
	c.Wait("BP1")

	require.Equal(t, 2, runner.calls(), "the failed intake left no artifact, so the message is the need")
	assert.Contains(t, runner.input(1).Task, "second need")
	assert.NotContains(t, runner.input(1).Task, "first need")

	need, err := st.LatestArtifact(ctx, "BP1", "need")
	require.NoError(t, err)
	assert.Equal(t, 2, need.Version)
	assert.Equal(t, "second need", need.Body)

	_, err = st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err)

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunFailed, runs[0].Status)
	assert.Equal(t, store.RunDone, runs[1].Status)
	assert.Equal(t, 2, runs[1].Attempt)
}

func TestFirstStageTask_CarriesTheProjectsAndTheNeed(t *testing.T) {
	projects := []string{"alpha", "beta"}

	tests := []struct {
		name string
		need string
		run  func(t *testing.T, c *Controller, st *store.Store)
	}{
		{
			name: "first run",
			need: "containerise both",
			run: func(t *testing.T, c *Controller, _ *store.Store) {
				t.Helper()

				in := startInput("BP1")
				in.Projects = projects
				require.NoError(t, c.Start(context.Background(), in))
				require.NoError(t, c.Message(context.Background(), "BP1", "containerise both", "m1"))
			},
		},
		{
			name: "recovery",
			need: "the stored need",
			run: func(t *testing.T, c *Controller, st *store.Store) {
				t.Helper()

				ctx := context.Background()
				now := time.Now().UTC()
				require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{
					ID: "BP1", Projects: projects, State: string(StateRunning), CreatedAt: now, UpdatedAt: now,
				}))
				_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "need", Body: "the stored need"})
				require.NoError(t, err)
				_, err = st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
				require.NoError(t, err)
				require.NoError(t, c.Start(ctx, startInput("BP1")))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, _ := newController(t, artifactRun("# Need"))
			tt.run(t, c, st)
			c.Wait("BP1")

			require.Equal(t, 1, runner.calls())

			task := runner.input(0).Task
			for _, p := range projects {
				assert.Contains(t, task, p)
			}

			assert.Contains(t, task, tt.need)
		})
	}
}

func TestMessage_AnswersAPendingQuestion(t *testing.T) {
	asked := make(chan struct{})
	c, st, _, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		close(asked)

		answer, err := in.Asker.Ask(ctx, stage.Question{Text: "Helm or kustomize?", Options: []string{"helm", "kustomize"}})
		if err != nil {
			return stage.Output{}, err
		}

		return stage.Output{
			Artifact: stage.Artifact{Body: "# Need\n\n" + answer},
			Result:   harness.Result{Completed: true, Reason: "done"},
		}, nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))

	<-asked
	rec.waitFor(t, EventQuestion)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateWaiting), bp.State)

	require.NoError(t, c.Message(ctx, "BP1", "helm", "m2"))
	c.Wait("BP1")

	art, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.Equal(t, "# Need\n\nhelm", art.Body)
	assert.True(t, rec.has(EventAnswer))
}

func TestAbort_CancelsTheRun(t *testing.T) {
	started := make(chan struct{})
	c, st, _, rec := newController(t, func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		close(started)
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	<-started

	require.NoError(t, c.Abort(ctx, "BP1", "m2"))
	c.Wait("BP1")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunCanceled, runs[0].Status)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)
	assert.True(t, rec.has(EventAborted))
}

func TestMessage_ConcurrentMessagesStartOneIntake(t *testing.T) {
	finish := make(chan struct{})
	c, st, runner, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		<-finish

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	const n = 8

	var wg sync.WaitGroup

	gate := make(chan struct{})

	for i := range n {
		wg.Go(func() {
			<-gate
			assert.NoError(t, c.Message(ctx, "BP1", fmt.Sprintf("message %d", i), fmt.Sprintf("m%d", i)))
		})
	}

	close(gate)
	wg.Wait()
	close(finish)
	c.Wait("BP1")

	assert.Equal(t, 1, runner.calls(), "one intake run")

	need, err := st.LatestArtifact(ctx, "BP1", "need")
	require.NoError(t, err)
	assert.Equal(t, 1, need.Version, "one need")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	assert.Len(t, runs, 1)
	assert.Equal(t, n-1, rec.count(EventNote), "every other message is a note")
}

func TestStart_OnAnAttachedSessionDoesNotRelaunchItsRun(t *testing.T) {
	started := make(chan struct{}, 4)
	finish := make(chan struct{})
	c, st, runner, _ := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		signal(started)
		<-finish

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	<-started

	require.NoError(t, c.Start(ctx, startInput("BP1")))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunRunning, runs[0].Status, "the live run is not marked interrupted")

	close(finish)
	c.Wait("BP1")

	assert.Equal(t, 1, runner.calls())

	runs, err = st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1, "no second run")
	assert.Equal(t, store.RunDone, runs[0].Status)
}

func TestEnd_CancelsTheRunAndKeepsTheBlueprint(t *testing.T) {
	started := make(chan struct{}, 4)
	canceled := make(chan struct{}, 4)
	finish := make(chan struct{})
	c, st, runner, rec := newController(t, func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		signal(started)
		<-ctx.Done()
		signal(canceled)
		<-finish

		return stage.Output{}, ctx.Err()
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	<-started

	require.NoError(t, c.End(ctx, "BP1"))
	<-canceled

	require.ErrorIs(t, c.Message(ctx, "BP1", "hello", "m2"), ErrNotFound, "no session after End")
	require.ErrorIs(t, c.End(ctx, "nope"), ErrNotFound)

	// Reopen while the canceled run is still ending: it is neither marked
	// interrupted nor launched again.
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunRunning, runs[0].Status)

	close(finish)
	c.Wait("BP1")

	runs, err = st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1, "no second run")
	assert.Equal(t, store.RunCanceled, runs[0].Status)
	assert.Equal(t, 1, runner.calls())

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err, "the blueprint persists after End")
	assert.Equal(t, string(StateIdle), bp.State)
	assert.Equal(t, 1, rec.count(EventCreated), "the reopen did not create a second blueprint")
	assert.True(t, rec.has(EventReopened))

	// The canceled intake left no artifact, so the next message is the need.
	require.NoError(t, c.Message(ctx, "BP1", "hello again", "m3"), "the reopened session is attached")
	receive(t, started, "a fresh intake run")
	assert.False(t, rec.has(EventNote))
	require.Equal(t, 2, runner.calls())
	assert.Contains(t, runner.input(1).Task, "hello again")

	need, err := st.LatestArtifact(ctx, "BP1", "need")
	require.NoError(t, err)
	assert.Equal(t, 2, need.Version)

	require.NoError(t, c.End(ctx, "BP1"))
	c.Wait("BP1")
	assert.Equal(t, 2, rec.count(EventEnded), "every End is audited")
}

func TestCancel_WhileParkedInAskHumanUnblocksTheRun(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(ctx context.Context, c *Controller) error
	}{
		{"abort", func(ctx context.Context, c *Controller) error { return c.Abort(ctx, "BP1", "m2") }},
		{"end", func(ctx context.Context, c *Controller) error { return c.End(ctx, "BP1") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			askErr := make(chan error, 4)
			c, st, _, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
				answer, err := in.Asker.Ask(ctx, stage.Question{Text: "Helm or kustomize?"})
				askErr <- err

				if err != nil {
					return stage.Output{}, err
				}

				return artifactOutput("# Need\n\n"+answer, in), nil
			})
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			rec.waitFor(t, EventQuestion)

			require.NoError(t, tt.cancel(ctx, c))
			c.Wait("BP1")

			require.ErrorIs(t, <-askErr, context.Canceled, "ask_human returns an error, not an answer")
			assert.False(t, rec.has(EventAnswer))

			runs, err := st.ListRuns(ctx, "BP1")
			require.NoError(t, err)
			require.Len(t, runs, 1)
			assert.Equal(t, store.RunCanceled, runs[0].Status)

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)
			assert.Equal(t, string(StateIdle), bp.State)

			_, err = st.LatestArtifact(ctx, "BP1", "intake")
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestStart_RerunsAStageInterruptedByACrash(t *testing.T) {
	c, st, runner, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{
		ID: "BP1", Projects: []string{"alpha"},
		Model: "m", State: string(StateRunning), CreatedAt: now, UpdatedAt: now,
	}))
	_, err := st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "need", Body: "the stored need"})
	require.NoError(t, err)
	oldRun, err := st.StartRun(ctx, store.Run{
		BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1,
		Status: store.RunRunning, StartedAt: now,
	})
	require.NoError(t, err)

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 1, runner.calls())
	assert.Contains(t, runner.input(0).Task, "the stored need")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, oldRun, runs[0].ID)
	assert.Equal(t, store.RunInterrupted, runs[0].Status)
	assert.Equal(t, store.RunDone, runs[1].Status)
	assert.Equal(t, 2, runs[1].Attempt)
}

func TestStart_RecoveryWithoutANeedClosesTheRun(t *testing.T) {
	c, st, runner, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{
		ID: "BP1", Projects: []string{"alpha"},
		Model: "m", State: string(StateRunning), CreatedAt: now, UpdatedAt: now,
	}))
	_, err := st.StartRun(ctx, store.Run{
		BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1,
		Status: store.RunRunning, StartedAt: now,
	})
	require.NoError(t, err)

	require.NoError(t, c.Start(ctx, startInput("BP1")), "a run with nothing to re-run from does not wedge the blueprint")
	c.Wait("BP1")
	assert.Equal(t, 0, runner.calls(), "nothing is relaunched")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunInterrupted, runs[0].Status)

	// The session is attached and the next message is the need.
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	require.Equal(t, 1, runner.calls())
	assert.Contains(t, runner.input(0).Task, "the need")

	runs, err = st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunDone, runs[1].Status)
}

func TestStart_TransientRecoveryErrorLeavesTheRunRecoverable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "architect.db")
	st, err := store.Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	c, _, runner, _ := newControllerOn(t, st, artifactRun("# Need"))
	now := time.Now().UTC()

	require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{
		ID: "BP1", Projects: []string{"alpha"},
		Model: "m", State: string(StateRunning), CreatedAt: now, UpdatedAt: now,
	}))
	_, err = st.PutArtifact(ctx, store.Artifact{BlueprintID: "BP1", Stage: "need", Body: "the stored need"})
	require.NoError(t, err)
	_, err = st.StartRun(ctx, store.Run{
		BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1,
		Status: store.RunRunning, StartedAt: now,
	})
	require.NoError(t, err)

	// A second connection hides the artifacts table, so loading the need
	// fails with an error that is not "not found".
	raw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	_, err = raw.ExecContext(ctx, `ALTER TABLE artifacts RENAME TO artifacts_hidden`)
	require.NoError(t, err)

	require.Error(t, c.Start(ctx, startInput("BP1")))
	require.ErrorIs(t, c.Message(ctx, "BP1", "hello", "m1"), ErrNotFound, "a failed recovery attaches no session")
	assert.Equal(t, 0, runner.calls())

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunRunning, runs[0].Status, "not marked interrupted while nothing can be relaunched")

	_, err = raw.ExecContext(ctx, `ALTER TABLE artifacts_hidden RENAME TO artifacts`)
	require.NoError(t, err)

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 1, runner.calls())
	assert.Contains(t, runner.input(0).Task, "the stored need")

	runs, err = st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunInterrupted, runs[0].Status)
	assert.Equal(t, store.RunDone, runs[1].Status)
}

func TestRunCapZero_DisablesThePerRunCap(t *testing.T) {
	tests := []struct {
		name       string
		capUSD     float64
		spentUSD   float64
		wantRunCap float64
		wantRefuse bool
	}{
		{name: "no caps at all runs unbounded", capUSD: 0, wantRunCap: 0},
		{name: "blueprint cap alone bounds the run by what is left", capUSD: 1.5, spentUSD: 0.5, wantRunCap: 1},
		{name: "an exhausted blueprint cap is refused, not read as no cap", capUSD: 1, spentUSD: 1, wantRefuse: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := newControllerWithRunCap(t, 0, artifactRun("# Need"))
			ctx := context.Background()
			in := startInput("BP1")
			in.CostCapUSD = tt.capUSD
			in.Price = testPrice()
			require.NoError(t, c.Start(ctx, in))

			if tt.spentUSD > 0 {
				now := time.Now().UTC()
				prior, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
				require.NoError(t, err)
				require.NoError(t, st.EndRun(ctx, prior, store.RunDone, "done", store.RunUsage{CostUSD: tt.spentUSD}, now))
			}

			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")

			if tt.wantRefuse {
				assert.Zero(t, runner.calls(), "no run starts")
				assert.True(t, rec.has(EventError))

				return
			}

			require.Equal(t, 1, runner.calls(), "the run starts")
			assert.InDelta(t, tt.wantRunCap, runner.input(0).MaxCostUSD, 1e-9)
			assert.False(t, rec.has(EventError))
		})
	}
}

func TestMessage_CostCap(t *testing.T) {
	tests := []struct {
		name       string
		capUSD     float64
		spentUSD   float64
		noPrice    bool
		wantRun    bool
		wantRunCap float64
	}{
		{name: "nothing left is refused", capUSD: 1, spentUSD: 1},
		{name: "less left than the run cap runs clamped", capUSD: 1.5, spentUSD: 0.5, wantRun: true, wantRunCap: 1},
		{name: "capped without a price is refused", capUSD: 1, spentUSD: 0, noPrice: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := newController(t, artifactRun("# Need"))
			ctx := context.Background()
			in := startInput("BP1")
			in.CostCapUSD = tt.capUSD

			if !tt.noPrice {
				in.Price = testPrice()
			}

			require.NoError(t, c.Start(ctx, in))

			now := time.Now().UTC()
			prior, err := st.StartRun(ctx, store.Run{
				BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1,
				Status: store.RunRunning, StartedAt: now,
			})
			require.NoError(t, err)
			require.NoError(t, st.EndRun(ctx, prior, store.RunDone, "done", store.RunUsage{CostUSD: tt.spentUSD}, now))

			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)
			assert.Equal(t, string(StateIdle), bp.State)

			if !tt.wantRun {
				assert.Equal(t, 0, runner.calls())

				ev, ok := rec.first(EventError)
				require.True(t, ok)
				assert.Equal(t, "m1", ev.Payload["message_id"], "the refused need stays in the audit log")
				assert.Equal(t, "the need", ev.Payload["need"])

				if tt.noPrice {
					assert.Contains(t, ev.Payload["message"], "session/model", "the refusal names the model")
				}

				return
			}

			require.Equal(t, 1, runner.calls())
			assert.InDelta(t, tt.wantRunCap, runner.input(0).MaxCostUSD, 1e-9, "the run cap is what is left of the blueprint cap")
			assert.False(t, rec.has(EventError))
		})
	}
}

func TestRun_CostIsPricedFromTokensWhenTheEndpointReportsNone(t *testing.T) {
	c, st, _, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		out := artifactOutput("# Need", in)
		out.Result.TotalCostUSD = 0
		out.Result.PromptTokens = 100_000
		out.Result.CompletionTokens = 10_000

		return out, nil
	})
	ctx := context.Background()
	in := startInput("BP1")
	in.Price = testPrice()
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.InDelta(t, 0.20, runs[0].Usage.CostUSD, 1e-9)
	assert.Equal(t, int64(100_000), runs[0].Usage.PromptTokens)

	ended, ok := rec.first(EventStageEnded)
	require.True(t, ok)
	assert.Equal(t, true, ended.Payload["priced"])
	assert.InDelta(t, 0.20, ended.Payload["cost_usd"], 1e-9)
}

func TestRun_ReportedCostWinsWhenHigher(t *testing.T) {
	c, st, _, _ := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		out := artifactOutput("# Need", in)
		out.Result.TotalCostUSD = 0.5
		out.Result.PromptTokens = 1000

		return out, nil
	})
	ctx := context.Background()
	in := startInput("BP1")
	in.Price = testPrice()
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.InDelta(t, 0.5, runs[0].Usage.CostUSD, 1e-9)
}

func TestRun_StopsWhenPricedUsagePassesTheRunCap(t *testing.T) {
	// The fake model "spends" through usage lines and then blocks until the
	// run is stopped, as a real model call would be cut off by the context.
	c, st, _, rec := newControllerWithRunCap(t, 0.5, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		// 0.20 and 0.40 USD are under the 0.5 cap; 0.60 passes it.
		var stopped []bool

		for range 3 {
			_, _ = in.Events.Write([]byte(`{"seq":1,"kind":"usage","data":{"prompt_tokens":100000,"completion_tokens":10000,"cost_usd":0}}` + "\n"))

			stopped = append(stopped, ctx.Err() != nil)
		}

		assert.Equal(t, []bool{false, false, true}, stopped, "the run stops on the usage line that passes the cap")

		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			return stage.Output{}, errors.New("run was not stopped by the cap")
		}

		// The harness reports the cut-off model call as an error line.
		_, _ = in.Events.Write([]byte(`{"seq":4,"kind":"error","data":{"error":"context canceled"}}` + "\n"))

		return stage.Output{Result: harness.Result{Reason: "error", PromptTokens: 300_000, CompletionTokens: 30_000}}, ctx.Err()
	})
	ctx := context.Background()
	in := startInput("BP1")
	in.Price = testPrice()
	in.CostCapUSD = 10
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunFailed, runs[0].Status)
	assert.Equal(t, "max_cost", runs[0].Reason)
	assert.InDelta(t, 0.60, runs[0].Usage.CostUSD, 1e-9)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "run cost cap")

	rec.mu.Lock()
	defer rec.mu.Unlock()

	for _, l := range rec.lines {
		assert.NotContains(t, string(l), `"kind":"error"`, "the cut-off call's cancel error is not forwarded")
	}
}

func TestMessage_NeedRefusedWhenTheCapIsSpent(t *testing.T) {
	c, st, runner, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	in := startInput("BP1")
	in.CostCapUSD = 1
	in.Price = testPrice()
	require.NoError(t, c.Start(ctx, in))

	now := time.Now().UTC()
	prior, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
	require.NoError(t, err)
	require.NoError(t, st.EndRun(ctx, prior, store.RunDone, "done", store.RunUsage{CostUSD: 1}, now))

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	assert.Equal(t, 0, runner.calls())
	assert.True(t, rec.has(EventError))

	_, err = st.LatestArtifact(ctx, "BP1", needStage)
	assert.ErrorIs(t, err, store.ErrNotFound, "a refused need is not stored as a version")
}

func TestMessage_CappedWithoutPriceStoresNoNeed(t *testing.T) {
	c, st, runner, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	in := startInput("BP1")
	in.CostCapUSD = 1
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	assert.Equal(t, 0, runner.calls())
	assert.True(t, rec.has(EventError))

	_, err := st.LatestArtifact(ctx, "BP1", needStage)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestRunError_IsRecorded(t *testing.T) {
	tests := []struct {
		name        string
		reason      string
		line        string
		wantState   State
		wantMessage string
		forwarded   bool
	}{
		{
			name: "a failed run", reason: "max_turns",
			line:      `{"seq":1,"kind":"error","data":{"message":"boom"}}`,
			wantState: StateFailed, wantMessage: "stage intake: no artifact", forwarded: true,
		},
		{
			// The harness's own cap is the same condition as the sink's stop.
			name: "the harness's own cost cap", reason: "max_cost",
			line:      `{"seq":1,"kind":"state_change","data":{"stop":"max_cost","cost_usd":2.1}}`,
			wantState: StateIdle, wantMessage: "run cost cap 2.00 USD reached",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
				_, _ = in.Events.Write([]byte(tt.line + "\n"))

				return stage.Output{Result: harness.Result{Reason: tt.reason, TotalCostUSD: 2.1, Messages: []llm.Message{{Role: "user", Content: "x"}}}},
					errors.New("stage intake: no artifact")
			})
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")

			runs, err := st.ListRuns(ctx, "BP1")
			require.NoError(t, err)
			require.Len(t, runs, 1)
			assert.Equal(t, store.RunFailed, runs[0].Status)
			assert.Equal(t, tt.reason, runs[0].Reason)

			_, err = st.GetTranscript(ctx, runs[0].ID)
			require.NoError(t, err, "a failed run still stores its transcript")

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)
			assert.Equal(t, string(tt.wantState), bp.State)

			ev, ok := rec.first(EventError)
			require.True(t, ok)
			assert.Contains(t, ev.Payload["message"], tt.wantMessage)
			assert.Equal(t, 1, rec.count(EventError))

			stored, err := st.ListEvents(ctx, "BP1")
			require.NoError(t, err)

			var audited int

			for _, e := range stored {
				if e.Kind == EventHarness {
					audited++
				}
			}

			assert.Equal(t, 1, audited, "harness events are audited even when the run errors")

			rec.mu.Lock()
			defer rec.mu.Unlock()

			var forwardedError bool

			for _, l := range rec.lines {
				if strings.Contains(string(l), `"kind":"error"`) {
					forwardedError = true
				}
			}

			assert.Equal(t, tt.forwarded, forwardedError)
		})
	}
}

func TestRun_PersonaOverrideIsUnpriced(t *testing.T) {
	tests := []struct {
		name      string
		override  string
		capUSD    float64
		wantRun   bool
		wantModel string
		wantCost  float64
		priced    bool
	}{
		{name: "capped blueprint refuses an overridden persona", override: "po/model", capUSD: 10},
		{name: "uncapped blueprint runs it unpriced", override: "po/model", wantRun: true, wantModel: "po/model", wantCost: 0.05},
		{name: "an override naming the session model keeps its price", override: "session/model", capUSD: 10, wantRun: true, wantModel: "session/model", wantCost: 0.20, priced: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
				out := artifactOutput("# Need", in)
				out.Result.TotalCostUSD = 0.05
				out.Result.PromptTokens = 100_000
				out.Result.CompletionTokens = 10_000

				return out, nil
			})
			c.opts.Models = map[string]string{"product_owner": tt.override}
			ctx := context.Background()
			in := startInput("BP1")
			in.Price = testPrice()
			in.CostCapUSD = tt.capUSD
			require.NoError(t, c.Start(ctx, in))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")

			runs, err := st.ListRuns(ctx, "BP1")
			require.NoError(t, err)

			if !tt.wantRun {
				assert.Equal(t, 0, runner.calls())
				assert.Empty(t, runs)

				ev, ok := rec.first(EventError)
				require.True(t, ok)
				assert.Contains(t, ev.Payload["message"], tt.override, "the refusal names the model")

				bp, err := st.GetBlueprint(ctx, "BP1")
				require.NoError(t, err)
				assert.Equal(t, string(StateIdle), bp.State)

				return
			}

			require.Len(t, runs, 1)
			assert.Equal(t, tt.wantModel, runs[0].Model)
			assert.InDelta(t, tt.wantCost, runs[0].Usage.CostUSD, 1e-9)

			ended, ok := rec.first(EventStageEnded)
			require.True(t, ok)
			assert.Equal(t, tt.priced, ended.Payload["priced"])
		})
	}
}

func TestRun_ArtifactSubmittedAsTheSessionEndsIsKept(t *testing.T) {
	started := make(chan struct{}, 4)
	c, st, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		signal(started)
		<-ctx.Done()

		// The run submitted before it saw the cancel.
		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	<-started

	require.NoError(t, c.End(ctx, "BP1"))
	c.Wait("BP1")

	art, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err, "a completed artifact is never lost")
	assert.Equal(t, "# Need", art.Body)

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunDone, runs[0].Status)
}

func TestSecrets_AreMaskedInTheRecordAndTheSink(t *testing.T) {
	const mask = "[REDACTED]"

	secrets := []string{testKey, testMCPKey, testGitToken}
	// leak carries every session secret, the way a model might echo them.
	leak := strings.Join(secrets, " ")

	tests := []struct {
		name   string
		answer bool
		run    func(context.Context, stage.Input) (stage.Output, error)
	}{
		{
			name:   "question, harness line, transcript and artifact",
			answer: true,
			run: func(ctx context.Context, in stage.Input) (stage.Output, error) {
				_, _ = in.Events.Write([]byte(`{"seq":1,"kind":"model_response","data":{"content":"keys ` + leak + `"}}` + "\n"))

				answer, err := in.Asker.Ask(ctx, stage.Question{
					Text: "Are " + leak + " yours?", Options: append([]string{"no"}, secrets...), Context: "seen " + leak,
				})
				if err != nil {
					return stage.Output{}, err
				}

				return stage.Output{
					Artifact: stage.Artifact{
						Body: "# Need\n\nuse " + leak + ", " + answer,
						Data: map[string]any{"key": leak, "list": []any{testKey, testMCPKey, testGitToken}},
					},
					Result: harness.Result{
						Completed: true, Reason: "done",
						Messages: []llm.Message{{Role: "assistant", Content: "the keys are " + leak}},
					},
				}, nil
			},
		},
		{
			name: "runner error",
			run: func(_ context.Context, _ stage.Input) (stage.Output, error) {
				return stage.Output{Result: harness.Result{
						Reason:   "error",
						Messages: []llm.Message{{Role: "assistant", Content: "the keys are " + leak}},
					}},
					fmt.Errorf("llm: 401 from provider: invalid keys %s", leak)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, runner, rec := newController(t, tt.run)
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "deploy with "+leak, "m1"))

			if tt.answer {
				rec.waitFor(t, EventQuestion)
				require.NoError(t, c.Message(ctx, "BP1", "yes, "+leak, "m2"))
			}

			c.Wait("BP1")

			require.Equal(t, 1, runner.calls())

			for _, secret := range secrets {
				assert.NotContains(t, runner.input(0).Task, secret, "the task is the stored, masked need")
			}

			// Every place the run put the secrets, by name.
			places := map[string]string{}

			stored, err := st.ListEvents(ctx, "BP1")
			require.NoError(t, err)

			var storedText []string
			for _, e := range stored {
				storedText = append(storedText, string(e.Payload))
			}

			places["stored events"] = strings.Join(storedText, "\n")

			var sinkText []string

			rec.mu.Lock()
			for _, e := range rec.events {
				b, err := json.Marshal(e.Payload)
				require.NoError(t, err)

				sinkText = append(sinkText, string(b))
			}

			for _, l := range rec.lines {
				places["forwarded harness lines"] += string(l) + "\n"
			}
			rec.mu.Unlock()

			places["sink events"] = strings.Join(sinkText, "\n")

			need, err := st.LatestArtifact(ctx, "BP1", "need")
			require.NoError(t, err)

			places["need"] = need.Body

			if art, err := st.LatestArtifact(ctx, "BP1", "intake"); err == nil {
				places["artifact"] = art.Body + "\n" + string(art.Data)
			}

			runs, err := st.ListRuns(ctx, "BP1")
			require.NoError(t, err)
			require.Len(t, runs, 1)

			tr, err := st.GetTranscript(ctx, runs[0].ID)
			require.NoError(t, err)

			places["transcript"] = string(tr)

			for place, text := range places {
				for _, secret := range secrets {
					assert.NotContains(t, text, secret, place)
				}

				assert.Contains(t, text, mask, place+" carried the secrets")
			}
		})
	}
}

func TestMessage_UnknownBlueprint(t *testing.T) {
	c, _, _, _ := newController(t, artifactRun("# Need"))
	require.ErrorIs(t, c.Message(context.Background(), "nope", "hi", "m1"), ErrNotFound)
}

func TestShutdown_StopsRunsAndLeavesThemRecoverable(t *testing.T) {
	started := make(chan struct{}, 4)
	c, st, _, rec := newController(t, func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		signal(started)
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	<-started

	require.NoError(t, c.Shutdown(ctx))

	c.mu.Lock()
	assert.Empty(t, c.runs, "every run goroutine has exited")
	c.mu.Unlock()

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunRunning, runs[0].Status, "left as after a crash")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateRunning), bp.State)
	assert.False(t, rec.has(EventStageEnded))
	assert.False(t, rec.has(EventError))

	require.ErrorIs(t, c.Message(ctx, "BP1", "more", "m2"), ErrShuttingDown)
	require.ErrorIs(t, c.Start(ctx, startInput("BP2")), ErrShuttingDown)

	// A second controller on the same store recovers the stage.
	c2, _, runner2, _ := newControllerOn(t, st, artifactRun("# Need"))
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")
	require.Equal(t, 1, runner2.calls())

	runs, err = st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunInterrupted, runs[0].Status)
	assert.Equal(t, 2, runs[1].Attempt)
}

func TestShutdown_ExpiredContextDoesNotHang(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	c, _, _, _ := newController(t, func(_ context.Context, _ stage.Input) (stage.Output, error) {
		signal(started)
		<-release

		return stage.Output{}, errors.New("ignored cancellation")
	})
	t.Cleanup(func() {
		close(release)
		c.Wait("BP1")
	})

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	<-started

	expired, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, c.Shutdown(expired), context.Canceled)
}

func TestShutdown_KeepsAnArtifactTheRunCompleted(t *testing.T) {
	started := make(chan struct{}, 4)
	c, st, _, rec := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		signal(started)
		<-ctx.Done()

		// The run submitted before it saw the cancel.
		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	<-started

	require.NoError(t, c.Shutdown(ctx))

	art, err := st.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err, "a completed artifact is never lost to a shutdown")
	assert.Equal(t, "# Need", art.Body)

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunDone, runs[0].Status)
	assert.True(t, rec.has(EventStageEnded))
}

func TestEnd_IsAudited(t *testing.T) {
	c, st, _, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))

	stored, err := st.ListEvents(ctx, "BP1")
	require.NoError(t, err)

	kinds := make([]string, 0, len(stored))
	for _, e := range stored {
		kinds = append(kinds, e.Kind)
	}

	assert.Equal(t, []string{EventCreated, EventEnded}, kinds)
}

// blockingLLM parks every request until its context ends, as a real
// endpoint does when the run is canceled mid-request.
type blockingLLM struct{ entered chan struct{} }

func (b *blockingLLM) Send(ctx context.Context, _ llm.Request) (llm.Response, error) {
	return b.wait(ctx)
}

func (b *blockingLLM) SendStream(ctx context.Context, _ llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return b.wait(ctx)
}

func (b *blockingLLM) wait(ctx context.Context) (llm.Response, error) {
	signal(b.entered)
	<-ctx.Done()

	return llm.Response{}, ctx.Err()
}

func TestCancel_IsNotShownAsAnError(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(ctx context.Context, c *Controller) error
	}{
		{"abort", func(ctx context.Context, c *Controller) error { return c.Abort(ctx, "BP1", "m2") }},
		{"end", func(ctx context.Context, c *Controller) error { return c.End(ctx, "BP1") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = st.Close() })

			pk, err := pack.LoadEmbedded()
			require.NoError(t, err)

			model := &blockingLLM{entered: make(chan struct{}, 4)}
			rec := newRecorder()
			c := New(Options{
				Store: st, Pack: pk, MaxRunCostUSD: 2, MaxTurns: 5, Sink: rec.sink, HarnessLine: rec.line,
				Runner: &stage.Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return model }},
			})

			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			receive(t, model.entered, "the model request")

			require.NoError(t, tt.cancel(ctx, c))
			c.Wait("BP1")

			assert.False(t, rec.has(EventError), "a cancel is not an error")
			assert.True(t, rec.has(EventStageEnded))

			rec.mu.Lock()
			for _, l := range rec.lines {
				var line struct{ Kind string }
				require.NoError(t, json.Unmarshal(l, &line))
				assert.NotEqual(t, "error", line.Kind, "the harness's cancel error is not forwarded: %s", l)
			}
			rec.mu.Unlock()

			stored, err := st.ListEvents(ctx, "BP1")
			require.NoError(t, err)

			var storedHarnessError bool

			for _, e := range stored {
				if e.Kind == EventHarness && strings.Contains(string(e.Payload), `"kind":"error"`) {
					storedHarnessError = true
				}
			}

			assert.True(t, storedHarnessError, "the harness's own record of the cancel is still audited")
		})
	}
}

func TestShutdown_StoresTheHarnessLinesOfARunThatCompleted(t *testing.T) {
	started := make(chan struct{}, 4)
	c, st, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		signal(started)
		<-ctx.Done()

		// The run finished its last turn as the cancel landed.
		_, _ = in.Events.Write([]byte(`{"seq":7,"kind":"state_change","data":{"stop":"done","via":"terminating_tool"}}` + "\n"))

		return artifactOutput("# Need", in), nil
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "the intake run")

	require.NoError(t, c.Shutdown(ctx))

	stored, err := st.ListEvents(ctx, "BP1")
	require.NoError(t, err)

	var trailing bool

	for _, e := range stored {
		if e.Kind == EventHarness && strings.Contains(string(e.Payload), `"seq":7`) {
			trailing = true
		}
	}

	assert.True(t, trailing, "the completed run's trailing harness line is audited")
}

func TestStart_AllZeroPriceIsNoPrice(t *testing.T) {
	zero := func() *protocol.ModelPrice { return &protocol.ModelPrice{} }

	t.Run("capped is refused and stores no price", func(t *testing.T) {
		c, st, runner, rec := newController(t, artifactRun("# Need"))
		ctx := context.Background()
		in := startInput("BP1")
		in.CostCapUSD = 1
		in.Price = zero()
		require.NoError(t, c.Start(ctx, in))
		require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
		c.Wait("BP1")

		assert.Equal(t, 0, runner.calls())
		assert.True(t, rec.has(EventError))

		bp, err := st.GetBlueprint(ctx, "BP1")
		require.NoError(t, err)
		assert.Equal(t, string(StateIdle), bp.State)
		assert.Nil(t, bp.Price)
	})

	t.Run("uncapped runs unpriced", func(t *testing.T) {
		c, st, runner, rec := newController(t, artifactRun("# Need"))
		ctx := context.Background()
		in := startInput("BP1")
		in.Price = zero()
		require.NoError(t, c.Start(ctx, in))

		created, ok := rec.first(EventCreated)
		require.True(t, ok)
		assert.Equal(t, false, created.Payload["priced"])

		require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
		c.Wait("BP1")

		require.Equal(t, 1, runner.calls())

		ended, ok := rec.first(EventStageEnded)
		require.True(t, ok)
		assert.Equal(t, false, ended.Payload["priced"])

		bp, err := st.GetBlueprint(ctx, "BP1")
		require.NoError(t, err)
		assert.Nil(t, bp.Price)
	})

	t.Run("one non-zero rate is still priced", func(t *testing.T) {
		c, st, _, rec := newController(t, artifactRun("# Need"))
		ctx := context.Background()
		in := startInput("BP1")
		in.Price = &protocol.ModelPrice{CompletionUSDPerMTok: 10}
		require.NoError(t, c.Start(ctx, in))

		created, ok := rec.first(EventCreated)
		require.True(t, ok)
		assert.Equal(t, true, created.Payload["priced"])

		bp, err := st.GetBlueprint(ctx, "BP1")
		require.NoError(t, err)
		assert.NotNil(t, bp.Price)
	})
}

func TestStart_ReopenWithoutAPriceKeepsTheCreationPrice(t *testing.T) {
	tests := []struct {
		name        string
		reopenModel string
		reopenPrice *protocol.ModelPrice
		wantPriced  bool
		wantCost    float64
	}{
		{name: "same model keeps the price it was created with", reopenModel: "session/model", wantPriced: true, wantCost: 0.20},
		{name: "an all-zero price is no price", reopenModel: "session/model", reopenPrice: &protocol.ModelPrice{}, wantPriced: true, wantCost: 0.20},
		{name: "another model runs unpriced", reopenModel: "other/model", wantCost: 0},
		{
			name: "a new price on the same model wins", reopenModel: "session/model", wantPriced: true, wantCost: 0.40,
			reopenPrice: &protocol.ModelPrice{PromptUSDPerMTok: 2, CompletionUSDPerMTok: 20},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
				out := artifactOutput("# Need", in)
				out.Result.TotalCostUSD = 0
				out.Result.PromptTokens = 100_000
				out.Result.CompletionTokens = 10_000

				return out, nil
			})
			ctx := context.Background()
			in := startInput("BP1")
			in.Price = testPrice()
			require.NoError(t, c.Start(ctx, in))
			require.NoError(t, c.End(ctx, "BP1"))

			// CM's price lookup failed on this start.
			reopen := startInput("BP1")
			reopen.Model = tt.reopenModel
			reopen.Price = tt.reopenPrice
			require.NoError(t, c.Start(ctx, reopen))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")

			runs, err := st.ListRuns(ctx, "BP1")
			require.NoError(t, err)
			require.Len(t, runs, 1)
			assert.Equal(t, tt.reopenModel, runs[0].Model)
			assert.InDelta(t, tt.wantCost, runs[0].Usage.CostUSD, 1e-9)

			ended, ok := rec.first(EventStageEnded)
			require.True(t, ok)
			assert.Equal(t, tt.wantPriced, ended.Payload["priced"])
		})
	}
}

func TestExport_WritesTheBundleAndMasksSecrets(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need with "+testKey+" "+testMCPKey+" "+testGitToken))
	c.opts.ExportDir = t.TempDir()
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	_, err := st.AddGate(ctx, store.Gate{BlueprintID: "BP1", Stage: signoffStage, Version: 1, Decision: store.GateReject, Feedback: "split the card", Responder: store.ResponderHuman, DecidedAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, st.AddEmission(ctx, store.Emission{BlueprintID: "BP1", Kind: store.EmitCard, DraftID: "D1", Project: "alpha", CMID: "ALPHA-042", CreatedAt: time.Now()}))

	require.NoError(t, c.Export(ctx, "BP1", "m2"))

	ev, ok := rec.first(EventExported)
	require.True(t, ok)

	dir, _ := ev.Payload["path"].(string)
	require.DirExists(t, dir)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.NotEmpty(t, entries)
	assert.Equal(t, false, ev.Payload["during_run"], "an idle blueprint leaves nothing working")

	var all []byte

	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		b, err := os.ReadFile(path) //nolint:gosec // a walk of the test's own temp dir
		all = append(all, b...)

		return err
	}))

	assert.NotContains(t, string(all), testKey, "the bundle is rendered from the masked record")
	assert.NotContains(t, string(all), testMCPKey)
	assert.NotContains(t, string(all), testGitToken)
	assert.Contains(t, string(all), "# Need with")

	summary, err := os.ReadFile(filepath.Join(dir, "blueprint.md"))
	require.NoError(t, err)
	assert.Contains(t, string(summary), "split the card")
	assert.Contains(t, string(summary), "ALPHA-042")
}

func TestExport_UnknownBlueprint(t *testing.T) {
	c, _, _, _ := newController(t, artifactRun("# Need"))
	c.opts.ExportDir = t.TempDir()

	assert.ErrorIs(t, c.Export(context.Background(), "nope", "m1"), ErrNotFound)
}

// An export for a session that is not attached is not found, as a message
// is: ContextMatrix then starts the session again and re-sends it, so the
// panel hears of the bundle.
func TestExport_ASessionThatIsNotAttachedIsNotFound(t *testing.T) {
	c, _, _, rec := newController(t, artifactRun("# Need"))
	c.opts.ExportDir = t.TempDir()
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.End(ctx, "BP1"))

	require.ErrorIs(t, c.Export(ctx, "BP1", "m2"), ErrNotFound)
	assert.False(t, rec.has(EventExported))

	written, err := os.ReadDir(c.opts.ExportDir)
	require.NoError(t, err)
	assert.Empty(t, written, "nothing is written")

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Export(ctx, "BP1", "m2"))
	assert.True(t, rec.has(EventExported), "the re-sent export is written and announced")
}

func TestExport_RefusesWithoutADirectoryAndUnsafeIDs(t *testing.T) {
	c, st, _, _ := newController(t, artifactRun("# Need"))
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	require.Error(t, c.Export(ctx, "BP1", "m1"), "no export directory")

	c.opts.ExportDir = t.TempDir()
	now := time.Now().UTC()
	require.NoError(t, st.CreateBlueprint(ctx, store.Blueprint{ID: "../escape", State: "new", CreatedAt: now, UpdatedAt: now}))
	require.ErrorIs(t, c.Export(ctx, "../escape", "m2"), ErrNotFound)
	assert.NoDirExists(t, filepath.Join(c.opts.ExportDir, "..", "escape"))
}

func TestExport_DoesNotDisturbARunningStage(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	c, st, _, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		close(started)
		<-release

		return artifactOutput("# Need", in), nil
	})
	c.opts.ExportDir = t.TempDir()
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	<-started

	require.NoError(t, c.Export(ctx, "BP1", "m2"))
	close(release)
	c.Wait("BP1")

	ev, ok := rec.first(EventExported)
	require.True(t, ok)
	assert.Equal(t, true, ev.Payload["during_run"], "the run goes on working after the export")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, store.RunDone, runs[0].Status)
}

func TestHarnessErrorLineIsForwardedWhenTheRunWasNotCancelled(t *testing.T) {
	c, _, _, rec := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		_, _ = in.Events.Write([]byte(`{"seq":1,"kind":"error","data":{"message":"boom"}}` + "\n"))

		return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("stage intake: boom")
	})
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	rec.mu.Lock()
	defer rec.mu.Unlock()

	var forwarded bool

	for _, line := range rec.lines {
		if bytes.Contains(line, []byte(`"kind":"error"`)) {
			forwarded = true
		}
	}

	assert.True(t, forwarded, "an error line on a run that was not cancelled reaches the panel")
}

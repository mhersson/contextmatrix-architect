package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// fakeWorkspace writes a tiny tree per checkout and records the calls. A
// non-nil hold parks every checkout until its context ends; a checkout of
// the tree named failOn fails.
type fakeWorkspace struct {
	root   string
	hold   chan struct{}
	failOn string

	mu    sync.Mutex
	calls []checkoutCall
}

type checkoutCall struct {
	blueprintID, name, url string
}

func (w *fakeWorkspace) Checkout(ctx context.Context, blueprintID, name, repoURL string, _ code.Credentials) (code.Checkout, error) {
	w.mu.Lock()
	w.calls = append(w.calls, checkoutCall{blueprintID: blueprintID, name: name, url: repoURL})
	w.mu.Unlock()

	if name == w.failOn {
		return code.Checkout{}, errors.New("code: fetch: remote unreachable")
	}

	if w.hold != nil {
		select {
		case <-w.hold:
		case <-ctx.Done():
			return code.Checkout{}, ctx.Err()
		}
	}

	dir := filepath.Join(w.TreeRoot(blueprintID), name)
	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o750); err != nil {
		return code.Checkout{}, err
	}

	files := map[string]string{
		"AGENTS.md":     "# agents\n",
		"internal/x.go": "package internal\n\nfunc Thing() {}\n",
		"repo.txt":      repoURL,
	}

	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
			return code.Checkout{}, err
		}
	}

	return code.Checkout{RepoURL: repoURL, DefaultBranch: "main", SHA: "sha-" + name, Dir: dir}, nil
}

func (w *fakeWorkspace) TreeRoot(blueprintID string) string {
	return filepath.Join(w.root, blueprintID)
}

func (w *fakeWorkspace) checkouts() int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.calls)
}

func (w *fakeWorkspace) recorded() []checkoutCall {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]checkoutCall(nil), w.calls...)
}

func listAlpha(context.Context, string) ([]cm.Project, error) {
	return []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "make test"}}}, nil
}

// stageScript answers each stage by id; the fake runner looks up the stage
// of every run it is asked for.
type stageScript map[string]func(ctx context.Context, in stage.Input) (stage.Output, error)

func scripted(script stageScript) func(context.Context, stage.Input) (stage.Output, error) {
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		fn, ok := script[in.Stage]
		if !ok {
			return stage.Output{}, fmt.Errorf("unscripted stage %s", in.Stage)
		}

		return fn(ctx, in)
	}
}

func done(body string, data map[string]any) stage.Output {
	return stage.Output{
		Artifact: stage.Artifact{Body: body, Data: data},
		Result:   harness.Result{Completed: true, Reason: "done", Messages: []llm.Message{{Role: "user", Content: "x"}}},
	}
}

func returns(body string, data map[string]any) func(context.Context, stage.Input) (stage.Output, error) {
	return func(context.Context, stage.Input) (stage.Output, error) { return done(body, data), nil }
}

func goodCards() map[string]any {
	return map[string]any{
		"cards": []any{map[string]any{
			"id": "D1", "project": "alpha", "title": "Add the thing", "type": "feature", "depends_on": []any{},
			"body":  "## Context\nwhy\n## Where\ninternal/x.go\n## Change\nadd\n## Acceptance criteria\n1. go test ./... passes.\n## Tests expected\nTestThing.\n## Out of scope\nrest.\n",
			"where": []any{map[string]any{"path": "internal/x.go", "symbol": "Thing"}},
		}},
		"order": []any{"D1"},
	}
}

func readiness(ready bool) map[string]any {
	findings := []any{}
	if !ready {
		findings = []any{"two deliverables"}
	}

	return map[string]any{"cards": []any{map[string]any{"id": "D1", "ready": ready, "findings": findings}}}
}

func newPipeline(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeWorkspace) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	return newPipelineOn(t, st, script, nil)
}

// newPipelineOn builds a controller on an existing store; a nil ws gets a
// fresh workspace, a shared one models trees that survive a restart.
func newPipelineOn(t *testing.T, st *store.Store, script stageScript, ws *fakeWorkspace) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeWorkspace) {
	t.Helper()

	pk, err := pack.LoadDir("testdata/pipeline")
	require.NoError(t, err)

	runner := &fakeRunner{fn: scripted(script)}
	rec := newRecorder()

	if ws == nil {
		ws = &fakeWorkspace{root: t.TempDir()}
	}

	c := New(Options{
		Store: st, Pack: pk, Runner: runner, MaxRunCostUSD: 2, MaxTurns: 10,
		Sink: rec.sink, HarnessLine: rec.line,
		Workspace: ws, Projects: listAlpha,
	})

	return c, st, runner, rec, ws
}

// drive runs intake and pastes the design, then waits for the pipeline to
// settle (no run in flight).
func drive(t *testing.T, c *Controller, id string) {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput(id)))
	require.NoError(t, c.Message(ctx, id, "the need", "m1"))
	c.Wait(id)
	require.NoError(t, c.Message(ctx, id, "the design", "m2"))
	c.Wait(id)
}

func happyScript() stageScript {
	return stageScript{
		"intake":        returns("# Need", map[string]any{"slices": 1}),
		"grounding":     returns("# Grounding", nil),
		"decomposition": returns("# Cards", goodCards()),
		"readiness":     returns("# Ready", readiness(true)),
	}
}

// eventsOf returns the stored and the published payloads of kind.
func eventsOf(t *testing.T, st *store.Store, rec *recorder, id, kind string) []string {
	t.Helper()

	stored, err := st.ListEvents(context.Background(), id)
	require.NoError(t, err)

	var out []string

	for _, e := range stored {
		if e.Kind == kind {
			out = append(out, string(e.Payload))
		}
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	for _, e := range rec.events {
		if e.Kind == kind {
			b, err := json.Marshal(e.Payload)
			require.NoError(t, err)

			out = append(out, string(b))
		}
	}

	return out
}

func TestPipeline_IntakeThenDesignThenCodeStagesThenReport(t *testing.T) {
	c, st, runner, rec, ws := newPipeline(t, happyScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	// Intake done: the design is awaited, nothing runs.
	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateWaiting), bp.State)

	await, ok := rec.first(EventAwaitInput)
	require.True(t, ok)
	assert.Equal(t, "design", await.Payload["stage"])
	assert.Equal(t, 1, runner.calls())

	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
	c.Wait("BP1")

	design, err := st.LatestArtifact(ctx, "BP1", "design")
	require.NoError(t, err)
	assert.Equal(t, "the design", design.Body)

	pasted := false

	for _, p := range eventsOf(t, st, rec, "BP1", EventArtifact) {
		if strings.Contains(p, `"stage":"design"`) {
			pasted = true

			assert.Contains(t, p, `"source":"human"`)
		}
	}

	assert.True(t, pasted, "the pasted design is an artifact event")

	// grounding, decomposition, readiness ran in order with the right inputs.
	require.Equal(t, 4, runner.calls())
	assert.Equal(t, "grounding", runner.input(1).Stage)
	assert.Contains(t, runner.input(1).Task, "the design")
	assert.Contains(t, runner.input(1).Task, "# Need")
	assert.NotEmpty(t, runner.input(1).Tools, "code stages get the code tools")
	assert.Nil(t, runner.input(1).Validate, "only a stage that checks cards validates")
	assert.Equal(t, "decomposition", runner.input(2).Stage)
	assert.Contains(t, runner.input(2).Task, "# Grounding")
	assert.NotNil(t, runner.input(2).Validate, "decomposition validates against the contract")
	assert.Contains(t, runner.input(2).Instructions, c.opts.Pack.CardTemplate(), "the card template is appended to decomposition's instructions")
	assert.Equal(t, "readiness", runner.input(3).Stage)
	assert.Contains(t, runner.input(3).Task, "# Cards")
	assert.Empty(t, runner.input(0).Tools, "intake has no code tools")

	assert.Equal(t, 1, ws.checkouts(), "one checkout per project, reused by later stages")

	grounded, ok := rec.first(EventGrounded)
	require.True(t, ok)
	assert.NotEmpty(t, grounded.Payload["sources"])
	assert.Equal(t, 1, rec.count(EventGrounded), "a reused tree is not announced again")

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, true, report.Payload["ready"])
	assert.Contains(t, report.Payload["body"], "# Ready")

	bp, err = st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	sources, err := st.ListSources(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "sha-alpha", sources[0].Version)
	assert.Equal(t, "grounding", sources[0].FirstUsedBy)
	assert.Equal(t, "https://example.com/alpha.git", sources[0].Identity)
}

func TestPipeline_MessageDuringIntakeIsANoteNotTheDesign(t *testing.T) {
	release := make(chan struct{})
	script := happyScript()
	script["intake"] = func(context.Context, stage.Input) (stage.Output, error) {
		<-release

		return done("# Need", nil), nil
	}

	c, st, _, rec, _ := newPipeline(t, script)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.NoError(t, c.Message(ctx, "BP1", "early design", "m2"))

	assert.Equal(t, 1, rec.count(EventNote))

	close(release)
	c.Wait("BP1")

	_, err := st.LatestArtifact(ctx, "BP1", "design")
	require.ErrorIs(t, err, store.ErrNotFound, "a note during intake never becomes the design")
	assert.True(t, rec.has(EventAwaitInput), "the design is still awaited")
}

func TestPipeline_ReadinessLoopsBackThenReports(t *testing.T) {
	var readinessRuns int

	script := happyScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		readinessRuns++

		return done(fmt.Sprintf("# Not ready %d", readinessRuns), readiness(false)), nil
	}

	c, st, runner, rec, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	// intake, grounding, then decomposition and readiness three times each.
	require.Equal(t, 8, runner.calls())
	assert.Equal(t, 3, readinessRuns)
	assert.Equal(t, "decomposition", runner.input(4).Stage)
	assert.Contains(t, runner.input(4).Task, "# Not ready 1", "the loop feeds the findings back into decomposition")

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, false, report.Payload["ready"])
	assert.Equal(t, 3, report.Payload["rounds"])
	assert.Contains(t, report.Payload["findings"], "D1: two deliverables")

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	// The pipeline has stopped: a message is a note and runs nothing.
	require.NoError(t, c.Message(context.Background(), "BP1", "try again", "m3"))
	c.Wait("BP1")
	assert.Equal(t, 8, runner.calls())
}

func TestPipeline_StagesSeeTheCardsInTheData(t *testing.T) {
	var readinessRuns int

	script := happyScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		readinessRuns++

		return done("# Readiness", readiness(readinessRuns == 2)), nil
	}

	c, _, runner, _, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	require.Equal(t, 6, runner.calls())

	// Readiness judges the cards, which live in decomposition's data.
	judged := runner.input(3)
	require.Equal(t, "readiness", judged.Stage)
	assert.Contains(t, judged.Task, `"id": "D1"`)
	assert.Contains(t, judged.Task, "TestThing.")

	// The decomposition the loop re-runs sees the cards it wrote and the
	// per-card findings, which live in readiness's data.
	rerun := runner.input(4)
	require.Equal(t, "decomposition", rerun.Stage)
	assert.Contains(t, rerun.Task, "Add the thing")
	assert.Contains(t, rerun.Task, "two deliverables")
}

func TestPipeline_ReadinessMustJudgeEveryCard(t *testing.T) {
	twoCards := func() map[string]any {
		d := goodCards()
		second := maps.Clone(d["cards"].([]any)[0].(map[string]any))
		second["id"] = "D2"
		second["title"] = "Add the other thing"
		d["cards"] = append(d["cards"].([]any), second)
		d["order"] = []any{"D1", "D2"}

		return d
	}

	script := happyScript()
	script["decomposition"] = returns("# Cards", twoCards())
	// Readiness only ever judges D1, and judges it ready.
	script["readiness"] = returns("# Ready", readiness(true))

	c, _, runner, rec, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	assert.Equal(t, 8, runner.calls(), "a verdict that skips a card is not ready, so the loop runs its rounds")

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, false, report.Payload["ready"])
	assert.Contains(t, report.Payload["findings"], "D2: not judged")
}

func TestVerdictOf(t *testing.T) {
	verdict := func(cards ...map[string]any) store.Artifact {
		list := make([]any, 0, len(cards))
		for _, c := range cards {
			list = append(list, c)
		}

		b, err := json.Marshal(map[string]any{"cards": list})
		require.NoError(t, err)

		return store.Artifact{Data: b}
	}

	ready := func(id string) map[string]any { return map[string]any{"id": id, "ready": true} }

	tests := []struct {
		name      string
		art       store.Artifact
		draft     []string
		wantReady bool
		want      []string
	}{
		{name: "every card judged ready", art: verdict(ready("D1"), ready("D2")), draft: []string{"D1", "D2"}, wantReady: true},
		{name: "a card left out", art: verdict(ready("D1")), draft: []string{"D1", "D2"}, want: []string{"D2: not judged"}},
		{name: "a card not ready", art: verdict(ready("D1"), map[string]any{"id": "D2", "ready": false, "findings": []any{"vague"}}), draft: []string{"D1", "D2"}, want: []string{"D2: vague"}},
		{name: "not ready without findings", art: verdict(map[string]any{"id": "D1", "ready": false}), draft: []string{"D1"}, want: []string{"D1: not ready"}},
		{name: "ready with findings is not ready", art: verdict(map[string]any{"id": "D1", "ready": true, "findings": []any{"path does not exist"}}), draft: []string{"D1"}, want: []string{"D1: path does not exist"}},
		{name: "no cards judged", art: verdict(), draft: []string{"D1"}, want: []string{"the stage reported no cards"}},
		{name: "no data", art: store.Artifact{}, draft: []string{"D1"}, want: []string{"the stage reported no cards"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, findings := verdictOf(tt.art, tt.draft)
			assert.Equal(t, tt.wantReady, ready)
			assert.Equal(t, tt.want, findings)
		})
	}
}

func TestPipeline_ReadinessPassesOnTheSecondRound(t *testing.T) {
	var readinessRuns int

	script := happyScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		readinessRuns++

		return done("# Readiness", readiness(readinessRuns == 2)), nil
	}

	c, _, runner, rec, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	assert.Equal(t, 6, runner.calls())

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, true, report.Payload["ready"])
	assert.Equal(t, 2, report.Payload["rounds"])
}

// keepSubmitting models a model that submits the same draft until the
// validator accepts it, counting its submits; it gives up after a few, as
// the run's turn cap would.
func keepSubmitting(draft map[string]any, wantRejection string, submits *int) func(context.Context, stage.Input) (stage.Output, error) {
	return func(_ context.Context, in stage.Input) (stage.Output, error) {
		for {
			*submits++

			if *submits > 2*maxContractRounds {
				return stage.Output{}, errors.New("the validator never accepted the draft")
			}

			if err := in.Validate(stage.Artifact{Body: "# Cards", Data: draft}); err != nil {
				if !strings.Contains(err.Error(), wantRejection) {
					return stage.Output{}, fmt.Errorf("unexpected rejection: %w", err)
				}

				continue
			}

			return done("# Cards", draft), nil
		}
	}
}

func TestPipeline_CardsFailingTheContractAreReturnedThenReported(t *testing.T) {
	tests := []struct {
		name    string
		draft   func() map[string]any
		reject  string
		finding string
	}{
		{
			name: "a card misses sections",
			draft: func() map[string]any {
				bad := goodCards()
				bad["cards"].([]any)[0].(map[string]any)["body"] = "## Context\nonly context\n"

				return bad
			},
			reject: "required_section", finding: "required_section",
		},
		{
			name: "the order does not cover the cards",
			draft: func() map[string]any {
				bad := goodCards()
				bad["order"] = []any{"D2"}

				return bad
			},
			reject: "order", finding: "D1 is missing from order",
		},
		{
			name: "the draft has no cards",
			draft: func() map[string]any {
				return map[string]any{"cards": []any{}, "order": []any{}}
			},
			reject: "no cards", finding: "no cards",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var submits int

			script := happyScript()
			// The model keeps submitting the same bad draft; the validator
			// rejects twice and accepts the third so the failure is reported.
			script["decomposition"] = keepSubmitting(tt.draft(), tt.reject, &submits)

			c, st, runner, rec, _ := newPipeline(t, script)
			drive(t, c, "BP1")

			assert.Equal(t, 3, submits)
			assert.Equal(t, 3, runner.calls(), "readiness does not run on cards that fail the contract")

			report, ok := rec.first(EventReport)
			require.True(t, ok)
			assert.Equal(t, false, report.Payload["ready"])
			assert.Equal(t, "decomposition", report.Payload["stage"])

			findings, _ := report.Payload["findings"].([]string)
			require.NotEmpty(t, findings)
			assert.Contains(t, strings.Join(findings, "\n"), tt.finding)

			art, err := st.LatestArtifact(context.Background(), "BP1", "decomposition")
			require.NoError(t, err, "the failing cards are stored for the record")
			assert.True(t, artifactStopped(art))
			assert.Equal(t, findings, findingsOf(art))

			bp, err := st.GetBlueprint(context.Background(), "BP1")
			require.NoError(t, err)
			assert.Equal(t, string(StateIdle), bp.State)

			// A stopped pipeline is not re-run by a message.
			require.NoError(t, c.Message(context.Background(), "BP1", "go on", "m3"))
			c.Wait("BP1")
			assert.Equal(t, 3, runner.calls())
		})
	}
}

func TestPipeline_ContractPassingOnTheSecondSubmitIsNotStopped(t *testing.T) {
	var submits int

	script := happyScript()
	script["decomposition"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		bad := goodCards()
		bad["order"] = []any{}

		for _, draft := range []map[string]any{bad, goodCards()} {
			submits++

			if err := in.Validate(stage.Artifact{Body: "# Cards", Data: draft}); err == nil {
				return done("# Cards", draft), nil
			}
		}

		return stage.Output{}, errors.New("no submit accepted")
	}

	c, st, runner, rec, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	assert.Equal(t, 2, submits)
	assert.Equal(t, 4, runner.calls(), "readiness runs on cards that pass")

	art, err := st.LatestArtifact(context.Background(), "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(art))

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, true, report.Payload["ready"])
}

func TestPipeline_RestartResumesAtTheNextStage(t *testing.T) {
	script := happyScript()
	script["decomposition"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		<-ctx.Done()

		return stage.Output{}, ctx.Err()
	}

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	c, _, runner, _, ws := newPipelineOn(t, st, script, nil)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))

	// grounding completes, decomposition is in flight: shut down like a crash.
	require.Eventually(t, func() bool { return runner.calls() == 3 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	// A new process on the same store and the same trees reopens the
	// blueprint: decomposition is interrupted and re-run from the stored
	// artifacts; grounding is not re-run and nothing is checked out again.
	c2, _, runner2, rec2, _ := newPipelineOn(t, st, happyScript(), ws)
	require.NoError(t, c2.Start(ctx, startInput("BP1")))
	c2.Wait("BP1")

	require.Equal(t, 2, runner2.calls())
	assert.Equal(t, "decomposition", runner2.input(0).Stage)
	assert.Contains(t, runner2.input(0).Task, "# Grounding")
	assert.Equal(t, "readiness", runner2.input(1).Stage)
	assert.True(t, rec2.has(EventReport))
	assert.Equal(t, 1, ws.checkouts(), "the tree from before the restart is reused")
	assert.False(t, rec2.has(EventGrounded))

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var got []string
	for _, r := range runs {
		got = append(got, r.Stage+":"+r.Status)
	}

	assert.Equal(t, []string{
		"intake:done", "grounding:done", "decomposition:interrupted", "decomposition:done", "readiness:done",
	}, got, "the cut-off decomposition is marked interrupted and re-run once")
}

func TestPipeline_ReopenRunsAStageWhoseInputsWereStored(t *testing.T) {
	// A crash fell between storing grounding's artifact and starting
	// decomposition: no run was left running.
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	c, _, runner, rec, ws := newPipelineOn(t, st, happyScript(), nil)
	seed(t, c, st, ws)

	require.NoError(t, c.Start(context.Background(), startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 2, runner.calls())
	assert.Equal(t, "decomposition", runner.input(0).Stage)
	assert.Equal(t, "readiness", runner.input(1).Stage)
	assert.True(t, rec.has(EventReport))
	assert.Equal(t, 1, ws.checkouts(), "no re-fetch: the recorded tree is reused")
}

// seed stores a blueprint whose stages up to grounding completed, with
// grounding's tree checked out and recorded, then detaches its session.
func seed(t *testing.T, c *Controller, st *store.Store, ws *fakeWorkspace, extra ...store.Artifact) {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))

	now := time.Now().UTC()

	arts := append([]store.Artifact{
		{Stage: needStage, Body: "the need"},
		{Stage: "intake", Body: "# Need"},
		{Stage: "design", Body: "the design"},
		{Stage: "grounding", Body: "# Grounding"},
	}, extra...)

	for _, a := range arts {
		a.BlueprintID = "BP1"
		_, err := st.PutArtifact(ctx, a)
		require.NoError(t, err)

		if a.Stage == needStage || a.Stage == "design" {
			continue
		}

		id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: a.Stage, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
		require.NoError(t, err)
		require.NoError(t, st.EndRun(ctx, id, store.RunDone, "done", store.RunUsage{}, now))
	}

	_, err := ws.Checkout(ctx, "BP1", "alpha", "https://example.com/alpha.git", nil)
	require.NoError(t, err)
	_, err = st.AddSource(ctx, store.Source{BlueprintID: "BP1", Kind: "repo", Identity: "https://example.com/alpha.git", Version: "sha-alpha", FetchedAt: now, FirstUsedBy: "grounding"})
	require.NoError(t, err)
}

func TestPipeline_ReopenInTheLoopRunsTheNextRound(t *testing.T) {
	// A crash fell between readiness's first verdict and decomposition's
	// second round: decomposition ran before, but not since the verdict.
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	c, _, runner, rec, ws := newPipelineOn(t, st, happyScript(), nil)

	cards, err := json.Marshal(goodCards())
	require.NoError(t, err)

	verdict, err := json.Marshal(readiness(false))
	require.NoError(t, err)

	seed(t, c, st, ws,
		store.Artifact{Stage: "decomposition", Body: "# Cards", Data: cards},
		store.Artifact{Stage: "readiness", Body: "# Not ready 1", Data: verdict})

	require.NoError(t, c.Start(context.Background(), startInput("BP1")))
	c.Wait("BP1")

	require.Equal(t, 2, runner.calls())
	assert.Equal(t, "decomposition", runner.input(0).Stage)
	assert.Contains(t, runner.input(0).Task, "# Not ready 1")
	assert.Equal(t, "readiness", runner.input(1).Stage)

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, true, report.Payload["ready"])
	assert.Equal(t, 2, report.Payload["rounds"])
}

func TestPipeline_FreshBlueprintDoesNotRunOnReopen(t *testing.T) {
	c, _, runner, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Equal(t, 0, runner.calls(), "nothing runs until the need is stated")
}

func TestPipeline_ReopenWhileWaitingForTheDesignRunsNothing(t *testing.T) {
	c, st, runner, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Equal(t, 1, runner.calls())

	// The reopen asks for the design again, after the reopened line.
	kinds := rec.kinds()
	require.GreaterOrEqual(t, len(kinds), 2)
	assert.Equal(t, []string{EventReopened, EventAwaitInput}, kinds[len(kinds)-2:])
	assert.Equal(t, 2, rec.count(EventAwaitInput))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateWaiting), bp.State)

	// The next message is still the design.
	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
	c.Wait("BP1")

	_, err = st.LatestArtifact(ctx, "BP1", "design")
	require.NoError(t, err)
	assert.Equal(t, 4, runner.calls())
}

func TestPipeline_ReopenAfterAnIdleIntakeAsksForTheDesign(t *testing.T) {
	// A blueprint whose intake completed before the design stage existed
	// sits idle, never prompted.
	c, st, runner, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))

	now := time.Now().UTC()

	for _, a := range []store.Artifact{{Stage: needStage, Body: "the need"}, {Stage: "intake", Body: "# Need"}} {
		a.BlueprintID = "BP1"
		_, err := st.PutArtifact(ctx, a)
		require.NoError(t, err)
	}

	id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
	require.NoError(t, err)
	require.NoError(t, st.EndRun(ctx, id, store.RunDone, "done", store.RunUsage{}, now))
	require.NoError(t, st.SetBlueprintState(ctx, "BP1", string(StateIdle)))

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Equal(t, 0, runner.calls())

	await, ok := rec.first(EventAwaitInput)
	require.True(t, ok)
	assert.Equal(t, "design", await.Payload["stage"])
	assert.Equal(t, c.opts.Pack.Instructions("design"), await.Payload["prompt"])

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateWaiting), bp.State)
}

func TestPipeline_AnUnreadablePositionIsReportedNotFatal(t *testing.T) {
	// The record names a stage the current pack does not have: the pack
	// changed under the blueprint.
	c, st, runner, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.End(ctx, "BP1"))

	now := time.Now().UTC()

	for _, a := range []store.Artifact{{Stage: needStage, Body: "the need"}, {Stage: "intake", Body: "# Need"}, {Stage: "slicing", Body: "# Slices"}} {
		a.BlueprintID = "BP1"
		_, err := st.PutArtifact(ctx, a)
		require.NoError(t, err)
	}

	for _, stage := range []string{"intake", "slicing"} {
		id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: stage, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
		require.NoError(t, err)
		require.NoError(t, st.EndRun(ctx, id, store.RunDone, "done", store.RunUsage{}, now))
	}

	require.NoError(t, c.Start(ctx, startInput("BP1")), "the reopen attaches the session")

	ev, ok := rec.first(EventError)
	require.True(t, ok, "the user is told why nothing runs")
	assert.Contains(t, ev.Payload["message"], "slicing")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	// A message is recorded and answered with the same error, not refused.
	require.NoError(t, c.Message(ctx, "BP1", "hello", "m1"))
	c.Wait("BP1")

	assert.Equal(t, 1, rec.count(EventNote))
	assert.Equal(t, 2, rec.count(EventError))
	assert.Equal(t, 0, runner.calls())
}

func TestPipeline_MessageAfterAnAbortRetriesTheStage(t *testing.T) {
	var groundingRuns atomic.Int32

	script := happyScript()
	script["grounding"] = func(ctx context.Context, _ stage.Input) (stage.Output, error) {
		if groundingRuns.Add(1) == 1 {
			<-ctx.Done()

			return stage.Output{}, ctx.Err()
		}

		return done("# Grounding", nil), nil
	}

	c, st, runner, rec, _ := newPipeline(t, script)
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
	require.Eventually(t, func() bool { return runner.calls() == 2 }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, c.Abort(ctx, "BP1", "m3"))
	c.Wait("BP1")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, store.RunCanceled, runs[len(runs)-1].Status)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)

	// Any message now retries grounding and the pipeline runs to the report.
	require.NoError(t, c.Message(ctx, "BP1", "go on", "m4"))
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, "grounding", note.Payload["retries"], "the note says which stage it re-runs")
	assert.True(t, rec.has(EventReport))
	assert.Equal(t, 5, runner.calls())
}

func TestPipeline_FailedStageWaitsForAMessageAcrossAReopen(t *testing.T) {
	var groundingRuns atomic.Int32

	script := happyScript()
	script["grounding"] = func(context.Context, stage.Input) (stage.Output, error) {
		if groundingRuns.Add(1) == 1 {
			return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("llm: 502 from provider")
		}

		return done("# Grounding", nil), nil
	}

	c, st, runner, rec, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)
	require.Equal(t, string(StateFailed), bp.State)

	ctx := context.Background()
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")
	assert.Equal(t, 2, runner.calls(), "a failed stage is not re-run by a reopen")

	require.NoError(t, c.Message(ctx, "BP1", "retry", "m3"))
	c.Wait("BP1")

	assert.Equal(t, 5, runner.calls())
	assert.True(t, rec.has(EventReport))
}

func TestPipeline_FailedReadinessInALaterRoundIsRetried(t *testing.T) {
	var readinessRuns atomic.Int32

	script := happyScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		switch readinessRuns.Add(1) {
		case 1:
			return done("# Not ready", readiness(false)), nil
		case 2:
			return stage.Output{Result: harness.Result{Reason: "error"}}, errors.New("llm: 502 from provider")
		default:
			return done("# Ready", readiness(true)), nil
		}
	}

	c, _, runner, rec, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	// intake, grounding, decomposition, readiness, decomposition, readiness.
	require.Equal(t, 6, runner.calls())
	assert.False(t, rec.has(EventReport))

	require.NoError(t, c.Message(context.Background(), "BP1", "retry", "m3"))
	c.Wait("BP1")

	require.Equal(t, 7, runner.calls())
	assert.Equal(t, "readiness", runner.input(6).Stage, "the second round's readiness is retried, not decomposition")

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, true, report.Payload["ready"])
	assert.Equal(t, 2, report.Payload["rounds"])
}

func TestPipeline_CodeStageWithoutAWorkspaceFails(t *testing.T) {
	c, st, runner, rec, _ := newPipeline(t, happyScript())
	c.opts.Workspace = nil

	drive(t, c, "BP1")

	assert.True(t, rec.has(EventError))
	assert.Equal(t, 1, runner.calls(), "the model is never called without the code")

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateFailed), bp.State)

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "grounding", runs[1].Stage)
	assert.Equal(t, store.RunFailed, runs[1].Status)
}

func TestPipeline_ProjectLookupFailureFailsTheRunAndIsRetried(t *testing.T) {
	c, st, runner, rec, _ := newPipeline(t, happyScript())

	var lookups atomic.Int32

	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		if lookups.Add(1) == 1 {
			return nil, errors.New("cm: connect to mcp endpoint: connection refused")
		}

		return listAlpha(ctx, key)
	}

	drive(t, c, "BP1")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "connection refused")

	require.NoError(t, c.Message(context.Background(), "BP1", "retry", "m3"))
	c.Wait("BP1")

	assert.Equal(t, 4, runner.calls())
	assert.True(t, rec.has(EventReport))

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 5)
	assert.Equal(t, store.RunFailed, runs[1].Status)
	assert.Equal(t, "grounding", runs[2].Stage)
	assert.Equal(t, store.RunDone, runs[2].Status)
}

func TestPipeline_SecretInAPrepareErrorIsMasked(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	c.opts.Projects = func(_ context.Context, key string) ([]cm.Project, error) {
		return nil, fmt.Errorf("cm: 401 for key %s", key)
	}

	drive(t, c, "BP1")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "[REDACTED]")

	for _, p := range eventsOf(t, st, rec, "BP1", EventError) {
		assert.NotContains(t, p, testMCPKey)
	}
}

func TestPipeline_AbortDuringTheCheckoutCancelsTheRun(t *testing.T) {
	c, st, runner, rec, ws := newPipeline(t, happyScript())
	ws.hold = make(chan struct{})

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
	require.Eventually(t, func() bool { return ws.checkouts() == 1 }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, c.Abort(ctx, "BP1", "m3"))
	c.Wait("BP1")

	assert.Equal(t, 1, runner.calls())
	assert.False(t, rec.has(EventError), "a cancel is not an error")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunCanceled, runs[1].Status)

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateIdle), bp.State)
}

func TestPipeline_RepoURLCredentialsNeverReachTheRecord(t *testing.T) {
	const secret = "repo-secret-0123456789"

	c, st, _, rec, ws := newPipeline(t, happyScript())
	c.opts.Projects = func(context.Context, string) ([]cm.Project, error) {
		return []cm.Project{{
			Name: "alpha", Repo: "https://user:" + secret + "@example.com/alpha.git",
			Verify: &cm.Verify{Command: "make test"},
		}}, nil
	}

	drive(t, c, "BP1")
	require.True(t, rec.has(EventReport))

	for _, call := range ws.recorded() {
		assert.Equal(t, "https://example.com/alpha.git", call.url, "the workspace gets the clean url")
	}

	sources, err := st.ListSources(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, "https://example.com/alpha.git", sources[0].Identity)

	grounded := eventsOf(t, st, rec, "BP1", EventGrounded)
	require.NotEmpty(t, grounded)

	for _, p := range grounded {
		assert.Contains(t, p, "https://example.com/alpha.git")
	}

	stored, err := st.ListEvents(context.Background(), "BP1")
	require.NoError(t, err)

	for _, e := range stored {
		assert.NotContains(t, string(e.Payload), secret, "stored %s event", e.Kind)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	for _, e := range rec.events {
		b, err := json.Marshal(e.Payload)
		require.NoError(t, err)
		assert.NotContains(t, string(b), secret, "published %s event", e.Kind)
	}
}

func TestPrepareCode_TreeNames(t *testing.T) {
	const (
		core = "https://example.com/core.git"
		docs = "https://example.com/docs.git"
		web  = "https://example.com/web.git"
	)

	tests := []struct {
		name      string
		project   cm.Project
		wantTrees map[string]string // tree name -> url
		wantRepo  bool
		wantErr   []string
	}{
		{
			name:      "the primary flag wins over the order",
			project:   cm.Project{Repos: []cm.Repo{{Name: "docs", URL: docs}, {Name: "core", URL: core, Primary: true}}},
			wantTrees: map[string]string{"alpha": core, "alpha@docs": docs},
			wantRepo:  true,
		},
		{
			name:      "without a primary flag the first is primary",
			project:   cm.Project{Repos: []cm.Repo{{Name: "core", URL: core}, {Name: "docs", URL: docs}}},
			wantTrees: map[string]string{"alpha": core, "alpha@docs": docs},
			wantRepo:  true,
		},
		{
			name:      "a repository without a url is skipped",
			project:   cm.Project{Repos: []cm.Repo{{Name: "core", Primary: true}, {Name: "docs", URL: docs}, {Name: "web", URL: web}}},
			wantTrees: map[string]string{"alpha": docs, "alpha@web": web},
			wantRepo:  true,
		},
		{
			name:      "the single repo field",
			project:   cm.Project{Repo: core},
			wantTrees: map[string]string{"alpha": core},
			wantRepo:  true,
		},
		{
			name:      "no repository",
			project:   cm.Project{Repos: []cm.Repo{{Name: "core"}}},
			wantTrees: map[string]string{},
		},
		{
			name:    "an unsafe repository name",
			project: cm.Project{Repos: []cm.Repo{{Name: "core", URL: core, Primary: true}, {Name: "../x", URL: docs}}},
			wantErr: []string{`"alpha"`, `"../x"`},
		},
		{
			name:    "an empty repository name",
			project: cm.Project{Repos: []cm.Repo{{Name: "core", URL: core, Primary: true}, {URL: docs}}},
			wantErr: []string{`"alpha"`, `""`},
		},
		{
			name:    "two repositories with one name",
			project: cm.Project{Repos: []cm.Repo{{Name: "core", URL: core, Primary: true}, {Name: "docs", URL: docs}, {Name: "docs", URL: web}}},
			wantErr: []string{"alpha@docs"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _, ws := newPipeline(t, happyScript())
			project := tt.project
			project.Name = "alpha"
			project.Verify = &cm.Verify{Command: "make test"}
			c.opts.Projects = func(context.Context, string) ([]cm.Project, error) { return []cm.Project{project}, nil }

			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			c.mu.Lock()
			sess := c.sessions["BP1"]
			c.mu.Unlock()

			facts, err := c.prepareCode(ctx, sess, bp, "grounding")
			if tt.wantErr != nil {
				require.Error(t, err)

				for _, want := range tt.wantErr {
					assert.Contains(t, err.Error(), want)
				}

				return
			}

			require.NoError(t, err)

			got := map[string]string{}
			for _, call := range ws.recorded() {
				got[call.name] = call.url
			}

			assert.Equal(t, tt.wantTrees, got)

			dirs := map[string]bool{}
			for _, r := range facts.repos {
				dirs[r.Dir] = true

				assert.Equal(t, "alpha", r.Project)
				assert.Equal(t, "AGENTS.md", r.InstructionFile)
				assert.Equal(t, "make test", r.VerifyCommand)
				assert.NotEmpty(t, r.SourceTag)
			}

			assert.Len(t, dirs, len(tt.wantTrees))

			p := facts.projects["alpha"]
			assert.Equal(t, tt.wantRepo, p.HasRepo)
			assert.True(t, p.HasVerify)

			if tt.wantRepo {
				assert.Equal(t, filepath.Join(ws.TreeRoot("BP1"), "alpha"), p.Root, "the primary tree is the contract root")
			} else {
				assert.Empty(t, p.Root)
			}
		})
	}
}

func TestPrepareCode_AnnouncesRecordedSourcesWhenALaterCheckoutFails(t *testing.T) {
	c, st, _, rec, ws := newPipeline(t, happyScript())
	ws.failOn = "alpha@docs"
	c.opts.Projects = func(context.Context, string) ([]cm.Project, error) {
		return []cm.Project{{Name: "alpha", Verify: &cm.Verify{Command: "make test"}, Repos: []cm.Repo{
			{Name: "core", URL: "https://example.com/core.git", Primary: true},
			{Name: "docs", URL: "https://example.com/docs.git"},
		}}}, nil
	}

	drive(t, c, "BP1")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "remote unreachable")

	sources, err := st.ListSources(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, sources, 1)

	grounded, ok := rec.first(EventGrounded)
	require.True(t, ok, "the source recorded before the failure is announced")
	assert.Len(t, grounded.Payload["sources"], 1)
}

func TestPrepareCode_RefusesUnsafeOrCollidingProjects(t *testing.T) {
	const (
		core = "https://example.com/core.git"
		web  = "https://example.com/web.git"
	)

	tests := []struct {
		name     string
		targets  []string
		projects []cm.Project
		wantErr  []string
	}{
		{
			name:     "an unsafe project name",
			targets:  []string{"../alpha"},
			projects: []cm.Project{{Name: "../alpha", Repo: core}},
			wantErr:  []string{`"../alpha"`},
		},
		{
			name:    "a project named like another project's secondary tree",
			targets: []string{"alpha", "alpha@web"},
			projects: []cm.Project{
				{Name: "alpha", Repos: []cm.Repo{{Name: "core", URL: core, Primary: true}, {Name: "web", URL: web}}},
				{Name: "alpha@web", Repo: web},
			},
			wantErr: []string{`"alpha"`, `"alpha@web"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _, ws := newPipeline(t, happyScript())
			c.opts.Projects = func(context.Context, string) ([]cm.Project, error) { return tt.projects, nil }

			ctx := context.Background()
			in := startInput("BP1")
			in.Projects = tt.targets
			require.NoError(t, c.Start(ctx, in))

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			c.mu.Lock()
			sess := c.sessions["BP1"]
			c.mu.Unlock()

			_, err = c.prepareCode(ctx, sess, bp, "grounding")
			require.Error(t, err)

			for _, want := range tt.wantErr {
				assert.Contains(t, err.Error(), want)
			}

			assert.Equal(t, 0, ws.checkouts(), "nothing is fetched")
		})
	}
}

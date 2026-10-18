package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/harness"
	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
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
	// empty names the trees that check out as a repository with no
	// commits: no files, no sha.
	empty map[string]bool

	// noAgents leaves AGENTS.md out of every tree.
	noAgents bool

	// extra adds files to every tree.
	extra map[string]string

	mu      sync.Mutex
	calls   []checkoutCall
	removed []string
	shas    map[string]string
	heads   map[string]string
	headErr error
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

	if w.empty[name] {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return code.Checkout{}, err
		}

		return code.Checkout{RepoURL: repoURL, Dir: dir}, nil
	}

	if err := os.MkdirAll(filepath.Join(dir, "internal"), 0o750); err != nil {
		return code.Checkout{}, err
	}

	files := map[string]string{
		"AGENTS.md":     "# agents\n",
		"internal/x.go": "package internal\n\nfunc Thing() {}\n",
		"repo.txt":      repoURL,
	}

	if w.noAgents {
		delete(files, "AGENTS.md")
	}

	maps.Copy(files, w.extra)

	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return code.Checkout{}, err
		}

		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return code.Checkout{}, err
		}
	}

	w.mu.Lock()
	if w.shas == nil {
		w.shas = map[string]string{}
	}

	w.shas[repoURL] = "sha-" + name
	w.mu.Unlock()

	return code.Checkout{RepoURL: repoURL, DefaultBranch: "main", SHA: "sha-" + name, Dir: dir}, nil
}

// RemoteHead reports the sha Checkout recorded, a moved sha from heads, or
// headErr.
func (w *fakeWorkspace) RemoteHead(_ context.Context, repoURL string, _ code.Credentials) (string, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.headErr != nil {
		return "", "", w.headErr
	}

	if sha, ok := w.heads[repoURL]; ok {
		return "main", sha, nil
	}

	return "main", w.shas[repoURL], nil
}

func (w *fakeWorkspace) Remove(_ context.Context, blueprintID string) error {
	w.mu.Lock()
	w.removed = append(w.removed, blueprintID)
	w.mu.Unlock()

	return os.RemoveAll(w.TreeRoot(blueprintID))
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

// listAlphaUnset is alpha with a repository but no verify command.
func listAlphaUnset(context.Context, string) ([]cm.Project, error) {
	return []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git"}}, nil
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

// setupCards is goodCards with the setup for a project with no verify
// command and no instruction file: goodCards' criteria already run
// "go test ./...".
func setupCards() map[string]any {
	cards := goodCards()
	cards["setup"] = map[string]any{"alpha": map[string]any{"verify_command": "go test ./...", "instruction_card": "D1"}}

	return cards
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
		Workspace: ws, Projects: listAlpha, CMTools: (&fakeCMTools{}).open,
		ConnectCM: newFakeCM().connect, CallerIdentity: "human:architect",
	})

	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	return c, st, runner, rec, ws
}

// drive states the need, then waits for the pipeline to settle (no run in flight).
func drive(t *testing.T, c *Controller, id string) {
	t.Helper()

	driveIn(t, c, startInput(id))
}

// driveIn is drive with a chosen start input, such as another mode.
func driveIn(t *testing.T, c *Controller, in StartInput) {
	t.Helper()

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, in.ID, "the need", "m1"))
	c.Wait(in.ID)
}

// sessionOf returns the attached session of id.
func sessionOf(t *testing.T, c *Controller, id string) *session {
	t.Helper()

	c.mu.Lock()
	defer c.mu.Unlock()

	s := c.sessions[id]
	require.NotNil(t, s, "session %s is attached", id)

	return s
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

func TestPipeline_IntakeThenCodeStagesThenReport(t *testing.T) {
	c, st, runner, rec, ws := newPipeline(t, happyScript())
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	// intake, grounding, decomposition, readiness ran in order with the
	// right inputs.
	require.Equal(t, 4, runner.calls())
	assert.Empty(t, runner.input(0).Tools, "intake has no code tools")
	assert.Equal(t, "grounding", runner.input(1).Stage)
	assert.Contains(t, runner.input(1).Task, "# Need")
	assert.NotEmpty(t, runner.input(1).Tools, "code stages get the code tools")
	assert.Nil(t, runner.input(1).Validate, "only a stage with checks validates")
	assert.Equal(t, "decomposition", runner.input(2).Stage)
	assert.Contains(t, runner.input(2).Task, "# Grounding")
	assert.NotNil(t, runner.input(2).Validate, "decomposition validates against the contract")
	assert.Contains(t, runner.input(2).Instructions, c.opts.Pack.CardTemplate(), "the card template is appended to decomposition's instructions")
	assert.Equal(t, "readiness", runner.input(3).Stage)
	assert.Contains(t, runner.input(3).Task, "# Cards")

	assert.Equal(t, 1, ws.checkouts(), "one checkout per project, reused by later stages")

	grounded, ok := rec.first(EventGrounded)
	require.True(t, ok)
	assert.NotEmpty(t, grounded.Payload["sources"])
	assert.Equal(t, 1, rec.count(EventGrounded), "a reused tree is not announced again")

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, true, report.Payload["ready"])
	assert.Contains(t, report.Payload["body"], "# Ready")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateWaiting), bp.State)

	sources, err := st.ListSources(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, sources, 2, "the repo, then the ContextMatrix board")
	assert.Equal(t, "sha-alpha", sources[0].Version)
	assert.Equal(t, "grounding", sources[0].FirstUsedBy)
	assert.Equal(t, "https://example.com/alpha.git", sources[0].Identity)
	assert.Equal(t, "cm", sources[1].Kind)
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
	return func(ctx context.Context, in stage.Input) (stage.Output, error) {
		for {
			*submits++

			if *submits > 2*maxContractRounds {
				return stage.Output{}, errors.New("the validator never accepted the draft")
			}

			if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: draft}); err != nil {
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
			assert.Equal(t, 3, report.Payload["rounds"], "the cards stopped on the last contract round")
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

			note, ok := rec.first(EventNote)
			require.True(t, ok)
			assert.Equal(t, true, note.Payload["ended"], "the note tells the panel no stage starts")
		})
	}
}

func TestPipeline_ContractPassingOnTheSecondSubmitIsNotStopped(t *testing.T) {
	var submits int

	script := happyScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		bad := goodCards()
		bad["order"] = []any{}

		for _, draft := range []map[string]any{bad, goodCards()} {
			submits++

			if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: draft}); err == nil {
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

func TestPipeline_EverySubmitReadsTheProjectsAgain(t *testing.T) {
	var fixed atomic.Bool

	script := happyScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		draft := goodCards()

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: draft}); err == nil || !strings.Contains(err.Error(), "type") {
			return stage.Output{}, fmt.Errorf("the first submit fails on the card type: %v", err)
		}

		// The user adds the card type in ContextMatrix during the run.
		fixed.Store(true)

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: draft}); err != nil {
			return stage.Output{}, fmt.Errorf("the same draft passes once ContextMatrix has the type: %w", err)
		}

		return done("# Cards", draft), nil
	}

	c, st, _, rec, _ := newPipeline(t, script)
	c.opts.Projects = func(context.Context, string) ([]cm.Project, error) {
		types := []string{"task"}
		if fixed.Load() {
			types = []string{"task", "feature"}
		}

		return []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "make test"}, Types: types}}, nil
	}

	drive(t, c, "BP1")

	art, err := st.LatestArtifact(context.Background(), "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(art), "a fix made in ContextMatrix counts at the next submit")

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, true, report.Payload["ready"])
}

func TestPipeline_ASubmitWhileContextMatrixCannotBeReadSpendsNoRound(t *testing.T) {
	var down atomic.Bool

	script := happyScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		bad := goodCards()
		bad["order"] = []any{}

		down.Store(true)

		for range maxContractRounds {
			if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: bad}); err == nil || !strings.Contains(err.Error(), "not judged") {
				return stage.Output{}, fmt.Errorf("a submit while ContextMatrix is down is not judged: %v", err)
			}
		}

		down.Store(false)

		// Had the refusals spent rounds, this bad draft would be accepted.
		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: bad}); err == nil {
			return stage.Output{}, errors.New("the first judged submit of a bad draft was accepted")
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()}); err != nil {
			return stage.Output{}, fmt.Errorf("the good draft: %w", err)
		}

		return done("# Cards", goodCards()), nil
	}

	c, st, _, _, _ := newPipeline(t, script)
	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		if down.Load() {
			return nil, errors.New("cm: list_projects: remote unreachable")
		}

		return listAlpha(ctx, key)
	}

	drive(t, c, "BP1")

	art, err := st.LatestArtifact(context.Background(), "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(art))
}

func TestPipeline_SetupIsWorkTheCardsCarry(t *testing.T) {
	script := happyScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()})
		if err == nil || !strings.Contains(err.Error(), "setup_verify") || !strings.Contains(err.Error(), "setup_instruction") {
			return stage.Output{}, fmt.Errorf("cards without the setup are refused: %v", err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: setupCards()}); err != nil {
			return stage.Output{}, fmt.Errorf("cards with the setup pass: %w", err)
		}

		return done("# Cards", setupCards()), nil
	}

	c, _, _, rec, ws := newPipeline(t, script)
	c.opts.Projects = listAlphaUnset
	ws.noAgents = true

	drive(t, c, "BP1")

	report, ok := rec.first(EventReport)
	require.True(t, ok)
	assert.Equal(t, true, report.Payload["ready"], "neither gap stops the pipeline")
}

func TestPipeline_AVerifyCommandSetDuringDecompositionCounts(t *testing.T) {
	var set atomic.Bool

	script := happyScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()}); err == nil || !strings.Contains(err.Error(), "setup_verify") {
			return stage.Output{}, fmt.Errorf("the first submit proposes no verify command: %v", err)
		}

		// The user sets the verify command in ContextMatrix meanwhile.
		set.Store(true)

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()}); err != nil {
			return stage.Output{}, fmt.Errorf("the same cards pass once the project has one: %w", err)
		}

		return done("# Cards", goodCards()), nil
	}

	c, st, _, _, _ := newPipeline(t, script)
	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		if set.Load() {
			return listAlpha(ctx, key)
		}

		return listAlphaUnset(ctx, key)
	}

	drive(t, c, "BP1")

	art, err := st.LatestArtifact(context.Background(), "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(art))
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
		{Stage: "grounding", Body: "# Grounding"},
	}, extra...)

	for _, a := range arts {
		a.BlueprintID = "BP1"
		_, err := st.PutArtifact(ctx, a)
		require.NoError(t, err)

		if a.Stage == needStage {
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

func TestPipeline_EndDuringTheCheckoutRerunsTheStageOnReopen(t *testing.T) {
	c, st, runner, rec, ws := newPipeline(t, happyScript())
	ws.hold = make(chan struct{})

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	require.Eventually(t, func() bool { return ws.checkouts() == 1 }, 5*time.Second, 10*time.Millisecond)

	require.NoError(t, c.End(ctx, "BP1"))
	c.Wait("BP1")

	assert.False(t, rec.has(EventError), "a cancel is not an error")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, store.RunCanceled, runs[1].Status)
	assert.Equal(t, reasonSessionEnded, runs[1].Reason)

	ws.hold = nil

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Greater(t, runner.calls(), 1, "the reopen runs the cut stage again")

	runs, err = st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Greater(t, len(runs), 2)
	assert.Equal(t, runs[1].Stage, runs[2].Stage)
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
	require.Len(t, sources, 2, "the repo, then the ContextMatrix board")
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
			assert.Equal(t, tt.wantRepo, p.HasInstructionFile, "the primary tree's AGENTS.md")

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

func TestDraftProblems_ManualEntriesAndTitle(t *testing.T) {
	card := func(id string) contract.Card { return contract.Card{ID: id, Project: "alpha", Title: "Add " + id} }

	tests := []struct {
		name  string
		draft contract.Draft
		want  []string
	}{
		{
			name:  "one card with a manual entry needs no title",
			draft: contract.Draft{Cards: []contract.Card{card("D1")}, Manual: []contract.Manual{{ID: "M1", Text: "check"}}, Order: []string{"D1", "M1"}},
		},
		{
			name:  "two cards need a playbook title",
			draft: contract.Draft{Cards: []contract.Card{card("D1"), card("D2")}, Order: []string{"D1", "D2"}},
			want:  []string{"the draft has several cards, so it needs a playbook title"},
		},
		{
			name:  "a manual entry missing from order",
			draft: contract.Draft{Title: "T", Cards: []contract.Card{card("D1"), card("D2")}, Manual: []contract.Manual{{ID: "M1", Text: "check"}}, Order: []string{"D1", "D2"}},
			want:  []string{"M1 is missing from order"},
		},
		{
			name:  "a manual entry twice in order",
			draft: contract.Draft{Cards: []contract.Card{card("D1")}, Manual: []contract.Manual{{ID: "M1", Text: "check"}}, Order: []string{"D1", "M1", "M1"}},
			want:  []string{"M1 appears more than once in order"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, draftProblems(tt.draft))
		})
	}
}

func TestPipeline_CodeStagesSeeTheTypesAndPrioritiesTheProjectDeclares(t *testing.T) {
	c, _, runner, _, _ := newPipeline(t, happyScript())
	c.opts.Projects = func(context.Context, string) ([]cm.Project, error) {
		return []cm.Project{{
			Name: "alpha", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "make test"},
			Types: []string{"feature", "bug"}, Priorities: []string{"low", "medium", "high"},
		}}, nil
	}

	drive(t, c, "BP1")

	require.GreaterOrEqual(t, runner.calls(), 3)

	decomposition := runner.input(2)
	require.Equal(t, "decomposition", decomposition.Stage)
	assert.Contains(t, decomposition.Task, "- project alpha declares card types feature, bug and priorities low, medium, high; use only these.\n")
}

func TestCheckoutLines_NameWhatEachProjectDeclares(t *testing.T) {
	facts := &codeFacts{
		repos: []stage.RepoFacts{
			{Project: "beta", Dir: "beta", SHA: "sha-b", SourceTag: "S2"},
			{Project: "alpha", Dir: "alpha", SHA: "sha-a", SourceTag: "S1"},
		},
		projects: map[string]contract.Project{
			"gamma": {},
			"beta":  {Priorities: []string{"low", "high"}},
			"alpha": {Types: []string{"feature", "bug"}, Priorities: []string{"medium"}},
			"delta": {Types: []string{"task"}},
		},
	}

	want := "Checkouts under the tool root (cite the tag for anything read there):\n" +
		"- beta/ is project beta at sha-b [S2]\n" +
		"- alpha/ is project alpha at sha-a [S1]\n" +
		"- project alpha declares card types feature, bug and priorities medium; use only these.\n" +
		"- project beta declares priorities low, high; use only these.\n" +
		"- project delta declares card types task; use only these.\n" +
		"\n"
	assert.Equal(t, want, checkoutLines(facts), "sorted by project; a project declaring neither adds no line")
}

func TestPrepareCode_FillsProjectTypesPrioritiesAndForeignRepos(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, happyScript())
	c.opts.Projects = func(context.Context, string) ([]cm.Project, error) {
		return []cm.Project{
			{Name: "gamma", Repo: "https://example.com/gamma.git"},
			{
				Name: "alpha", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "make test"},
				Types: []string{"feature"}, Priorities: []string{"medium"},
			},
			{
				Name: "beta", Repo: "https://example.com/beta.git",
				Repos: []cm.Repo{{Name: "api", URL: "https://example.com/beta-api.git", Primary: true}, {Name: "web", URL: "https://example.com/beta-web.git"}},
			},
		}, nil
	}

	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	c.mu.Lock()
	sess := c.sessions["BP1"]
	c.mu.Unlock()

	bp, err := c.opts.Store.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	facts, err := c.prepareCode(ctx, sess, bp, "grounding")
	require.NoError(t, err)

	alpha := facts.projects["alpha"]
	assert.Equal(t, []string{"feature"}, alpha.Types)
	assert.Equal(t, []string{"medium"}, alpha.Priorities)
	assert.Equal(t, []string{
		"https://example.com/gamma.git",
		"https://example.com/beta.git", "https://example.com/beta-api.git", "https://example.com/beta-web.git",
	}, alpha.ForeignRepos, "CM's project order; the legacy repo and every repos url, as CM's lint collects them")
}

func TestPrepareCode_AnEmptyRepositoryGrounds(t *testing.T) {
	c, st, _, _, ws := newPipeline(t, happyScript())
	ws.empty = map[string]bool{"alpha": true}
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	facts, err := c.prepareCode(ctx, sessionOf(t, c, "BP1"), bp, "grounding")
	require.NoError(t, err)
	require.Len(t, facts.repos, 1)
	assert.True(t, facts.repos[0].Empty)
	assert.Empty(t, facts.repos[0].SHA)
	assert.Empty(t, facts.repos[0].InstructionFile)
	assert.Contains(t, checkoutLines(facts), "- alpha/ is project alpha, an empty repository with no commits [S1]\n")

	p := facts.projects["alpha"]
	assert.True(t, p.HasRepo)
	assert.NotEmpty(t, p.Root)
	assert.False(t, p.HasInstructionFile, "an empty tree has no instruction file, so a card must write AGENTS.md")

	sources, err := st.ListSources(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Empty(t, sources[0].Version, "a repository with no commits is recorded with no sha")

	_, err = c.prepareCode(ctx, sessionOf(t, c, "BP1"), bp, "decomposition")
	require.NoError(t, err)
	assert.Equal(t, 1, ws.checkouts(), "a later stage reuses the empty tree")
}

// fakeCMTools hands a cm stage one read tool and counts the sessions it
// opened and closed.
type fakeCMTools struct {
	mu     sync.Mutex
	opened int
	closed int
	tags   []string
	keys   []string
	err    error
}

func (f *fakeCMTools) open(_ context.Context, mcpKey, tag string) ([]tools.Tool, func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.err != nil {
		return nil, nil, f.err
	}

	f.opened++
	f.tags = append(f.tags, tag)
	f.keys = append(f.keys, mcpKey)

	return []tools.Tool{cmReadStub{}}, func() {
		f.mu.Lock()
		f.closed++
		f.mu.Unlock()
	}, nil
}

func (f *fakeCMTools) counts() (opened, closed int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.opened, f.closed
}

type cmReadStub struct{}

func (cmReadStub) Name() string   { return "cm_list_cards" }
func (cmReadStub) ReadOnly() bool { return true }
func (cmReadStub) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: "cm_list_cards", Parameters: json.RawMessage(`{"type":"object"}`)}}
}

func (cmReadStub) Execute(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{Text: "[]"}, nil
}

func toolNames(in stage.Input) []string {
	names := make([]string, 0, len(in.Tools))
	for _, t := range in.Tools {
		names = append(names, t.Name())
	}

	return names
}

func TestPipeline_CMStagesGetTheReadToolsAndOneSource(t *testing.T) {
	c, st, runner, _, _ := newPipeline(t, happyScript())
	fake := &fakeCMTools{}
	c.opts.CMTools = fake.open

	drive(t, c, "BP1")

	// intake, grounding, decomposition, readiness
	require.Equal(t, 4, runner.calls())
	assert.Equal(t, []string{"read", "grep", "glob", "repo_info", "cm_list_cards"}, toolNames(runner.input(1)), "grounding: code tools, then cm")
	assert.NotContains(t, toolNames(runner.input(2)), "cm_list_cards", "decomposition has no cm tools")
	assert.Contains(t, toolNames(runner.input(3)), "cm_list_cards", "readiness has the cm tools")

	sources, err := st.ListSources(context.Background(), "BP1")
	require.NoError(t, err)

	var cmTags []string

	for _, s := range sources {
		if s.Kind == "cm" {
			cmTags = append(cmTags, s.Tag())
		}
	}

	require.Len(t, cmTags, 1, "one cm source per blueprint")
	assert.Equal(t, []string{cmTags[0], cmTags[0]}, fake.tags)
	assert.Equal(t, []string{testMCPKey, testMCPKey}, fake.keys, "the session's MCP key")
	assert.Contains(t, runner.input(1).Task, "["+cmTags[0]+"]")

	opened, closed := fake.counts()
	assert.Equal(t, 2, opened)
	assert.Equal(t, 2, closed, "every run closes its CM session")
}

func TestPipeline_CMStageWithoutTheToolSourceFails(t *testing.T) {
	c, _, runner, rec, ws := newPipeline(t, happyScript())
	c.opts.CMTools = nil

	drive(t, c, "BP1")

	assert.Equal(t, 1, runner.calls(), "grounding never ran")
	assert.Zero(t, ws.checkouts(), "a server without the tool source fails before any fetch")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Equal(t, "prepare", ev.Payload["reason"])
	assert.Equal(t, errNoCMSupport.Error(), ev.Payload["message"])
}

func TestPipeline_CMToolsThatFailToOpenFailTheStage(t *testing.T) {
	c, st, runner, rec, _ := newPipeline(t, happyScript())
	fake := &fakeCMTools{err: errors.New("cm: connect to mcp endpoint: connection refused")}
	c.opts.CMTools = fake.open

	drive(t, c, "BP1")

	assert.Equal(t, 1, runner.calls(), "grounding never ran and no later stage started")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Equal(t, "prepare", ev.Payload["reason"])
	assert.Contains(t, ev.Payload["message"], "open the ContextMatrix tools")
	assert.Contains(t, ev.Payload["message"], "connection refused")

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, "grounding", runs[1].Stage)
	assert.Equal(t, store.RunFailed, runs[1].Status)

	opened, closed := fake.counts()
	assert.Zero(t, opened)
	assert.Zero(t, closed)
}

func TestPipeline_CMToolsWithoutACloserRun(t *testing.T) {
	c, _, runner, rec, _ := newPipeline(t, happyScript())
	c.opts.CMTools = func(context.Context, string, string) ([]tools.Tool, func(), error) {
		return []tools.Tool{cmReadStub{}}, nil, nil
	}

	drive(t, c, "BP1")

	assert.Equal(t, 4, runner.calls())
	assert.True(t, rec.has(EventReport), "a nil closer is not called when the run ends")
}

func TestPipeline_GroundingReportsTheSetupGaps(t *testing.T) {
	const (
		core = "https://example.com/core.git"
		docs = "https://example.com/docs.git"
	)

	twoRepos := func(context.Context, string) ([]cm.Project, error) {
		return []cm.Project{{
			Name: "alpha", Verify: &cm.Verify{Command: "make test"},
			Repos: []cm.Repo{{Name: "core", URL: core, Primary: true}, {Name: "docs", URL: docs}},
		}}, nil
	}

	gap := func(verify, file, empty bool) []any {
		return []any{map[string]any{"project": "alpha", "has_verify": verify, "has_instruction_file": file, "empty": empty}}
	}

	tests := []struct {
		name     string
		projects func(context.Context, string) ([]cm.Project, error)
		noAgents bool
		empty    map[string]bool
		want     []any // nil: no report
	}{
		{name: "set up", projects: listAlpha},
		{name: "no verify command", projects: listAlphaUnset, want: gap(false, true, false)},
		{name: "no instruction file", projects: listAlpha, noAgents: true, want: gap(true, false, false)},
		{name: "both", projects: listAlphaUnset, noAgents: true, want: gap(false, false, false)},
		{name: "an empty repository", projects: listAlpha, empty: map[string]bool{"alpha": true}, want: gap(true, false, true)},
		{name: "an empty secondary repository", projects: twoRepos, empty: map[string]bool{"alpha@docs": true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _, rec, ws := newPipeline(t, happyScript())
			c.opts.Projects = tt.projects
			ws.noAgents = tt.noAgents
			ws.empty = tt.empty

			drive(t, c, "BP1")

			ev, ok := rec.first(EventSetupGaps)
			if tt.want == nil {
				assert.False(t, ok, "nothing to report")

				return
			}

			require.True(t, ok)
			assert.Equal(t, tt.want, ev.Payload["projects"])

			rec.mu.Lock()
			events := slices.Clone(rec.events)
			rec.mu.Unlock()

			at := func(kind, stage string) int {
				return slices.IndexFunc(events, func(e Event) bool {
					return e.Kind == kind && (stage == "" || e.Payload["stage"] == stage)
				})
			}

			assert.Less(t, at(EventStageEnded, "grounding"), at(EventSetupGaps, ""), "reported when grounding ends")
			assert.Less(t, at(EventSetupGaps, ""), at(EventStageStarted, "decomposition"), "before the paid stages")
		})
	}
}

func TestPipeline_AVerifyCommandSetDuringGroundingIsNotReported(t *testing.T) {
	var set atomic.Bool

	script := happyScript()
	script["grounding"] = func(context.Context, stage.Input) (stage.Output, error) {
		set.Store(true) // the user sets it in ContextMatrix while grounding runs

		return done("# Grounding", nil), nil
	}

	c, _, _, rec, _ := newPipeline(t, script)
	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		if set.Load() {
			return listAlpha(ctx, key)
		}

		return listAlphaUnset(ctx, key)
	}

	drive(t, c, "BP1")

	assert.False(t, rec.has(EventSetupGaps), "the report reads ContextMatrix when grounding ends")
}

func TestPipeline_AnUnreadableSetupIsNotReportedAndThePipelineGoesOn(t *testing.T) {
	var calls atomic.Int32

	c, _, runner, rec, _ := newPipeline(t, happyScript())
	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		// The second read is the report's: after grounding's checkout,
		// before decomposition's.
		if calls.Add(1) == 2 {
			return nil, errors.New("cm: list_projects: remote unreachable")
		}

		return listAlphaUnset(ctx, key)
	}

	drive(t, c, "BP1")

	assert.False(t, rec.has(EventSetupGaps))
	assert.GreaterOrEqual(t, runner.calls(), 3, "decomposition still runs")
}

// abortedCtx reports a cancel without cutting the I/O under it short, so a
// report that ignores the abort would still read and publish.
type abortedCtx struct{ context.Context } //nolint:containedctx // a test double that wraps a context

func (abortedCtx) Err() error { return context.Canceled }

func TestReportSetup_AnAbortedRunReportsNothing(t *testing.T) {
	var reads atomic.Int32

	c, st, _, rec, _ := newPipeline(t, happyScript())
	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		reads.Add(1)

		return listAlphaUnset(ctx, key)
	}

	drive(t, c, "BP1")
	require.Equal(t, 1, rec.count(EventSetupGaps), "the drive reported the gap")

	before := reads.Load()

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)

	c.reportSetup(abortedCtx{context.Background()}, sessionOf(t, c, "BP1"), bp)

	assert.Equal(t, before, reads.Load(), "an aborted run reads nothing")
	assert.Equal(t, 1, rec.count(EventSetupGaps), "an aborted run tells the session nothing more")
}

func TestPosition_ReadsOnlyStageRuns(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	// A responder run that ended after readiness names a stage the pack
	// lacks; read as a stage, it would make the position unreadable.
	now := time.Now().UTC()
	id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "nosuch", Kind: store.RunKindResponder, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
	require.NoError(t, err)
	require.NoError(t, st.EndRun(ctx, id, store.RunDone, "done", store.RunUsage{}, now))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	out, _, err := c.position(ctx, bp)
	require.NoError(t, err)
	assert.True(t, out.end && out.ready, "readiness ended the pipeline ready")
}

func TestPosition_StatusIgnoresNonStageRunsOfTheNextStage(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	// Intake has not started as a stage; a failed responder run naming it
	// must not make the position read it as started and failed.
	now := time.Now().UTC()
	id, err := st.StartRun(ctx, store.Run{BlueprintID: "BP1", Stage: "intake", Kind: store.RunKindResponder, Attempt: 1, Round: 1, Status: store.RunRunning, StartedAt: now})
	require.NoError(t, err)
	require.NoError(t, st.EndRun(ctx, id, store.RunFailed, "boom", store.RunUsage{}, now))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	out, last, err := c.position(ctx, bp)
	require.NoError(t, err)
	assert.Equal(t, "intake", out.stage)
	assert.Empty(t, last.Status, "only stage runs give a stage its status")
}

func TestPipeline_TheGatePlanFollowsTheFirstStage(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	ctx := context.Background()
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.SetPolicy(ctx, "BP1", "grounding", "human", "m0"))

	drive(t, c, "BP1")

	plan, ok := rec.first(EventGatePlan)
	require.True(t, ok)
	assert.Equal(t, []any{
		map[string]any{"stage": "intake", "policy": "human", "gate": false},
		map[string]any{"stage": "grounding", "policy": "human", "gate": false},
		map[string]any{"stage": "decomposition", "policy": "auto", "gate": false},
		map[string]any{"stage": "readiness", "policy": "auto", "gate": false},
	}, plan.Payload["stages"], "model stages only, each with its effective policy")
	assert.Equal(t, "human", plan.Payload["signoff"])

	rec.mu.Lock()
	events := slices.Clone(rec.events)
	rec.mu.Unlock()

	at := func(kind, stage string) int {
		return slices.IndexFunc(events, func(e Event) bool {
			return e.Kind == kind && (stage == "" || e.Payload["stage"] == stage)
		})
	}

	assert.Less(t, at(EventStageEnded, "intake"), at(EventGatePlan, ""), "posted when intake ends")
	assert.Less(t, at(EventGatePlan, ""), at(EventStageStarted, "grounding"), "and before grounding starts")
	assert.Equal(t, 1, rec.count(EventGatePlan))
}

func TestNotes_ReachEveryLaterRun(t *testing.T) {
	started := make(chan struct{}, 1)
	finish := make(chan struct{})

	script := happyScript()
	script["intake"] = func(context.Context, stage.Input) (stage.Output, error) {
		signal(started)
		<-finish

		return done("# Need", map[string]any{"slices": 1}), nil
	}

	c, _, runner, rec, _ := newPipeline(t, script)
	release := closer(t, finish)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "intake")
	require.NoError(t, c.Message(ctx, "BP1", "keep the API stable", "m2"))
	release()
	c.Wait("BP1")

	note, ok := rec.first(EventNote)
	require.True(t, ok)
	assert.Equal(t, "intake", note.Payload["stage"], "the stage that was open")

	got := map[string]bool{}

	for i := range runner.calls() {
		in := runner.input(i)
		got[in.Stage] = strings.Contains(in.Task, "keep the API stable")
	}

	assert.Equal(t, map[string]bool{"intake": false, "grounding": true, "decomposition": true, "readiness": true}, got,
		"every run that starts after the note gets it; the run in flight does not")
}

func TestNotes_ABareDecisionWordReachesNoRun(t *testing.T) {
	started := make(chan struct{}, 1)
	finish := make(chan struct{})

	script := happyScript()
	script["intake"] = func(context.Context, stage.Input) (stage.Output, error) {
		signal(started)
		<-finish

		return done("# Need", map[string]any{"slices": 1}), nil
	}

	c, _, runner, rec, _ := newPipeline(t, script)
	release := closer(t, finish)
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, started, "intake")

	for i, text := range []string{"APPROVE", " REJECT ", "keep the API stable", "REJECT the cache"} {
		require.NoError(t, c.Message(ctx, "BP1", text, fmt.Sprintf("n%d", i)))
	}

	release()
	c.Wait("BP1")

	assert.Equal(t, 4, rec.count(EventNote), "every message is kept")
	require.Equal(t, 4, runner.calls())

	for i := 1; i < runner.calls(); i++ {
		in := runner.input(i)
		assert.Contains(t, in.Task, "keep the API stable", in.Stage)
		assert.Contains(t, in.Task, "REJECT the cache", "%s: a decision word with more text is a note", in.Stage)
		assert.NotContains(t, in.Task, "APPROVE", in.Stage)
		assert.Equal(t, 1, strings.Count(in.Task, "REJECT"), "%s: only the note with more text", in.Stage)
	}
}

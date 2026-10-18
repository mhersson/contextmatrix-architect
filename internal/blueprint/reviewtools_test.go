package blueprint

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/research"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestReviewer_GetsTheCodeToolsAndOnlyAGuardedFetch(t *testing.T) {
	var (
		mu       sync.Mutex
		names    []string
		task     string
		runID    int64
		fetchErr error
	)

	script := reviewedScript()
	script["intake.review"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		mu.Lock()
		defer mu.Unlock()

		names, task, runID = toolNames(in), in.Task, runOf(ctx)

		for _, tl := range in.Tools {
			if tl.Name() == research.FetchToolName {
				_, fetchErr = tl.Execute(ctx, map[string]any{"url": "https://docs.example.org/alpha/setup"})
			}
		}

		return done("# Review", map[string]any{"score": float64(5)}), nil
	}

	c, st, _, _ := newReviewed(t, script)
	driveIn(t, c, researchStart("BP1"))

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, []string{"read", "grep", "glob", "repo_info", research.FetchToolName}, names)
	assert.NotContains(t, names, research.SearchToolName, "a reviewer opens what is cited; it does not search")
	assert.Contains(t, task, "[S1]", "the task names the checkout's source tag")
	refusedBy(t, fetchErr, research.RuleName)

	runs, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	i := slices.IndexFunc(runs, func(r store.Run) bool { return r.Kind == store.RunKindReviewer })
	require.GreaterOrEqual(t, i, 0)
	assert.Equal(t, runs[i].ID, runID)

	payloads, eventRuns := researchPayloads(t, st)
	require.Len(t, payloads, 1)
	assert.Equal(t, research.RuleName, payloads[0]["refused"])
	assert.Equal(t, []int64{runs[i].ID}, eventRuns, "the refusal is recorded under the reviewer's run")
}

func TestReviewer_WithResearchOffGetsTheCodeToolsAlone(t *testing.T) {
	var (
		mu    sync.Mutex
		names []string
	)

	script := reviewedScript()
	script["intake.review"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		mu.Lock()
		defer mu.Unlock()

		names = toolNames(in)

		return done("# Review", map[string]any{"score": float64(5)}), nil
	}

	c, _, _, rec := newReviewed(t, script)
	driveIn(t, c, startInput("BP1"))

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, []string{"read", "grep", "glob", "repo_info"}, names)
	assert.False(t, rec.has(EventResearchOff), "the session's notice is a stage's, never a reviewer's")
}

func TestReviewer_ACheckoutThatFailsStopsTheReview(t *testing.T) {
	script := reviewedScript()

	c, _, runner, rec := newReviewed(t, script)
	c.opts.Workspace = &fakeWorkspace{root: t.TempDir(), failOn: "alpha"}
	driveIn(t, c, startInput("BP1"))

	assert.Empty(t, inputsOf(runner, "intake.review"), "no reviewer runs without its tools")
	assert.True(t, rec.has(EventError))
	assert.False(t, rec.has(EventGate))
}

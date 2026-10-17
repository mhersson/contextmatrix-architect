package blueprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/research"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func listAlphaPrefixed(context.Context, string) ([]cm.Project, error) {
	return []cm.Project{{Name: "alpha", Prefix: "ALPHA", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "make test"}}}, nil
}

// newResearchController attaches session BP1 to a controller over the
// single-stage pack; a nil ws leaves the controller without a workspace.
func newResearchController(t *testing.T, ws *fakeWorkspace, projects func(context.Context, string) ([]cm.Project, error)) (*Controller, *store.Store, *recorder) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	pk, err := pack.LoadDir("testdata/single")
	require.NoError(t, err)

	rec := newRecorder()
	opts := Options{
		Store: st, Pack: pk, MaxRunCostUSD: 2, MaxTurns: 10, Sink: rec.sink, HarnessLine: rec.line, Projects: projects,
		Runner: &fakeRunner{fn: func(context.Context, stage.Input) (stage.Output, error) { return done("# Need", nil), nil }},
	}

	// Set only when given: a nil *fakeWorkspace in the interface is not nil.
	if ws != nil {
		opts.Workspace = ws
	}

	c := New(opts)

	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	require.NoError(t, c.Start(context.Background(), startInput("BP1")))

	return c, st, rec
}

func researchPayloads(t *testing.T, st *store.Store) ([]map[string]any, []int64) {
	t.Helper()

	events, err := st.ListEventsOfKind(context.Background(), "BP1", EventResearch)
	require.NoError(t, err)

	payloads, runs := make([]map[string]any, 0, len(events)), make([]int64, 0, len(events))

	for _, e := range events {
		var p map[string]any
		require.NoError(t, json.Unmarshal(e.Payload, &p))

		payloads, runs = append(payloads, p), append(runs, e.RunID)
	}

	return payloads, runs
}

func refusedBy(t *testing.T, err error, rule string) {
	t.Helper()

	var r *research.Refusal
	require.ErrorAs(t, err, &r)
	assert.Equal(t, rule, r.Rule, r.Error())
}

func TestResearchLedger_RecordsEveryLookupUnderTheCallingRun(t *testing.T) {
	c, st, _ := newResearchController(t, nil, listAlpha)
	l := researchLedger{c: c, au: sessionOf(t, c, "BP1").audit, id: "BP1", stage: "intake"}
	ctx := withRun(context.Background(), 42)

	l.Lookup(ctx, research.Lookup{Tool: research.SearchToolName, Query: "postgres operator backups", Results: 7})
	l.Lookup(ctx, research.Lookup{Tool: research.SearchToolName, Query: "main.go crash", Refused: research.RuleFile})
	l.Lookup(ctx, research.Lookup{Tool: research.FetchToolName, URL: "https://x.example/a", Final: "https://x.example/b", Tag: "S3", Title: "B", Bytes: 10})

	payloads, runs := researchPayloads(t, st)
	require.Len(t, payloads, 3)
	assert.Equal(t, []int64{42, 42, 42}, runs)

	assert.Equal(t, map[string]any{"stage": "intake", "tool": "web_search", "query": "postgres operator backups", "results": 7.0}, payloads[0])
	assert.Equal(t, map[string]any{"stage": "intake", "tool": "web_search", "query": "main.go crash", "refused": "file"}, payloads[1])
	assert.Equal(t, map[string]any{
		"stage": "intake", "tool": "web_fetch", "url": "https://x.example/a", "final_url": "https://x.example/b",
		"tag": "S3", "title": "B", "bytes": 10.0,
	}, payloads[2])
}

func TestResearchLedger_RecordsAPageAsAURLSource(t *testing.T) {
	c, st, _ := newResearchController(t, nil, listAlpha)
	l := researchLedger{c: c, au: sessionOf(t, c, "BP1").audit, id: "BP1", stage: "intake"}
	ctx := context.Background()

	first, err := l.Page(ctx, research.Page{URL: "https://docs.example.org/a", Text: "one"})
	require.NoError(t, err)

	again, err := l.Page(ctx, research.Page{URL: "https://docs.example.org/a", Text: "one"})
	require.NoError(t, err)
	assert.Equal(t, first, again, "the same page read again is the same source")

	changed, err := l.Page(ctx, research.Page{URL: "https://docs.example.org/a", Text: "two"})
	require.NoError(t, err)
	assert.NotEqual(t, first, changed, "a changed page is a new version")

	sources, err := st.ListSources(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, sources, 2)

	sum := sha256.Sum256([]byte("one"))

	assert.Equal(t, "url", sources[0].Kind)
	assert.Equal(t, "https://docs.example.org/a", sources[0].Identity)
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), sources[0].Version)
	assert.Equal(t, "intake", sources[0].FirstUsedBy)
	assert.Equal(t, first, sources[0].Tag())
}

func TestResearchGuard_RefusesTheBlueprintsOwnTerms(t *testing.T) {
	ws := &fakeWorkspace{root: t.TempDir(), extra: map[string]string{
		"internal/y.go": "package internal\n\nfunc thingDoer() {}\n",
		"go.mod":        "module example.com/owner/alpha\n",
	}}
	c, st, rec := newResearchController(t, ws, listAlphaPrefixed)
	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)

	g := c.researchGuard(sessionOf(t, c, "BP1"), bp, "intake", nil)
	ctx := context.Background()

	checkouts := func() int {
		ws.mu.Lock()
		defer ws.mu.Unlock()

		return len(ws.calls)
	}

	refusedBy(t, g.Query(ctx, "internal/x panics"), research.RulePath)
	assert.Zero(t, checkouts(), "a shape rule refuses before anything is checked out")

	refusedBy(t, g.Query(ctx, "thingDoer timeout"), research.RuleIdentifier)
	assert.Equal(t, 1, checkouts(), "a stage without the code tools checks out at its first query that needs the terms")
	assert.True(t, rec.has(EventGrounded))

	refusedBy(t, g.Query(ctx, "alpha rollout"), research.RuleName)
	refusedBy(t, g.Query(ctx, "ALPHA-007 status"), research.RuleCard)
	refusedBy(t, g.Query(ctx, "owner kubernetes"), research.RuleModule)
	require.NoError(t, g.Query(ctx, "postgres operator backups"))
	assert.Equal(t, 1, checkouts(), "the terms are read once")
}

func TestResearchGuard_UsesTheCheckoutAStageAlreadyHas(t *testing.T) {
	ws := &fakeWorkspace{root: t.TempDir()}
	c, st, _ := newResearchController(t, ws, listAlphaPrefixed)
	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)

	sess := sessionOf(t, c, "BP1")

	facts, err := c.prepareCode(context.Background(), sess, bp, "design")
	require.NoError(t, err)

	g := c.researchGuard(sess, bp, "design", facts)
	refusedBy(t, g.Query(context.Background(), "ALPHA-007"), research.RuleCard)

	ws.mu.Lock()
	defer ws.mu.Unlock()

	assert.Len(t, ws.calls, 1, "no second checkout")
}

func TestResearchGuard_FailsClosedWithoutAWorkspace(t *testing.T) {
	c, st, _ := newResearchController(t, nil, listAlpha)
	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)

	g := c.researchGuard(sessionOf(t, c, "BP1"), bp, "intake", nil)
	refusedBy(t, g.Query(context.Background(), "postgres operator backups"), research.RuleIndex)
}

func TestRepoNames(t *testing.T) {
	tests := []struct {
		url  string
		want []string
	}{
		{"https://example.com/alpha.git", []string{"alpha"}},
		{"https://github.com/acme/payments.git", []string{"acme", "payments"}},
		{"https://user:token@github.com/acme/payments", []string{"acme", "payments"}},
		{"git@github.com:acme/payments.git", []string{"acme", "payments"}},
		{"https://gitlab.example.com/group/sub/repo", []string{"sub", "repo"}},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, repoNames(tt.url), tt.url)
	}
}

func TestResearchLedger_PinsTheEventPayloadOfEveryOutcome(t *testing.T) {
	tests := []struct {
		name string
		lk   research.Lookup
		want map[string]any
	}{
		{
			name: "a search that failed",
			lk:   research.Lookup{Tool: research.SearchToolName, Query: "postgres operator", Error: "provider unavailable"},
			want: map[string]any{"stage": "intake", "tool": "web_search", "query": "postgres operator", "error": "provider unavailable"},
		},
		{
			name: "a fetch that failed",
			lk:   research.Lookup{Tool: research.FetchToolName, URL: "https://x.example/a", Error: "timeout"},
			want: map[string]any{"stage": "intake", "tool": "web_fetch", "url": "https://x.example/a", "error": "timeout"},
		},
		{
			name: "a page cut at the cap",
			lk:   research.Lookup{Tool: research.FetchToolName, URL: "https://x.example/a", Final: "https://x.example/a", Tag: "S1", Title: "A", Bytes: 99, Truncated: true},
			want: map[string]any{"stage": "intake", "tool": "web_fetch", "url": "https://x.example/a", "final_url": "https://x.example/a", "tag": "S1", "title": "A", "bytes": 99.0, "truncated": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _ := newResearchController(t, nil, listAlpha)
			l := researchLedger{c: c, au: sessionOf(t, c, "BP1").audit, id: "BP1", stage: "intake"}

			l.Lookup(context.Background(), tt.lk)

			payloads, _ := researchPayloads(t, st)
			require.Len(t, payloads, 1)
			assert.Equal(t, tt.want, payloads[0])
		})
	}
}

func TestResearchLedger_MasksSessionSecretsInTheEvent(t *testing.T) {
	c, st, _ := newResearchController(t, nil, listAlpha)
	l := researchLedger{c: c, au: sessionOf(t, c, "BP1").audit, id: "BP1", stage: "intake"}

	l.Lookup(context.Background(), research.Lookup{Tool: research.SearchToolName, Query: "how to use " + testMCPKey, Results: 1})
	l.Lookup(context.Background(), research.Lookup{Tool: research.FetchToolName, URL: "https://x.example/?t=" + testGitToken, Error: "boom"})

	events, err := st.ListEventsOfKind(context.Background(), "BP1", EventResearch)
	require.NoError(t, err)
	require.Len(t, events, 2)

	for _, e := range events {
		assert.NotContains(t, string(e.Payload), testMCPKey)
		assert.NotContains(t, string(e.Payload), testGitToken)
	}
}

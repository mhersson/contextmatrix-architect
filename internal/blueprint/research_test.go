package blueprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
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

const testSearchKey = "brave-search-key-0123456789"

type fakeSearcher struct{ results []research.Result }

func (f fakeSearcher) Search(context.Context, string) ([]research.Result, error) {
	return f.results, nil
}

type fakeFetcher struct{ page research.Page }

func (f fakeFetcher) Fetch(context.Context, string) (research.Page, error) { return f.page, nil }

func searchOK(string, string) (research.Searcher, error) { return fakeSearcher{}, nil }

func researchStart(id string) StartInput {
	in := startInput(id)
	in.SearchProvider, in.SearchAPIKey = research.ProviderBrave, testSearchKey

	return in
}

// newResearchPipeline runs the research test pack on runner, with search
// as the session's provider factory.
func newResearchPipeline(t *testing.T, runner Runner, search func(string, string) (research.Searcher, error), fetch research.PageFetcher) (*Controller, *store.Store, *recorder) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	pk, err := pack.LoadDir("testdata/research")
	require.NoError(t, err)

	rec := newRecorder()
	c := New(Options{
		Store: st, Pack: pk, Runner: runner, MaxRunCostUSD: 2, MaxTurns: 10,
		Sink: rec.sink, HarnessLine: rec.line, ExportDir: t.TempDir(),
		Workspace: &fakeWorkspace{root: t.TempDir()}, Projects: listAlpha,
		Search: search, Fetch: fetch,
	})
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	return c, st, rec
}

func TestResearch_StagesGetTheWebToolsWhenResearchIsOn(t *testing.T) {
	var (
		mu   sync.Mutex
		runs = map[string]int64{}
	)

	runner := &fakeRunner{fn: func(ctx context.Context, in stage.Input) (stage.Output, error) {
		mu.Lock()
		runs[in.Stage] = runOf(ctx)
		mu.Unlock()

		return done("# "+in.Stage, nil), nil
	}}

	c, st, rec := newResearchPipeline(t, runner, searchOK, fakeFetcher{})
	driveIn(t, c, researchStart("BP1"))

	require.Equal(t, 2, runner.calls())

	for i := range 2 {
		assert.Subset(t, toolNames(runner.input(i)), []string{"web_search", "web_fetch"}, runner.input(i).Stage)
	}

	assert.False(t, rec.has(EventResearchOff))

	stored, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	for _, r := range stored {
		assert.Equal(t, r.ID, runs[r.Stage], "the run's context carries its id, for %s", r.Stage)
	}
}

func TestResearch_OffIsSaidOncePerSession(t *testing.T) {
	unknown := researchStart("BP1")
	unknown.SearchProvider = "nosuch"

	tests := []struct {
		name   string
		in     StartInput
		search func(string, string) (research.Searcher, error)
		reason string
	}{
		{"ContextMatrix has research off", startInput("BP1"), searchOK, "off"},
		{"an unknown provider", unknown, research.NewSearcher, `"nosuch"`},
		{"no research support", researchStart("BP1"), nil, "without research"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{fn: func(_ context.Context, in stage.Input) (stage.Output, error) {
				return done("# "+in.Stage, nil), nil
			}}
			c, _, rec := newResearchPipeline(t, runner, tt.search, fakeFetcher{})
			driveIn(t, c, tt.in)

			require.Equal(t, 2, runner.calls())

			for i := range 2 {
				assert.NotContains(t, toolNames(runner.input(i)), "web_search")
			}

			offs := rec.all(EventResearchOff)
			require.Len(t, offs, 1, "said once, though two stages have the group")
			assert.Equal(t, "intake", offs[0].Payload["stage"])
			assert.Contains(t, offs[0].Payload["reason"], tt.reason)
		})
	}
}

func TestResearch_TheSearchKeyIsMaskedLikeEverySecret(t *testing.T) {
	runner := &fakeRunner{fn: func(_ context.Context, in stage.Input) (stage.Output, error) {
		return done("# Need with "+testSearchKey, nil), nil
	}}
	c, st, _ := newResearchPipeline(t, runner, searchOK, fakeFetcher{})
	driveIn(t, c, researchStart("BP1"))

	art, err := st.LatestArtifact(context.Background(), "BP1", "intake")
	require.NoError(t, err)
	assert.NotContains(t, art.Body, testSearchKey)
}

func TestResearch_ARoundSaysWhetherItsStageResearched(t *testing.T) {
	tests := []struct {
		name string
		in   StartInput
		off  bool
	}{
		{"research on", researchStart("BP1"), false},
		{"research off", startInput("BP1"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{fn: func(ctx context.Context, in stage.Input) (stage.Output, error) {
				if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Store", "Which store?", "sqlite", "postgres")}}); err != nil {
					return stage.Output{}, err
				}

				return askedOutput(in), nil
			}}
			c, _, _ := newResearchPipeline(t, runner, searchOK, fakeFetcher{})
			driveIn(t, c, tt.in)

			assert.Equal(t, tt.off, latestRound(t, c, "BP1", 1).Frame.ResearchOff)
		})
	}
}

func TestResearch_ResponderLookupsCarryTheResponderRun(t *testing.T) {
	var (
		mu             sync.Mutex
		responderRun   int64
		responderTools []string
	)

	runner := &fakeRunner{}
	runner.fn = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		switch in.Stage {
		case "shape":
			if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Store", "Which store?", "sqlite", "postgres")}}); err != nil {
				return stage.Output{}, err
			}
		case "shape.responder":
			mu.Lock()
			responderRun, responderTools = runOf(ctx), toolNames(in)
			mu.Unlock()

			return done("", map[string]any{"answer": "sqlite", "reasoning": "small"}), nil
		}

		return done("# "+in.Stage, nil), nil
	}

	c, st, _ := newResearchPipeline(t, runner, searchOK, fakeFetcher{})
	driveIn(t, c, researchStart("BP1"))

	stored, err := st.ListRuns(context.Background(), "BP1")
	require.NoError(t, err)

	i := slices.IndexFunc(stored, func(r store.Run) bool { return r.Kind == store.RunKindResponder })
	require.GreaterOrEqual(t, i, 0, "the responder ran")

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, stored[i].ID, responderRun)
	assert.Subset(t, responderTools, []string{"web_search", "web_fetch"}, "the responder researches with the asking stage's tools")
}

// scriptLLM replays responses across every run it serves and keeps a copy
// of each request.
type scriptLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	requests  []llm.Request
}

func (s *scriptLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return s.next(req), nil
}

func (s *scriptLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return s.next(req), nil
}

func (s *scriptLLM) next(req llm.Request) llm.Response {
	s.mu.Lock()
	defer s.mu.Unlock()

	req.Messages = slices.Clone(req.Messages)
	s.requests = append(s.requests, req)

	if len(s.responses) == 0 {
		return llm.Response{Content: "done", FinishReason: "stop"}
	}

	r := s.responses[0]
	s.responses = s.responses[1:]

	return r
}

func call(id, name, args string) llm.Response {
	return llm.Response{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: args}}}}
}

func TestResearch_SearchResultListsAreNeverStored(t *testing.T) {
	model := &scriptLLM{responses: []llm.Response{
		call("c1", "web_search", `{"query":"postgres operator backups"}`),
		call("c2", "web_fetch", `{"url":"https://docs.example.org/backup"}`),
		call("c3", "submit_artifact", `{"body":"# Need"}`),
		call("c4", "submit_artifact", `{"body":"# Shape"}`),
	}}

	searcher := fakeSearcher{results: []research.Result{{Title: "Result title zebra", URL: "https://hit.example.org/zebra", Snippet: "snippet quokka"}}}
	fetcher := fakeFetcher{page: research.Page{URL: "https://docs.example.org/backup", Title: "Backup docs", Text: "page body walrus " + testSearchKey}}
	runner := &stage.Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return model }}

	c, st, _ := newResearchPipeline(t, runner, func(string, string) (research.Searcher, error) { return searcher, nil }, fetcher)
	driveIn(t, c, researchStart("BP1"))

	ctx := context.Background()

	// The model read the list in its run.
	model.mu.Lock()

	var sawList bool

	for _, req := range model.requests {
		for _, m := range req.Messages {
			sawList = sawList || strings.Contains(m.Content, "Result title zebra")
		}
	}

	model.mu.Unlock()
	assert.True(t, sawList, "the model saw the result list")

	forbidden := []string{"zebra", "quokka", testSearchKey}

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)

	var transcripts strings.Builder

	for _, r := range runs {
		raw, err := st.GetTranscript(ctx, r.ID)
		if err == nil {
			transcripts.Write(raw)
		}
	}

	assert.Contains(t, transcripts.String(), `web_search: 1 result for \"postgres operator backups\"`)
	assert.Contains(t, transcripts.String(), "page body walrus", "a fetched page is kept")

	events, err := st.ListEvents(ctx, "BP1")
	require.NoError(t, err)

	for _, e := range events {
		for _, word := range forbidden {
			assert.NotContains(t, string(e.Payload), word, "event %s", e.Kind)
		}
	}

	for _, word := range forbidden {
		assert.NotContains(t, transcripts.String(), word)
	}

	var fetchRun int64

	for _, e := range events {
		if e.Kind == EventResearch && strings.Contains(string(e.Payload), "web_fetch") {
			fetchRun = e.RunID
		}
	}

	intake := slices.IndexFunc(runs, func(r store.Run) bool { return r.Stage == "intake" })
	require.GreaterOrEqual(t, intake, 0)
	assert.Equal(t, runs[intake].ID, fetchRun, "a research event carries the run that made the call")

	sources, err := st.ListSources(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(sources, func(s store.Source) bool {
		return s.Kind == "url" && s.Identity == "https://docs.example.org/backup"
	}))

	require.NoError(t, c.Export(ctx, "BP1", "m-export"))

	exportFS := os.DirFS(c.opts.ExportDir)

	require.NoError(t, fs.WalkDir(exportFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		b, err := fs.ReadFile(exportFS, path)
		if err != nil {
			return err
		}

		for _, word := range forbidden {
			assert.NotContains(t, string(b), word, path)
		}

		return nil
	}))
}

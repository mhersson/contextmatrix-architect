package research

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSearcher struct {
	results []Result
	err     error
	queries []string
}

func (f *fakeSearcher) Search(_ context.Context, q string) ([]Result, error) {
	f.queries = append(f.queries, q)

	return f.results, f.err
}

type fakeFetcher struct {
	page Page
	err  error
	urls []string
}

func (f *fakeFetcher) Fetch(_ context.Context, u string) (Page, error) {
	f.urls = append(f.urls, u)

	return f.page, f.err
}

type fakeLedger struct {
	lookups []Lookup
	pages   []Page
	tag     string
	err     error
}

func (l *fakeLedger) Lookup(_ context.Context, lk Lookup) { l.lookups = append(l.lookups, lk) }

func (l *fakeLedger) Page(_ context.Context, p Page) (string, error) {
	l.pages = append(l.pages, p)

	return l.tag, l.err
}

func researchTools(s *fakeSearcher, f *fakeFetcher, l *fakeLedger) (tools.Tool, tools.Tool) {
	ts := Tools(s, f, testGuard(), l)

	return ts[0], ts[1]
}

func TestWebSearch_ListsResultsTheRecordDrops(t *testing.T) {
	s := &fakeSearcher{results: []Result{
		{Title: "Backups", URL: "https://cloudnative-pg.io/backup/", Snippet: "barman"},
		{Title: "Recovery", URL: "https://cloudnative-pg.io/recovery/"},
	}}
	l := &fakeLedger{}
	search, _ := researchTools(s, &fakeFetcher{}, l)

	assert.Equal(t, SearchToolName, search.Name())

	res, err := search.Execute(context.Background(), map[string]any{"query": " postgres operator backups "})
	require.NoError(t, err)
	assert.Contains(t, res.Text, "https://cloudnative-pg.io/backup/")
	assert.Contains(t, res.Text, "barman")
	assert.Equal(t, []string{"postgres operator backups"}, s.queries)
	assert.Equal(t, []Lookup{{Tool: SearchToolName, Query: "postgres operator backups", Results: 2}}, l.lookups)

	stored, ok := search.(interface{ Stored(string) string })
	require.True(t, ok, "web_search is transient")

	marker := stored.Stored(res.Text)
	assert.Contains(t, marker, `2 results for "postgres operator backups"`)
	assert.NotContains(t, marker, "cloudnative-pg.io")
	assert.Equal(t, marker, stored.Stored(marker), "storing a marker again changes nothing")
	assert.Equal(t, "tool error: x", stored.Stored("tool error: x"), "text it did not produce is kept")
}

func TestWebSearch_TheMarkerCountsTheResults(t *testing.T) {
	tests := []struct {
		results int
		want    string
	}{
		{0, `web_search: 0 results for "kustomize overlays"`},
		{1, `web_search: 1 result for "kustomize overlays"`},
		{3, `web_search: 3 results for "kustomize overlays"`},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			s := &fakeSearcher{results: make([]Result, tt.results)}
			search, _ := researchTools(s, &fakeFetcher{}, &fakeLedger{})

			res, err := search.Execute(context.Background(), map[string]any{"query": "kustomize overlays"})
			require.NoError(t, err)

			stored, ok := search.(interface{ Stored(string) string })
			require.True(t, ok, "web_search is transient")

			marker := stored.Stored(res.Text)
			assert.Contains(t, marker, tt.want)
			assert.Equal(t, marker, stored.Stored(marker), "storing a marker again changes nothing")
		})
	}
}

func TestWebSearch_RefusedQueriesNeverLeave(t *testing.T) {
	s := &fakeSearcher{}
	l := &fakeLedger{}
	search, _ := researchTools(s, &fakeFetcher{}, l)

	_, err := search.Execute(context.Background(), map[string]any{"query": "main.go crash"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rule file")
	assert.Empty(t, s.queries, "a refused query never reaches the provider")
	assert.Equal(t, []Lookup{{Tool: SearchToolName, Query: "main.go crash", Refused: RuleFile}}, l.lookups)

	_, err = search.Execute(context.Background(), map[string]any{"query": " "})
	require.Error(t, err)
	assert.Empty(t, s.queries, "an empty query never reaches the provider")
	assert.Len(t, l.lookups, 1, "an empty query is an argument error, not a logged call")
}

func TestWebSearch_ProviderFailureIsLogged(t *testing.T) {
	l := &fakeLedger{}
	search, _ := researchTools(&fakeSearcher{err: errors.New("rate limited (HTTP 429)")}, &fakeFetcher{}, l)

	_, err := search.Execute(context.Background(), map[string]any{"query": "kustomize overlays"})
	require.ErrorContains(t, err, "rate limited")
	assert.Equal(t, []Lookup{{Tool: SearchToolName, Query: "kustomize overlays", Error: "rate limited (HTTP 429)"}}, l.lookups)
}

func TestWebFetch_ReadsAPageAsASource(t *testing.T) {
	f := &fakeFetcher{page: Page{URL: "https://cloudnative-pg.io/backup/", Title: "Backups", Text: "Use barman."}}
	l := &fakeLedger{tag: "S7"}
	_, fetch := researchTools(&fakeSearcher{}, f, l)

	assert.Equal(t, FetchToolName, fetch.Name())

	res, err := fetch.Execute(context.Background(), map[string]any{"url": "https://cloudnative-pg.io/backup"})
	require.NoError(t, err)
	assert.Equal(t, "[S7] Backups\nhttps://cloudnative-pg.io/backup/\n\nUse barman.", res.Text)
	assert.Equal(t, []Page{f.page}, l.pages)
	assert.Equal(t, []Lookup{{
		Tool: FetchToolName, URL: "https://cloudnative-pg.io/backup", Final: "https://cloudnative-pg.io/backup/",
		Tag: "S7", Title: "Backups", Bytes: len("Use barman."),
	}}, l.lookups)

	_, isTransient := fetch.(interface{ Stored(string) string })
	assert.False(t, isTransient, "a fetched page is kept")
}

func TestWebFetch_MarksACutPage(t *testing.T) {
	f := &fakeFetcher{page: Page{URL: "https://example.com/long", Text: "start", Truncated: true}}
	_, fetch := researchTools(&fakeSearcher{}, f, &fakeLedger{tag: "S2"})

	res, err := fetch.Execute(context.Background(), map[string]any{"url": "https://example.com/long"})
	require.NoError(t, err)
	assert.Equal(t, "[S2] https://example.com/long\n\nstart\n\n[the page is cut at the size limit]", res.Text)
}

func TestWebFetch_RefusalsAreLogged(t *testing.T) {
	tests := []struct {
		name, url string
		fetchErr  error
		rule      string
		fetched   bool
	}{
		{"by the guard", "https://example.com/search?q=healthzHandler", nil, RuleIdentifier, false},
		{"by the address", "https://intranet.example.com/", &AddressError{Addr: netip.MustParseAddr("10.0.0.5"), Kind: "private"}, RuleAddress, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeFetcher{err: tt.fetchErr}
			l := &fakeLedger{}
			_, fetch := researchTools(&fakeSearcher{}, f, l)

			_, err := fetch.Execute(context.Background(), map[string]any{"url": tt.url})
			require.Error(t, err)
			assert.Equal(t, tt.fetched, len(f.urls) == 1)
			require.Len(t, l.lookups, 1)
			assert.Equal(t, tt.rule, l.lookups[0].Refused)
			assert.Equal(t, tt.url, l.lookups[0].URL)
		})
	}
}

func TestWebFetch_APageThatCannotBeRecordedIsAnError(t *testing.T) {
	f := &fakeFetcher{page: Page{URL: "https://example.com/", Text: "x"}}
	l := &fakeLedger{err: errors.New("store closed")}
	_, fetch := researchTools(&fakeSearcher{}, f, l)

	_, err := fetch.Execute(context.Background(), map[string]any{"url": "https://example.com/"})
	require.ErrorContains(t, err, "store closed")
	require.Len(t, l.lookups, 1)
	assert.Contains(t, l.lookups[0].Error, "store closed")
}

func TestWebFetch_APageWithNoReadableTextIsAnError(t *testing.T) {
	tests := []struct{ name, text string }{
		{"empty", ""},
		{"only white space", " \n\t \n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeFetcher{page: Page{URL: "https://example.com/app", Title: "App", Text: tt.text}}
			l := &fakeLedger{tag: "S3"}
			_, fetch := researchTools(&fakeSearcher{}, f, l)

			_, err := fetch.Execute(context.Background(), map[string]any{"url": "https://example.com/app"})
			require.ErrorContains(t, err, "no readable text")
			assert.Empty(t, l.pages, "a page with no text is not recorded as a source")
			require.Len(t, l.lookups, 1)
			assert.Equal(t, "https://example.com/app", l.lookups[0].URL)
			assert.Empty(t, l.lookups[0].Refused)
			assert.Empty(t, l.lookups[0].Tag)
			assert.Contains(t, l.lookups[0].Error, "no readable text")
		})
	}
}

func TestWebFetch_FailureThatIsNoRefusalIsLoggedAsError(t *testing.T) {
	tests := []struct {
		name     string
		fetchErr error
	}{
		{"unsupported content type", &TypeError{Type: "application/pdf"}},
		{"connection reset", errors.New("connection reset")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeFetcher{err: tt.fetchErr}
			l := &fakeLedger{}
			_, fetch := researchTools(&fakeSearcher{}, f, l)

			_, err := fetch.Execute(context.Background(), map[string]any{"url": "https://example.com/doc"})
			require.ErrorIs(t, err, tt.fetchErr)
			assert.Len(t, f.urls, 1)
			assert.Empty(t, l.pages, "a failed fetch is not recorded as a source")
			assert.Equal(t, []Lookup{{
				Tool: FetchToolName, URL: "https://example.com/doc", Error: tt.fetchErr.Error(),
			}}, l.lookups)
		})
	}
}

func TestWebFetch_MissingOrNonStringURLIsAnArgumentError(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{"missing", map[string]any{}},
		{"not a string", map[string]any{"url": 42}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeFetcher{}
			l := &fakeLedger{}
			_, fetch := researchTools(&fakeSearcher{}, f, l)

			_, err := fetch.Execute(context.Background(), tt.args)
			require.Error(t, err)
			assert.Empty(t, f.urls, "the fetcher is never called")
			assert.Empty(t, l.lookups, "an argument error is not a logged call")
		})
	}
}

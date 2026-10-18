package research

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// The research tools' names.
const (
	SearchToolName = "web_search"
	FetchToolName  = "web_fetch"
)

// searchMarker starts every web_search result; the result's first line is
// all the record keeps of it.
const searchMarker = SearchToolName + ": "

// Lookup is one research call as the event log keeps it: what was asked
// and how it ended, never a result list.
type Lookup struct {
	Tool string
	// Query is a web_search query; URL is the URL web_fetch was given.
	Query string
	URL   string
	// Results counts what a search returned.
	Results int
	// Final, Tag, Title, Bytes and Truncated describe a page web_fetch read.
	Final     string
	Tag       string
	Title     string
	Bytes     int
	Truncated bool
	// Refused names the guard rule, or RuleAddress, that refused the call.
	Refused string
	// Error is why a call nothing refused failed.
	Error string
}

// Ledger records what the research tools do: every call, and every page
// read as a source whose tag the page's result carries.
type Ledger interface {
	Lookup(ctx context.Context, l Lookup)
	Page(ctx context.Context, p Page) (string, error)
}

// PageFetcher reads one page; *Fetcher implements it.
type PageFetcher interface {
	Fetch(ctx context.Context, rawURL string) (Page, error)
}

// Tools returns web_search and web_fetch over one guard and ledger.
func Tools(s Searcher, f PageFetcher, g *Guard, l Ledger) []tools.Tool {
	return []tools.Tool{
		searchTool{searcher: s, guard: g, ledger: l},
		fetchTool{fetcher: f, guard: g, ledger: l},
	}
}

type searchTool struct {
	searcher Searcher
	guard    *Guard
	ledger   Ledger
}

func (searchTool) Name() string { return SearchToolName }

func (searchTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: SearchToolName,
		Description: "Search the web: titles, URLs and snippets of up to ten pages. The list is yours in this run only and is never kept, " +
			"so web_fetch every page you rely on or cite. Use plain words and technology names; a query with a path, a file name, code, " +
			"the project's own identifiers or names, or a card id is refused.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {"query": {"type": "string", "description": "plain words and technology names, one sentence"}},
			"required": ["query"]
		}`),
	}}
}

func (t searchTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	q, _ := args["query"].(string)
	q = strings.TrimSpace(q)

	if q == "" {
		return tools.Result{}, errors.New("web_search: query is required")
	}

	if err := t.guard.Query(ctx, q); err != nil {
		t.ledger.Lookup(ctx, Lookup{Tool: SearchToolName, Query: q, Refused: ruleOf(err)})

		return tools.Result{}, fmt.Errorf("web_search: %w; search with plain words and technology names, never the project's own names, paths or code", err)
	}

	results, err := t.searcher.Search(ctx, q)
	if err != nil {
		t.ledger.Lookup(ctx, Lookup{Tool: SearchToolName, Query: q, Error: err.Error()})

		return tools.Result{}, fmt.Errorf("web_search: %w", err)
	}

	t.ledger.Lookup(ctx, Lookup{Tool: SearchToolName, Query: q, Results: len(results)})

	var b strings.Builder

	noun := "results"
	if len(results) == 1 {
		noun = "result"
	}

	fmt.Fprintf(&b, "%s%d %s for %q. The list is not kept after this run: web_fetch a page to read and cite it.\n", searchMarker, len(results), noun, q)

	for i, r := range results {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", i+1, r.Title, r.URL)

		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}

	return tools.Result{Text: b.String()}, nil
}

// Stored keeps a result's first line, the query and the result count: the
// provider's terms forbid keeping the list. Other text is returned as it is.
func (searchTool) Stored(result string) string {
	head, _, _ := strings.Cut(result, "\n")
	if !strings.HasPrefix(head, searchMarker) {
		return result
	}

	return head
}

type fetchTool struct {
	fetcher PageFetcher
	guard   *Guard
	ledger  Ledger
}

func (fetchTool) Name() string { return FetchToolName }

func (fetchTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: FetchToolName,
		Description: "Read a public web page as text. The result starts with the page's source tag, such as [S4]; cite it for anything " +
			"taken from the page. Internal and private addresses are refused.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {"url": {"type": "string", "description": "an absolute http or https URL"}},
			"required": ["url"]
		}`),
	}}
}

func (t fetchTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	raw, _ := args["url"].(string)
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return tools.Result{}, errors.New("web_fetch: url is required")
	}

	lookup := Lookup{Tool: FetchToolName, URL: raw}

	fail := func(err error) (tools.Result, error) {
		if lookup.Refused = ruleOf(err); lookup.Refused == "" {
			lookup.Error = err.Error()
		}

		t.ledger.Lookup(ctx, lookup)

		return tools.Result{}, fmt.Errorf("web_fetch: %w", err)
	}

	u, err := t.guard.URL(ctx, raw)
	if err != nil {
		return fail(err)
	}

	page, err := t.fetcher.Fetch(ctx, u.String())
	if err != nil {
		return fail(err)
	}

	// A page with no text is no source: citing it would read as verified.
	if strings.TrimSpace(page.Text) == "" {
		return fail(errors.New("the page has no readable text; it may need JavaScript"))
	}

	tag, err := t.ledger.Page(ctx, page)
	if err != nil {
		return fail(fmt.Errorf("record the page as a source: %w", err))
	}

	lookup.Final, lookup.Tag, lookup.Title = page.URL, tag, page.Title
	lookup.Bytes, lookup.Truncated = len(page.Text), page.Truncated
	t.ledger.Lookup(ctx, lookup)

	var b strings.Builder

	fmt.Fprintf(&b, "[%s] %s\n", tag, cmp.Or(page.Title, page.URL))

	if page.Title != "" {
		b.WriteString(page.URL + "\n")
	}

	b.WriteString("\n" + page.Text)

	if page.Truncated {
		b.WriteString("\n\n[the page is cut at the size limit]")
	}

	return tools.Result{Text: b.String()}, nil
}

// ruleOf names the rule a refusal broke: a guard rule, RuleAddress for a
// non-public address, or "" for an error that is no refusal.
func ruleOf(err error) string {
	var (
		r *Refusal
		a *AddressError
	)

	switch {
	case errors.As(err, &a):
		return RuleAddress
	case errors.As(err, &r):
		return r.Rule
	}

	return ""
}

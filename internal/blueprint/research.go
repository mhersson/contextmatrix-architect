package blueprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/research"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// sourceKindURL is the kind of a source that is a web page.
const sourceKindURL = "url"

// runKey carries the id of the run a context belongs to: the asking run
// and its responder share their research tools, and each call is recorded
// under the run that made it.
type runKey struct{}

func withRun(ctx context.Context, runID int64) context.Context {
	return context.WithValue(ctx, runKey{}, runID)
}

func runOf(ctx context.Context) int64 {
	id, _ := ctx.Value(runKey{}).(int64)

	return id
}

// researchLedger records one stage's research: every call as a research
// event, every page read as a url source.
type researchLedger struct {
	c     *Controller
	au    audit
	id    string
	stage string
}

func (l researchLedger) Lookup(ctx context.Context, lk research.Lookup) {
	p := map[string]any{"stage": l.stage, "tool": lk.Tool}

	put := func(key string, v any, ok bool) {
		if ok {
			p[key] = v
		}
	}

	put("query", lk.Query, lk.Query != "")
	put("url", lk.URL, lk.URL != "")
	put("results", lk.Results, lk.Tool == research.SearchToolName && lk.Refused == "" && lk.Error == "")
	put("final_url", lk.Final, lk.Final != "")
	put("tag", lk.Tag, lk.Tag != "")
	put("title", lk.Title, lk.Title != "")
	put("bytes", lk.Bytes, lk.Tag != "")
	put("truncated", lk.Truncated, lk.Truncated)
	put("refused", lk.Refused, lk.Refused != "")
	put("error", lk.Error, lk.Error != "")

	l.au.event(context.WithoutCancel(ctx), Event{Kind: EventResearch, BlueprintID: l.id, RunID: runOf(ctx), Payload: p})
}

// Page records a page as a url source versioned by its text's hash, and
// returns its tag: the same page read again is the same source.
func (l researchLedger) Page(ctx context.Context, page research.Page) (string, error) {
	bg := context.WithoutCancel(ctx)
	sum := sha256.Sum256([]byte(page.Text))
	version := "sha256:" + hex.EncodeToString(sum[:])
	identity := l.au.text(page.URL)

	known, err := l.c.opts.Store.ListSources(bg, l.id)
	if err != nil {
		return "", fmt.Errorf("blueprint: list sources: %w", err)
	}

	for _, s := range known {
		if s.Kind == sourceKindURL && s.Identity == identity && s.Version == version {
			return s.Tag(), nil
		}
	}

	src, err := l.c.opts.Store.AddSource(bg, store.Source{
		BlueprintID: l.id, Kind: sourceKindURL, Identity: identity, Version: version,
		FetchedAt: l.c.opts.Now().UTC(), FirstUsedBy: l.stage,
	})
	if err != nil {
		return "", fmt.Errorf("blueprint: record source: %w", err)
	}

	return src.Tag(), nil
}

// sourceTitles maps the tag of every page web_fetch read to the page's
// title, from the research events that recorded the reads.
func (c *Controller) sourceTitles(ctx context.Context, id string) (map[string]string, error) {
	events, err := c.opts.Store.ListEventsOfKind(ctx, id, EventResearch)
	if err != nil {
		return nil, fmt.Errorf("blueprint: read the research events: %w", err)
	}

	out := map[string]string{}

	for _, e := range events {
		var p struct {
			Tag   string `json:"tag"`
			Title string `json:"title"`
		}

		if json.Unmarshal(e.Payload, &p) == nil && p.Tag != "" && p.Title != "" {
			out[p.Tag] = p.Title
		}
	}

	return out, nil
}

// researchGuard is the leak guard of one run of stageID. It reads its
// terms at the first check that needs them: from facts when the stage has
// the code tools, else from a checkout made then, so a stage with only the
// research tools checks out only once a query needs it.
func (c *Controller) researchGuard(sess *session, bp store.Blueprint, stageID string, facts *codeFacts) *research.Guard {
	return research.NewGuard(func(ctx context.Context) (research.Terms, error) {
		f := facts
		if f == nil {
			var err error
			if f, err = c.prepareCode(ctx, sess, bp, stageID); err != nil {
				return research.Terms{}, err
			}
		}

		return guardTerms(ctx, bp, f)
	})
}

// guardTerms is what the leak guard refuses for bp: the compound
// identifiers and module paths its checkouts declare, the target projects
// and their repositories' owner and repository names, and the card id
// prefix of every project ContextMatrix lists.
func guardTerms(ctx context.Context, bp store.Blueprint, facts *codeFacts) (research.Terms, error) {
	var t research.Terms

	for _, r := range facts.repos {
		names, err := code.Declared(ctx, filepath.Join(facts.root, r.Dir))
		if err != nil {
			return research.Terms{}, fmt.Errorf("blueprint: read %s for the leak guard: %w", r.Dir, err)
		}

		t.Identifiers = append(t.Identifiers, names.Identifiers...)
		t.Modules = append(t.Modules, names.Modules...)
	}

	t.Names = append(t.Names, bp.Projects...)

	for _, p := range facts.listed {
		if p.Prefix != "" {
			t.CardPrefixes = append(t.CardPrefixes, p.Prefix)
		}

		if !slices.Contains(bp.Projects, p.Name) {
			continue
		}

		for _, u := range p.RepoURLs() {
			t.Names = append(t.Names, repoNames(u)...)
		}

		for _, r := range p.Repos {
			if r.Name != "" {
				t.Names = append(t.Names, r.Name)
			}
		}
	}

	return t, nil
}

// repoNames are the owner and repository names in a repository URL:
// https://host/acme/payments.git and git@host:acme/payments.git both give
// acme and payments.
func repoNames(raw string) []string {
	s := code.CleanURL(raw)

	if _, rest, ok := strings.Cut(s, "://"); ok {
		_, s, _ = strings.Cut(rest, "/")
	} else if _, rest, ok := strings.Cut(s, ":"); ok {
		s = rest
	}

	segs := strings.FieldsFunc(s, func(r rune) bool { return r == '/' })
	if len(segs) > 2 {
		segs = segs[len(segs)-2:]
	}

	out := make([]string, 0, len(segs))

	for _, seg := range segs {
		if seg = strings.TrimSuffix(seg, ".git"); seg != "" {
			out = append(out, seg)
		}
	}

	return out
}

// The lines a research stage's task starts with. They are the controller's,
// not the pack's, because they depend on the session.
const (
	researchOnTask = "Web research: web_search lists pages for a query, and web_fetch reads one; its result starts with the source tag to cite. " +
		"Research a question with real alternatives before you ask it. Cite and quote only pages you fetched, never a search result or its snippet. " +
		"Search result lists are not kept after this run; the pages you fetch are, so fetch what you will cite before you ask. " +
		"Queries use plain words and technology names: the leak guard refuses paths, file names, code, the project's own identifiers and names, and card ids.\n\n"
	researchOffTask = "Research is off for this session: there are no web tools, so options rest on your own knowledge and the code; say so instead of citing a web source.\n\n"
)

// sessionResearch is the search provider a start's research settings name,
// or why research is off.
func (c *Controller) sessionResearch(in StartInput, au audit) (research.Searcher, string) {
	switch {
	case in.SearchProvider == "":
		return nil, "ContextMatrix has research off"
	case c.opts.Search == nil || c.opts.Fetch == nil:
		return nil, "this architect runs without research tools"
	}

	s, err := c.opts.Search(in.SearchProvider, in.SearchAPIKey)
	if err != nil {
		reason := "its search settings were refused: " + au.text(err.Error())
		c.opts.Logger.Warn("blueprint: research off", "blueprint", in.ID, "reason", reason)

		return nil, reason
	}

	return s, ""
}

// researchOn reports whether a run of st gets the research tools: the pack
// gives st the group and the session has research on.
func (c *Controller) researchOn(sess *session, st pack.Stage) bool {
	return slices.Contains(st.Tools, pack.ToolResearch) && sess.searcher != nil && c.opts.Fetch != nil
}

// noteResearchOff says, the first time a stage with the research group runs
// in a session without research, that research is off and why.
func (c *Controller) noteResearchOff(ctx context.Context, sess *session, id string, runID int64, stageID string) {
	if !sess.offNoted.CompareAndSwap(false, true) {
		return
	}

	sess.audit.event(context.WithoutCancel(ctx), Event{Kind: EventResearchOff, BlueprintID: id, RunID: runID, Payload: map[string]any{
		"stage": stageID, "reason": sess.researchOff,
	}})
}

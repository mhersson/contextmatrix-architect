package blueprint

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/code"
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

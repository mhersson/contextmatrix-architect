package stage

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"github.com/mhersson/contextmatrix-harness/tools"
)

// leadingTags matches the source tags a tool result leads with, as the tool
// wrappers write them: [S3] or [S2,S4].
var leadingTags = regexp.MustCompile(`^\s*\[\s*(S\d+(?:\s*,\s*S\d+)*)\s*\]`)

// returnedTags are the source tags a run's tool results led with, each once,
// in the order first returned. Tags deeper in a result do not count.
type returnedTags struct {
	mu   sync.Mutex
	seen map[string]bool
	tags []string
}

func (r *returnedTags) add(result string) {
	lead := leadingTags.FindStringSubmatch(result)
	if lead == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.seen == nil {
		r.seen = map[string]bool{}
	}

	for tag := range strings.SplitSeq(lead[1], ",") {
		if tag = strings.TrimSpace(tag); !r.seen[tag] {
			r.seen[tag] = true
			r.tags = append(r.tags, tag)
		}
	}
}

func (r *returnedTags) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.tags...)
}

// watch wraps each of ts so its results add their leading tags to r.
func (r *returnedTags) watch(ts []tools.Tool) []tools.Tool {
	out := make([]tools.Tool, 0, len(ts))
	for _, t := range ts {
		out = append(out, tagWatch{Tool: t, tags: r})
	}

	return out
}

// tagWatch hands a tool's results back unchanged and keeps their leading
// tags. ReadOnly and Terminal are the wrapped tool's: the harness asserts
// them on the wrapper.
type tagWatch struct {
	tools.Tool
	tags *returnedTags
}

func (t tagWatch) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	res, err := t.Tool.Execute(ctx, args)
	if err == nil {
		t.tags.add(res.Text)
	}

	return res, err
}

func (t tagWatch) ReadOnly() bool {
	ro, ok := t.Tool.(tools.ReadOnly)

	return ok && ro.ReadOnly()
}

func (t tagWatch) Terminal() bool {
	term, ok := t.Tool.(tools.Terminal)

	return ok && term.Terminal()
}

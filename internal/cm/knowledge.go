package cm

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"github.com/mhersson/contextmatrix-harness/llm"
	"github.com/mhersson/contextmatrix-harness/tools"
)

// Knowledge is what a run gives its knowledge tools.
type Knowledge interface {
	// Caller is the caller CM's knowledge log records for the run.
	Caller(ctx context.Context) string
	// Source returns the tag a result with items from source (CM's source
	// value) is cited by. ids are the ids of the result's items and related
	// entries, each once, in result order.
	Source(ctx context.Context, source string, ids []string) (string, error)
}

// KnowledgeTools wraps the knowledge reads CM serves; a name CM does not
// serve is skipped, and a CM with no knowledge providers serves none. The
// call adds no timeout of its own: CM's context and query tools may take 55 s.
func (c *Client) KnowledgeTools(ctx context.Context, k Knowledge) ([]tools.Tool, error) {
	listed, err := c.listTools(ctx)
	if err != nil {
		return nil, err
	}

	var out []tools.Tool

	for _, t := range listed {
		if !slices.Contains(KnowledgeReads(), t.Name) {
			continue
		}

		params, _, err := modelSchema(t.InputSchema, "caller")
		if err != nil {
			return nil, fmt.Errorf("cm: schema of %s: %w", t.Name, err)
		}

		out = append(out, &knowledgeTool{
			client: c, k: k, name: t.Name, description: t.Description,
			params: params, types: schemaPropTypes(t.InputSchema),
		})
	}

	return out, nil
}

// OpenKnowledge connects and wraps the knowledge reads. With none served the
// session is closed at once. The closer is never nil when err is nil.
func OpenKnowledge(ctx context.Context, baseURL, apiKey string, base http.RoundTripper, k Knowledge) ([]tools.Tool, func(), error) {
	c, err := Connect(ctx, baseURL, apiKey, base)
	if err != nil {
		return nil, nil, err
	}

	ts, err := c.KnowledgeTools(ctx, k)
	if err != nil {
		_ = c.Close()

		return nil, nil, err
	}

	if len(ts) == 0 {
		_ = c.Close()

		return nil, func() {}, nil
	}

	return ts, func() { _ = c.Close() }, nil
}

// knowledgeEntry is the part of a result's item the wrapper reads.
type knowledgeEntry struct {
	ID string `json:"id"`
}

// knowledgeTool is one CM knowledge read offered to a run.
type knowledgeTool struct {
	client      *Client
	k           Knowledge
	name        string
	description string
	params      json.RawMessage
	types       propTypes
}

func (t *knowledgeTool) Name() string   { return ToolPrefix + t.name }
func (t *knowledgeTool) ReadOnly() bool { return true }

func (t *knowledgeTool) Schema() llm.Tool {
	return llm.Tool{Type: "function", Function: llm.ToolFunction{
		Name: t.Name(), Description: t.description, Parameters: t.params,
	}}
}

// Execute fills caller from the run, never from the model, so CM's knowledge
// log names the architect's run. An answer without items has nothing to cite
// and is returned untagged.
func (t *knowledgeTool) Execute(ctx context.Context, args map[string]any) (tools.Result, error) {
	call := coerceArgs(args, t.types)
	delete(call, "agent_id")
	delete(call, "caller")

	if caller := t.k.Caller(ctx); caller != "" {
		call["caller"] = caller
	}

	text, err := t.client.CallText(ctx, t.name, call)
	if err != nil {
		return tools.Result{}, scrubbedError{err}
	}

	text = stripURLUserinfo(text)

	var reply struct {
		Source  string           `json:"source"`
		Items   []knowledgeEntry `json:"items"`
		Related []knowledgeEntry `json:"related"`
	}

	if json.Unmarshal([]byte(text), &reply) != nil || len(reply.Items)+len(reply.Related) == 0 {
		return tools.Result{Text: text}, nil
	}

	var ids []string

	for _, e := range slices.Concat(reply.Items, reply.Related) {
		if e.ID != "" && !slices.Contains(ids, e.ID) {
			ids = append(ids, e.ID)
		}
	}

	tag, err := t.k.Source(ctx, cmp.Or(reply.Source, "knowledge"), ids)
	if err != nil {
		return tools.Result{}, fmt.Errorf("cm: record knowledge source: %w", err)
	}

	return tools.Result{Text: "[" + tag + "] " + text}, nil
}

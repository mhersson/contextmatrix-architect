// Package cmtest holds a fixture ContextMatrix MCP server for tests.
package cmtest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Fixture replies, in the shape ContextMatrix's knowledge tools answer.
const (
	AgreedRule   = `{"source":"knowledge:intent","items":[{"id":"R-12","level":"what","kind":"rule","title":"Prices include VAT","text":"Every price shown includes VAT.","status":"agreed","agreed":true,"url":"https://u:secret@knowledge.example/r/12"}],"note":"one rule"}`
	OpenDecision = `{"source":"knowledge:intent","items":[{"id":"D-3","level":"what","kind":"decision","title":"Currencies","text":"Which currencies to support is open.","status":"open"}],"related":[],"missing":["D-9"]}`
	NoProvider   = `{"source":"knowledge","items":[],"reason":"project alpha names no knowledge provider"}`
	// ProviderTimedOut is CM's answer when it called the provider and the call timed out.
	ProviderTimedOut = `{"source":"knowledge:intent","items":[],"reason":"the knowledge provider timed out"}`
)

var knowledgeTools = []string{"get_knowledge_context", "query_knowledge", "get_knowledge_item"}

const (
	sharedProps = `"project":{"type":"string"},"card_id":{"type":"string"},"caller":{"type":"string"},"agent_id":{"type":"string"}`

	contextSchema = `{"type":"object","properties":{` + sharedProps + `,"task":{"type":"string"},"limit":{"type":"integer"}},"required":["task"]}`
	querySchema   = `{"type":"object","properties":{` + sharedProps + `,"query":{"type":"string"},"kinds":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer"}},"required":["query"]}`
	itemSchema    = `{"type":"object","properties":{` + sharedProps + `,"ids":{"type":"array","items":{"type":"string"}},"related":{"type":"boolean"}},"required":["ids"]}`
)

// Knowledge is a ContextMatrix MCP server serving knowledge tools.
type Knowledge struct {
	*httptest.Server

	mu      sync.Mutex
	replies map[string]string
	fails   map[string]string
	calls   map[string][]map[string]any
	bearers []string
}

// NewKnowledge starts the server serving the named tools; none named serves
// the three knowledge tools. A name outside them is served with an empty
// object schema and answers {}.
func NewKnowledge(t testing.TB, served ...string) *Knowledge {
	t.Helper()

	k := &Knowledge{
		replies: map[string]string{}, fails: map[string]string{}, calls: map[string][]map[string]any{},
	}

	if len(served) == 0 {
		served = knowledgeTools
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "cm-knowledge-test", Version: "0"}, nil)

	for _, name := range served {
		schema := `{"type":"object","properties":{}}`

		switch name {
		case "get_knowledge_context":
			schema = contextSchema
		case "query_knowledge":
			schema = querySchema
		case "get_knowledge_item":
			schema = itemSchema
		}

		server.AddTool(&mcp.Tool{
			Name: name, Description: name + " description",
			InputSchema: json.RawMessage(schema), OutputSchema: json.RawMessage(`{"type":"object"}`),
		}, k.handler(name))
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	k.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k.mu.Lock()
		k.bearers = append(k.bearers, r.Header.Get("Authorization"))
		k.mu.Unlock()
		mcpHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(k.Close)

	return k
}

func (k *Knowledge) handler(name string) mcp.ToolHandler {
	return func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any

		_ = json.Unmarshal(req.Params.Arguments, &args)

		k.mu.Lock()
		k.calls[name] = append(k.calls[name], args)
		failure, failed := k.fails[name]
		reply, answered := k.replies[name]
		k.mu.Unlock()

		if failed {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: failure}}, IsError: true}, nil
		}

		switch {
		case slices.Contains(knowledgeTools, name) && !answered:
			reply = NoProvider
		case !answered:
			reply = `{}`
		}

		res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply}}}
		if json.Valid([]byte(reply)) {
			res.StructuredContent = json.RawMessage(reply)
		}

		return res, nil
	}
}

// Answer sets the reply a tool gives from now on.
func (k *Knowledge) Answer(tool, reply string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.replies[tool] = reply
}

// Fail makes a tool answer with an error result carrying text.
func (k *Knowledge) Fail(tool, text string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.fails[tool] = text
}

// Calls returns the arguments of every call a tool took.
func (k *Knowledge) Calls(tool string) []map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()

	return slices.Clone(k.calls[tool])
}

// Bearers returns the Authorization header of every request received.
func (k *Knowledge) Bearers() []string {
	k.mu.Lock()
	defer k.mu.Unlock()

	return slices.Clone(k.bearers)
}

package cm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listProjectsReply is CM's raw project config: a repository url may carry
// a credential.
const listProjectsReply = `{"projects":[{"name":"beta","repo":"https://u:secret@example.com/beta.git",` +
	`"repos":[{"url":"git@example.com:acme/beta-web.git"},{"url":"https://example.com/beta-api.git"}],` +
	`"description":"Mail ops@example.com first."}]}`

// toolsServer serves list_projects, get_card and create_card, recording the
// arguments every call carried. get_playbook and list_cards are absent, as
// on an older CM.
type toolsServer struct {
	*httptest.Server

	mu   sync.Mutex
	args map[string][]map[string]any
}

func (s *toolsServer) record(name string, raw json.RawMessage) {
	var m map[string]any

	_ = json.Unmarshal(raw, &m)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.args[name] = append(s.args[name], m)
}

func (s *toolsServer) calls(name string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.args[name]
}

func newToolsServer(t *testing.T) *toolsServer {
	t.Helper()

	ts := &toolsServer{args: map[string][]map[string]any{}}
	server := mcp.NewServer(&mcp.Implementation{Name: "cm-test", Version: "0"}, nil)

	add := func(name, schema, reply string, isErr bool) {
		server.AddTool(&mcp.Tool{Name: name, Description: name + " description", InputSchema: json.RawMessage(schema)},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				ts.record(name, req.Params.Arguments)

				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: reply}}, IsError: isErr}, nil
			})
	}

	add("list_projects", `{"type":"object","properties":{"agent_id":{"type":"string"}},"required":["agent_id"]}`, listProjectsReply, false)
	add("get_card", `{"type":"object","properties":{
		"card_id":{"type":"string"},
		"agent_id":{"type":"string"},
		"include_images":{"type":["null","boolean"]},
		"include_activity_log":{"type":["null","boolean"]}
	},"required":["card_id","agent_id"]}`, `{"id":"ALPHA-001","title":"Add the thing"}`, false)
	add("create_card", `{"type":"object","properties":{"title":{"type":"string"}}}`, `{"id":"ALPHA-002"}`, false)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts.Server = httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts
}

func TestReadTools_AllowlistedPrefixedAndReadOnly(t *testing.T) {
	srv := newToolsServer(t)

	c, err := Connect(context.Background(), srv.URL, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.ReadTools(context.Background(), StageReads(), "S3")
	require.NoError(t, err)

	names := map[string]bool{}
	for _, tool := range got {
		names[tool.Name()] = true

		ro, ok := tool.(interface{ ReadOnly() bool })
		require.True(t, ok, "%s declares ReadOnly", tool.Name())
		assert.True(t, ro.ReadOnly())
		assert.Equal(t, tool.Name(), tool.Schema().Function.Name)
	}

	assert.Equal(t, map[string]bool{"cm_list_projects": true, "cm_get_card": true}, names,
		"allowlisted tools CM serves, prefixed; create_card is not allowlisted, the absent ones are skipped")
}

func TestReadTools_SchemaHidesAgentIDAndImages(t *testing.T) {
	srv := newToolsServer(t)

	c, err := Connect(context.Background(), srv.URL, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.ReadTools(context.Background(), StageReads(), "S3")
	require.NoError(t, err)

	schemas := map[string]map[string]any{}

	for _, tool := range got {
		var schema map[string]any
		require.NoError(t, json.Unmarshal(tool.Schema().Function.Parameters, &schema))

		schemas[tool.Name()] = schema
	}

	getCard := schemas["cm_get_card"]
	require.NotNil(t, getCard, "cm_get_card offered")

	props, _ := getCard["properties"].(map[string]any)
	assert.NotContains(t, props, "agent_id")
	assert.NotContains(t, props, "include_images")
	assert.Contains(t, props, "card_id")
	assert.Equal(t, []any{"card_id"}, getCard["required"])

	listProjects := schemas["cm_list_projects"]
	require.NotNil(t, listProjects, "cm_list_projects offered")
	assert.NotContains(t, listProjects, "required", "a required list left empty is dropped")
}

func TestReadTools_CallDropsAgentIDForcesNoImagesAndTagsTheResult(t *testing.T) {
	srv := newToolsServer(t)

	c, err := Connect(context.Background(), srv.URL, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.ReadTools(context.Background(), StageReads(), "S3")
	require.NoError(t, err)

	for _, tool := range got {
		if tool.Name() != "cm_get_card" {
			continue
		}

		res, err := tool.Execute(context.Background(), map[string]any{
			"card_id": "ALPHA-001", "agent_id": "human:someone", "include_images": "true", "include_activity_log": "false",
		})
		require.NoError(t, err)
		assert.Equal(t, `[S3] {"id":"ALPHA-001","title":"Add the thing"}`, res.Text)

		calls := srv.calls("get_card")
		require.Len(t, calls, 1)
		assert.NotContains(t, calls[0], "agent_id", "the stage model never names a caller")
		assert.Equal(t, false, calls[0]["include_images"])
		assert.Equal(t, false, calls[0]["include_activity_log"], "string scalars are coerced")

		return
	}

	t.Fatal("cm_get_card not offered")
}

func TestReadTools_ErrorResultIsAToolError(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "cm-test", Version: "0"}, nil)
	server.AddTool(&mcp.Tool{Name: "get_card", InputSchema: json.RawMessage(`{"type":"object","properties":{"card_id":{"type":"string"}}}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "card not found"}}, IsError: true}, nil
		})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(hs.Close)

	c, err := Connect(context.Background(), hs.URL, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.ReadTools(context.Background(), StageReads(), "S3")
	require.NoError(t, err)
	require.Len(t, got, 1)

	_, err = got[0].Execute(context.Background(), map[string]any{"card_id": "X-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "card not found")
}

func TestReadTools_ErrorResultCarriesNoURLCredential(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "cm-test", Version: "0"}, nil)
	server.AddTool(&mcp.Tool{Name: "get_card", InputSchema: json.RawMessage(`{"type":"object","properties":{"card_id":{"type":"string"}}}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
				Text: "clone failed: https://u:secret@example.com/beta.git",
			}}, IsError: true}, nil
		})

	hs := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(hs.Close)

	c, err := Connect(context.Background(), hs.URL, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.ReadTools(context.Background(), StageReads(), "S3")
	require.NoError(t, err)
	require.Len(t, got, 1)

	_, err = got[0].Execute(context.Background(), map[string]any{"card_id": "X-1"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret")
	assert.Contains(t, err.Error(), "https://example.com/beta.git")

	orig := errors.Unwrap(err)
	require.Error(t, orig, "the scrubbed error still unwraps to the original")
	assert.Contains(t, orig.Error(), "secret")
	assert.ErrorIs(t, err, orig)
}

// offeredTool returns the offered tool with the given model-facing name.
func offeredTool(t *testing.T, offered []tools.Tool, name string) tools.Tool {
	t.Helper()

	for _, tool := range offered {
		if tool.Name() == name {
			return tool
		}
	}

	t.Fatalf("%s not offered", name)

	return nil
}

func TestReadTools_ResultCarriesNoURLCredential(t *testing.T) {
	srv := newToolsServer(t)

	c, err := Connect(context.Background(), srv.URL, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	got, err := c.ReadTools(context.Background(), StageReads(), "S3")
	require.NoError(t, err)

	res, err := offeredTool(t, got, "cm_list_projects").Execute(context.Background(), map[string]any{})
	require.NoError(t, err)

	assert.NotContains(t, res.Text, "secret")
	assert.Equal(t, `[S3] {"projects":[{"name":"beta","repo":"https://example.com/beta.git",`+
		`"repos":[{"url":"git@example.com:acme/beta-web.git"},{"url":"https://example.com/beta-api.git"}],`+
		`"description":"Mail ops@example.com first."}]}`, res.Text,
		"only the userinfo goes; scp form, plain urls and email addresses stay")
}

func TestStripURLUserinfo(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"user and password", "https://u:secret@example.com/beta.git", "https://example.com/beta.git"},
		{"token only", "clone https://t0ken@example.com/acme/beta then", "clone https://example.com/acme/beta then"},
		{"an at sign in the password", "https://u:p@ss@example.com/x", "https://example.com/x"},
		{"other schemes", "ssh://git@example.com/acme/beta.git", "ssh://example.com/acme/beta.git"},
		{"two urls", `["https://a:1@x.com/a","HTTP://b:2@y.com"]`, `["https://x.com/a","HTTP://y.com"]`},
		{"no userinfo", "https://example.com/beta.git", "https://example.com/beta.git"},
		{"an at sign in the path", "https://example.com/users/@me", "https://example.com/users/@me"},
		{"an email address", "mail ops@example.com", "mail ops@example.com"},
		{"scp form", "git@example.com:acme/beta.git", "git@example.com:acme/beta.git"},
		{"quote in the password", "https://u:pa'ss@example.com/x", "https://example.com/x"},
		{"json escaped ampersand", `https://u:p\u0026ss@example.com/x`, "https://example.com/x"},
		{"underscores around the scheme", "__https://u:tok@example.com/x__", "__https://example.com/x__"},
		{"digit before the scheme", "1https://u:tok@example.com/x", "1https://example.com/x"},
		{
			"json escaped newline ends the url", `{"body":"Docs: https://docs.example.com\nalice@example.com owns it"}`,
			`{"body":"Docs: https://docs.example.com\nalice@example.com owns it"}`,
		},
		{"email in prose", "mail alice@example.com today", "mail alice@example.com today"},
		{"at sign in query and fragment", "https://example.com/x?to=a@b.c#frag@x", "https://example.com/x?to=a@b.c#frag@x"},
		{"caret in the password", "https://u:p^ss@h/x", "https://h/x"},
		{"brackets in the password", "https://u:p[1]@h/x", "https://h/x"},
		{"pipe in the password", "https://u:p|q@h/x", "https://h/x"},
		{"non-ascii in the password", "https://u:pä@h/x", "https://h/x"},
		{"markdown link then email", "[docs](https://example.com/x) and bob@example.com", "[docs](https://example.com/x) and bob@example.com"},
		{"two credentialed urls", "https://a:1@x.com/ and https://b:2@y.com/", "https://x.com/ and https://y.com/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, stripURLUserinfo(tt.in))
		})
	}
}

func TestStageReads_IsAFreshCopyOfTheAllowlist(t *testing.T) {
	reads := StageReads()
	assert.Equal(t, []string{"list_projects", "list_cards", "get_card", "get_playbook"}, reads)

	reads[0] = "create_card"

	assert.Equal(t, "list_projects", StageReads()[0], "a caller cannot widen the allowlist")
}

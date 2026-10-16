package cm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testServer(t *testing.T, wantKey string) *httptest.Server {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "cm-test", Version: "0"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "list_projects", Description: "list"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			text := `{"projects":[` +
				`{"name":"alpha","repo":"https://github.com/acme/alpha","verify":{"command":"make test"}},` +
				`{"name":"beta","repos":[{"name":"api","url":"https://github.com/acme/beta-api","primary":true},{"name":"web","url":"https://github.com/acme/beta-web"}]}` +
				`]}`

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		handler.ServeHTTP(w, r)
	}))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return srv
}

func TestListProjects(t *testing.T) {
	srv := testServer(t, "mcp-key")

	c, err := Connect(context.Background(), srv.URL, "mcp-key")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	projects, err := c.ListProjects(context.Background())
	require.NoError(t, err)
	require.Len(t, projects, 2)

	assert.Equal(t, []string{"https://github.com/acme/alpha"}, projects[0].RepoURLs())
	require.NotNil(t, projects[0].Verify)
	assert.Equal(t, "make test", projects[0].Verify.Command)

	assert.Equal(t, []string{"https://github.com/acme/beta-api", "https://github.com/acme/beta-web"}, projects[1].RepoURLs())
	assert.Nil(t, projects[1].Verify)
}

func TestConnect_WrongKeyFails(t *testing.T) {
	srv := testServer(t, "mcp-key")

	_, err := Connect(context.Background(), srv.URL, "wrong")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "wrong", "the key never reaches an error")
}

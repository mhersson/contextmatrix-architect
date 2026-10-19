package cm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCM is a fake CM MCP endpoint. dropSessions forgets every session, as a
// restarted CM would; dials counts the sessions clients opened.
type testCM struct {
	*httptest.Server

	handler atomic.Pointer[mcp.StreamableHTTPHandler]
	server  *mcp.Server
	dials   atomic.Int32
}

func (s *testCM) dropSessions() {
	s.handler.Store(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.server }, nil))
}

func testServer(t *testing.T, wantKey string) *testCM {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "cm-test", Version: "0"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "list_projects", Description: "list"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			text := `{"projects":[` +
				`{"name":"alpha","prefix":"ALPHA","repo":"https://github.com/acme/alpha","verify":{"command":"make test"},"poc":true},` +
				`{"name":"beta","repos":[{"name":"api","url":"https://github.com/acme/beta-api","primary":true},{"name":"web","url":"https://github.com/acme/beta-web"}]}` +
				`]}`

			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})

	cm := &testCM{server: server}
	cm.dropSessions()

	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		// Every new session opens with an initialize request.
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))

			if bytes.Contains(body, []byte(`"method":"initialize"`)) {
				cm.dials.Add(1)
			}
		}

		cm.handler.Load().ServeHTTP(w, r)
	}))

	cm.Server = httptest.NewServer(mux)
	t.Cleanup(cm.Close)

	return cm
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
	assert.Equal(t, "ALPHA", projects[0].Prefix)
	assert.True(t, projects[0].POC)

	assert.Equal(t, []string{"https://github.com/acme/beta-api", "https://github.com/acme/beta-web"}, projects[1].RepoURLs())
	assert.Nil(t, projects[1].Verify)
	assert.False(t, projects[1].POC, "an absent poc reads false, as from a ContextMatrix that predates it")
}

func TestConnect_WrongKeyFails(t *testing.T) {
	const wrongKey = "sk-cm-9f8e7d6c5b4a39281706f5e4d3c2b1a0"

	srv := testServer(t, "mcp-key")

	_, err := Connect(context.Background(), srv.URL, wrongKey)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), wrongKey, "the key never reaches an error")
}

func TestCallText_RedialsOnceWhenTheSessionIsGone(t *testing.T) {
	srv := testServer(t, "mcp-key")

	c, err := Connect(context.Background(), srv.URL, "mcp-key")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	_, err = c.ListProjects(context.Background())
	require.NoError(t, err)
	require.Equal(t, int32(1), srv.dials.Load())

	srv.dropSessions()

	projects, err := c.ListProjects(context.Background())
	require.NoError(t, err, "the call succeeds on a fresh session")
	assert.Len(t, projects, 2)
	assert.Equal(t, int32(2), srv.dials.Load(), "exactly one redial")

	_, err = c.ListProjects(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(2), srv.dials.Load(), "the fresh session is kept")
}

func TestCallText_AfterCloseFailsWithoutRedialling(t *testing.T) {
	srv := testServer(t, "mcp-key")

	c, err := Connect(context.Background(), srv.URL, "mcp-key")
	require.NoError(t, err)
	require.NoError(t, c.Close())

	_, err = c.CallText(context.Background(), "list_projects", nil)
	require.ErrorIs(t, err, ErrClosed)
	assert.Equal(t, int32(1), srv.dials.Load(), "a closed client never redials")
	require.NoError(t, c.Close(), "Close is idempotent")
}

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/config"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

const (
	e2eKey = "end-to-end-key-0123456789abcdef0"
	// e2eLLMKey is the session's LLM API key. The fake model writes it into
	// the artifact, so only the architect's masking keeps it off the panel.
	e2eLLMKey = "sk-or-0123456789"
)

// e2eSearchKey is the session's search key. The fake model writes it into
// the artifact, so only the architect's masking keeps it off the panel.
const e2eSearchKey = "brave-e2e-key-0123456789"

// fakeLLM answers every stage with one submit_artifact call and records the
// tools each request offers.
type fakeLLM struct {
	mu    sync.Mutex
	calls int
	tools [][]string
}

func (f *fakeLLM) Send(_ context.Context, req llm.Request) (llm.Response, error) {
	return f.respond(req), nil
}

func (f *fakeLLM) SendStream(_ context.Context, req llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return f.respond(req), nil
}

func (f *fakeLLM) respond(req llm.Request) llm.Response {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++

	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Function.Name)
	}

	f.tools = append(f.tools, names)

	return llm.Response{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
		ID: "c1", Type: "function",
		Function: llm.FunctionCall{Name: "submit_artifact", Arguments: `{"body":"# Need\n\nRun alpha on Kubernetes with ` + e2eLLMKey + ` and ` + e2eSearchKey + `.","data":{"slices":1}}`},
	}}}
}

func sign(t *testing.T, req *http.Request, body []byte) {
	t.Helper()

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := protocol.SignPayloadWithTimestamp(e2eKey, req.Method, req.URL.RequestURI(), body, ts)
	req.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	req.Header.Set(protocol.TimestampHeader, ts)
}

func post(t *testing.T, client *http.Client, url string, body any) *http.Response {
	t.Helper()

	raw, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(raw))
	require.NoError(t, err)
	sign(t, req, raw)

	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	return resp
}

func TestServe_EndToEnd(t *testing.T) {
	cfg := &config.Config{
		ContextMatrixURL: "http://cm.invalid", APIKey: e2eKey, Port: 0, LogLevel: "info",
		StateDir: t.TempDir(), Pack: "../blueprint/testdata/research", MaxRunCostUSD: 1, MaxTurns: 5, WorkspaceMaxAgeDays: 7,
		ReplaySkew: 5 * time.Minute, ReplayCacheSize: 100, MessageDedupTTL: time.Minute, MessageDedupCacheSize: 100,
	}

	model := &fakeLLM{}
	runner := &stage.Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return model }}

	build := protocol.BuildInfo{Commit: strings.Repeat("a", 40), BuildTime: "2026-10-21T08:00:00Z", Changes: []protocol.Change{}}
	deps, err := loadRuntime(cfg)
	require.NoError(t, err)

	srv, cleanup, err := buildServer(context.Background(), cfg, deps, testLogger(t), runner, build)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanup(context.Background()) })

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	// /health reports the build serve was given.
	health, err := ts.Client().Get(ts.URL + "/health")
	require.NoError(t, err)
	t.Cleanup(func() { _ = health.Body.Close() })

	var hr protocol.HealthResponse
	require.NoError(t, json.NewDecoder(health.Body).Decode(&hr))
	require.NotNil(t, hr.Build)
	assert.Equal(t, build, *hr.Build)

	// Start the blueprint session the way CM does.
	resp := post(t, ts.Client(), ts.URL+"/chat/start", protocol.ChatStartPayload{
		SessionID: "S1", Model: "test/model", MCPAPIKey: "mcp", GitCredentialsToken: "S1.mac",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openrouter", APIKey: e2eLLMKey},
		Architect: &protocol.ArchitectStart{
			Projects: []string{"alpha"}, Mode: "default",
			Research: &protocol.ResearchStart{Provider: "brave", APIKey: e2eSearchKey},
		},
	})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Connect to the session's log stream after the start, the way CM does.
	// The deadline ends a read that would otherwise block forever.
	logsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	logsReq, err := http.NewRequestWithContext(logsCtx, http.MethodGet, ts.URL+"/logs?session_id=S1", nil)
	require.NoError(t, err)
	sign(t, logsReq, nil)

	logsResp, err := ts.Client().Do(logsReq)
	require.NoError(t, err)
	t.Cleanup(func() { _ = logsResp.Body.Close() })
	require.Equal(t, http.StatusOK, logsResp.StatusCode)

	reader := bufio.NewReader(logsResp.Body)
	first, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, ": connected\n", first)

	// The entry published during the start was retained for this consumer.
	var created protocol.LogEntry

	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)

		if strings.HasPrefix(line, "data: ") {
			require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &created))

			break
		}
	}

	assert.Equal(t, "system", created.Type, "the created entry is replayed first")

	// The user states the need.
	resp = post(t, ts.Client(), ts.URL+"/message", protocol.MessagePayload{SessionID: "S1", Content: "run alpha on kubernetes", MessageID: "m1"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Collect entries until the artifact arrives and the indicator is idle.
	var (
		types       []string
		sawArtifact bool
		idle        bool
	)

	for !idle {
		line, err := reader.ReadString('\n')
		require.NoError(t, err, "stream ended; saw %v", types)

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var entry protocol.LogEntry
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &entry))
		types = append(types, entry.Type)

		if entry.Marker != nil && entry.Marker.Kind == "artifact" && entry.Marker.Stage == "intake" {
			sawArtifact = true
		}

		idle = sawArtifact && entry.Type == "status" && entry.Content == "idle"

		assert.NotContains(t, entry.Content, e2eLLMKey, "the API key never reaches the panel")
		assert.NotContains(t, entry.Content, e2eSearchKey, "the search key never reaches the panel")
	}

	model.mu.Lock()
	require.NotEmpty(t, model.tools)
	assert.Subset(t, model.tools[0], []string{"web_search", "web_fetch"}, "intake researches when CM sends research settings")
	model.mu.Unlock()

	assert.Contains(t, types, "system", "stage boundaries are system entries")
	assert.Contains(t, types, "tool_call", "harness tool calls stream through the bridge")

	// End detaches; a message after that is a 404.
	resp = post(t, ts.Client(), ts.URL+"/chat/end", protocol.ChatEndPayload{SessionID: "S1"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	resp = post(t, ts.Client(), ts.URL+"/message", protocol.MessagePayload{SessionID: "S1", Content: "more", MessageID: "m2"})
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	// The record survives on disk.
	assert.FileExists(t, filepath.Join(cfg.StateDir, "architect.db"))
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

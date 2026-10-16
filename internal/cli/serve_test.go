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
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/config"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

const e2eKey = "end-to-end-key-0123456789abcdef0"

// fakeLLM answers the intake with one submit_artifact call.
type fakeLLM struct{ calls int }

func (f *fakeLLM) Send(_ context.Context, _ llm.Request) (llm.Response, error) {
	return f.respond(), nil
}

func (f *fakeLLM) SendStream(_ context.Context, _ llm.Request, _ func(llm.Delta)) (llm.Response, error) {
	return f.respond(), nil
}

func (f *fakeLLM) respond() llm.Response {
	f.calls++

	return llm.Response{FinishReason: "tool_calls", ToolCalls: []llm.ToolCall{{
		ID: "c1", Type: "function",
		Function: llm.FunctionCall{Name: "submit_artifact", Arguments: `{"body":"# Need\n\nRun alpha on Kubernetes.","data":{"slices":1}}`},
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
		StateDir: t.TempDir(), MaxRunCostUSD: 1, MaxTurns: 5,
		ReplaySkew: 5 * time.Minute, ReplayCacheSize: 100, MessageDedupTTL: time.Minute, MessageDedupCacheSize: 100,
	}

	runner := &stage.Runner{NewLLM: func(protocol.LLMEndpoint) llm.LLM { return &fakeLLM{} }}

	srv, cleanup, err := buildServer(context.Background(), cfg, testLogger(t), runner)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanup(context.Background()) })

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	// Subscribe to the session's log stream before anything happens.
	logsReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/logs?session_id=S1", nil)
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

	// Start the blueprint session the way CM does.
	resp := post(t, ts.Client(), ts.URL+"/chat/start", protocol.ChatStartPayload{
		SessionID: "S1", Model: "test/model", MCPAPIKey: "mcp", GitCredentialsToken: "S1.mac",
		LLMEndpoint: &protocol.LLMEndpoint{Type: "openrouter", APIKey: "sk-or-0123456789"},
		Architect:   &protocol.ArchitectStart{Projects: []string{"alpha"}, Mode: "default"},
	})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	// The user states the need.
	resp = post(t, ts.Client(), ts.URL+"/message", protocol.MessagePayload{SessionID: "S1", Content: "run alpha on kubernetes", MessageID: "m1"})
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	// Collect entries until the artifact arrives.
	deadline := time.After(10 * time.Second)

	var (
		types       []string
		sawArtifact bool
	)

	for !sawArtifact {
		select {
		case <-deadline:
			t.Fatalf("no artifact entry; saw %v", types)
		default:
		}

		line, err := reader.ReadString('\n')
		require.NoError(t, err)

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var entry protocol.LogEntry
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &entry))
		types = append(types, entry.Type)

		if entry.Type == "text" && strings.Contains(entry.Content, "Run alpha on Kubernetes") {
			sawArtifact = true
		}

		assert.NotContains(t, entry.Content, "sk-or-0123456789", "the API key never reaches the panel")
	}

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

package webhook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/blueprint"
)

const testKey = "test-secret-key-0123456789abcdef"

type stubController struct {
	mu       sync.Mutex
	starts   []blueprint.StartInput
	messages []string
	ends     []string
	startErr error
	msgErr   error
	endErr   error
	// ctxErrs is the context's error as each call saw it, in call order.
	ctxErrs []error
}

func (s *stubController) Start(ctx context.Context, in blueprint.StartInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.starts = append(s.starts, in)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())

	return s.startErr
}

func (s *stubController) Message(ctx context.Context, id, text, msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.messages = append(s.messages, id+"|"+text+"|"+msgID)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())

	return s.msgErr
}

func (s *stubController) End(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ends = append(s.ends, id)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())

	return s.endErr
}

func newTestServer(t *testing.T, ctl *stubController) (*Server, *logbridge.Hub) {
	t.Helper()

	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, nil)
	srv := NewServer(Config{APIKey: testKey, Hub: hub, Controller: ctl, Logger: slog.New(slog.DiscardHandler)})

	return srv, hub
}

// signed builds a request signed the way CM signs webhook calls; ts varies
// per call so the replay cache never sees two identical signatures.
func signed(t *testing.T, method, path string, body any, ts string) *http.Request {
	t.Helper()

	var raw []byte

	if body != nil {
		var err error

		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}

	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	sig := protocol.SignPayloadWithTimestamp(testKey, method, r.URL.RequestURI(), raw, ts)
	r.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	r.Header.Set(protocol.TimestampHeader, ts)

	return r
}

var tsCounter int64

func nextTS() string {
	tsCounter++

	// Distinct per call and never ahead of now, so it stays inside the skew window.
	return strconv.FormatInt(time.Now().Unix()-tsCounter, 10)
}

func validStart() protocol.ChatStartPayload {
	return protocol.ChatStartPayload{
		SessionID:           "01HZZZZZZZZZZZZZZZZZZZZZZZ",
		MCPAPIKey:           "mcp-key",
		Model:               "anthropic/claude-sonnet-4",
		LLMEndpoint:         &protocol.LLMEndpoint{Type: "openrouter", APIKey: "sk-or-key"},
		GitCredentialsToken: "01HZZ.mac",
		Architect:           &protocol.ArchitectStart{Projects: []string{"alpha"}, Mode: "default", CostCapUSD: 2},
	}
}

func do(srv *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	srv.Routes().ServeHTTP(w, r)

	return w
}

func TestStart_HappyPath(t *testing.T) {
	ctl := &stubController{}
	srv, _ := newTestServer(t, ctl)

	w := do(srv, signed(t, http.MethodPost, "/chat/start", validStart(), nextTS()))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())

	var resp protocol.ChatStartResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.OK)
	assert.Equal(t, "01HZZZZZZZZZZZZZZZZZZZZZZZ", resp.ContainerID)

	require.Len(t, ctl.starts, 1)
	in := ctl.starts[0]
	assert.Equal(t, "01HZZZZZZZZZZZZZZZZZZZZZZZ", in.ID)
	assert.Equal(t, []string{"alpha"}, in.Projects)
	assert.Equal(t, "default", in.Mode)
	assert.InDelta(t, 2.0, in.CostCapUSD, 1e-9)
	assert.Equal(t, "anthropic/claude-sonnet-4", in.Model)
	assert.Equal(t, "sk-or-key", in.Endpoint.APIKey)
}

func TestStart_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*protocol.ChatStartPayload)
		status int
		code   string
	}{
		{"no llm endpoint", func(p *protocol.ChatStartPayload) { p.LLMEndpoint = nil }, http.StatusInternalServerError, protocol.CodeInternal},
		{"no architect block", func(p *protocol.ChatStartPayload) { p.Architect = nil }, http.StatusBadRequest, protocol.CodeInvalidField},
		{"no projects", func(p *protocol.ChatStartPayload) { p.Architect.Projects = nil }, http.StatusBadRequest, protocol.CodeInvalidField},
		{"bad mode", func(p *protocol.ChatStartPayload) { p.Architect.Mode = "turbo" }, http.StatusBadRequest, protocol.CodeInvalidField},
		{"bad session id", func(p *protocol.ChatStartPayload) { p.SessionID = "../x" }, http.StatusBadRequest, protocol.CodeInvalidField},
		{"empty session id", func(p *protocol.ChatStartPayload) { p.SessionID = "" }, http.StatusBadRequest, protocol.CodeInvalidField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &stubController{}
			srv, _ := newTestServer(t, ctl)

			p := validStart()
			tc.mutate(&p)

			w := do(srv, signed(t, http.MethodPost, "/chat/start", p, nextTS()))
			assert.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tc.code)
			assert.Empty(t, ctl.starts, "refused before the controller is touched")
		})
	}
}

func TestStart_ControllerErrorIs500(t *testing.T) {
	ctl := &stubController{startErr: context.DeadlineExceeded}
	srv, _ := newTestServer(t, ctl)

	w := do(srv, signed(t, http.MethodPost, "/chat/start", validStart(), nextTS()))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), protocol.CodeInternal)
}

func TestARequestCancelDoesNotReachTheController(t *testing.T) {
	cases := []struct {
		path string
		body any
	}{
		{"/chat/start", validStart()},
		{"/message", protocol.MessagePayload{SessionID: "S1", Content: "hi", MessageID: "m1"}},
		{"/chat/end", protocol.ChatEndPayload{SessionID: "S1"}},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			ctl := &stubController{}
			srv, _ := newTestServer(t, ctl)

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			w := do(srv, signed(t, http.MethodPost, tc.path, tc.body, nextTS()).WithContext(ctx))
			require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
			require.Len(t, ctl.ctxErrs, 1)
			assert.NoError(t, ctl.ctxErrs[0], "the call runs to completion once the request is in")
		})
	}
}

func TestShuttingDownIs503Draining(t *testing.T) {
	cases := []struct {
		path string
		body any
	}{
		{"/chat/start", validStart()},
		{"/message", protocol.MessagePayload{SessionID: "S1", Content: "hi", MessageID: "m1"}},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			ctl := &stubController{startErr: blueprint.ErrShuttingDown, msgErr: blueprint.ErrShuttingDown}
			srv, _ := newTestServer(t, ctl)

			w := do(srv, signed(t, http.MethodPost, tc.path, tc.body, nextTS()))
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())

			var resp protocol.ErrorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, protocol.CodeDraining, resp.Code)
		})
	}
}

func TestUnsignedIsRefused(t *testing.T) {
	cases := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/chat/start", validStart()},
		{http.MethodPost, "/message", protocol.MessagePayload{SessionID: "S1", Content: "hi", MessageID: "m1"}},
		{http.MethodPost, "/chat/end", protocol.ChatEndPayload{SessionID: "S1"}},
		{http.MethodGet, "/logs?session_id=S1", nil},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			ctl := &stubController{}
			srv, _ := newTestServer(t, ctl)

			var raw []byte

			if tc.body != nil {
				var err error

				raw, err = json.Marshal(tc.body)
				require.NoError(t, err)
			}

			w := do(srv, httptest.NewRequest(tc.method, tc.path, bytes.NewReader(raw)))
			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Empty(t, ctl.starts)
			assert.Empty(t, ctl.messages)
			assert.Empty(t, ctl.ends)
		})
	}
}

func TestMessage_DeliversAndDedups(t *testing.T) {
	ctl := &stubController{}
	srv, _ := newTestServer(t, ctl)

	msg := protocol.MessagePayload{SessionID: "S1", Content: "containerise it", MessageID: "m1"}

	w := do(srv, signed(t, http.MethodPost, "/message", msg, nextTS()))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Equal(t, []string{"S1|containerise it|m1"}, ctl.messages)

	w = do(srv, signed(t, http.MethodPost, "/message", msg, nextTS()))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "duplicate")
	assert.Len(t, ctl.messages, 1, "a retry is acknowledged, not redelivered")
}

func TestMessage_UnknownBlueprintIs404AndNotRecorded(t *testing.T) {
	ctl := &stubController{msgErr: blueprint.ErrNotFound}
	srv, _ := newTestServer(t, ctl)

	msg := protocol.MessagePayload{SessionID: "S1", Content: "hi", MessageID: "m1"}

	w := do(srv, signed(t, http.MethodPost, "/message", msg, nextTS()))
	assert.Equal(t, http.StatusNotFound, w.Code)

	ctl.msgErr = nil
	w = do(srv, signed(t, http.MethodPost, "/message", msg, nextTS()))
	assert.Equal(t, http.StatusAccepted, w.Code, "the failed delivery was rolled back from the dedup cache")
}

func TestEnd_UnknownIs200KnownIs202(t *testing.T) {
	ctl := &stubController{endErr: blueprint.ErrNotFound}
	srv, _ := newTestServer(t, ctl)

	w := do(srv, signed(t, http.MethodPost, "/chat/end", protocol.ChatEndPayload{SessionID: "S1"}, nextTS()))
	assert.Equal(t, http.StatusOK, w.Code)

	ctl.endErr = nil
	w = do(srv, signed(t, http.MethodPost, "/chat/end", protocol.ChatEndPayload{SessionID: "S1"}, nextTS()))
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, []string{"S1", "S1"}, ctl.ends)
}

func TestLogs_StreamsEntriesForTheSession(t *testing.T) {
	srv, hub := newTestServer(t, &stubController{})
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	// The deadline ends a read that would otherwise block forever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/logs?session_id=S1", nil)
	require.NoError(t, err)

	tsUsed := nextTS()
	sig := protocol.SignPayloadWithTimestamp(testKey, http.MethodGet, "/logs?session_id=S1", nil, tsUsed)
	req.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	req.Header.Set(protocol.TimestampHeader, tsUsed)

	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, ": connected\n", first)

	hub.Publish(protocol.LogEntry{SessionID: "OTHER", Type: "text", Content: "not for us"})
	hub.Publish(protocol.LogEntry{SessionID: "S1", Type: "text", Content: "hello"})

	var data string

	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)

		if strings.HasPrefix(line, "data: ") {
			data = line

			break
		}
	}

	assert.Contains(t, data, `"content":"hello"`)
	assert.NotContains(t, data, "not for us")
}

func TestHealthAndReadyz(t *testing.T) {
	srv, _ := newTestServer(t, &stubController{})

	w := do(srv, httptest.NewRequest(http.MethodGet, "/health", nil))
	assert.Equal(t, http.StatusOK, w.Code)

	w = do(srv, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, w.Code)
}

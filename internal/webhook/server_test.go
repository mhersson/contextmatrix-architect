package webhook

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	mu        sync.Mutex
	starts    []blueprint.StartInput
	messages  []string
	aborts    []string
	exports   []string
	policies  []string
	ends      []string
	startErr  error
	msgErr    error
	endErr    error
	abortErr  error
	exportErr error
	view      protocol.BlueprintView
	viewErr   error
	art       protocol.BlueprintArtifact
	artErr    error
	artCalls  []string
	frames    chan protocol.LiveFrame
	subErr    error
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

func (s *stubController) Abort(ctx context.Context, id, msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.aborts = append(s.aborts, id+"|"+msgID)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())

	return s.abortErr
}

func (s *stubController) SetPolicy(ctx context.Context, id, stageID, policy, msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.policies = append(s.policies, id+"|"+stageID+"|"+policy+"|"+msgID)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())

	return nil
}

func (s *stubController) Export(ctx context.Context, id, msgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.exports = append(s.exports, id+"|"+msgID)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())

	return s.exportErr
}

func (s *stubController) End(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ends = append(s.ends, id)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())

	return s.endErr
}

func (s *stubController) View(_ context.Context, _ string) (protocol.BlueprintView, error) {
	return s.view, s.viewErr
}

func (s *stubController) Artifact(_ context.Context, id, stageID string, version int) (protocol.BlueprintArtifact, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.artCalls = append(s.artCalls, fmt.Sprintf("%s|%s|%d", id, stageID, version))

	return s.art, s.artErr
}

func (s *stubController) Subscribe(ctx context.Context, _ string) (<-chan protocol.LiveFrame, error) {
	if s.subErr != nil {
		return nil, s.subErr
	}

	out := make(chan protocol.LiveFrame)

	go func() {
		defer close(out)

		for {
			select {
			case f := <-s.frames:
				select {
				case out <- f:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
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

// tsWindow is how far behind now a test timestamp may fall, in seconds,
// inside the server's skew window (protocol.DefaultMaxClockSkew).
const tsWindow = 4 * 60

// tsSource hands out request timestamps. The replay cache rejects a second
// request with the same signature, so two signings of one body need distinct
// timestamps. Counting down from the last value, not from now, keeps them
// distinct when the clock crosses a second boundary between calls, and
// starting again at now before leaving tsWindow keeps them inside the skew
// window however many calls a process makes. Any tsWindow consecutive values
// are distinct, and no test signs that many requests against one server.
type tsSource struct {
	now  func() time.Time
	last int64
}

func (s *tsSource) next() string {
	now := s.now().Unix()

	v := s.last - 1
	if v < now-tsWindow || v > now {
		v = now
	}

	s.last = v

	return strconv.FormatInt(v, 10)
}

var testTS = &tsSource{now: time.Now}

func nextTS() string {
	return testTS.next()
}

func TestNextTS_StaysDistinctAndInsideTheSkewWindow(t *testing.T) {
	tests := []struct {
		name string
		tick time.Duration
	}{
		{"the clock crosses a second boundary between calls", time.Second},
		{"the clock stands still", 0},
		{"the clock moves faster than the calls", 3 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A nanosecond before a second boundary, so the first tick crosses it.
			clock, called := time.Unix(1_700_000_000, 999_999_999), time.Time{}
			src := &tsSource{now: func() time.Time {
				called = clock
				clock = clock.Add(tt.tick)

				return called
			}}

			seen := map[string]int{}

			for i := range 1000 {
				ts := src.next()

				v, err := strconv.ParseInt(ts, 10, 64)
				require.NoError(t, err)
				require.LessOrEqual(t, v, called.Unix(), "call %d is never ahead of now", i)
				require.Greater(t, v, called.Unix()-int64(protocol.DefaultMaxClockSkew/time.Second), "call %d is inside the skew window", i)

				if prev, ok := seen[ts]; ok {
					require.GreaterOrEqual(t, i-prev, tsWindow, "calls %d and %d share a timestamp", prev, i)
				}

				seen[ts] = i
			}
		})
	}
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

	payload := validStart()
	payload.Architect.Price = &protocol.ModelPrice{PromptUSDPerMTok: 3}

	w := do(srv, signed(t, http.MethodPost, "/chat/start", payload, nextTS()))
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
	assert.Equal(t, "mcp-key", in.MCPAPIKey, "code stages list projects with the session's MCP key")
	assert.Equal(t, "01HZZ.mac", in.GitCredentialsToken, "and fetch git credentials with its token")
	require.NotNil(t, in.Price)
	assert.InDelta(t, 3.0, in.Price.PromptUSDPerMTok, 1e-9)
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

// The controller answers not found for an export to a session it does not
// hold, as for a message, so ContextMatrix starts the session and re-sends.
func TestExport_UnattachedBlueprintIs404AndNotRecorded(t *testing.T) {
	ctl := &stubController{exportErr: blueprint.ErrNotFound}
	srv, _ := newTestServer(t, ctl)

	msg := protocol.MessagePayload{SessionID: "S1", Content: "export", MessageID: "m1"}

	w := do(srv, signed(t, http.MethodPost, "/message", msg, nextTS()))
	assert.Equal(t, http.StatusNotFound, w.Code)

	var body protocol.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, protocol.CodeNotFound, body.Code, "ContextMatrix restarts the session only on not_found")

	ctl.mu.Lock()
	ctl.exportErr = nil
	ctl.mu.Unlock()

	w = do(srv, signed(t, http.MethodPost, "/message", msg, nextTS()))
	assert.Equal(t, http.StatusAccepted, w.Code, "the re-sent export is delivered, not taken for a duplicate")
	assert.Equal(t, []string{"S1|m1", "S1|m1"}, ctl.exports)
	assert.Empty(t, ctl.messages)
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

func TestMessage_CommandsAreRouted(t *testing.T) {
	tests := []struct {
		text         string
		wantAborts   int
		wantExports  int
		wantPolicies []string
		wantMessages int
	}{
		{text: "abort", wantAborts: 1},
		{text: "  Export ", wantExports: 1},
		{text: "Gate Grounding HUMAN", wantPolicies: []string{"BP1|grounding|human|m-Gate Grounding HUMAN"}},
		{text: "please abort this", wantMessages: 1},
		{text: "gate it behind a flag", wantMessages: 1},
		{text: "the design", wantMessages: 1},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			ctl := &stubController{}
			srv, _ := newTestServer(t, ctl)

			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, signed(t, http.MethodPost, "/message",
				protocol.MessagePayload{SessionID: "BP1", Content: tt.text, MessageID: "m-" + strings.TrimSpace(tt.text)}, nextTS()))
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

			assert.Len(t, ctl.aborts, tt.wantAborts)
			assert.Len(t, ctl.exports, tt.wantExports)
			assert.Equal(t, tt.wantPolicies, ctl.policies)
			assert.Len(t, ctl.messages, tt.wantMessages)
		})
	}
}

func TestStart_RefusedWhileDraining(t *testing.T) {
	ctl := &stubController{}
	hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, nil)

	var draining atomic.Bool
	draining.Store(true)

	srv := NewServer(Config{APIKey: testKey, Hub: hub, Controller: ctl, Draining: &draining, Logger: slog.New(slog.DiscardHandler)})

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, signed(t, http.MethodPost, "/chat/start", validStart(), nextTS()))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, ctl.starts)
}

func TestMessage_FailedDeliveryIsNotDeduplicated(t *testing.T) {
	ctl := &stubController{msgErr: errors.New("boom")}
	srv, _ := newTestServer(t, ctl)

	payload := protocol.MessagePayload{SessionID: "BP1", Content: "hello", MessageID: "m-same"}

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, signed(t, http.MethodPost, "/message", payload, nextTS()))
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	ctl.mu.Lock()
	ctl.msgErr = nil
	ctl.mu.Unlock()

	rec = httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, signed(t, http.MethodPost, "/message", payload, nextTS()))
	assert.Equal(t, http.StatusAccepted, rec.Code, "a rolled-back id is delivered on retry")
	assert.Len(t, ctl.messages, 2)
}

func TestCommand_FailedDeliveryIsNotDeduplicated(t *testing.T) {
	tests := []struct {
		text string
		fail func(*stubController)
		heal func(*stubController)
		got  func(*stubController) int
	}{
		{"abort", func(c *stubController) { c.abortErr = errors.New("boom") }, func(c *stubController) { c.abortErr = nil }, func(c *stubController) int { return len(c.aborts) }},
		{"export", func(c *stubController) { c.exportErr = errors.New("boom") }, func(c *stubController) { c.exportErr = nil }, func(c *stubController) int { return len(c.exports) }},
	}

	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			ctl := &stubController{}
			tt.fail(ctl)

			srv, _ := newTestServer(t, ctl)
			payload := protocol.MessagePayload{SessionID: "BP1", Content: tt.text, MessageID: "m-same"}

			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, signed(t, http.MethodPost, "/message", payload, nextTS()))
			require.Equal(t, http.StatusInternalServerError, rec.Code)

			ctl.mu.Lock()
			tt.heal(ctl)
			ctl.mu.Unlock()

			rec = httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, signed(t, http.MethodPost, "/message", payload, nextTS()))
			assert.Equal(t, http.StatusAccepted, rec.Code, "a rolled-back id is delivered on retry")
			assert.Equal(t, 2, tt.got(ctl))
		})
	}
}

// get serves a signed GET of path on srv and returns the recorder.
func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	srv.Routes().ServeHTTP(rec, signed(t, http.MethodGet, path, nil, nextTS()))

	return rec
}

func TestBlueprintRoutes_RefuseUnsignedRequests(t *testing.T) {
	srv, _ := newTestServer(t, &stubController{})

	for _, path := range []string{"/blueprints/BP1", "/blueprints/BP1/artifacts/intake/1", "/blueprints/BP1/live"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

func TestView_Route(t *testing.T) {
	want := protocol.BlueprintView{ID: "BP1", Mode: "default", State: "idle", Seq: 9, Pipeline: []protocol.PipelineStep{}, Artifacts: []protocol.ArtifactRef{}}

	tests := []struct {
		name string
		path string
		err  error
		code int
	}{
		{name: "a view", path: "/blueprints/BP1", code: http.StatusOK},
		{name: "an extra query parameter is ignored", path: "/blueprints/BP1?nonce=ABCDEF234567", code: http.StatusOK},
		{name: "an unknown blueprint", path: "/blueprints/BP1", err: blueprint.ErrNotFound, code: http.StatusNotFound},
		{name: "a failed read", path: "/blueprints/BP1", err: errors.New("disk"), code: http.StatusInternalServerError},
		{name: "an id that is not path-safe", path: "/blueprints/a.b", code: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestServer(t, &stubController{view: want, viewErr: tt.err})

			rec := get(t, srv, tt.path)
			require.Equal(t, tt.code, rec.Code, rec.Body.String())

			if tt.code == http.StatusOK {
				var got protocol.BlueprintView
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
				assert.Equal(t, want, got)
			}
		})
	}
}

func TestArtifact_Route(t *testing.T) {
	art := protocol.BlueprintArtifact{Stage: "solution_design", Version: 2, Body: "# Design"}

	tests := []struct {
		name string
		path string
		err  error
		code int
		call string
	}{
		{name: "a version", path: "/blueprints/BP1/artifacts/solution_design/2", code: http.StatusOK, call: "BP1|solution_design|2"},
		{name: "with a nonce", path: "/blueprints/BP1/artifacts/signoff/1?nonce=X", code: http.StatusOK, call: "BP1|signoff|1"},
		{name: "not found", path: "/blueprints/BP1/artifacts/emission/1", err: blueprint.ErrNotFound, code: http.StatusNotFound, call: "BP1|emission|1"},
		{name: "a dotted stage", path: "/blueprints/BP1/artifacts/design.review/1", code: http.StatusBadRequest},
		{name: "version zero", path: "/blueprints/BP1/artifacts/intake/0", code: http.StatusBadRequest},
		{name: "a signed version", path: "/blueprints/BP1/artifacts/intake/+2", code: http.StatusBadRequest},
		{name: "a zero-padded version", path: "/blueprints/BP1/artifacts/intake/02", code: http.StatusBadRequest},
		{name: "a version that is no number", path: "/blueprints/BP1/artifacts/intake/latest", code: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctl := &stubController{art: art, artErr: tt.err}
			srv, _ := newTestServer(t, ctl)

			rec := get(t, srv, tt.path)
			require.Equal(t, tt.code, rec.Code, rec.Body.String())

			if tt.call != "" {
				assert.Equal(t, []string{tt.call}, ctl.artCalls)
			} else {
				assert.Empty(t, ctl.artCalls, "a bad request reaches no controller")
			}

			if tt.code == http.StatusOK {
				var got protocol.BlueprintArtifact
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
				assert.Equal(t, art, got)
			}
		})
	}
}

// openLive starts a signed live stream from a real server and returns its
// reader once the connected comment arrived.
func openLive(t *testing.T, srv *Server, path string) *bufio.Reader {
	t.Helper()

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	return openLiveAt(t, ts.URL, ts.Client(), path)
}

// openLiveAt opens the signed live stream at base+path.
func openLiveAt(t *testing.T, base string, client *http.Client, path string) *bufio.Reader {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	require.NoError(t, err)

	tsUsed := nextTS()
	sig := protocol.SignPayloadWithTimestamp(testKey, http.MethodGet, path, nil, tsUsed)
	req.Header.Set(protocol.SignatureHeader, "sha256="+sig)
	req.Header.Set(protocol.TimestampHeader, tsUsed)

	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	reader := bufio.NewReader(resp.Body)

	first, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, ": connected\n", first)

	return reader
}

// lineWith reads lines until one starts with prefix.
func lineWith(t *testing.T, r *bufio.Reader, prefix string) string {
	t.Helper()

	for {
		line, err := r.ReadString('\n')
		require.NoError(t, err)

		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
}

func TestLive_StreamsFramesAndHeartbeats(t *testing.T) {
	ctl := &stubController{frames: make(chan protocol.LiveFrame)}
	srv, _ := newTestServer(t, ctl)
	srv.keepalive = 20 * time.Millisecond

	r := openLive(t, srv, "/blueprints/BP1/live?nonce=N1")

	ctl.frames <- protocol.LiveFrame{Type: "artifact_stored", Stage: "intake", Version: 1}

	data := lineWith(t, r, "data: ")
	assert.JSONEq(t, `{"type":"artifact_stored","stage":"intake","version":1}`, strings.TrimSpace(strings.TrimPrefix(data, "data: ")))

	beat := lineWith(t, r, ":")
	assert.NotEqual(t, ": connected\n", beat)
}

// drainReader reads r to its end and returns an error only if the stream
// stays open past the test's patience.
func drainReader(r *bufio.Reader) error {
	for {
		if _, err := r.ReadString('\n'); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}
	}
}

func TestLive_EndsAtDrain(t *testing.T) {
	srv, _ := newTestServer(t, &stubController{frames: make(chan protocol.LiveFrame)})

	ts := httptest.NewUnstartedServer(srv.Routes())
	ts.Config.RegisterOnShutdown(srv.CloseSSE)
	ts.Start()

	t.Cleanup(ts.Close)

	r := openLiveAt(t, ts.URL, ts.Client(), "/blueprints/BP1/live")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, ts.Config.Shutdown(ctx), "an open live stream does not hold the shutdown")

	assert.NoError(t, drainReader(r), "the stream ended")
}

func TestLive_EndsWhenTheFramesEnd(t *testing.T) {
	srv, _ := newTestServer(t, &stubController{})
	srv.ctl = closedFrames{&stubController{}}

	r := openLive(t, srv, "/blueprints/BP1/live")

	assert.NoError(t, drainReader(r), "a closed frames channel ends the stream")
}

// closedFrames subscribes to a channel that is already closed.
type closedFrames struct{ *stubController }

func (closedFrames) Subscribe(context.Context, string) (<-chan protocol.LiveFrame, error) {
	ch := make(chan protocol.LiveFrame)
	close(ch)

	return ch, nil
}

func TestLive_Refusals(t *testing.T) {
	tests := []struct {
		name string
		path string
		err  error
		code int
	}{
		{name: "a bad id", path: "/blueprints/a.b/live", code: http.StatusBadRequest},
		{name: "a failed subscribe", path: "/blueprints/BP1/live", err: errors.New("disk"), code: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := newTestServer(t, &stubController{subErr: tt.err})

			assert.Equal(t, tt.code, get(t, srv, tt.path).Code)
		})
	}
}

func TestLive_UnknownBlueprint(t *testing.T) {
	srv, _ := newTestServer(t, &stubController{subErr: blueprint.ErrNotFound})

	assert.Equal(t, http.StatusNotFound, get(t, srv, "/blueprints/BP1/live").Code)
}

package webhook

import (
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/mhersson/contextmatrix-backendkit/logbridge"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatStartClaudeCode(t *testing.T) {
	tests := []struct {
		name        string
		endpoint    protocol.LLMEndpoint
		unavailable error
		status      int
		code        string
		contains    []string
		started     bool
	}{
		{
			name:     "no token",
			endpoint: protocol.LLMEndpoint{Type: "claude_code"},
			status:   http.StatusBadRequest, code: protocol.CodeInvalidField,
			contains: []string{"oauth_token"},
		},
		{
			name:        "unavailable",
			endpoint:    protocol.LLMEndpoint{Type: "claude_code", OAuthToken: "sk-oauth-secret"},
			unavailable: errors.New("claude: not found"),
			status:      http.StatusInternalServerError, code: protocol.CodeInternal,
			contains: []string{"claude_code mode is unavailable", "claude: not found"},
		},
		{
			name:     "ok",
			endpoint: protocol.LLMEndpoint{Type: "claude_code", OAuthToken: "sk-oauth-secret"},
			status:   http.StatusAccepted, started: true,
		},
		{
			name:        "endpoint mode ignores the check",
			endpoint:    protocol.LLMEndpoint{Type: "openrouter", APIKey: "k"},
			unavailable: errors.New("x"),
			status:      http.StatusAccepted, started: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &stubController{}
			hub := logbridge.NewHub(func(e protocol.LogEntry) string { return e.SessionID }, nil)
			srv := NewServer(Config{
				APIKey: testKey, Hub: hub, Controller: ctl, Logger: slog.New(slog.DiscardHandler),
				ClaudeCodeUnavailable: tc.unavailable,
			})

			p := validStart()
			ep := tc.endpoint
			p.LLMEndpoint = &ep

			w := do(srv, signed(t, http.MethodPost, "/chat/start", p, nextTS()))
			assert.Equal(t, tc.status, w.Code, w.Body.String())

			if tc.code != "" {
				assert.Contains(t, w.Body.String(), tc.code)
			}

			for _, c := range tc.contains {
				assert.Contains(t, w.Body.String(), c)
			}

			assert.NotContains(t, w.Body.String(), "sk-oauth-secret")

			if !tc.started {
				assert.Empty(t, ctl.starts)

				return
			}

			require.Len(t, ctl.starts, 1)
			assert.Equal(t, tc.endpoint.OAuthToken, ctl.starts[0].Endpoint.OAuthToken)
		})
	}
}

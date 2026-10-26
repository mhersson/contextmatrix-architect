package cli

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mhersson/contextmatrix-harness/llm"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/config"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

// privateCAServer stands for an LLM gateway and ContextMatrix behind a
// private CA: httptest's own certificate, which the system trust store does
// not hold. It returns the server, the certificate as a PEM file and the
// count of requests that reached a handler.
func privateCAServer(t *testing.T) (*httptest.Server, string, *atomic.Int32) {
	t.Helper()

	var reached atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	})
	mux.HandleFunc("GET /api/worker/git-credentials", func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)

		_, _ = w.Write([]byte(`{"username":"x-access-token","token":"ghs_abc"}`))
	})

	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600))

	return srv, caFile, &reached
}

func TestLoadRuntime_CACertFileReachesAPrivateCAHost(t *testing.T) {
	srv, caFile, reached := privateCAServer(t)

	ep := protocol.LLMEndpoint{Type: "openai", BaseURL: srv.URL + "/v1", APIKey: "k"}
	req := llm.Request{Model: "m", Messages: []llm.Message{{Role: "user", Content: "ping"}}}

	deps, err := loadRuntime(&config.Config{CACertFile: caFile})
	require.NoError(t, err)

	resp, err := stage.NewRunner("", false, deps.transport).NewLLM(ep).Send(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, "hi", resp.Content)

	_, token, err := code.NewCMCredentials(srv.URL, "bearer", deps.transport).Fetch(context.Background(), "github.com", "acme/alpha")
	require.NoError(t, err)
	assert.Equal(t, "ghs_abc", token)

	caPEM, err := os.ReadFile(caFile)
	require.NoError(t, err)
	assert.Equal(t, caPEM, deps.caBundle, "go-git trusts the same file")

	// Without the file every client keeps the system trust store, which
	// refuses the host.
	deps, err = loadRuntime(&config.Config{})
	require.NoError(t, err)
	assert.Nil(t, deps.transport)
	assert.Nil(t, deps.caBundle)

	_, _, err = code.NewCMCredentials(srv.URL, "bearer", deps.transport).Fetch(context.Background(), "github.com", "acme/alpha")
	require.ErrorAs(t, err, new(x509.UnknownAuthorityError))

	// The deadline ends the LLM client's retries.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err = stage.NewRunner("", false, deps.transport).NewLLM(ep).Send(ctx, req)
	require.Error(t, err)

	assert.Equal(t, int32(2), reached.Load(), "only the clients built with the file reached the host")
}

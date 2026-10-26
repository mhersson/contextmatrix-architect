package code

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBearer = "s1.cmVhbC1sb29raW5nLWJlYXJlcg.mac0123456789"

// cmError writes CM's APIError body.
func cmError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}

func TestCMCredentials_Fetch(t *testing.T) {
	var gotAuth, gotHost, gotPath, gotURLPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.URL.Query().Get("host")
		gotPath = r.URL.Query().Get("path")
		gotURLPath = r.URL.Path

		switch gotPath {
		case "acme/cold":
			cmError(w, http.StatusConflict, "WORKER_NOT_RUNNING", "chat session is not running")
		case "acme/gone":
			cmError(w, http.StatusNotFound, "CHAT_NOT_FOUND", "chat session not found")
		case "acme/binding":
			cmError(w, http.StatusConflict, "VALIDATION_ERROR", "project credential unavailable")
		case "acme/provider":
			cmError(w, http.StatusConflict, "VALIDATION_ERROR", "git credential unavailable")
		case "acme/mint":
			cmError(w, http.StatusBadGateway, "INTERNAL_ERROR", "token mint failed")
		case "acme/echo":
			cmError(w, http.StatusUnauthorized, "UNAUTHORIZED", "bad bearer "+gotAuth)
		case "acme/opaque":
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"username":"x-access-token","token":"ghs_abc"}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := NewCMCredentials(srv.URL+"/", testBearer, nil)

	user, token, err := c.Fetch(context.Background(), "github.com", "acme/alpha")
	require.NoError(t, err)
	assert.Equal(t, "x-access-token", user)
	assert.Equal(t, "ghs_abc", token)
	assert.Equal(t, "Bearer "+testBearer, gotAuth)
	assert.Equal(t, "github.com", gotHost)
	assert.Equal(t, "acme/alpha", gotPath)
	assert.Equal(t, "/api/worker/git-credentials", gotURLPath, "a trailing slash on the base url is trimmed")

	tests := []struct {
		path    string
		notLive bool
		carries string
	}{
		{path: "acme/cold", notLive: true},
		{path: "acme/gone", notLive: true},
		{path: "acme/binding", carries: "project credential unavailable"},
		{path: "acme/provider", carries: "git credential unavailable"},
		{path: "acme/mint", carries: "token mint failed"},
		{path: "acme/echo", carries: "bad bearer"},
		{path: "acme/opaque", carries: "502"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			_, _, err := c.Fetch(context.Background(), "github.com", tt.path)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), testBearer, "the bearer never reaches an error")

			if tt.notLive {
				require.ErrorIs(t, err, ErrSessionNotLive)

				return
			}

			require.NotErrorIs(t, err, ErrSessionNotLive)
			assert.Contains(t, err.Error(), tt.carries)
		})
	}
}

func TestCMCredentials_FetchesOverTheBaseTransport(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"username":"x-access-token","token":"ghs_abc"}`))
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)

	// The test server's own client trusts its certificate.
	_, token, err := NewCMCredentials(srv.URL, testBearer, srv.Client().Transport).Fetch(context.Background(), "github.com", "acme/alpha")
	require.NoError(t, err)
	assert.Equal(t, "ghs_abc", token)

	_, _, err = NewCMCredentials(srv.URL, testBearer, nil).Fetch(context.Background(), "github.com", "acme/alpha")
	require.ErrorAs(t, err, new(x509.UnknownAuthorityError), "the system trust store refuses the certificate")
	assert.NotContains(t, err.Error(), testBearer)
}

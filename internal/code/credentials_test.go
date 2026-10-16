package code

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCMCredentials_Fetch(t *testing.T) {
	var gotAuth, gotHost, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotHost = r.URL.Query().Get("host")
		gotPath = r.URL.Query().Get("path")

		switch gotPath {
		case "acme/cold":
			w.WriteHeader(http.StatusConflict)
		case "acme/broken":
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"username":"x-access-token","token":"ghs_abc"}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := NewCMCredentials(srv.URL, "sess.mac")

	user, token, err := c.Fetch(context.Background(), "github.com", "acme/alpha")
	require.NoError(t, err)
	assert.Equal(t, "x-access-token", user)
	assert.Equal(t, "ghs_abc", token)
	assert.Equal(t, "Bearer sess.mac", gotAuth)
	assert.Equal(t, "github.com", gotHost)
	assert.Equal(t, "acme/alpha", gotPath)

	_, _, err = c.Fetch(context.Background(), "github.com", "acme/cold")
	require.ErrorIs(t, err, ErrSessionNotLive)

	_, _, err = c.Fetch(context.Background(), "github.com", "acme/broken")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "sess.mac", "the bearer never reaches an error")
}

package research

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBraveKey = "brave-key-0123456789abcdef"

const braveResponse = `{"type":"search","web":{"type":"search","results":[
{"title":"Backup and Recovery - <strong>CloudNativePG</strong>","url":"https://cloudnative-pg.io/documentation/current/backup/","description":"How to back up <strong>PostgreSQL</strong> clusters &amp; restore them."},
{"title":"No address","url":"","description":"dropped"}]}}`

func braveAt(srv *httptest.Server) *Brave {
	b := NewBrave(testBraveKey)
	b.endpoint = srv.URL + "/res/v1/web/search"

	return b
}

func TestBrave_Search(t *testing.T) {
	var (
		mu  sync.Mutex
		got *http.Request
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Clone(context.Background())
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, braveResponse)
	}))
	t.Cleanup(srv.Close)

	results, err := braveAt(srv).Search(context.Background(), "cloudnative-pg backups")
	require.NoError(t, err)

	assert.Equal(t, []Result{{
		Title:   "Backup and Recovery - CloudNativePG",
		URL:     "https://cloudnative-pg.io/documentation/current/backup/",
		Snippet: "How to back up PostgreSQL clusters & restore them.",
	}}, results)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, http.MethodGet, got.Method)
	assert.Equal(t, "/res/v1/web/search", got.URL.Path)
	assert.Equal(t, "cloudnative-pg backups", got.URL.Query().Get("q"))
	assert.Equal(t, "10", got.URL.Query().Get("count"))
	assert.Equal(t, testBraveKey, got.Header.Get("X-Subscription-Token"))
	assert.Equal(t, "application/json", got.Header.Get("Accept"))
	assert.NotContains(t, got.URL.String(), testBraveKey, "the key travels in a header, never in the URL")
}

func TestBrave_Failures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"key refused", http.StatusUnauthorized, `{}`, "refused"},
		{"plan refused", http.StatusForbidden, `{}`, "refused"},
		{"rate limited", http.StatusTooManyRequests, `{}`, "rate limit"},
		{"server error", http.StatusBadGateway, `bad gateway`, "HTTP 502"},
		{"not json", http.StatusOK, `<html>`, "decode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			t.Cleanup(srv.Close)

			_, err := braveAt(srv).Search(context.Background(), "kustomize overlays")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.NotContains(t, err.Error(), testBraveKey)
		})
	}
}

func TestBrave_StopsWithTheContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := braveAt(srv).Search(ctx, "kustomize overlays")
	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), testBraveKey)
}

func TestBrave_DoesNotFollowRedirects(t *testing.T) {
	var (
		mu   sync.Mutex
		hits int
	)

	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	t.Cleanup(elsewhere.Close)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, err := braveAt(srv).Search(context.Background(), "kustomize overlays")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 302")
	assert.NotContains(t, err.Error(), testBraveKey)

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 0, hits, "the key must never reach a host the redirect names")
}

func TestNewSearcher(t *testing.T) {
	s, err := NewSearcher(ProviderBrave, testBraveKey)
	require.NoError(t, err)
	assert.IsType(t, &Brave{}, s)

	_, err = NewSearcher("nosuch", testBraveKey)
	require.ErrorContains(t, err, `"nosuch"`)

	_, err = NewSearcher(ProviderBrave, " ")
	assert.ErrorContains(t, err, "key")
}

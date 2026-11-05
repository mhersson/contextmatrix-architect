package design

import (
	"context"
	"crypto/sha1" //nolint:gosec // npm integrity strings still use sha1
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tarballServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Gatherer) {
	t.Helper()

	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	return srv, NewGatherer(t.TempDir(), srv.Client().Transport)
}

func tgzFiles(t *testing.T, g *Gatherer) []string {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(g.cacheDir, "*.tgz"))
	require.NoError(t, err)

	return files
}

func requireGap(t *testing.T, err error, word string) {
	t.Helper()

	var gap gapError

	require.ErrorAs(t, err, &gap)
	assert.Contains(t, gap.Error(), word)
}

func TestTarball_FetchesAndVerifies(t *testing.T) {
	body := tgz(t, map[string]string{"package/package.json": `{"name":"@acme/ui"}`})

	var requests atomic.Int32

	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)

		_, _ = w.Write(body)
	})

	for range 2 {
		path, verified, err := g.tarball(t.Context(), srv.URL+"/@acme/ui/-/ui-1.2.3.tgz", integrity(body))
		require.NoError(t, err)
		assert.True(t, verified)

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, body, got)
	}

	assert.Equal(t, int32(1), requests.Load())
}

func TestTarball_NoIntegrityIsUnverified(t *testing.T) {
	body := tgz(t, map[string]string{"package/package.json": `{}`})

	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})

	path, verified, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "")
	require.NoError(t, err)
	assert.False(t, verified)
	assert.FileExists(t, path)
}

func TestTarball_UnknownIntegrityAlgorithmIsUnverified(t *testing.T) {
	body := tgz(t, map[string]string{"package/package.json": `{}`})

	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})

	_, verified, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "md5-AAAAAAAAAAAAAAAAAAAAAA==")
	require.NoError(t, err)
	assert.False(t, verified)
}

func TestTarball_IntegrityAlgorithms(t *testing.T) {
	body := tgz(t, map[string]string{"package/package.json": `{}`})
	other := tgz(t, map[string]string{"package/package.json": `{"other":true}`})

	sri := func(algo string, sum []byte) string {
		return algo + "-" + base64.StdEncoding.EncodeToString(sum)
	}

	sha1Sum := sha1.Sum(body) //nolint:gosec // npm integrity strings still use sha1
	sha256Sum := sha256.Sum256(body)
	sha384Sum := sha512.Sum384(body)
	sha512Sum := sha512.Sum512(body)
	wrong512 := sha512.Sum512(other)
	wrong1 := sha1.Sum(other) //nolint:gosec // npm integrity strings still use sha1

	tests := []struct {
		name      string
		integrity string
		verified  bool
		wantGap   bool
	}{
		{"sha1", sri("sha1", sha1Sum[:]), true, false},
		{"sha256", sri("sha256", sha256Sum[:]), true, false},
		{"sha384", sri("sha384", sha384Sum[:]), true, false},
		{"sha512", sri("sha512", sha512Sum[:]), true, false},
		{"unpadded", strings.TrimRight(sri("sha512", sha512Sum[:]), "="), true, false},
		{"option suffix", sri("sha512", sha512Sum[:]) + "?ext=1", true, false},
		{"strongest wins over a matching weaker one", sri("sha1", sha1Sum[:]) + " " + sri("sha512", wrong512[:]), false, true},
		{"strongest matching passes beside a wrong weaker one", sri("sha1", wrong1[:]) + " " + sri("sha512", sha512Sum[:]), true, false},
		{"any of several strongest passes", sri("sha512", wrong512[:]) + " " + sri("sha512", sha512Sum[:]), true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(body)
			})

			_, verified, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", tt.integrity)
			if tt.wantGap {
				requireGap(t, err, "match")
				assert.Empty(t, tgzFiles(t, g))

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.verified, verified)
		})
	}
}

func TestTarball_IntegrityMismatch(t *testing.T) {
	body := tgz(t, map[string]string{"package/package.json": `{}`})
	other := tgz(t, map[string]string{"package/package.json": `{"other":true}`})

	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})

	_, verified, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", integrity(other))
	requireGap(t, err, "match")
	assert.False(t, verified)
	assert.Empty(t, tgzFiles(t, g))
}

func TestTarball_CachedFileIsCheckedAgainstIntegrity(t *testing.T) {
	body := tgz(t, map[string]string{"package/package.json": `{}`})
	other := tgz(t, map[string]string{"package/package.json": `{"other":true}`})

	var requests atomic.Int32

	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)

		_, _ = w.Write(body)
	})

	pkgURL := srv.URL + "/pkg.tgz"

	_, _, err := g.tarball(t.Context(), pkgURL, "")
	require.NoError(t, err)

	_, verified, err := g.tarball(t.Context(), pkgURL, integrity(other))
	requireGap(t, err, "match")
	assert.False(t, verified)
	assert.Empty(t, tgzFiles(t, g))

	path, verified, err := g.tarball(t.Context(), pkgURL, integrity(body))
	require.NoError(t, err)
	assert.True(t, verified)
	assert.FileExists(t, path)
	assert.Equal(t, int32(2), requests.Load())
}

func TestTarball_LoginNeeded(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var sawAuth atomic.Bool

			srv, g := tarballServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					sawAuth.Store(true)
				}

				w.WriteHeader(status)
			})

			_, _, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "")
			requireGap(t, err, "login")
			assert.False(t, sawAuth.Load())
			assert.Empty(t, tgzFiles(t, g))
		})
	}
}

func TestTarball_OtherStatusNamesTheCode(t *testing.T) {
	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	_, _, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "")
	requireGap(t, err, "404")
	assert.Empty(t, tgzFiles(t, g))
}

func TestTarball_HTMLIsNotAnArchive(t *testing.T) {
	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>sign in</body></html>"))
	})

	_, _, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "")
	requireGap(t, err, "package archive")
	assert.Empty(t, tgzFiles(t, g))
}

func TestTarball_EmptyBodyIsNotAnArchive(t *testing.T) {
	srv, g := tarballServer(t, func(http.ResponseWriter, *http.Request) {})

	_, _, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "")
	requireGap(t, err, "package archive")
}

func TestTarball_RefusesOversizedBodyWithoutTrustingContentLength(t *testing.T) {
	prefix := []byte{0x1f, 0x8b}
	chunk := make([]byte, 1<<20)

	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		// No Content-Length: chunked, so only the read can enforce the cap.
		_, _ = w.Write(prefix)

		for range maxTarballBytes/len(chunk) + 1 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})

	_, _, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "")
	requireGap(t, err, "larger")
	assert.Empty(t, tgzFiles(t, g))

	left, err := filepath.Glob(filepath.Join(g.cacheDir, "*"))
	require.NoError(t, err)
	assert.Empty(t, left, "no temp file is left behind")
}

func TestTarball_BodyAtTheCapIsKept(t *testing.T) {
	body := append([]byte{0x1f, 0x8b}, make([]byte, maxTarballBytes-2)...)

	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	})

	path, _, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "")
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, int64(maxTarballBytes), info.Size())
}

func TestTarball_RefusesHTTP(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	t.Cleanup(srv.Close)

	g := NewGatherer(t.TempDir(), nil)

	_, _, err := g.tarball(t.Context(), srv.URL+"/pkg.tgz", "")
	requireGap(t, err, "https")
	assert.Zero(t, requests.Load())
}

func TestTarball_RefusesUnusableURLs(t *testing.T) {
	var requests atomic.Int32

	srv, g := tarballServer(t, func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	})

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	tests := []struct {
		name string
		url  string
		word string
	}{
		{"user and password", "https://user:pw@" + u.Host + "/pkg.tgz", "anonymous"},
		{"user only", "https://token@" + u.Host + "/pkg.tgz", "anonymous"},
		{"empty userinfo", "https://:@" + u.Host + "/pkg.tgz", "anonymous"},
		{"no host", "https:///pkg.tgz", "address"},
		{"not a url", "https://exa mple.test/pkg.tgz", "address"},
		{"relative", "/pkg.tgz", "https"},
		{"other scheme", "ftp://" + u.Host + "/pkg.tgz", "https"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := g.tarball(t.Context(), tt.url, "")
			requireGap(t, err, tt.word)
			assert.Empty(t, tgzFiles(t, g))
		})
	}

	assert.Zero(t, requests.Load())
}

func TestTarball_UserinfoErrorDoesNotEchoTheCredentials(t *testing.T) {
	g := NewGatherer(t.TempDir(), nil)

	_, _, err := g.tarball(t.Context(), "https://user:s3cr3t-pw@example.test/pkg.tgz", "")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cr3t-pw")
}

func TestTarball_Redirects(t *testing.T) {
	body := tgz(t, map[string]string{"package/package.json": `{}`})

	var (
		sawAuth  atomic.Bool
		finalHit atomic.Int32
		srvURL   string
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}

		switch {
		case r.URL.Path == "/final":
			finalHit.Add(1)

			_, _ = w.Write(body)
		case r.URL.Path == "/to-http":
			http.Redirect(w, r, strings.Replace(srvURL, "https://", "http://", 1)+"/final", http.StatusFound)
		case r.URL.Path == "/to-userinfo":
			http.Redirect(w, r, strings.Replace(srvURL, "https://", "https://user:pw@", 1)+"/final", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/hop/"):
			n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)

				return
			}

			if n == 0 {
				http.Redirect(w, r, "/final", http.StatusFound)

				return
			}

			http.Redirect(w, r, fmt.Sprintf("/hop/%d", n-1), http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	srvURL = srv.URL

	tests := []struct {
		name    string
		path    string
		word    string
		wantHit int32
	}{
		{"one redirect is followed", "/hop/0", "", 1},
		{"five redirects are followed", "/hop/4", "", 1},
		{"six redirects are refused", "/hop/5", "redirect", 0},
		{"redirect to http is refused", "/to-http", "https", 0},
		{"redirect to a userinfo url is refused", "/to-userinfo", "anonymous", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finalHit.Store(0)

			g := NewGatherer(t.TempDir(), srv.Client().Transport)

			_, _, err := g.tarball(t.Context(), srv.URL+tt.path, "")
			if tt.word == "" {
				require.NoError(t, err)
			} else {
				requireGap(t, err, tt.word)
				assert.Empty(t, tgzFiles(t, g))
			}

			assert.Equal(t, tt.wantHit, finalHit.Load())
		})
	}

	assert.False(t, sawAuth.Load(), "no request carried an Authorization header")
}

// countingTransport counts the requests a client sends.
type countingTransport struct {
	rt http.RoundTripper
	n  atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)

	return c.rt.RoundTrip(r)
}

func hangingGatherer(t *testing.T) (*httptest.Server, *Gatherer, *countingTransport) {
	t.Helper()

	srv, g := tarballServer(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	ct := &countingTransport{rt: g.client.Transport}
	g.client.Transport = ct

	return srv, g, ct
}

func TestTarball_AnUnreachableHostIsRememberedForAWhile(t *testing.T) {
	srv, g, ct := hangingGatherer(t)
	g.client.Timeout = 200 * time.Millisecond

	now := time.Now()
	g.now = func() time.Time { return now }

	for _, name := range []string{"a", "b", "c"} {
		_, _, err := g.tarball(t.Context(), srv.URL+"/"+name+".tgz", "")
		require.Error(t, err)
	}

	assert.EqualValues(t, 1, ct.n.Load())

	now = now.Add(g.downFor - time.Second)

	_, _, err := g.tarball(t.Context(), srv.URL+"/d.tgz", "")
	require.Error(t, err)
	assert.EqualValues(t, 1, ct.n.Load())

	now = now.Add(2 * time.Second)

	_, _, err = g.tarball(t.Context(), srv.URL+"/e.tgz", "")
	require.Error(t, err)
	assert.EqualValues(t, 2, ct.n.Load())
}

func TestTarball_AStatusErrorIsNotRemembered(t *testing.T) {
	var requests atomic.Int32

	srv, g := tarballServer(t, func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		_, _ = w.Write(tgz(t, map[string]string{"package/a.css": "a{}"}))
	})

	_, _, err := g.tarball(t.Context(), srv.URL+"/one.tgz", "")
	requireGap(t, err, "login")

	_, _, err = g.tarball(t.Context(), srv.URL+"/two.tgz", "")
	require.NoError(t, err)
	assert.EqualValues(t, 2, requests.Load())
}

func TestTarball_ADoneContextDoesNotMarkTheHost(t *testing.T) {
	srv, g, ct := hangingGatherer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, _, err := g.tarball(ctx, srv.URL+"/a.tgz", "")
	require.Error(t, err)

	g.client.Timeout = 100 * time.Millisecond

	_, _, err = g.tarball(t.Context(), srv.URL+"/b.tgz", "")
	require.Error(t, err)
	assert.EqualValues(t, 2, ct.n.Load())
}

func TestTarball_ACachedArchiveIsServedWhileTheHostIsDown(t *testing.T) {
	srv, g, ct := hangingGatherer(t)
	g.client.Timeout = 100 * time.Millisecond

	_, _, err := g.tarball(t.Context(), srv.URL+"/down.tgz", "")
	require.Error(t, err)
	require.EqualValues(t, 1, ct.n.Load())

	cachedURL := srv.URL + "/cached.tgz"
	sum := sha256.Sum256([]byte(cachedURL))
	cached := filepath.Join(g.cacheDir, hex.EncodeToString(sum[:])+".tgz")
	require.NoError(t, os.WriteFile(cached, tgz(t, map[string]string{"package/a.css": "a{}"}), 0o600))

	path, _, err := g.tarball(t.Context(), cachedURL, "")
	require.NoError(t, err)
	assert.Equal(t, cached, path)
	assert.EqualValues(t, 1, ct.n.Load())
}

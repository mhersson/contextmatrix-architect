package research

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func allowAll(netip.AddrPort) error { return nil }

func testFetcher(allow func(netip.AddrPort) error) *Fetcher {
	l := defaultLimits()
	l.total, l.header = 2*time.Second, time.Second

	return newFetcher(allow, l)
}

func serveText(t *testing.T, contentType, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	return srv
}

func portOf(t *testing.T, raw string) uint16 {
	t.Helper()

	u, err := url.Parse(raw)
	require.NoError(t, err)

	p, err := strconv.Atoi(u.Port())
	require.NoError(t, err)

	return uint16(p)
}

func TestFetch_ReadsAnHTMLPage(t *testing.T) {
	srv := serveText(t, "text/html; charset=utf-8", docHTML)

	page, err := testFetcher(allowAll).Fetch(context.Background(), srv.URL+"/doc")
	require.NoError(t, err)

	assert.Equal(t, srv.URL+"/doc", page.URL)
	assert.Equal(t, "Backups | CloudNativePG", page.Title)
	assert.Contains(t, page.Text, "Use barman & object storage.")
	assert.NotContains(t, page.Text, "Home")
	assert.False(t, page.Truncated)
}

func TestFetch_ReadsATextPageAsItIs(t *testing.T) {
	srv := serveText(t, "text/plain", "line one\nline two\n")

	page, err := testFetcher(allowAll).Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "line one\nline two", page.Text)
}

func TestFetch_RefusesWhatIsNotAPage(t *testing.T) {
	pdf := serveText(t, "application/pdf", "%PDF-1.7")

	_, err := testFetcher(allowAll).Fetch(context.Background(), pdf.URL)

	var typeErr *TypeError
	require.ErrorAs(t, err, &typeErr)
	assert.Equal(t, "application/pdf", typeErr.Type)

	missing := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(missing.Close)

	_, err = testFetcher(allowAll).Fetch(context.Background(), missing.URL)
	assert.ErrorContains(t, err, "HTTP 404")
}

func TestFetch_CutsALargePage(t *testing.T) {
	srv := serveText(t, "text/plain; charset=utf-8", strings.Repeat("é", 4000))

	l := defaultLimits()
	l.body, l.text = 4096, 1024

	page, err := newFetcher(allowAll, l).Fetch(context.Background(), srv.URL)
	require.NoError(t, err)

	assert.True(t, page.Truncated)
	assert.LessOrEqual(t, len(page.Text), 1024)
	assert.True(t, utf8.ValidString(page.Text), "a cut never splits a character")
}

func TestFetch_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	l := defaultLimits()
	l.header = 200 * time.Millisecond

	start := time.Now()
	_, err := newFetcher(allowAll, l).Fetch(context.Background(), srv.URL)

	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestFetch_DefaultPolicyRefusesInternalAddresses(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)

	// An address, and a name that resolves to one: the check runs after
	// resolution.
	for _, raw := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		_, err := NewFetcher().Fetch(context.Background(), raw)

		var addrErr *AddressError
		require.ErrorAs(t, err, &addrErr, raw)
		assert.Equal(t, "loopback", addrErr.Kind)
	}

	assert.Zero(t, hits.Load(), "no request reached the server")
}

func TestFetch_ChecksEveryRedirect(t *testing.T) {
	var internalHits atomic.Int32

	internal := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { internalHits.Add(1) }))
	t.Cleanup(internal.Close)

	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/admin", http.StatusFound)
	}))
	t.Cleanup(public.Close)

	// Only the first server counts as public.
	publicPort := portOf(t, public.URL)
	onlyPublic := func(ap netip.AddrPort) error {
		if ap.Port() == publicPort {
			return nil
		}

		return &AddressError{Addr: ap.Addr(), Kind: "private"}
	}

	_, err := testFetcher(onlyPublic).Fetch(context.Background(), public.URL)

	var addrErr *AddressError
	require.ErrorAs(t, err, &addrErr)
	assert.Zero(t, internalHits.Load(), "the redirect never reached the internal server")
}

func TestFetch_RefusesARedirectToAnotherScheme(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, err := testFetcher(allowAll).Fetch(context.Background(), srv.URL)
	assert.Equal(t, RuleScheme, refusedRule(err))
}

func TestFetch_StopsAfterFiveRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, err := testFetcher(allowAll).Fetch(context.Background(), srv.URL)
	assert.ErrorContains(t, err, "stopped after 5 redirects")
}

func TestFetch_CapsAnOversizedTitle(t *testing.T) {
	srv := serveText(t, "text/html", "<html><head><title>"+strings.Repeat("é", 100_000)+"</title></head><body><p>Body</p></body></html>")

	page, err := testFetcher(allowAll).Fetch(context.Background(), srv.URL)
	require.NoError(t, err)

	assert.LessOrEqual(t, len(page.Title), 512)
	assert.True(t, utf8.ValidString(page.Title), "a cut never splits a character")
	assert.False(t, page.Truncated, "a cut title does not truncate the page")
	assert.Equal(t, "Body", page.Text)
}

func TestFetch_FollowsExactlyFiveRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/hop/"))
		if hops == 0 {
			_, _ = io.WriteString(w, "arrived")

			return
		}

		http.Redirect(w, r, "/hop/"+strconv.Itoa(hops-1), http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	page, err := testFetcher(allowAll).Fetch(context.Background(), srv.URL+"/hop/5")
	require.NoError(t, err)
	assert.Equal(t, "arrived", page.Text)

	_, err = testFetcher(allowAll).Fetch(context.Background(), srv.URL+"/hop/6")
	assert.ErrorContains(t, err, "stopped after 5 redirects")
}

func TestFetch_RefusesARedirectWithCredentials(t *testing.T) {
	var hits atomic.Int32

	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	t.Cleanup(target.Close)

	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(target.URL, "http://", "http://user:pass@", 1)+"/x", http.StatusFound)
	}))
	t.Cleanup(public.Close)

	_, err := testFetcher(allowAll).Fetch(context.Background(), public.URL)

	assert.Equal(t, RuleCredentials, refusedRule(err))
	assert.Zero(t, hits.Load(), "the target was never requested")
}

func TestFetch_RefusesADeclaredTypeWithoutReadingTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		// The body never ends: a fetch that reads it would run to its timeout.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	_, err := testFetcher(allowAll).Fetch(context.Background(), srv.URL)

	var typeErr *TypeError
	require.ErrorAs(t, err, &typeErr)
	assert.Equal(t, "application/pdf", typeErr.Type)
	assert.Less(t, time.Since(start), time.Second, "the body is never read")
}

func TestFetch_StopsATrickledBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()

		for range 50 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}

			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)

	l := defaultLimits()
	l.total = 300 * time.Millisecond

	start := time.Now()
	_, err := newFetcher(allowAll, l).Fetch(context.Background(), srv.URL)

	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}

func TestPublicAddr(t *testing.T) {
	tests := []struct{ addr, kind string }{
		{"127.0.0.1", "loopback"},
		{"::1", "loopback"},
		{"::ffff:127.0.0.1", "loopback"},
		{"10.1.2.3", "private"},
		{"172.16.0.1", "private"},
		{"192.168.1.1", "private"},
		{"100.64.0.1", "private"},
		{"fd00::1", "private"},
		{"169.254.169.254", "link-local"},
		{"fe80::1", "link-local"},
		{"0.0.0.0", "unspecified"},
		{"::", "unspecified"},
		{"0.1.2.3", "unspecified"},
		{"239.1.2.3", "multicast"},
		{"255.255.255.255", "reserved"},
		{"64:ff9b::a00:1", "private"},
		{"64:ff9b::808:808", ""},
		{"8.8.8.8", ""},
		{"2606:4700:4700::1111", ""},
		{"192.0.0.1", "reserved"},
		{"192.0.2.1", "reserved"},
		{"198.18.0.1", "reserved"},
		{"198.51.100.1", "reserved"},
		{"203.0.113.1", "reserved"},
		{"2001:db8::1", "reserved"},
		{"2002::1", "reserved"},
		{"100::1", "reserved"},
		{"64:ff9b:1::1", "private"},
		{"::7f00:1", "reserved"},
		{"::ffff:0:7f00:1", "reserved"},
		{"fec0::1", "reserved"},
		{"2001::1", "reserved"},
	}

	for _, tt := range tests {
		err := publicAddr(netip.MustParseAddr(tt.addr))

		if tt.kind == "" {
			require.NoError(t, err, tt.addr)

			continue
		}

		var addrErr *AddressError
		require.ErrorAs(t, err, &addrErr, tt.addr)
		assert.Equal(t, tt.kind, addrErr.Kind, tt.addr)
	}
}

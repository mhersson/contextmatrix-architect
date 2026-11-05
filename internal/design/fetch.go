package design

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // npm integrity strings still use sha1
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxTarballBytes = 100 << 20
	maxRedirects    = 5
	fetchTimeout    = 2 * time.Minute
	hostDownFor     = 5 * time.Minute
	gatherFetchFor  = 3 * time.Minute
)

// gapError is a failure to get a package that the mockup designer is told
// about; reason is a plain sentence a person reads.
type gapError struct{ reason string }

func (e gapError) Error() string { return e.reason }

// Gatherer fetches the packages a project's CSS imports. Downloads are
// anonymous and cached in cacheDir.
type Gatherer struct {
	cacheDir string
	client   *http.Client

	now      func() time.Time
	downFor  time.Duration // how long a host that failed on the network is skipped
	fetchFor time.Duration // the budget for one Gather's downloads

	mu   sync.Mutex
	down map[string]time.Time // host to the time it may be tried again
}

// transportError is a download that failed on the network rather than on an
// answer from the registry.
type transportError struct{ err error }

func (e transportError) Error() string { return e.err.Error() }
func (e transportError) Unwrap() error { return e.err }

// NewGatherer returns a Gatherer caching in cacheDir. A nil rt means
// http.DefaultTransport.
func NewGatherer(cacheDir string, rt http.RoundTripper) *Gatherer {
	if rt == nil {
		rt = http.DefaultTransport
	}

	return &Gatherer{
		cacheDir: cacheDir,
		now:      time.Now,
		downFor:  hostDownFor,
		fetchFor: gatherFetchFor,
		down:     map[string]time.Time{},
		client: &http.Client{
			Transport:     rt,
			Timeout:       fetchTimeout,
			CheckRedirect: checkRedirect,
		},
	}
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return gapError{"the registry redirected too many times"}
	}

	return checkURL(req.URL)
}

// checkURL refuses anything but an https address without userinfo: net/http
// would turn userinfo into an Authorization header.
func checkURL(u *url.URL) error {
	switch {
	case u.Scheme != "https":
		return gapError{"the package address is not https"}
	case u.Host == "":
		return gapError{"the package address is not valid"}
	case u.User != nil:
		return gapError{"the package address carries a username, and packages are downloaded anonymously"}
	}

	return nil
}

// tarball returns the path of the cached package archive at rawURL, fetching
// it first when needed. verified is true only when integrity named an
// algorithm this package checks and the archive matched it. A cached archive
// is checked against integrity again.
func (g *Gatherer) tarball(ctx context.Context, rawURL, integrity string) (path string, verified bool, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false, gapError{"the package address is not valid"}
	}

	if err := checkURL(u); err != nil {
		return "", false, err
	}

	want := parseIntegrity(integrity)

	sum := sha256.Sum256([]byte(rawURL))
	path = filepath.Join(g.cacheDir, hex.EncodeToString(sum[:])+".tgz")

	hit, verified, err := checkCached(path, want)
	if err != nil {
		return "", false, err
	}

	if hit {
		return path, verified, nil
	}

	if g.hostDown(u.Host) {
		return "", false, gapError{"the registry could not be reached"}
	}

	verified, err = g.download(ctx, rawURL, path, want)
	if err != nil {
		var te transportError
		if errors.As(err, &te) && ctx.Err() == nil {
			g.markDown(u.Host)
		}

		return "", false, err
	}

	return path, verified, nil
}

func (g *Gatherer) hostDown(host string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.now().Before(g.down[host])
}

func (g *Gatherer) markDown(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.down[host] = g.now().Add(g.downFor)
}

// checkCached reports whether path exists and, when want is set, whether it
// matches. A cached archive that fails the check is deleted.
func checkCached(path string, want *integrityCheck) (hit, verified bool, err error) {
	f, err := os.Open(path) //nolint:gosec // path is under the cache dir, named by a hash
	if errors.Is(err, fs.ErrNotExist) {
		return false, false, nil
	}

	if err != nil {
		return false, false, fmt.Errorf("design: open cached package: %w", err)
	}

	defer func() { _ = f.Close() }()

	if want == nil {
		return true, false, nil
	}

	h := want.newHash()
	if _, err := io.Copy(h, f); err != nil {
		return false, false, fmt.Errorf("design: read cached package: %w", err)
	}

	if !want.matches(h.Sum(nil)) {
		if err := os.Remove(path); err != nil {
			return false, false, fmt.Errorf("design: remove cached package: %w", err)
		}

		return false, false, errIntegrity
	}

	return true, true, nil
}

var errIntegrity = gapError{"the downloaded package does not match the checksum in the lockfile"}

// download streams the archive at rawURL into a temp file and renames it to
// path once it has passed every check, so path never holds a bad archive and
// concurrent downloads of one url are safe.
func (g *Gatherer) download(ctx context.Context, rawURL, path string, want *integrityCheck) (verified bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, fmt.Errorf("design: build package request: %w", err)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		var gap gapError
		if errors.As(err, &gap) {
			return false, gap
		}

		// url.Error repeats the address, which can carry a token in its query.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}

		return false, transportError{fmt.Errorf("design: fetch package: %w", err)}
	}

	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return false, gapError{"the registry needs a login"}
	case resp.StatusCode != http.StatusOK:
		return false, gapError{fmt.Sprintf("the registry answered %d", resp.StatusCode)}
	}

	// One byte over the cap tells a body that is exactly the cap from a larger one.
	body := bufio.NewReader(io.LimitReader(resp.Body, maxTarballBytes+1))

	magic, err := body.Peek(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, bodyError("design: read package", err)
	}

	if len(magic) < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return false, gapError{"the registry did not return a package archive"}
	}

	if err := os.MkdirAll(g.cacheDir, 0o750); err != nil {
		return false, fmt.Errorf("design: create cache dir: %w", err)
	}

	tmp, err := os.CreateTemp(g.cacheDir, "download-*.tmp")
	if err != nil {
		return false, fmt.Errorf("design: create temp file: %w", err)
	}

	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	var (
		dst io.Writer = tmp
		h   hash.Hash
	)

	if want != nil {
		h = want.newHash()
		dst = io.MultiWriter(tmp, h)
	}

	n, err := io.Copy(dst, body)
	if err != nil {
		return false, bodyError("design: download package", err)
	}

	if n > maxTarballBytes {
		return false, gapError{"the package archive is larger than 100 MiB"}
	}

	if h != nil && !want.matches(h.Sum(nil)) {
		return false, errIntegrity
	}

	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("design: write package: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, fmt.Errorf("design: store package: %w", err)
	}

	return h != nil, nil
}

// bodyError wraps a failed body read; a network error is a transportError.
func bodyError(op string, err error) error {
	err = fmt.Errorf("%s: %w", op, err)

	var ne net.Error
	if errors.As(err, &ne) {
		return transportError{err}
	}

	return err
}

// integrityCheck is the strongest digest in an npm integrity string.
type integrityCheck struct {
	newHash func() hash.Hash
	want    []string // base64 digests without padding; any one matches
}

// parseIntegrity picks the strongest of sha512, sha384, sha256 and sha1 from a
// space-separated integrity string; nil when it names none of them.
func parseIntegrity(integrity string) *integrityCheck {
	algos := []struct {
		name    string
		newHash func() hash.Hash
	}{
		{"sha512", sha512.New},
		{"sha384", sha512.New384},
		{"sha256", sha256.New},
		{"sha1", sha1.New}, //nolint:gosec // npm integrity strings still use sha1
	}

	entries := strings.Fields(integrity)

	for _, algo := range algos {
		var want []string

		for _, entry := range entries {
			name, digest, ok := strings.Cut(entry, "-")
			if !ok || !strings.EqualFold(name, algo.name) {
				continue
			}

			digest, _, _ = strings.Cut(digest, "?")
			want = append(want, strings.TrimRight(digest, "="))
		}

		if len(want) > 0 {
			return &integrityCheck{newHash: algo.newHash, want: want}
		}
	}

	return nil
}

func (c *integrityCheck) matches(sum []byte) bool {
	got := strings.TrimRight(base64.StdEncoding.EncodeToString(sum), "=")

	for _, want := range c.want {
		if got == want {
			return true
		}
	}

	return false
}

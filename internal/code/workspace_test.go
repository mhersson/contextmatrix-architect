package code

import (
	"context"
	"errors"
	"net/http"
	"net/http/cgi" //nolint:gosec // test-only git http-backend fixture; httpoxy affects Go before 1.6.3 only
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureRepo builds a repository with one commit on "main" and returns its
// path. Fetching it goes through go-git's file transport, which execs
// git-upload-pack; production fetches over HTTPS and never execs git.
func fixtureRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required for the file transport in tests")
	}

	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: plumbing.Main}})
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	for name, content := range files {
		full := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))

		_, err = wt.Add(name)
		require.NoError(t, err)
	}

	hash, err := wt.Commit("init", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}})
	require.NoError(t, err)

	return dir, hash.String()
}

// emptyRepo builds a repository with no commits whose default branch would
// be main, and returns its path.
func emptyRepo(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary required for the file transport in tests")
	}

	dir := t.TempDir()
	_, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{InitOptions: git.InitOptions{DefaultBranch: plumbing.Main}})
	require.NoError(t, err)

	return dir
}

// gitServer serves the repositories under root over smart HTTP through git
// http-backend. A path under /broken/ answers 500. With held set, every
// request signals entered and waits until release is called; release is also
// called at cleanup, so a held server never blocks the test's end.
func gitServer(t *testing.T, root string, held bool) (base string, entered <-chan struct{}, release func()) {
	t.Helper()

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary required for git http-backend in tests")
	}

	backend := &cgi.Handler{
		Path: gitBin, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "HOME=" + t.TempDir()},
	}

	in := make(chan struct{}, 16)
	gate := make(chan struct{})

	var once sync.Once

	release = func() { once.Do(func() { close(gate) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/broken/") {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		if held {
			select {
			case in <- struct{}{}:
			default:
			}

			<-gate
		}

		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(release)

	return srv.URL, in, release
}

// gateCreds parks every Fetch until the test lets it go, then refuses.
type gateCreds struct {
	entered chan struct{}
	release chan struct{}

	mu             sync.Mutex
	inflight, peak int
}

func newGateCreds() *gateCreds {
	return &gateCreds{entered: make(chan struct{}, 4), release: make(chan struct{}, 4)}
}

func (g *gateCreds) Fetch(ctx context.Context, _, _ string) (string, string, error) {
	g.mu.Lock()
	g.inflight++
	g.peak = max(g.peak, g.inflight)
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		g.inflight--
		g.mu.Unlock()
	}()

	g.entered <- struct{}{}

	select {
	case <-g.release:
	case <-ctx.Done():
	}

	return "", "", errors.New("no credential")
}

func newTestWorkspace(root string, now func() time.Time) *Workspace {
	ws := NewWorkspace(root, now)
	ws.allowLocal = true

	return ws
}

func TestWorkspace_CheckoutExportsTheTreeAtTheDefaultBranch(t *testing.T) {
	src, sha := fixtureRepo(t, map[string]string{"AGENTS.md": "# agents\n", "cmd/main.go": "package main\n"})
	ws := newTestWorkspace(t.TempDir(), time.Now)

	co, err := ws.Checkout(context.Background(), "BP1", "alpha", src, nil)
	require.NoError(t, err)
	assert.Equal(t, sha, co.SHA)
	assert.Equal(t, "main", co.DefaultBranch)
	assert.Equal(t, filepath.Join(ws.TreeRoot("BP1"), "alpha"), co.Dir)

	data, err := os.ReadFile(filepath.Join(co.Dir, "cmd", "main.go"))
	require.NoError(t, err)
	assert.Equal(t, "package main\n", string(data))

	_, err = os.Stat(filepath.Join(co.Dir, ".git"))
	assert.True(t, os.IsNotExist(err), "a plain tree, no .git")

	info, err := Inspect(context.Background(), co.Dir)
	require.NoError(t, err)
	assert.Equal(t, "AGENTS.md", info.InstructionFile)
}

func TestWorkspace_ParallelCheckoutsShareOneMirror(t *testing.T) {
	src, sha := fixtureRepo(t, map[string]string{"a.txt": "a\n"})
	root := t.TempDir()
	ws := newTestWorkspace(root, time.Now)

	var wg sync.WaitGroup

	results := make([]Checkout, 4)
	errs := make([]error, 4)

	for i := range 4 {
		wg.Go(func() {
			results[i], errs[i] = ws.Checkout(context.Background(), "BP"+string(rune('1'+i)), "alpha", src, nil)
		})
	}

	wg.Wait()

	for i := range 4 {
		require.NoError(t, errs[i])
		assert.Equal(t, sha, results[i].SHA)
		assert.FileExists(t, filepath.Join(results[i].Dir, "a.txt"))
	}

	mirrors, err := os.ReadDir(filepath.Join(root, "mirrors"))
	require.NoError(t, err)
	assert.Len(t, mirrors, 1, "one mirror for one repository")

	trees, err := os.ReadDir(filepath.Join(root, "trees"))
	require.NoError(t, err)
	assert.Len(t, trees, 4, "one tree root per blueprint")
}

func TestWorkspace_RemoveAndSweep(t *testing.T) {
	src, _ := fixtureRepo(t, map[string]string{"a.txt": "a\n"})
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	ws := newTestWorkspace(t.TempDir(), func() time.Time { return now })

	_, err := ws.Checkout(context.Background(), "OLD", "alpha", src, nil)
	require.NoError(t, err)

	stale := now.Add(-10 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(ws.TreeRoot("OLD"), stale, stale))

	fresh, err := ws.Checkout(context.Background(), "FRESH", "alpha", src, nil)
	require.NoError(t, err)

	removed, err := ws.Sweep(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.NoDirExists(t, ws.TreeRoot("OLD"))
	assert.DirExists(t, fresh.Dir)

	mirrors, err := os.ReadDir(filepath.Join(ws.root, "mirrors"))
	require.NoError(t, err)
	assert.Len(t, mirrors, 1, "mirrors are never swept")

	require.NoError(t, ws.Remove(context.Background(), "FRESH"))
	assert.NoDirExists(t, ws.TreeRoot("FRESH"))
	require.NoError(t, ws.Remove(context.Background(), "FRESH"), "idempotent")
}

func TestRepoKey(t *testing.T) {
	tests := []struct {
		in, key, host, path string
	}{
		{in: "https://github.com/Acme/Alpha.git", key: "github.com__acme__alpha", host: "github.com", path: "Acme/Alpha"},
		{in: "git@github.com:acme/beta", key: "github.com__acme__beta", host: "github.com", path: "acme/beta"},
		{in: "ssh://git@github.com/acme/gamma", key: "github.com__acme__gamma", host: "github.com", path: "acme/gamma"},
		{in: "/tmp/fixtures/alpha", key: "local__alpha", host: "local", path: "alpha"},
	}

	for _, tt := range tests {
		key, host, path, err := repoKey(tt.in)
		require.NoError(t, err, tt.in)
		assert.Equal(t, tt.key, key)
		assert.Equal(t, tt.host, host)
		assert.Equal(t, tt.path, path)
	}

	_, _, _, err := repoKey("not a url")
	require.Error(t, err)
}

func TestWorkspace_CheckoutRefusesUnsafeInput(t *testing.T) {
	src, _ := fixtureRepo(t, map[string]string{"a.txt": "a\n"})
	root := t.TempDir()
	ws := newTestWorkspace(root, time.Now)

	for _, name := range []string{"", ".", "..", "../x", "a/../b", "/abs", `a\b`, "a/b/c", "a//b"} {
		_, err := ws.Checkout(context.Background(), "BP", name, src, nil)
		require.Error(t, err, "name %q", name)
	}

	_, err := ws.Checkout(context.Background(), "../BP", "alpha", src, nil)
	require.Error(t, err)

	_, err = ws.Checkout(context.Background(), "BP", "alpha/beta", src, nil)
	require.Error(t, err, "a tree name is one path segment")

	_, err = ws.Checkout(context.Background(), "BP", "alpha@beta", src, nil)
	require.NoError(t, err, "a secondary repository is <project>@<repo>")
}

func TestWorkspace_SecondaryRepositoryIsASiblingTree(t *testing.T) {
	primary, _ := fixtureRepo(t, map[string]string{"beta/own.txt": "primary\n"})
	secondary, _ := fixtureRepo(t, map[string]string{"web.txt": "secondary\n"})
	ws := newTestWorkspace(t.TempDir(), time.Now)
	ctx := context.Background()

	sec, err := ws.Checkout(ctx, "BP", "alpha@beta", secondary, nil)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(ws.TreeRoot("BP"), "alpha@beta"), sec.Dir)

	pri, err := ws.Checkout(ctx, "BP", "alpha", primary, nil)
	require.NoError(t, err)

	assert.FileExists(t, filepath.Join(sec.Dir, "web.txt"), "re-exporting the primary keeps the secondary")
	assert.FileExists(t, filepath.Join(pri.Dir, "beta", "own.txt"), "a primary directory named like the secondary is its own")
	assert.NoFileExists(t, filepath.Join(pri.Dir, "beta", "web.txt"))
}

func TestWorkspace_UserinfoNeverReachesDiskErrorsOrTheResult(t *testing.T) {
	const secret = "s3cr3t-token-0123456789"

	src, sha := fixtureRepo(t, map[string]string{"a.txt": "a\n"})
	base, _, _ := gitServer(t, filepath.Dir(src), false)
	root := t.TempDir()
	ws := newTestWorkspace(root, time.Now)
	withUser := strings.Replace(base, "http://", "http://user:"+secret+"@", 1)

	co, err := ws.Checkout(context.Background(), "BP", "alpha", withUser+"/"+filepath.Base(src), nil)
	require.NoError(t, err)
	assert.Equal(t, sha, co.SHA)
	assert.NotContains(t, co.RepoURL, secret)
	assert.Equal(t, base+"/"+filepath.Base(src), co.RepoURL)

	_, err = ws.Checkout(context.Background(), "BP", "beta", withUser+"/broken/"+filepath.Base(src), nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret, "a failing fetch's error carries no userinfo")

	configs, err := filepath.Glob(filepath.Join(root, "mirrors", "*", "config"))
	require.NoError(t, err)
	require.Len(t, configs, 2)

	for _, cfg := range configs {
		data, err := os.ReadFile(cfg)
		require.NoError(t, err)
		assert.NotContains(t, string(data), secret, "the mirror's config carries no userinfo")
	}
}

func TestWorkspace_SweepAndRemoveWaitForACheckout(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	ws := newTestWorkspace(t.TempDir(), func() time.Time { return now })
	stale := now.Add(-10 * 24 * time.Hour)

	for _, id := range []string{"BUSY", "IDLE"} {
		require.NoError(t, os.MkdirAll(filepath.Join(ws.TreeRoot(id), "alpha"), 0o750))
		require.NoError(t, os.Chtimes(ws.TreeRoot(id), stale, stale))
	}

	creds := newGateCreds()
	done := make(chan error, 1)

	go func() {
		_, err := ws.Checkout(context.Background(), "BUSY", "alpha", "https://github.com/acme/alpha", creds)
		done <- err
	}()

	<-creds.entered

	removed, err := ws.Sweep(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.DirExists(t, ws.TreeRoot("BUSY"), "a blueprint mid-checkout is not swept")
	assert.NoDirExists(t, ws.TreeRoot("IDLE"))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, ws.Remove(ctx, "BUSY"), context.DeadlineExceeded, "Remove waits for the checkout")
	assert.DirExists(t, ws.TreeRoot("BUSY"))

	creds.release <- struct{}{}

	require.Error(t, <-done)

	removed, err = ws.Sweep(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.NoDirExists(t, ws.TreeRoot("BUSY"))
}

func TestWorkspace_CheckoutsOfOneBlueprintSerialise(t *testing.T) {
	ws := newTestWorkspace(t.TempDir(), time.Now)
	creds := newGateCreds()

	var wg sync.WaitGroup

	for _, name := range []string{"alpha", "alpha@beta"} {
		wg.Go(func() {
			_, _ = ws.Checkout(context.Background(), "BP", name, "https://github.com/acme/"+name, creds)
		})
	}

	<-creds.entered

	select {
	case <-creds.entered:
		t.Error("a second checkout of the blueprint ran alongside the first")
	case <-time.After(200 * time.Millisecond):
	}

	for range 2 {
		creds.release <- struct{}{}
	}

	wg.Wait()

	assert.Equal(t, 1, creds.peak)
}

func TestWorkspace_CheckoutStopsWaitingForTheRepoLockWithItsContext(t *testing.T) {
	src, _ := fixtureRepo(t, map[string]string{"a.txt": "a\n"})
	base, entered, release := gitServer(t, filepath.Dir(src), true)
	repo := base + "/" + filepath.Base(src)
	ws := newTestWorkspace(t.TempDir(), time.Now)

	first := make(chan error, 1)

	go func() {
		_, err := ws.Checkout(context.Background(), "BP1", "alpha", repo, nil)
		first <- err
	}()

	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	second := make(chan error, 1)

	go func() {
		_, err := ws.Checkout(ctx, "BP2", "alpha", repo, nil)
		second <- err
	}()

	select {
	case err := <-second:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(3 * time.Second):
		t.Error("a checkout waiting for the repo lock ignored its context")
	}

	release()
	require.NoError(t, <-first)
}

func TestWorkspace_CheckoutIsHTTPSOnly(t *testing.T) {
	ws := NewWorkspace(t.TempDir(), time.Now)

	for _, u := range []string{"ssh://git@github.com/acme/a", "git@github.com:acme/a", "http://github.com/acme/a", "/tmp/fixtures/a"} {
		_, err := ws.Checkout(context.Background(), "BP", "alpha", u, nil)
		require.Error(t, err, u)
		assert.Contains(t, err.Error(), "only https", u)
	}
}

func TestCleanURL(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{name: "user and password", in: "https://user:s3cr3t@example.com/acme/alpha.git", want: "https://example.com/acme/alpha.git"},
		{name: "token as user", in: "https://s3cr3t@example.com/acme/alpha.git", want: "https://example.com/acme/alpha.git"},
		{name: "surrounding space", in: "  https://user:s3cr3t@example.com/alpha.git \n", want: "https://example.com/alpha.git"},
		{name: "already clean", in: "https://example.com/acme/alpha.git", want: "https://example.com/acme/alpha.git"},
		{name: "scp-like form is not a url", in: "git@example.com:acme/alpha.git", want: "git@example.com:acme/alpha.git"},
		{name: "empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CleanURL(tt.in)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "s3cr3t")
		})
	}
}

func TestRepoKey_RefusesUnsafeKeys(t *testing.T) {
	for _, in := range []string{"git@../../..:x", "https://github.com/../../etc", "https://github.com/a/..", "git@host:a\\b"} {
		_, _, _, err := repoKey(in)
		require.Error(t, err, in)
	}
}

func TestTreeTarget(t *testing.T) {
	base := t.TempDir()

	got, err := treeTarget(base, "cmd/main.go")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "cmd", "main.go"), got)

	for _, name := range []string{"../evil", "a/../../evil", "/abs"} {
		_, err := treeTarget(base, name)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), name)
	}
}

func TestExportTree_StopsWhenCancelled(t *testing.T) {
	src, sha := fixtureRepo(t, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	root := t.TempDir()
	ws := newTestWorkspace(root, time.Now)

	_, err := ws.Checkout(context.Background(), "BP", "alpha", src, nil)
	require.NoError(t, err)

	mirrors, err := filepath.Glob(filepath.Join(root, "mirrors", "*"))
	require.NoError(t, err)
	require.Len(t, mirrors, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dir := filepath.Join(t.TempDir(), "tree")

	err = exportTree(ctx, mirrors[0], sha, dir)
	require.ErrorIs(t, err, context.Canceled)
	assert.NoDirExists(t, dir)
	assert.NoDirExists(t, dir+".partial", "a cancelled export leaves no partial tree")
}

func TestWorkspace_RemoteHeadFollowsTheDefaultBranch(t *testing.T) {
	src, first := fixtureRepo(t, map[string]string{"a.txt": "1\n"})
	ws := newTestWorkspace(t.TempDir(), time.Now)
	ctx := context.Background()

	branch, sha, err := ws.RemoteHead(ctx, src, nil)
	require.NoError(t, err)
	assert.Equal(t, "main", branch)
	assert.Equal(t, first, sha)

	repo, err := git.PlainOpen(src)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("2\n"), 0o600))

	_, err = wt.Add("a.txt")
	require.NoError(t, err)
	second, err := wt.Commit("move", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}})
	require.NoError(t, err)

	_, sha, err = ws.RemoteHead(ctx, src, nil)
	require.NoError(t, err)
	assert.Equal(t, second.String(), sha, "a moved branch reads its new head")

	_, err = os.Stat(filepath.Join(ws.root, mirrorsDir))
	assert.True(t, os.IsNotExist(err), "no mirror is written")
}

func TestWorkspace_RemoteHeadIsHTTPSOnly(t *testing.T) {
	ws := NewWorkspace(t.TempDir(), time.Now)

	_, _, err := ws.RemoteHead(context.Background(), "git@example.com:acme/alpha.git", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only https")
}

// failCreds refuses every Fetch and records what it was asked for.
type failCreds struct {
	mu    sync.Mutex
	asked [][2]string
}

var errNoCredential = errors.New("no credential")

func (f *failCreds) Fetch(_ context.Context, host, path string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.asked = append(f.asked, [2]string{host, path})

	return "", "", errNoCredential
}

func TestWorkspace_RemoteHeadStopsWhenCredentialsFail(t *testing.T) {
	var requests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	ws := newTestWorkspace(t.TempDir(), time.Now)
	creds := &failCreds{}

	_, _, err := ws.RemoteHead(context.Background(), srv.URL+"/acme/alpha.git", creds)
	require.ErrorIs(t, err, errNoCredential)

	assert.Zero(t, requests.Load(), "nothing is listed without a credential")
	assert.Equal(t, [][2]string{{"127.0.0.1", "acme/alpha"}}, creds.asked, "the credential is asked for the repository's host and path")
}

func TestWorkspace_AnEmptyRepositoryChecksOutAsAnEmptyTree(t *testing.T) {
	src := emptyRepo(t)
	ws := newTestWorkspace(t.TempDir(), time.Now)
	ctx := context.Background()

	co, err := ws.Checkout(ctx, "BP1", "alpha", src, nil)
	require.NoError(t, err)
	assert.Empty(t, co.SHA)
	assert.Empty(t, co.DefaultBranch)

	entries, err := os.ReadDir(co.Dir)
	require.NoError(t, err)
	assert.Empty(t, entries)

	info, err := Inspect(ctx, co.Dir)
	require.NoError(t, err)
	assert.Empty(t, info.InstructionFile)

	// The mirror made while the repository was empty takes its first commit.
	repo, err := git.PlainOpen(src)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(src, "AGENTS.md"), []byte("# agents\n"), 0o600))

	_, err = wt.Add("AGENTS.md")
	require.NoError(t, err)
	first, err := wt.Commit("first", &git.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}})
	require.NoError(t, err)

	co, err = ws.Checkout(ctx, "BP1", "alpha", src, nil)
	require.NoError(t, err)
	assert.Equal(t, first.String(), co.SHA)
	assert.Equal(t, "main", co.DefaultBranch)

	info, err = Inspect(ctx, co.Dir)
	require.NoError(t, err)
	assert.Equal(t, "AGENTS.md", info.InstructionFile)
}

func TestWorkspace_AnEmptyRepositoryHasNoHead(t *testing.T) {
	tests := []struct {
		name string
		url  func(t *testing.T, src string) string
	}{
		{name: "file", url: func(_ *testing.T, src string) string { return src }},
		{name: "smart http", url: func(t *testing.T, src string) string {
			t.Helper()

			base, _, _ := gitServer(t, filepath.Dir(src), false)

			return base + "/" + filepath.Base(src)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := tt.url(t, emptyRepo(t))
			ws := newTestWorkspace(t.TempDir(), time.Now)
			ctx := context.Background()

			branch, sha, err := ws.RemoteHead(ctx, url, nil)
			require.NoError(t, err)
			assert.Empty(t, branch)
			assert.Empty(t, sha)

			co, err := ws.Checkout(ctx, "BP1", "alpha", url, nil)
			require.NoError(t, err)
			assert.Empty(t, co.SHA)
		})
	}
}

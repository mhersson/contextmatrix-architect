package code

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
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

func TestWorkspace_CheckoutExportsTheTreeAtTheDefaultBranch(t *testing.T) {
	src, sha := fixtureRepo(t, map[string]string{"AGENTS.md": "# agents\n", "cmd/main.go": "package main\n"})
	ws := NewWorkspace(t.TempDir(), time.Now)

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

	assert.Equal(t, "AGENTS.md", Inspect(co.Dir).InstructionFile)
}

func TestWorkspace_ParallelCheckoutsShareOneMirror(t *testing.T) {
	src, sha := fixtureRepo(t, map[string]string{"a.txt": "a\n"})
	root := t.TempDir()
	ws := NewWorkspace(root, time.Now)

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
	ws := NewWorkspace(t.TempDir(), func() time.Time { return now })

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

	require.NoError(t, ws.Remove("FRESH"))
	assert.NoDirExists(t, ws.TreeRoot("FRESH"))
	require.NoError(t, ws.Remove("FRESH"), "idempotent")
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

func TestWorkspace_SecondCheckoutWaitsForTheFetchInFlight(t *testing.T) {
	src, _ := fixtureRepo(t, map[string]string{"a.txt": "a\n"})
	ws := NewWorkspace(t.TempDir(), time.Now)

	var (
		mu      sync.Mutex
		fetches int
	)

	realFetch := ws.fetch
	started := make(chan struct{})
	release := make(chan struct{})

	ws.fetch = func(ctx context.Context, mirror, repoURL string, auth *githttp.BasicAuth) (string, string, error) {
		mu.Lock()
		fetches++
		first := fetches == 1
		mu.Unlock()

		if first {
			close(started)
			<-release
		}

		return realFetch(ctx, mirror, repoURL, auth)
	}

	var wg sync.WaitGroup

	errs := make([]error, 2)

	wg.Go(func() { _, errs[0] = ws.Checkout(context.Background(), "BP1", "alpha", src, nil) })

	<-started

	wg.Go(func() { _, errs[1] = ws.Checkout(context.Background(), "BP2", "alpha", src, nil) })

	// Let the second checkout reach the in-flight fetch before it completes.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	assert.Equal(t, 1, fetches, "the second checkout reuses the fetch in flight")
	assert.FileExists(t, filepath.Join(ws.TreeRoot("BP2"), "alpha", "a.txt"))
}

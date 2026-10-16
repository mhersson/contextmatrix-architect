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

	assert.Equal(t, "AGENTS.md", Inspect(co.Dir).InstructionFile)
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

func TestWorkspace_SweepSkipsBusyBlueprints(t *testing.T) {
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	ws := NewWorkspace(t.TempDir(), func() time.Time { return now })
	stale := now.Add(-10 * 24 * time.Hour)

	for _, id := range []string{"BUSY", "IDLE"} {
		require.NoError(t, os.MkdirAll(filepath.Join(ws.TreeRoot(id), "alpha"), 0o750))
		require.NoError(t, os.Chtimes(ws.TreeRoot(id), stale, stale))
	}

	ws.markBusy("BUSY", 1)

	removed, err := ws.Sweep(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.DirExists(t, ws.TreeRoot("BUSY"))
	assert.NoDirExists(t, ws.TreeRoot("IDLE"))

	ws.markBusy("BUSY", -1)

	removed, err = ws.Sweep(context.Background(), 7*24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.NoDirExists(t, ws.TreeRoot("BUSY"))
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
	require.NoError(t, err, "a secondary repository nests under its project")
}

func TestWorkspace_CheckoutIsHTTPSOnly(t *testing.T) {
	ws := NewWorkspace(t.TempDir(), time.Now)

	for _, u := range []string{"ssh://git@github.com/acme/a", "git@github.com:acme/a", "http://github.com/acme/a", "/tmp/fixtures/a"} {
		_, err := ws.Checkout(context.Background(), "BP", "alpha", u, nil)
		require.Error(t, err, u)
		assert.Contains(t, err.Error(), "only https", u)
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

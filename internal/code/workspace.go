package code

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

const (
	mirrorsDir   = "mirrors"
	treesDir     = "trees"
	fetchTimeout = 2 * time.Minute
)

// Workspace is the on-disk cache: <root>/mirrors/<repoKey> holds one bare
// mirror per repository, <root>/trees/<blueprintID>/<name> one plain tree per
// blueprint and project, so one jail root per blueprint covers every target
// project. A mirror has at most one fetch in flight; a checkout that arrives
// meanwhile waits for it and reuses its result instead of fetching again.
// Trees are written from the mirror's objects without a git binary, so a
// fetch never touches a tree another blueprint is reading.
type Workspace struct {
	root string
	now  func() time.Time

	// fetch is the mirror fetch; a field so tests can observe and hold it.
	fetch func(ctx context.Context, mirror, repoURL string, auth *githttp.BasicAuth) (branch, sha string, err error)

	mu      sync.Mutex
	flights map[string]*flight
}

// flight is one in-progress mirror fetch and, once done is closed, its result.
type flight struct {
	done   chan struct{}
	branch string
	sha    string
	err    error
}

// NewWorkspace returns a workspace under root. now is injectable for the sweep.
func NewWorkspace(root string, now func() time.Time) *Workspace {
	w := &Workspace{root: root, now: now, flights: map[string]*flight{}}
	w.fetch = fetchMirror

	return w
}

// Checkout is a blueprint's tree of one repository at the commit it records.
type Checkout struct {
	RepoURL       string
	DefaultBranch string
	SHA           string
	Dir           string
}

// TreeRoot is the directory holding every tree of a blueprint: the jail root
// for its read tools. A path's first segment is the project name.
func (w *Workspace) TreeRoot(blueprintID string) string {
	return filepath.Join(w.root, treesDir, blueprintID)
}

// Checkout fetches the repository's mirror and exports its default branch
// into <TreeRoot(blueprintID)>/<name>, replacing any earlier tree there.
// creds may be nil for a repository that needs none.
func (w *Workspace) Checkout(ctx context.Context, blueprintID, name, repoURL string, creds Credentials) (Checkout, error) {
	if !validSegment(blueprintID) {
		return Checkout{}, fmt.Errorf("code: blueprint id %q is not valid", blueprintID)
	}

	if name == "" || strings.Contains(name, "..") || filepath.IsAbs(name) {
		return Checkout{}, fmt.Errorf("code: tree name %q is not valid", name)
	}

	key, host, path, err := repoKey(repoURL)
	if err != nil {
		return Checkout{}, err
	}

	mirror := filepath.Join(w.root, mirrorsDir, key)

	branch, sha, err := w.syncMirror(ctx, mirror, repoURL, host, path, creds)
	if err != nil {
		return Checkout{}, err
	}

	dir := filepath.Join(w.TreeRoot(blueprintID), filepath.FromSlash(name))

	if err := exportTree(mirror, sha, dir); err != nil {
		return Checkout{}, err
	}

	// A nested tree changes only its parent's mtime; stamp the root the sweep reads.
	now := w.now()
	if err := os.Chtimes(w.TreeRoot(blueprintID), now, now); err != nil {
		return Checkout{}, fmt.Errorf("code: stamp tree root: %w", err)
	}

	return Checkout{RepoURL: repoURL, DefaultBranch: branch, SHA: sha, Dir: dir}, nil
}

func validSegment(s string) bool {
	return s != "" && s != "." && !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
}

// syncMirror brings the mirror up to date and returns the default branch and
// its sha. The first caller fetches; callers arriving during that fetch wait
// and share its result. A waiter whose leader was cancelled retries.
func (w *Workspace) syncMirror(ctx context.Context, mirror, repoURL, host, path string, creds Credentials) (string, string, error) {
	for {
		w.mu.Lock()

		f, inFlight := w.flights[mirror]
		if !inFlight {
			f = &flight{done: make(chan struct{})}
			w.flights[mirror] = f
		}

		w.mu.Unlock()

		if inFlight {
			select {
			case <-f.done:
			case <-ctx.Done():
				return "", "", ctx.Err()
			}

			if f.err != nil && ctx.Err() == nil &&
				(errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) {
				continue
			}

			return f.branch, f.sha, f.err
		}

		f.branch, f.sha, f.err = w.lead(ctx, mirror, repoURL, host, path, creds)

		w.mu.Lock()
		delete(w.flights, mirror)
		w.mu.Unlock()
		close(f.done)

		return f.branch, f.sha, f.err
	}
}

// lead mints the credential and runs the fetch.
func (w *Workspace) lead(ctx context.Context, mirror, repoURL, host, path string, creds Credentials) (string, string, error) {
	var auth *githttp.BasicAuth

	if creds != nil {
		user, token, err := creds.Fetch(ctx, host, path)
		if err != nil {
			return "", "", err
		}

		auth = &githttp.BasicAuth{Username: user, Password: token}
	}

	return w.fetch(ctx, mirror, repoURL, auth)
}

// fetchMirror creates or updates the bare mirror and returns the default
// branch and its sha. Only heads are fetched, never tags or pull refs.
func fetchMirror(ctx context.Context, mirror, repoURL string, auth *githttp.BasicAuth) (string, string, error) {
	repo, err := git.PlainOpen(mirror)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		if err := os.MkdirAll(mirror, 0o750); err != nil {
			return "", "", fmt.Errorf("code: mirror dir: %w", err)
		}

		repo, err = git.PlainInit(mirror, true)
		if err != nil {
			return "", "", fmt.Errorf("code: init mirror: %w", err)
		}

		if _, err := repo.CreateRemote(&config.RemoteConfig{
			Name:  "origin",
			URLs:  []string{repoURL},
			Fetch: []config.RefSpec{"+refs/heads/*:refs/heads/*"},
		}); err != nil {
			return "", "", fmt.Errorf("code: mirror remote: %w", err)
		}
	} else if err != nil {
		return "", "", fmt.Errorf("code: open mirror: %w", err)
	}

	var authMethod transport.AuthMethod
	if auth != nil {
		authMethod = auth
	}

	fetchCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	err = repo.FetchContext(fetchCtx, &git.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{"+refs/heads/*:refs/heads/*"},
		Auth:       authMethod,
		Tags:       git.NoTags,
		Force:      true,
		Prune:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", "", fmt.Errorf("code: fetch %s: %w", redactURL(repoURL), err)
	}

	remote, err := repo.Remote("origin")
	if err != nil {
		return "", "", fmt.Errorf("code: mirror remote: %w", err)
	}

	refs, err := remote.ListContext(fetchCtx, &git.ListOptions{Auth: authMethod})
	if err != nil {
		return "", "", fmt.Errorf("code: list %s: %w", redactURL(repoURL), err)
	}

	branch := defaultBranch(refs)
	if branch == "" {
		return "", "", fmt.Errorf("code: %s has no default branch", redactURL(repoURL))
	}

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return "", "", fmt.Errorf("code: resolve %s: %w", branch, err)
	}

	return branch, ref.Hash().String(), nil
}

// defaultBranch reads the remote's HEAD symbolic ref; without the symref
// capability it falls back to main, then master, when either exists.
func defaultBranch(refs []*plumbing.Reference) string {
	names := map[string]bool{}

	for _, r := range refs {
		if r.Name() == plumbing.HEAD && r.Type() == plumbing.SymbolicReference {
			return r.Target().Short()
		}

		if r.Name().IsBranch() {
			names[r.Name().Short()] = true
		}
	}

	for _, candidate := range []string{"main", "master"} {
		if names[candidate] {
			return candidate
		}
	}

	return ""
}

// Remove deletes every tree of blueprintID. Mirrors stay.
func (w *Workspace) Remove(blueprintID string) error {
	if !validSegment(blueprintID) {
		return fmt.Errorf("code: blueprint id %q is not valid", blueprintID)
	}

	if err := os.RemoveAll(w.TreeRoot(blueprintID)); err != nil {
		return fmt.Errorf("code: remove trees of %s: %w", blueprintID, err)
	}

	return nil
}

// Sweep removes every blueprint's trees not modified within maxAge. Mirrors
// are never removed.
func (w *Workspace) Sweep(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := w.now().Add(-maxAge)
	removed := 0

	roots, err := os.ReadDir(filepath.Join(w.root, treesDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}

		return 0, fmt.Errorf("code: read trees: %w", err)
	}

	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return removed, err
		}

		if !root.IsDir() {
			continue
		}

		info, err := root.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}

		if err := os.RemoveAll(filepath.Join(w.root, treesDir, root.Name())); err != nil {
			return removed, fmt.Errorf("code: sweep %s: %w", root.Name(), err)
		}

		removed++
	}

	return removed, nil
}

// repoKey derives the on-disk key and the (host, path) CM matches
// credentials on. Accepts https://, ssh:// and scp-like git@host:owner/repo
// forms; a bare filesystem path (tests) keys under "local".
func repoKey(repoURL string) (key, host, path string, err error) {
	raw := strings.TrimSpace(repoURL)
	if raw == "" {
		return "", "", "", errors.New("code: empty repository url")
	}

	switch {
	case strings.Contains(raw, "://"):
		u, perr := url.Parse(raw)
		if perr != nil || u.Hostname() == "" {
			return "", "", "", errors.New("code: repository url is not valid")
		}

		host, path = u.Hostname(), u.Path
	case strings.Contains(raw, "@") && strings.Contains(raw, ":"):
		_, rest, _ := strings.Cut(raw, "@")
		host, path, _ = strings.Cut(rest, ":")
	case strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "."):
		host, path = "local", filepath.Base(filepath.Clean(raw))
	default:
		return "", "", "", errors.New("code: repository url is not valid")
	}

	host = strings.ToLower(strings.TrimSpace(host))
	path = strings.TrimSuffix(strings.Trim(strings.TrimSpace(path), "/"), ".git")

	if host == "" || path == "" {
		return "", "", "", errors.New("code: repository url is not valid")
	}

	key = strings.ToLower(host + "__" + strings.ReplaceAll(path, "/", "__"))

	return key, host, path, nil
}

// redactURL strips userinfo from a url before it reaches an error.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}

	u.User = nil

	return u.String()
}

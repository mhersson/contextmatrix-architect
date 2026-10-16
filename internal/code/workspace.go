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
// project. Mirrors are fetched under a per-repository lock so parallel
// blueprints never fetch the same repository at once; trees are written from
// the mirror's objects without a git binary, so a fetch never touches a tree
// another blueprint is reading.
type Workspace struct {
	root string
	now  func() time.Time

	// allowLocal admits filesystem repositories; only the package's tests set it.
	allowLocal bool

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	busy  map[string]int
}

// NewWorkspace returns a workspace under root. now is injectable for the sweep.
func NewWorkspace(root string, now func() time.Time) *Workspace {
	return &Workspace{root: root, now: now, locks: map[string]*sync.Mutex{}, busy: map[string]int{}}
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
// name is "<project>" or "<project>/<repo>". Only https repositories are
// accepted. creds may be nil for a repository that needs none; otherwise
// every Checkout mints its own credential.
func (w *Workspace) Checkout(ctx context.Context, blueprintID, name, repoURL string, creds Credentials) (Checkout, error) {
	if !validSegment(blueprintID) {
		return Checkout{}, fmt.Errorf("code: blueprint id %q is not valid", blueprintID)
	}

	if !validTreeName(name) {
		return Checkout{}, fmt.Errorf("code: tree name %q is not valid", name)
	}

	key, host, path, err := repoKey(repoURL)
	if err != nil {
		return Checkout{}, err
	}

	if scheme := repoScheme(repoURL); scheme != "https" && !w.allowLocal {
		return Checkout{}, fmt.Errorf("code: repository scheme %q is not supported, only https", scheme)
	}

	w.markBusy(blueprintID, 1)
	defer w.markBusy(blueprintID, -1)

	var auth *githttp.BasicAuth

	if creds != nil {
		user, token, err := creds.Fetch(ctx, host, path)
		if err != nil {
			return Checkout{}, err
		}

		auth = &githttp.BasicAuth{Username: user, Password: token}
	}

	mirror := filepath.Join(w.root, mirrorsDir, key)

	branch, sha, err := w.fetchMirror(ctx, mirror, repoURL, auth)
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

func (w *Workspace) markBusy(blueprintID string, delta int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.busy[blueprintID] += delta
	if w.busy[blueprintID] <= 0 {
		delete(w.busy, blueprintID)
	}
}

func (w *Workspace) isBusy(blueprintID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.busy[blueprintID] > 0
}

func validSegment(s string) bool {
	return s != "" && s != "." && !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
}

// validTreeName admits "<project>" or "<project>/<repo>", each a safe segment.
func validTreeName(name string) bool {
	segments := strings.Split(name, "/")
	if len(segments) > 2 {
		return false
	}

	for _, seg := range segments {
		if !validSegment(seg) {
			return false
		}
	}

	return true
}

// repoScheme names how a repository url would be reached.
func repoScheme(raw string) string {
	raw = strings.TrimSpace(raw)

	if before, _, ok := strings.Cut(raw, "://"); ok {
		return strings.ToLower(before)
	}

	if strings.Contains(raw, "@") && strings.Contains(raw, ":") {
		return "ssh"
	}

	return "file"
}

// fetchMirror creates or updates the bare mirror under its per-repository lock
// and returns the default branch and its sha. Only heads are fetched, never
// tags or pull refs.
func (w *Workspace) fetchMirror(ctx context.Context, mirror, repoURL string, auth *githttp.BasicAuth) (string, string, error) {
	lock := w.lockFor(mirror)
	lock.Lock()
	defer lock.Unlock()

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

func (w *Workspace) lockFor(mirror string) *sync.Mutex {
	w.mu.Lock()
	defer w.mu.Unlock()

	l, ok := w.locks[mirror]
	if !ok {
		l = &sync.Mutex{}
		w.locks[mirror] = l
	}

	return l
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

		if !root.IsDir() || w.isBusy(root.Name()) {
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
	if !validSegment(key) {
		return "", "", "", errors.New("code: repository url is not valid")
	}

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

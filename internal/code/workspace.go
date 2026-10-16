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
	"github.com/go-git/go-git/v5/storage/memory"
)

const (
	mirrorsDir = "mirrors"
	treesDir   = "trees"
	// fetchTimeout bounds one fetch. The first fetch of a large repository is
	// a full mirror clone; the caller's context still ends it earlier.
	fetchTimeout = 10 * time.Minute
)

// Workspace is the on-disk cache: <root>/mirrors/<repoKey> holds one bare
// mirror per repository, <root>/trees/<blueprintID>/<name> one plain tree per
// blueprint and repository, so one jail root per blueprint covers every
// target project. Mirrors are fetched under a per-repository lock so parallel
// blueprints never fetch the same repository at once; trees are written from
// the mirror's objects without a git binary, so a fetch never touches a tree
// another blueprint is reading. A per-blueprint lock serialises a
// blueprint's checkouts, its removal and the sweep of its trees.
type Workspace struct {
	root string
	now  func() time.Time

	// allowLocal admits every scheme, not only https; only the package's
	// tests set it, for filesystem and plain-http fixtures.
	allowLocal bool

	mu         sync.Mutex
	repos      map[string]slot
	blueprints map[string]slot
}

// slot is a lock a waiter can give up on: a channel with room for one holder.
type slot chan struct{}

func (s slot) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s slot) tryLock() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s slot) unlock() { <-s }

// NewWorkspace returns a workspace under root. now is injectable for the sweep.
func NewWorkspace(root string, now func() time.Time) *Workspace {
	return &Workspace{root: root, now: now, repos: map[string]slot{}, blueprints: map[string]slot{}}
}

// Checkout is a blueprint's tree of one repository at the commit it records.
// A repository with no commits has an empty DefaultBranch and SHA, and its
// tree is an empty directory.
type Checkout struct {
	// RepoURL is the repository url without any userinfo.
	RepoURL       string
	DefaultBranch string
	SHA           string
	Dir           string
}

// TreeRoot is the directory holding every tree of a blueprint: the jail root
// for its read tools. A path's first segment is a tree name: "<project>" for
// a project's primary repository, "<project>@<repo>" for a secondary one.
func (w *Workspace) TreeRoot(blueprintID string) string {
	return filepath.Join(w.root, treesDir, blueprintID)
}

// Checkout fetches the repository's mirror and exports its default branch
// into <TreeRoot(blueprintID)>/<name>, replacing any earlier tree there.
// name is one path segment: "<project>" for a project's primary repository,
// "<project>@<repo>" for a secondary one, so every tree is a sibling of the
// others. Only https repositories are accepted, and userinfo in repoURL is
// dropped before it reaches the mirror, an error or the result. creds may be
// nil for a repository that needs none; otherwise every Checkout mints its
// own credential. Checkouts of one blueprint run one at a time.
func (w *Workspace) Checkout(ctx context.Context, blueprintID, name, repoURL string, creds Credentials) (Checkout, error) {
	if !validSegment(blueprintID) {
		return Checkout{}, fmt.Errorf("code: blueprint id %q is not valid", blueprintID)
	}

	if !validSegment(name) {
		return Checkout{}, fmt.Errorf("code: tree name %q is not valid", name)
	}

	repoURL = CleanURL(repoURL)

	key, host, path, err := repoKey(repoURL)
	if err != nil {
		return Checkout{}, err
	}

	if scheme := repoScheme(repoURL); scheme != "https" && !w.allowLocal {
		return Checkout{}, fmt.Errorf("code: repository scheme %q is not supported, only https", scheme)
	}

	bp := w.slotFor(w.blueprints, blueprintID)
	if err := bp.lock(ctx); err != nil {
		return Checkout{}, fmt.Errorf("code: wait for blueprint %s: %w", blueprintID, err)
	}
	defer bp.unlock()

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

	dir := filepath.Join(w.TreeRoot(blueprintID), name)

	if err := exportTree(ctx, mirror, sha, dir); err != nil {
		return Checkout{}, err
	}

	// The sweep reads the root's mtime; stamp it with the workspace clock.
	now := w.now()
	if err := os.Chtimes(w.TreeRoot(blueprintID), now, now); err != nil {
		return Checkout{}, fmt.Errorf("code: stamp tree root: %w", err)
	}

	return Checkout{RepoURL: repoURL, DefaultBranch: branch, SHA: sha, Dir: dir}, nil
}

// RemoteHead lists the repository's refs without fetching and returns its
// default branch and that branch's sha: what a checkout now would record.
// A repository with no commits returns both empty. It writes nothing to
// disk, so it needs no lock.
func (w *Workspace) RemoteHead(ctx context.Context, repoURL string, creds Credentials) (string, string, error) {
	repoURL = CleanURL(repoURL)

	_, host, path, err := repoKey(repoURL)
	if err != nil {
		return "", "", err
	}

	if scheme := repoScheme(repoURL); scheme != "https" && !w.allowLocal {
		return "", "", fmt.Errorf("code: repository scheme %q is not supported, only https", scheme)
	}

	var auth transport.AuthMethod

	if creds != nil {
		user, token, err := creds.Fetch(ctx, host, path)
		if err != nil {
			return "", "", err
		}

		auth = &githttp.BasicAuth{Username: user, Password: token}
	}

	listCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{repoURL}})

	refs, err := remote.ListContext(listCtx, &git.ListOptions{Auth: auth})

	switch {
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		return "", "", nil
	case err != nil:
		return "", "", fmt.Errorf("code: list %s: %w", repoURL, err)
	}

	branch := defaultBranch(refs)
	if branch == "" {
		return "", "", fmt.Errorf("code: %s has no default branch", repoURL)
	}

	want := plumbing.NewBranchReferenceName(branch)

	for _, r := range refs {
		if r.Name() == want {
			return branch, r.Hash().String(), nil
		}
	}

	return "", "", fmt.Errorf("code: %s lists no ref for its default branch %s", repoURL, branch)
}

// slotFor returns the lock for key in locks, creating it on first use.
func (w *Workspace) slotFor(locks map[string]slot, key string) slot {
	w.mu.Lock()
	defer w.mu.Unlock()

	l, ok := locks[key]
	if !ok {
		l = make(slot, 1)
		locks[key] = l
	}

	return l
}

// validSegment admits one safe path segment. "@" is allowed: it joins a
// project and a secondary repository in a tree name.
func validSegment(s string) bool {
	return s != "" && s != "." && !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
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
	lock := w.slotFor(w.repos, mirror)
	if err := lock.lock(ctx); err != nil {
		return "", "", fmt.Errorf("code: wait for the mirror of %s: %w", repoURL, err)
	}
	defer lock.unlock()

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
	switch {
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		// No commits, so no default branch: the tree is empty, and the
		// mirror kept here takes the first commit on a later fetch.
		return "", "", nil
	case err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate):
		return "", "", fmt.Errorf("code: fetch %s: %w", repoURL, err)
	}

	remote, err := repo.Remote("origin")
	if err != nil {
		return "", "", fmt.Errorf("code: mirror remote: %w", err)
	}

	refs, err := remote.ListContext(fetchCtx, &git.ListOptions{Auth: authMethod})
	if err != nil {
		return "", "", fmt.Errorf("code: list %s: %w", repoURL, err)
	}

	branch := defaultBranch(refs)
	if branch == "" {
		return "", "", fmt.Errorf("code: %s has no default branch", repoURL)
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

// Remove deletes every tree of blueprintID once no checkout of it is in
// flight. Mirrors stay.
func (w *Workspace) Remove(ctx context.Context, blueprintID string) error {
	if !validSegment(blueprintID) {
		return fmt.Errorf("code: blueprint id %q is not valid", blueprintID)
	}

	bp := w.slotFor(w.blueprints, blueprintID)
	if err := bp.lock(ctx); err != nil {
		return fmt.Errorf("code: wait for blueprint %s: %w", blueprintID, err)
	}
	defer bp.unlock()

	if err := os.RemoveAll(w.TreeRoot(blueprintID)); err != nil {
		return fmt.Errorf("code: remove trees of %s: %w", blueprintID, err)
	}

	return nil
}

// Sweep removes every blueprint's trees not written within maxAge, skipping
// a blueprint whose lock a checkout or a removal holds. Mirrors are never
// removed.
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

		ok, err := w.sweepOne(root.Name(), cutoff)
		if err != nil {
			return removed, err
		}

		if ok {
			removed++
		}
	}

	return removed, nil
}

// sweepOne removes one blueprint's trees when they are older than cutoff and
// no one holds its lock; the age is read and the trees removed under it.
func (w *Workspace) sweepOne(blueprintID string, cutoff time.Time) (bool, error) {
	bp := w.slotFor(w.blueprints, blueprintID)
	if !bp.tryLock() {
		return false, nil
	}
	defer bp.unlock()

	dir := w.TreeRoot(blueprintID)

	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("code: sweep %s: %w", blueprintID, err)
	}

	if !info.ModTime().Before(cutoff) {
		return false, nil
	}

	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("code: sweep %s: %w", blueprintID, err)
	}

	return true, nil
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

// CleanURL is the repository url as Checkout uses and reports it: trimmed,
// with any userinfo dropped. Callers that record a repository key it on
// this form, so a credential embedded in a project's url never reaches a
// store row, an event or the panel.
func CleanURL(raw string) string {
	return stripUserinfo(strings.TrimSpace(raw))
}

// stripUserinfo drops userinfo from a url, so a credential embedded in it
// never reaches the mirror's config, a fetch error or a Checkout. Anything
// that does not parse as a url is returned as is.
func stripUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}

	u.User = nil

	return u.String()
}

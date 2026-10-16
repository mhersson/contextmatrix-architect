package code

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// exportTree writes the tree of commit sha from the bare mirror into dir as
// plain files, replacing dir. An empty sha, a repository with no commits,
// writes an empty dir. Pure Go: no index, no .git, no git binary. Symlinks
// are skipped; submodules are not in the file iterator. A cancelled ctx
// stops it between files and leaves dir as it was.
func exportTree(ctx context.Context, mirror, sha, dir string) error {
	var tree *object.Tree

	if sha != "" {
		repo, err := git.PlainOpen(mirror)
		if err != nil {
			return fmt.Errorf("code: open mirror: %w", err)
		}

		commit, err := repo.CommitObject(plumbing.NewHash(sha))
		if err != nil {
			return fmt.Errorf("code: commit %s: %w", sha, err)
		}

		tree, err = commit.Tree()
		if err != nil {
			return fmt.Errorf("code: tree of %s: %w", sha, err)
		}
	}

	tmp := dir + ".partial"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("code: clear partial tree: %w", err)
	}

	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return fmt.Errorf("code: partial tree: %w", err)
	}

	if tree != nil {
		if err := writeFiles(ctx, tree, tmp); err != nil {
			_ = os.RemoveAll(tmp)

			return fmt.Errorf("code: export tree: %w", err)
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("code: replace tree: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return fmt.Errorf("code: tree parent: %w", err)
	}

	if err := os.Rename(tmp, dir); err != nil {
		return fmt.Errorf("code: place tree: %w", err)
	}

	return nil
}

// writeFiles writes every file of tree under base, stopping between files
// once ctx ends. Symlinks are skipped.
func writeFiles(ctx context.Context, tree *object.Tree, base string) error {
	return tree.Files().ForEach(func(f *object.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}

		if f.Mode == filemode.Symlink {
			return nil
		}

		perm := os.FileMode(0o644)
		if f.Mode == filemode.Executable {
			perm = 0o755
		}

		target, err := treeTarget(base, f.Name)
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}

		reader, err := f.Reader()
		if err != nil {
			return err
		}

		defer func() { _ = reader.Close() }()

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
		if err != nil {
			return err
		}

		if _, err := io.Copy(out, reader); err != nil {
			_ = out.Close()

			return err
		}

		return out.Close()
	})
}

// treeTarget places a tree entry under base, refusing any name that would
// resolve outside it.
func treeTarget(base, name string) (string, error) {
	local := filepath.FromSlash(name)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("tree entry %q escapes the checkout", name)
	}

	return filepath.Join(base, local), nil
}

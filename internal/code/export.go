package code

import (
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
// plain files, replacing dir. Pure Go: no index, no .git, no git binary.
// Symlinks are skipped; submodules are not in the file iterator.
func exportTree(mirror, sha, dir string) error {
	repo, err := git.PlainOpen(mirror)
	if err != nil {
		return fmt.Errorf("code: open mirror: %w", err)
	}

	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return fmt.Errorf("code: commit %s: %w", sha, err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return fmt.Errorf("code: tree of %s: %w", sha, err)
	}

	tmp := dir + ".partial"
	if err := os.RemoveAll(tmp); err != nil {
		return fmt.Errorf("code: clear partial tree: %w", err)
	}

	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return fmt.Errorf("code: partial tree: %w", err)
	}

	err = tree.Files().ForEach(func(f *object.File) error {
		if f.Mode == filemode.Symlink {
			return nil
		}

		perm := os.FileMode(0o644)
		if f.Mode == filemode.Executable {
			perm = 0o755
		}

		target := filepath.Join(tmp, filepath.FromSlash(f.Name))
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
	if err != nil {
		_ = os.RemoveAll(tmp)

		return fmt.Errorf("code: export tree: %w", err)
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

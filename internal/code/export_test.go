package code

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeObject writes o into repo's object store and returns its hash.
func storeObject(t *testing.T, repo *git.Repository, o interface {
	Encode(plumbing.EncodedObject) error
},
) plumbing.Hash {
	t.Helper()

	obj := repo.Storer.NewEncodedObject()
	require.NoError(t, o.Encode(obj))

	h, err := repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)

	return h
}

// storeBlob writes content into repo's object store as a blob.
func storeBlob(t *testing.T, repo *git.Repository, content string) plumbing.Hash {
	t.Helper()

	obj := repo.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)

	w, err := obj.Writer()
	require.NoError(t, err)
	_, err = w.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	h, err := repo.Storer.SetEncodedObject(obj)
	require.NoError(t, err)

	return h
}

func TestExportTree_WritesEachBlobWithItsModeAndNoSymlink(t *testing.T) {
	type entry struct {
		name, content string
		mode          filemode.FileMode
		perm          fs.FileMode // 0: not written
	}

	// Sorted by name, as git orders a tree.
	entries := []entry{
		{name: "link", content: "plain.txt", mode: filemode.Symlink},
		{name: "plain.txt", content: "plain\n", mode: filemode.Regular, perm: 0o644},
		{name: "run.sh", content: "#!/bin/sh\n", mode: filemode.Executable, perm: 0o755},
	}

	mirror := t.TempDir()
	repo, err := git.PlainInit(mirror, true)
	require.NoError(t, err)

	tree := &object.Tree{}
	for _, e := range entries {
		tree.Entries = append(tree.Entries, object.TreeEntry{Name: e.name, Mode: e.mode, Hash: storeBlob(t, repo, e.content)})
	}

	sig := object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}
	sha := storeObject(t, repo, &object.Commit{Author: sig, Committer: sig, Message: "init", TreeHash: storeObject(t, repo, tree)})

	dir := filepath.Join(t.TempDir(), "tree")
	require.NoError(t, exportTree(context.Background(), mirror, sha.String(), dir))

	for _, e := range entries {
		t.Run(e.name, func(t *testing.T) {
			path := filepath.Join(dir, e.name)

			info, err := os.Lstat(path)
			if e.perm == 0 {
				require.ErrorIs(t, err, fs.ErrNotExist, "a symlink is never written")

				return
			}

			require.NoError(t, err)
			assert.True(t, info.Mode().IsRegular())
			assert.Equal(t, e.perm, info.Mode().Perm())

			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, e.content, string(got))
		})
	}
}

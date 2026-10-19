package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_PathWithURICharacters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "a#b?c%d")
	require.NoError(t, os.MkdirAll(dir, 0o750))

	path := filepath.Join(dir, "architect.db")
	now := time.Now().UTC().Truncate(time.Second)

	s, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{
		ID: "BP1", Title: "t", Projects: []string{"alpha"}, Mode: "yolo",
		State: "new", CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, s.Close())

	_, err = os.Stat(path)
	require.NoError(t, err, "the database must be at exactly the given path")

	for _, cut := range []string{
		filepath.Join(root, "a"),
		filepath.Join(root, "a#b"),
		filepath.Join(root, "a#b?c%d.db"),
	} {
		_, err = os.Stat(cut)
		require.ErrorIs(t, err, os.ErrNotExist, "no file may appear at %s", cut)
	}

	s, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, "t", got.Title)
}

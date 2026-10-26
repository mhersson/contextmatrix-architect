package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/code"
)

func TestSweepWorkspaces_RemovesStaleTreesAndStopsOnCancel(t *testing.T) {
	root := t.TempDir()
	ws := code.NewWorkspace(root, time.Now, nil)

	stale := filepath.Join(ws.TreeRoot("OLD"), "alpha")
	require.NoError(t, os.MkdirAll(stale, 0o750))

	past := time.Now().Add(-10 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(ws.TreeRoot("OLD"), past, past))

	fresh := filepath.Join(ws.TreeRoot("NEW"), "alpha")
	require.NoError(t, os.MkdirAll(fresh, 0o750))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		sweepWorkspaces(ctx, ws, 7*24*time.Hour, 10*time.Millisecond, testLogger(t))
		close(done)
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(ws.TreeRoot("OLD"))

		return os.IsNotExist(err)
	}, 2*time.Second, 10*time.Millisecond)

	assert.DirExists(t, fresh)

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweep loop did not stop on cancel")
	}
}

package blueprint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func preflightOf(t *testing.T, c *Controller, st *store.Store, fake *fakeCM) preflightResult {
	t.Helper()

	ctx := context.Background()

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	draft, err := c.draft(ctx, "BP1")
	require.NoError(t, err)

	res, err := c.preflight(ctx, sessionOf(t, c, "BP1"), bp, draft, fake)
	require.NoError(t, err)

	return res
}

func TestPreflight(t *testing.T) {
	tests := []struct {
		name     string
		arrange  func(t *testing.T, fake *fakeCM, ws *fakeWorkspace, root string)
		blocking string // substring expected among the blocking problems
		warning  string // substring expected among the warnings
	}{
		{name: "clean", arrange: func(*testing.T, *fakeCM, *fakeWorkspace, string) {}},
		{
			name:     "project missing in ContextMatrix",
			arrange:  func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) { f.projects = nil },
			blocking: "project alpha is not in ContextMatrix",
		},
		{
			name: "no verify command",
			arrange: func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) {
				f.projects = []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git"}}
			},
			blocking: "declares no verify command",
		},
		{
			name: "no instruction file",
			arrange: func(t *testing.T, _ *fakeCM, _ *fakeWorkspace, root string) {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(root, "alpha", "AGENTS.md")))
			},
			blocking: "no AGENTS.md or CLAUDE.md",
		},
		{
			name: "checkout swept",
			arrange: func(t *testing.T, _ *fakeCM, _ *fakeWorkspace, root string) {
				t.Helper()
				require.NoError(t, os.RemoveAll(filepath.Join(root, "alpha")))
			},
			warning: "instruction file was not re-checked",
		},
		{
			name: "default branch moved",
			arrange: func(_ *testing.T, _ *fakeCM, ws *fakeWorkspace, _ string) {
				ws.heads = map[string]string{"https://example.com/alpha.git": "sha-new"}
			},
			warning: "moved since grounding",
		},
		{
			name:    "default branch unreadable",
			arrange: func(_ *testing.T, _ *fakeCM, ws *fakeWorkspace, _ string) { ws.headErr = errors.New("auth failed") },
			warning: "could not read the default branch",
		},
		{
			name: "title in another runnable playbook",
			arrange: func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) {
				f.list = []cm.PlaybookSummary{{ID: "other", Runnable: true}, {ID: "draft-only", Runnable: false}}
				f.bookSeq["other"] = []cm.Playbook{{ID: "other", Entries: []cm.PlaybookEntry{{Type: "card", Card: "ALPHA-090", CardTitle: " add the thing "}}}}
				f.bookSeq["draft-only"] = []cm.Playbook{{ID: "draft-only", Entries: []cm.PlaybookEntry{{Type: "card", Card: "ALPHA-091", CardTitle: "Add the thing"}}}}
			},
			warning: "has the title of ALPHA-090 in runnable playbook other",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _, ws := newPipeline(t, happyScript())
			fake := newFakeCM()
			c.opts.ConnectCM = fake.connect

			drive(t, c, "BP1")
			tt.arrange(t, fake, ws, ws.TreeRoot("BP1"))

			res := preflightOf(t, c, st, fake)

			if tt.blocking == "" {
				assert.Empty(t, res.Blocking)
			} else {
				require.Len(t, res.Blocking, 1)
				assert.Contains(t, res.Blocking[0], tt.blocking)
			}

			if tt.warning == "" {
				assert.Empty(t, res.Warnings)
			} else {
				require.Len(t, res.Warnings, 1)
				assert.Contains(t, res.Warnings[0], tt.warning)
			}
		})
	}
}

func TestEmit_RefusesWhilePreflightBlocks(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	fake.projects = []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git"}}
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	emitNow(t, c, st, "BP1")

	assert.Empty(t, fake.titles())

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "declares no verify command")
}

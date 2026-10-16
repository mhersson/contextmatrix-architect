package blueprint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	unset := []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git"}}
	noop := func(*testing.T, *fakeCM, *fakeWorkspace, string) {}

	tests := []struct {
		name     string
		cards    map[string]any // the draft; nil is goodCards
		arrange  func(t *testing.T, fake *fakeCM, ws *fakeWorkspace, root string)
		blocking string // substring expected among the blocking problems
		warning  string // substring expected among the warnings
		step     string // substring expected among the steps
	}{
		{name: "clean", arrange: noop},
		{
			name:     "project missing in ContextMatrix",
			arrange:  func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) { f.projects = nil },
			blocking: "project alpha is not in ContextMatrix",
		},
		{
			name: "no repository",
			arrange: func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) {
				f.projects = []cm.Project{{Name: "alpha", Verify: &cm.Verify{Command: "make test"}}}
			},
			blocking: "project alpha declares no repository",
		},
		{
			name: "no verify command, and the cards propose one", cards: setupCards(),
			arrange: func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) { f.projects = unset },
			step:    "set verify command `go test ./...` on project alpha in ContextMatrix",
		},
		{
			name:    "no verify command, and the cards propose none",
			arrange: func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) { f.projects = unset },
			warning: "project alpha declares no verify command and the cards propose none",
		},
		{name: "a proposal for a project that has a verify command", cards: setupCards(), arrange: noop},
		{
			name: "no instruction file",
			arrange: func(t *testing.T, _ *fakeCM, _ *fakeWorkspace, root string) {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(root, "alpha", "AGENTS.md")))
			},
		},
		{
			name: "checkout swept",
			arrange: func(t *testing.T, _ *fakeCM, _ *fakeWorkspace, root string) {
				t.Helper()
				require.NoError(t, os.RemoveAll(filepath.Join(root, "alpha")))
			},
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
			name: "the repository lost every commit since grounding",
			arrange: func(_ *testing.T, _ *fakeCM, ws *fakeWorkspace, _ string) {
				ws.heads = map[string]string{"https://example.com/alpha.git": ""}
			},
			warning: "to an empty repository",
			step:    "push an initial commit to https://example.com/alpha.git's default branch before running the cards; the agent needs a base branch",
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
			script := happyScript()
			if tt.cards != nil {
				script["decomposition"] = returns("# Cards", tt.cards)
			}

			c, st, _, _, ws := newPipeline(t, script)
			fake := newFakeCM()
			c.opts.ConnectCM = fake.connect

			drive(t, c, "BP1")
			tt.arrange(t, fake, ws, ws.TreeRoot("BP1"))

			res := preflightOf(t, c, st, fake)

			for _, check := range []struct {
				got  []string
				want string
			}{{res.Blocking, tt.blocking}, {res.Warnings, tt.warning}, {res.Steps, tt.step}} {
				if check.want == "" {
					assert.Empty(t, check.got)

					continue
				}

				require.Len(t, check.got, 1)
				assert.Contains(t, check.got[0], check.want)
			}
		})
	}
}

func TestEmit_RefusesWhilePreflightBlocks(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	fake.projects = []cm.Project{{Name: "alpha", Verify: &cm.Verify{Command: "make test"}}}
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	emitNow(t, c, st, "BP1")

	assert.Empty(t, fake.titles())

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "declares no repository")
}

func TestMovedRepos_EmptyRepositories(t *testing.T) {
	const url = "https://example.com/alpha.git"

	const push = "push an initial commit to https://example.com/alpha.git's default branch before running the cards; the agent needs a base branch"

	tests := []struct {
		name     string
		recorded string // the sha grounding recorded; "" for no commits
		head     string // the sha the default branch has now; "" for no commits
		targets  map[string]bool
		warnings []string
		steps    []string
	}{
		{name: "still empty", targets: map[string]bool{url: true}, steps: []string{push}},
		{name: "a repository no card runs in", targets: map[string]bool{}},
		{name: "a first commit since grounding", head: "0123456789abcdef", targets: map[string]bool{url: true}, warnings: []string{"from an empty repository to 0123456789ab"}},
		{name: "emptied since grounding", recorded: "0123456789abcdef", targets: map[string]bool{url: true}, warnings: []string{"from 0123456789ab to an empty repository"}, steps: []string{push}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, st, _, _, ws := newPipeline(t, happyScript())
			ctx := context.Background()
			require.NoError(t, c.Start(ctx, startInput("BP1")))

			_, err := st.AddSource(ctx, store.Source{
				BlueprintID: "BP1", Kind: "repo", Identity: url, Version: tt.recorded,
				FetchedAt: time.Now().UTC(), FirstUsedBy: "grounding",
			})
			require.NoError(t, err)

			ws.heads = map[string]string{url: tt.head}

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)

			warnings, steps, err := c.movedRepos(ctx, sessionOf(t, c, "BP1"), bp, tt.targets)
			require.NoError(t, err)
			assert.Equal(t, tt.steps, steps)
			require.Len(t, warnings, len(tt.warnings))

			for i, want := range tt.warnings {
				assert.Contains(t, warnings[i], want)
			}
		})
	}
}

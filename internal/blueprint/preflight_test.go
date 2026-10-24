package blueprint

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
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

// replacingCards is setupCards with alpha's setup naming, in replaces, the
// declared command its verify command replaces.
func replacingCards(replaces string) map[string]any {
	cards := setupCards()
	setup, _ := cards["setup"].(map[string]any)
	alpha, _ := setup["alpha"].(map[string]any)
	alpha["replaces"] = replaces

	return cards
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
		verify   []verifyGap
		empty    []string
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
			verify:  []verifyGap{{Project: "alpha", Command: "go test ./..."}},
		},
		{
			name:    "no verify command, and the cards propose none",
			arrange: func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) { f.projects = unset },
			warning: "project alpha declares no verify command and the cards propose none",
		},
		{
			name: "a proposal that replaces the declared command", cards: replacingCards("make test"), arrange: noop,
			verify:  []verifyGap{{Project: "alpha", Command: "go test ./...", Declared: "make test"}},
			warning: "project alpha declares verify command `make test`, which a worker cannot run; set `go test ./...` in ContextMatrix before the cards run",
		},
		{name: "a proposal that names no replaced command", cards: setupCards(), arrange: noop},
		{
			name: "a replacement after the person set their own command", cards: replacingCards("make test"),
			arrange: func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) {
				f.projects = []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "make lint"}}}
			},
		},
		{
			name: "a proposal equal to the declared command", cards: setupCards(),
			arrange: func(_ *testing.T, f *fakeCM, _ *fakeWorkspace, _ string) {
				f.projects = []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "go test ./..."}}}
			},
		},
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
			warning: "moved since the checkout",
		},
		{
			name:    "default branch unreadable",
			arrange: func(_ *testing.T, _ *fakeCM, ws *fakeWorkspace, _ string) { ws.headErr = errors.New("auth failed") },
			warning: "could not read the default branch",
		},
		{
			name: "the repository lost every commit since the checkout",
			arrange: func(_ *testing.T, _ *fakeCM, ws *fakeWorkspace, _ string) {
				ws.heads = map[string]string{"https://example.com/alpha.git": ""}
			},
			warning: "to an empty repository",
			step:    "push an initial commit to https://example.com/alpha.git's default branch before running the cards; the agent needs a base branch",
			empty:   []string{"https://example.com/alpha.git"},
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

			assert.Equal(t, tt.empty, res.EmptyRepos)
			assert.Equal(t, tt.verify, res.Verify)

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

// A first commit to a repository that was empty at the checkout is the push
// step done: no warning, no step.
func TestPreflight_AFirstCommitIsNotAMove(t *testing.T) {
	c, st, _, _, ws := newPipeline(t, happyScript())
	ws.empty = map[string]bool{"alpha": true}
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")

	before := preflightOf(t, c, st, fake)
	assert.Equal(t, []string{"https://example.com/alpha.git"}, before.EmptyRepos)

	ws.mu.Lock()
	ws.heads = map[string]string{"https://example.com/alpha.git": "sha-first"}
	ws.mu.Unlock()

	res := preflightOf(t, c, st, fake)
	assert.Empty(t, res.Warnings)
	assert.Empty(t, res.Steps)
	assert.Empty(t, res.EmptyRepos)
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
	assert.Equal(t, "emission", ev.Payload["resume"], "the stop offers approve to resume")
}

func TestMovedRepos_EmptyRepositories(t *testing.T) {
	const url = "https://example.com/alpha.git"

	tests := []struct {
		name     string
		recorded string // the sha the checkout recorded; "" for no commits
		head     string // the sha the default branch has now; "" for no commits
		targets  map[string]bool
		warnings []string
		empty    []string
	}{
		{name: "still empty", targets: map[string]bool{url: true}, empty: []string{url}},
		{name: "a repository no card runs in", targets: map[string]bool{}},
		{name: "a first commit since the checkout", head: "0123456789abcdef", targets: map[string]bool{url: true}},
		{name: "emptied since the checkout", recorded: "0123456789abcdef", targets: map[string]bool{url: true}, warnings: []string{"from 0123456789ab to an empty repository"}, empty: []string{url}},
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

			warnings, empty, err := c.movedRepos(ctx, sessionOf(t, c, "BP1"), bp, tt.targets)
			require.NoError(t, err)
			assert.Equal(t, tt.empty, empty)
			require.Len(t, warnings, len(tt.warnings))

			for i, want := range tt.warnings {
				assert.Contains(t, warnings[i], want)
			}
		})
	}
}

func TestPreflightSetup(t *testing.T) {
	const push = "push an initial commit to https://example.com/alpha.git's default branch before running the cards; the agent needs a base branch"

	res := preflightResult{Verify: []verifyGap{{Project: "alpha", Command: "go test ./..."}}, Steps: []string{push}}
	replaced := preflightResult{Verify: []verifyGap{{Project: "alpha", Command: "go test ./...", Declared: "make test"}}}

	tests := []struct {
		name       string
		res        preflightResult
		runsAtOnce bool
		steps      []string
		notices    []string
	}{
		{name: "a person can still set it", res: res, steps: []string{verifyStep, push}, notices: []string{}},
		{name: "the run starts at once", res: res, runsAtOnce: true, steps: []string{push}, notices: []string{verifyNotice("alpha", "go test ./...")}},
		{
			name: "a replacement a person can still set", res: replaced,
			steps: []string{"set verify command `go test ./...` on project alpha in ContextMatrix, replacing `make test`"}, notices: []string{},
		},
		{
			name: "a replacement, the run starts at once", res: replaced, runsAtOnce: true,
			steps: []string{"set verify command `go test ./...` on project alpha in ContextMatrix, replacing `make test`"}, notices: []string{},
		},
		{name: "nothing found", steps: []string{}, notices: []string{}},
		{name: "nothing found, the run starts at once", runsAtOnce: true, steps: []string{}, notices: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			steps, notices := tt.res.setup(tt.runsAtOnce)

			assert.Equal(t, tt.steps, steps)
			assert.Equal(t, tt.notices, notices)
		})
	}
}

func TestPreflightRunsAtOnce(t *testing.T) {
	tests := []struct {
		name  string
		mode  string
		empty []string
		want  bool
	}{
		{name: "yolo", mode: protocol.ArchitectModeYolo, want: true},
		{name: "yolo with a repository with no commits", mode: protocol.ArchitectModeYolo, empty: []string{"https://example.com/alpha.git"}},
		{name: "auto+emit", mode: protocol.ArchitectModeAutoEmit},
		{name: "auto", mode: protocol.ArchitectModeAuto},
		{name: "default", mode: protocol.ArchitectModeDefault},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, preflightResult{EmptyRepos: tt.empty}.runsAtOnce(tt.mode))
		})
	}
}

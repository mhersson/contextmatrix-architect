package blueprint

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mhersson/contextmatrix-harness/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/cm/cmtest"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

var knowledgeNames = []string{"cm_get_knowledge_context", "cm_query_knowledge", "cm_get_knowledge_item"}

// knowledgeSessions counts the knowledge sessions a controller opens and
// closes; a session with no tools counts as closed at once.
type knowledgeSessions struct {
	mu     sync.Mutex
	opened int
	closed int
}

func (s *knowledgeSessions) open(srv *cmtest.Knowledge) func(context.Context, string, cm.Knowledge) ([]tools.Tool, func(), error) {
	return func(ctx context.Context, key string, k cm.Knowledge) ([]tools.Tool, func(), error) {
		ts, closer, err := cm.OpenKnowledge(ctx, srv.URL, key, nil, k)
		if err != nil {
			return nil, nil, err
		}

		s.mu.Lock()
		s.opened++

		if len(ts) == 0 {
			s.closed++
		}
		s.mu.Unlock()

		return ts, func() {
			closer()

			if len(ts) > 0 {
				s.mu.Lock()
				s.closed++
				s.mu.Unlock()
			}
		}, nil
	}
}

func (s *knowledgeSessions) counts() (opened, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.opened, s.closed
}

func newKnowledgePipeline(t *testing.T, script stageScript, srv *cmtest.Knowledge) (*Controller, *store.Store, *fakeRunner, *recorder, *knowledgeSessions) {
	t.Helper()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	pk, err := pack.LoadDir("testdata/knowledge")
	require.NoError(t, err)

	runner := &fakeRunner{fn: scripted(script)}
	rec := newRecorder()
	sessions := &knowledgeSessions{}

	opts := Options{
		Store: st, Pack: pk, Runner: runner, MaxRunCostUSD: 2, MaxTurns: 10,
		Sink: rec.sink, HarnessLine: rec.line,
		Workspace: &fakeWorkspace{root: t.TempDir()}, Projects: listAlpha, Credentials: fakeCredentials,
		CMTools: (&fakeCMTools{}).open, ConnectCM: newFakeCM().connect, CallerIdentity: "human:architect",
	}
	if srv != nil {
		opts.KnowledgeTools = sessions.open(srv)
	}

	c := New(opts)

	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	return c, st, runner, rec, sessions
}

func knowledgeScript() stageScript {
	return stageScript{
		"intake":        returns("# Need", map[string]any{"slices": 1}),
		"intake.review": returns("# Review", map[string]any{"score": float64(5), "findings": []any{}}),
		"grounding":     returns("# Grounding", nil),
		"decomposition": returns("# Cards", goodCards()),
		"readiness":     returns("# Ready", readiness(true)),
	}
}

// callKnowledge runs the offered tool name; a tool the run was not offered is
// an error, not a test failure, since it runs inside a stage run.
func callKnowledge(ctx context.Context, in stage.Input, name string, args map[string]any) (string, error) {
	for _, t := range in.Tools {
		if t.Name() == name {
			res, err := t.Execute(ctx, args)
			if err != nil {
				return "", fmt.Errorf("call %s: %w", name, err)
			}

			return res.Text, nil
		}
	}

	return "", fmt.Errorf("%s is not offered to stage %s", name, in.Stage)
}

// calling wraps fn as stage name's script: it runs fn, then answers as the
// base script does.
func calling(base stageScript, name string, fn func(context.Context, stage.Input) error) stageScript {
	inner := base[name]
	base[name] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if err := fn(ctx, in); err != nil {
			return stage.Output{}, err
		}

		return inner(ctx, in)
	}

	return base
}

func TestKnowledge_StagesWithTheGroupGetTheToolsAndTheirCaller(t *testing.T) {
	srv := cmtest.NewKnowledge(t)
	srv.Answer("get_knowledge_context", cmtest.AgreedRule)

	script := calling(knowledgeScript(), "intake", func(ctx context.Context, in stage.Input) error {
		_, err := callKnowledge(ctx, in, "cm_get_knowledge_context", map[string]any{"project": "alpha", "task": "the need"})

		return err
	})
	c, _, runner, rec, sessions := newKnowledgePipeline(t, script, srv)

	drive(t, c, "BP1")

	require.True(t, rec.has(EventSignOff))
	assert.Subset(t, toolNames(inputsOf(runner, "intake")[0]), knowledgeNames)
	assert.NotContains(t, toolNames(inputsOf(runner, "decomposition")[0]), "cm_get_knowledge_context")
	assert.Subset(t, toolNames(inputsOf(runner, "readiness")[0]), knowledgeNames)

	calls := srv.Calls("get_knowledge_context")
	require.NotEmpty(t, calls)
	assert.Equal(t, "blueprint BP1 stage intake", calls[0]["caller"])

	bearers := srv.Bearers()
	require.NotEmpty(t, bearers)

	for _, b := range bearers {
		assert.Equal(t, "Bearer "+testMCPKey, b)
	}

	opened, closed := sessions.counts()
	assert.Positive(t, opened)
	assert.Equal(t, opened, closed)
}

func TestKnowledge_AResultWithItemsIsOneSourcePerProvider(t *testing.T) {
	srv := cmtest.NewKnowledge(t)
	srv.Answer("get_knowledge_context", cmtest.AgreedRule)

	var (
		mu      sync.Mutex
		results = map[string][]string{}
	)

	record := func(key, text string) {
		mu.Lock()
		defer mu.Unlock()

		results[key] = append(results[key], text)
	}

	script := calling(knowledgeScript(), "intake", func(ctx context.Context, in stage.Input) error {
		text, err := callKnowledge(ctx, in, "cm_get_knowledge_context", map[string]any{"project": "alpha", "task": "the need"})
		if err != nil {
			return err
		}

		record("context", text)

		text, err = callKnowledge(ctx, in, "cm_query_knowledge", map[string]any{"project": "alpha", "query": "prices"})
		if err != nil {
			return err
		}

		record("query", text)

		return nil
	})
	script = calling(script, "readiness", func(ctx context.Context, in stage.Input) error {
		text, err := callKnowledge(ctx, in, "cm_get_knowledge_context", map[string]any{"project": "alpha", "task": "the cards"})
		if err != nil {
			return err
		}

		record("context", text)

		return nil
	})
	c, st, _, _, _ := newKnowledgePipeline(t, script, srv)

	drive(t, c, "BP1")

	sources, err := st.ListSources(context.Background(), "BP1")
	require.NoError(t, err)

	var found []store.Source

	for _, s := range sources {
		if s.Kind == "knowledge" {
			found = append(found, s)
		}
	}

	require.Len(t, found, 1)
	assert.Equal(t, "knowledge:intent", found[0].Identity)
	assert.Equal(t, "intake", found[0].FirstUsedBy)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, results["context"], 2)

	for _, text := range results["context"] {
		assert.True(t, strings.HasPrefix(text, "["+found[0].Tag()+"] "), "a result with items leads with its tag: %q", text)
	}

	require.Len(t, results["query"], 1)
	assert.True(t, strings.HasPrefix(results["query"][0], "{"), "a result without items is not tagged: %q", results["query"][0])
}

func TestKnowledge_ACMWithoutTheToolsRunsTheStagesAsBefore(t *testing.T) {
	srvA := cmtest.NewKnowledge(t, "get_card")
	cA, _, runnerA, recA, sessionsA := newKnowledgePipeline(t, knowledgeScript(), srvA)
	drive(t, cA, "BP1")

	cB, _, runnerB, recB, _ := newKnowledgePipeline(t, knowledgeScript(), nil)
	drive(t, cB, "BP1")

	require.True(t, recA.has(EventSignOff))
	require.True(t, recB.has(EventSignOff))

	for _, id := range []string{"intake", "intake.review", "readiness"} {
		a, b := inputsOf(runnerA, id), inputsOf(runnerB, id)
		require.NotEmpty(t, a, id)
		require.Len(t, a, len(b), id)
		assert.Equal(t, toolNames(b[0]), toolNames(a[0]), "%s: tools", id)
		assert.Equal(t, b[0].Task, a[0].Task, "%s: task", id)
	}

	opened, closed := sessionsA.counts()
	assert.Positive(t, opened)
	assert.Equal(t, opened, closed)
}

func TestKnowledge_ASessionThatFailsToOpenLeavesTheRunWithoutIt(t *testing.T) {
	srv := cmtest.NewKnowledge(t)
	c, _, runner, rec, _ := newKnowledgePipeline(t, knowledgeScript(), srv)
	c.opts.KnowledgeTools = func(context.Context, string, cm.Knowledge) ([]tools.Tool, func(), error) {
		return nil, nil, errors.New("cm: connect to mcp endpoint: connection refused")
	}

	drive(t, c, "BP1")

	require.NotEmpty(t, inputsOf(runner, "intake"))

	for _, name := range toolNames(inputsOf(runner, "intake")[0]) {
		assert.NotContains(t, knowledgeNames, name)
	}

	assert.NotEmpty(t, inputsOf(runner, "readiness"))
	assert.True(t, rec.has(EventSignOff))
	assert.False(t, rec.has(EventError))
}

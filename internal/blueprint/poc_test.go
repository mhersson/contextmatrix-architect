package blueprint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// listAlphaPOC is listAlpha with alpha flagged a proof of concept.
func listAlphaPOC(context.Context, string) ([]cm.Project, error) {
	return []cm.Project{{Name: "alpha", Repo: "https://example.com/alpha.git", Verify: &cm.Verify{Command: "make test"}, POC: true}}, nil
}

// runTraced drives BP1 on the traced pack, with projects as ContextMatrix's
// project list, past its design gate to the sign-off, and returns the
// runner that saw every run.
func runTraced(t *testing.T, projects func(context.Context, string) ([]cm.Project, error), script stageScript) *fakeRunner {
	t.Helper()

	c, _, runner, _, _ := newDialog(t, script)
	c.opts.Pack = tracedPack(t)
	c.opts.Projects = projects

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	return runner
}

func TestPOCProjects(t *testing.T) {
	c, st, _, _, _ := newDialog(t, dialogScript())
	ctx := context.Background()

	in := startInput("BP1")
	in.Projects = []string{"gamma", "beta", "alpha"}
	require.NoError(t, c.Start(ctx, in))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	var keys []string

	flags := func(_ context.Context, key string) ([]cm.Project, error) {
		keys = append(keys, key)

		return []cm.Project{{Name: "alpha", POC: true}, {Name: "beta"}, {Name: "gamma", POC: true}, {Name: "zeta", POC: true}}, nil
	}

	step := func(key string) pipelineStep {
		s, ok := c.stepAt(key)
		require.True(t, ok, key)

		return s
	}

	// No test pack has a stage that checks a readiness verdict without
	// looping; the predicate reads only checks and the loop.
	verdict := pipelineStep{Stage: pack.Stage{ID: "verdict", Checks: []string{pack.CheckReadiness}}, ID: "verdict"}
	flagged := []string{"gamma", "alpha"}

	tests := []struct {
		name string
		st   pipelineStep
		want []string
	}{
		{name: "intake", st: step("intake")},
		{name: "slicing", st: step("slicing")},
		{name: "solution design", st: step("solution_design")},
		{name: "grounding", st: step("grounding")},
		{name: "the stage that checks criteria", st: step("acceptance"), want: flagged},
		{name: "the stage that checks cards", st: step("decomposition"), want: flagged},
		{name: "the stage that loops to the cards", st: step("readiness"), want: flagged},
		{name: "a stage that checks a readiness verdict", st: verdict, want: flagged},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys = nil
			c.opts.Projects = flags

			got, err := c.pocProjects(ctx, sessionOf(t, c, "BP1"), bp, tt.st)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got, "the flagged targets in target order; zeta is no target")

			if tt.want == nil {
				assert.Empty(t, keys, "a run that shapes no card never reads the flags")
			} else {
				assert.Equal(t, []string{testMCPKey}, keys, "read again for the run, with the session's key")
			}
		})
	}

	t.Run("a read that fails", func(t *testing.T) {
		errDown := errors.New("cm: list_projects: remote unreachable")
		c.opts.Projects = func(context.Context, string) ([]cm.Project, error) { return nil, errDown }

		_, err := c.pocProjects(ctx, sessionOf(t, c, "BP1"), bp, step("acceptance"))
		require.ErrorIs(t, err, errDown, "the run's error, never a guess")

		got, err := c.pocProjects(ctx, sessionOf(t, c, "BP1"), bp, step("intake"))
		require.NoError(t, err, "a run that reads nothing cannot fail on it")
		assert.Nil(t, got)
	})

	t.Run("no project lister", func(t *testing.T) {
		c.opts.Projects = nil

		got, err := c.pocProjects(ctx, sessionOf(t, c, "BP1"), bp, step("acceptance"))
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

func TestWritePOC(t *testing.T) {
	var none strings.Builder

	writePOC(&none, nil)
	assert.Empty(t, none.String(), "no POC target, no section: the task reads as before")

	var b strings.Builder

	writePOC(&b, []string{"gamma", "alpha"})

	gamma, alpha := strings.Index(b.String(), "gamma"), strings.Index(b.String(), "alpha")
	require.NotEqual(t, -1, gamma)
	require.NotEqual(t, -1, alpha)
	assert.Less(t, gamma, alpha, "named in the order given, the targets' order")
}

func TestPOC_OnlyTheRunsThatShapeCardsAreTold(t *testing.T) {
	plain := runTraced(t, listAlpha, dialogScript())
	poc := runTraced(t, listAlphaPOC, dialogScript())

	tests := []struct {
		key  string
		told bool
	}{
		{key: "intake"},
		{key: "slicing"},
		{key: "solution_design"},
		{key: "solution_design.review"},
		{key: "grounding"},
		{key: "acceptance", told: true},
		{key: "acceptance.review", told: true},
		{key: "decomposition", told: true},
		{key: "readiness", told: true},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			before, after := inputsOf(plain, tt.key), inputsOf(poc, tt.key)
			require.NotEmpty(t, before)
			require.Len(t, after, len(before), "the flag adds or drops no run")

			if tt.told {
				var want strings.Builder

				writePOC(&want, []string{"alpha"})
				assert.Equal(t, 1, strings.Count(after[0].Task, want.String()), "told which targets are proofs of concept, once")
				assert.Equal(t, before[0].Task, strings.Replace(after[0].Task, want.String(), "", 1), "and nothing else differs")
			} else {
				assert.Equal(t, before[0].Task, after[0].Task, "designed as for any project")
			}
		})
	}
}

func TestPOC_TheResponderIsToldTheRunsPOCTargets(t *testing.T) {
	script := dialogScript()
	script["acceptance"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if _, err := in.Interviewer.Ask(ctx, stage.Round{Questions: []stage.RoundQuestion{roundQ("Scope", "How much to cover?", "main path", "everything")}}); err != nil {
			return stage.Output{}, err
		}

		return done("# Acceptance", goodAcceptance()), nil
	}
	script["acceptance.responder"] = returns("# Answer", map[string]any{"answer": "main path", "reasoning": "r", "grounded": false})

	plain := runTraced(t, listAlpha, script)
	poc := runTraced(t, listAlphaPOC, script)

	before, after := inputsOf(plain, "acceptance.responder"), inputsOf(poc, "acceptance.responder")
	require.Len(t, before, 1)
	require.Len(t, after, 1)

	var want strings.Builder

	writePOC(&want, []string{"alpha"})
	assert.Equal(t, 1, strings.Count(after[0].Task, want.String()), "told which targets are proofs of concept, once")
	assert.Equal(t, before[0].Task, strings.Replace(after[0].Task, want.String(), "", 1), "and nothing else differs")
}

func TestPOC_AnUnreadableFlagFailsTheRunThatNeedsIt(t *testing.T) {
	var broken atomic.Bool

	script := dialogScript()
	script["grounding"] = func(context.Context, stage.Input) (stage.Output, error) {
		// ContextMatrix goes away after the last run that reads no flag.
		broken.Store(true)

		return done("# Grounding", nil), nil
	}

	c, st, runner, rec, _ := newDialog(t, script)
	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		if broken.Load() {
			return nil, fmt.Errorf("cm: 401 for key %s", key)
		}

		return listAlphaPOC(ctx, key)
	}

	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	assert.Empty(t, inputsOf(runner, "acceptance"), "acceptance never runs on a guess about its projects")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Equal(t, "prepare", ev.Payload["reason"])
	assert.Equal(t, "acceptance", ev.Payload["stage"])
	assert.Contains(t, ev.Payload["message"], "[REDACTED]", "the key never reaches the record")

	runs, err := st.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.NotEmpty(t, runs)
	assert.Equal(t, "acceptance", runs[len(runs)-1].Stage)
	assert.Equal(t, store.RunFailed, runs[len(runs)-1].Status)

	broken.Store(false)
	require.NoError(t, c.Message(ctx, "BP1", "again", "m3"))
	c.Wait("BP1")

	assert.NotEmpty(t, inputsOf(runner, "acceptance"), "the next message reads the flags again and runs")
}

func TestPOC_AnUnreadableFlagStopsTheTestersReview(t *testing.T) {
	var broken atomic.Bool

	script := dialogScript()
	script["acceptance"] = func(context.Context, stage.Input) (stage.Output, error) {
		// ContextMatrix goes away after acceptance read the flags.
		broken.Store(true)

		return done("# Acceptance", goodAcceptance()), nil
	}

	c, st, runner, rec, _ := newDialog(t, script)
	c.opts.Projects = func(ctx context.Context, key string) ([]cm.Project, error) {
		if broken.Load() {
			return nil, fmt.Errorf("cm: 401 for key %s", key)
		}

		return listAlphaPOC(ctx, key)
	}

	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	require.NotEmpty(t, inputsOf(runner, "acceptance"))
	assert.Empty(t, inputsOf(runner, "acceptance.review"), "the tester never judges on a guess")
	assert.Empty(t, inputsOf(runner, "decomposition"), "and nothing after it runs")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "[REDACTED]", "the key never reaches the record")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateFailed), bp.State)

	broken.Store(false)
	require.NoError(t, c.Message(ctx, "BP1", "again", "m3"))
	c.Wait("BP1")

	assert.NotEmpty(t, inputsOf(runner, "acceptance.review"), "the next message reads the flags again and the tester's review runs")
}

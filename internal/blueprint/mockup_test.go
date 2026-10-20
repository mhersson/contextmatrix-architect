package blueprint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func mockupPack(t *testing.T) *pack.Pack {
	t.Helper()

	pk, err := pack.LoadDir("testdata/mockup")
	require.NoError(t, err)

	return pk
}

// newMockup is newDialog on the mockup test pack.
func newMockup(t *testing.T, script stageScript) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeWorkspace) {
	t.Helper()

	c, st, runner, rec, ws := newPipeline(t, script)
	c.opts.Pack = mockupPack(t)

	return c, st, runner, rec, ws
}

// withScreen has intake state screen, and no re-design with a screen; nil
// leaves the keys out, as an intake stored before mockups did.
func withScreen(script stageScript, screen any) stageScript {
	data := map[string]any{}
	if screen != nil {
		data["screen"] = screen
	}

	if screen == true {
		data["redesign"] = false
	}

	script["intake"] = returns("# Need", data)

	return script
}

func TestMockup_ANeedWithoutAScreenNeverShowsTheStep(t *testing.T) {
	for name, screen := range map[string]any{"no screen": false, "an intake from before screens": nil} {
		t.Run(name, func(t *testing.T) {
			c, st, runner, rec, _ := newMockup(t, withScreen(dialogScript(), screen))
			drive(t, c, "BP1")

			assert.NotContains(t, stagesRun(runner), "mockup")
			assert.Contains(t, stagesRun(runner), "slicing")
			assert.Equal(t, "skipped", stepOf(t, view(t, c), "mockup").Status)

			gates, err := st.ListGates(context.Background(), "BP1", "mockup")
			require.NoError(t, err)
			assert.Empty(t, gates, "the collapse writes no fold row")

			for _, e := range rec.events() {
				assert.NotEqual(t, "mockup", e.Payload["stage"], "without a word: %s", e.Kind)
			}

			plans := rec.all(EventGatePlan)
			require.NotEmpty(t, plans)

			for _, plan := range plans {
				entries, _ := plan.Payload["stages"].([]any)
				require.NotEmpty(t, entries)

				for _, e := range entries {
					assert.NotEqual(t, "mockup", e.(map[string]any)["stage"], "the plan lists the collapsed step")
				}
			}

			for i := range runner.inputs {
				assert.NotContains(t, runner.input(i).Task, "## mockup", "%s is told of a mockup", runner.input(i).Stage)
			}
		})
	}
}

func TestMockup_AScreenPutsTheStepInThePipeline(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = func(context.Context, stage.Input) (stage.Output, error) {
		return stage.Output{}, errors.New("model down")
	}

	// auto mode takes no offer, so the step runs at once.
	c, _, runner, _, _ := newMockup(t, script)
	driveIn(t, c, autoStart("BP1"))

	assert.Contains(t, stagesRun(runner), "mockup")
	assert.NotContains(t, stagesRun(runner), "slicing")
	assert.Equal(t, "failed", stepOf(t, view(t, c), "mockup").Status)
}

func TestMockup_IntakeMustSayWhetherThereIsAScreen(t *testing.T) {
	script := withScreen(dialogScript(), false)
	script["intake"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if err := in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"screen": "yes"}}); err == nil || !strings.Contains(err.Error(), "screen_shape") {
			return stage.Output{}, fmt.Errorf("a screen that is not a boolean passed: %v", err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"screen": true}}); err == nil || !strings.Contains(err.Error(), "redesign_shape") {
			return stage.Output{}, fmt.Errorf("a screen without the re-design flag passed: %v", err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Need", Data: map[string]any{"screen": false}}); err != nil {
			return stage.Output{}, fmt.Errorf("no screen needs no re-design flag: %w", err)
		}

		return done("# Need", map[string]any{"screen": false}), nil
	}

	c, _, runner, _, _ := newMockup(t, script)
	drive(t, c, "BP1")

	assert.Contains(t, stagesRun(runner), "slicing", "intake's run went on after the refusal")
}

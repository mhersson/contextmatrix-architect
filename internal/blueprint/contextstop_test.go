package blueprint

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-harness/harness"
	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func lastError(t *testing.T, rec *recorder) Event {
	t.Helper()

	errs := rec.all(EventError)
	require.NotEmpty(t, errs, "an error was posted")

	return errs[len(errs)-1]
}

// refusedWith fails a run with err, as the runner does before any call.
func refusedWith(reason string, err error) func(context.Context, stage.Input) (stage.Output, error) {
	return func(_ context.Context, in stage.Input) (stage.Output, error) {
		return stage.Output{Result: harness.Result{Reason: reason}}, fmt.Errorf("stage %s: %w", in.Stage, err)
	}
}

func TestContextStop_ATaskTooBigToStartNamesItsBiggestInputs(t *testing.T) {
	tests := []struct {
		name, refused, kind, biggest string
	}{
		{"a stage run", "shape", store.RunKindStage, "intake"},
		{"a reviewer run", "shape.review", store.RunKindReviewer, "shape"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			script := gatedScript()
			script["intake"] = returns("# Need\n\n"+strings.Repeat("the need ", 100), map[string]any{"slices": 1})
			script["shape"] = returns("# Shape\n\n"+strings.Repeat("the shape ", 1000), nil)
			script[tt.refused] = refusedWith(stage.ReasonTooBig, &stage.TooBigError{Estimate: 120_000, Limit: 108_800, Window: 128_000})

			c, st, _, rec := newGated(t, script)
			startGated(t, c, protocol.ArchitectModeDefault)

			stop := lastError(t, rec)
			assert.Equal(t, "shape", stop.Payload["stage"])
			assert.Equal(t, tt.kind, stop.Payload["kind"])
			assert.Equal(t, stage.ReasonTooBig, stop.Payload["reason"])
			assert.Equal(t, 128_000, stop.Payload["window"])
			assert.Equal(t, 108_800, stop.Payload["limit"])
			assert.Equal(t, 120_000, stop.Payload["estimate"])

			parts, _ := stop.Payload["parts"].([]any)
			require.NotEmpty(t, parts)

			biggest, _ := parts[0].(map[string]any)
			assert.Equal(t, tt.biggest, biggest["name"], "the biggest input first")

			rest, _ := stop.Payload["rest_tokens"].(int)

			measured := 0

			for _, p := range parts {
				measured += p.(map[string]any)["tokens"].(int)
			}

			assert.Equal(t, 120_000-measured, rest, "the rest is the estimate less the named parts")

			ctx := context.Background()

			bp, err := st.GetBlueprint(ctx, "BP1")
			require.NoError(t, err)
			assert.Equal(t, string(StateFailed), bp.State)

			if tt.kind == store.RunKindReviewer {
				for _, g := range rec.all(EventGate) {
					assert.NotEqual(t, "shape", g.Payload["stage"], "a refused reviewer opens no gate")
				}

				gates, err := st.ListGates(ctx, "BP1", "shape")
				require.NoError(t, err)
				assert.Empty(t, gates, "and no gate row is stored")
			}

			runs, err := st.ListRuns(ctx, "BP1")
			require.NoError(t, err)

			last := runs[len(runs)-1]
			assert.Equal(t, tt.kind, last.Kind)
			assert.Equal(t, store.RunFailed, last.Status)
			assert.Equal(t, stage.ReasonTooBig, last.Reason)
			assert.Zero(t, last.Usage.CostUSD, "a refused run costs nothing")
		})
	}
}

func TestContextStop_AFullWindowSaysSo(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = refusedWith(stage.ReasonContextLimit, &stage.ContextFullError{Window: 128_000, Limit: 108_800})

	c, st, _, rec := newGated(t, script)
	startGated(t, c, protocol.ArchitectModeDefault)

	stop := lastError(t, rec)
	assert.Equal(t, "shape", stop.Payload["stage"])
	assert.Equal(t, store.RunKindReviewer, stop.Payload["kind"])
	assert.Equal(t, stage.ReasonContextLimit, stop.Payload["reason"])
	assert.Equal(t, 128_000, stop.Payload["window"])
	assert.Equal(t, 108_800, stop.Payload["limit"])
	assert.NotContains(t, stop.Payload, "estimate")

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateFailed), bp.State)
}

func TestContextStop_ARefusalNamesThreePartsAndTheRestAddsUp(t *testing.T) {
	script := designsWithContracts()
	script["decomposition@SL2"] = refusedWith(stage.ReasonTooBig, &stage.TooBigError{Estimate: 120_000, Limit: 108_800, Window: 128_000})

	c, _, _, rec := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))

	stop := lastError(t, rec)
	require.Equal(t, "decomposition@SL2", stop.Payload["stage"])

	parts, _ := stop.Payload["parts"].([]any)
	require.Len(t, parts, shownParts, "only the biggest inputs are named")

	named := 0

	for _, p := range parts {
		named += p.(map[string]any)["tokens"].(int)
	}

	rest, _ := stop.Payload["rest_tokens"].(int)
	assert.Equal(t, 120_000, named+rest, "the named parts and the rest make the estimate")
}

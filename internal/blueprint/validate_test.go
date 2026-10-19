package blueprint

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

func TestFatalValidator(t *testing.T) {
	ctx := context.Background()
	fatal := problem("slicing", "no_slices", "slicing needs at least one slice")
	other := problem("SL1", "slice_title", "a slice needs a title")

	tests := []struct {
		name  string
		third []string
		// want is what the third submit is refused for good over; nil when
		// it is accepted.
		want []string
	}{
		{name: "a fatal problem is refused for good", third: []string{fatal}, want: []string{fatal}},
		{name: "only the fatal problems are named", third: []string{other, fatal}, want: []string{fatal}},
		{name: "a message quoting a fatal check is no fatal problem", third: []string{problem("SL1", "basis", `basis "x: no_slices: y" is unknown`)}},
		{name: "other problems are accepted and kept", third: []string{other}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submits := 0

			v, state := fatalValidator("slices", []string{"shape", "no_slices"}, func(context.Context, stage.Artifact) ([]string, error) {
				submits++

				if submits < maxContractRounds {
					return []string{fatal}, nil
				}

				return tt.third, nil
			})

			var refused *stage.FatalCheckError

			for range maxContractRounds - 1 {
				err := v(ctx, stage.Artifact{Body: "# Slices"})
				require.Error(t, err)
				assert.NotErrorAs(t, err, &refused, "a submit before the last is refused as any check refuses")
			}

			err := v(ctx, stage.Artifact{Body: "# Slices"})

			if tt.want == nil {
				require.NoError(t, err)
				assert.Equal(t, tt.third, state.last(), "the accepted submit keeps its problems")

				return
			}

			require.ErrorAs(t, err, &refused)
			assert.Equal(t, tt.want, refused.Problems)
		})
	}

	t.Run("a check that cannot judge spends no round", func(t *testing.T) {
		calls := 0

		v, _ := fatalValidator("slices", []string{"no_slices"}, func(context.Context, stage.Artifact) ([]string, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("store closed")
			}

			return []string{fatal}, nil
		})

		require.Error(t, v(ctx, stage.Artifact{}))

		var refused *stage.FatalCheckError

		for range maxContractRounds - 1 {
			require.NotErrorAs(t, v(ctx, stage.Artifact{}), &refused)
		}

		require.ErrorAs(t, v(ctx, stage.Artifact{}), &refused, "the third judged submit is the last")
	})
}

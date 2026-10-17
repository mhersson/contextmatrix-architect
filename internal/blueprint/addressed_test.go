package blueprint

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func entry(finding any, note any) map[string]any {
	e := map[string]any{"finding": finding}
	if note != nil {
		e["note"] = note
	}

	return e
}

func TestAddressedProblems(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want []string // substrings, one per problem; nil means none
	}{
		{name: "no list", data: map[string]any{"slices": []any{}}},
		{name: "a null list", data: map[string]any{"addressed": nil}},
		{name: "findings the round was given", data: map[string]any{"addressed": []any{entry(1.0, "named the owner"), entry(3.0, "split it")}}},
		{name: "a note may be left out", data: map[string]any{"addressed": []any{entry(2.0, nil)}}},
		{name: "a finding past the last", data: map[string]any{"addressed": []any{entry(4.0, "x")}}, want: []string{"finding 4 does not exist"}},
		{name: "finding zero", data: map[string]any{"addressed": []any{entry(0.0, "x")}}, want: []string{"finding 0 does not exist"}},
		{name: "a huge number", data: map[string]any{"addressed": []any{entry(1e30, "x")}}, want: []string{"addressed_unknown"}},
		{name: "a fraction", data: map[string]any{"addressed": []any{entry(1.5, "x")}}, want: []string{"addressed_shape"}},
		{name: "a number as text", data: map[string]any{"addressed": []any{entry("1", "x")}}, want: []string{"addressed_shape"}},
		{name: "an entry that is not an object", data: map[string]any{"addressed": []any{"finding 1"}}, want: []string{"addressed_shape"}},
		{name: "a note that is not text", data: map[string]any{"addressed": []any{entry(1.0, 7.0)}}, want: []string{"addressed_shape"}},
		{name: "not a list", data: map[string]any{"addressed": "all of them"}, want: []string{"addressed_shape"}},
		{name: "each bad entry is its own problem", data: map[string]any{"addressed": []any{entry(9.0, "x"), entry(1.0, "ok"), "x"}}, want: []string{"addressed[1]", "addressed[3]"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := addressedProblems(tt.data, 3)

			require.Len(t, got, len(tt.want))

			for i, w := range tt.want {
				assert.Contains(t, got[i], w)
			}
		})
	}
}

func TestAddressedOf(t *testing.T) {
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		require.NoError(t, err)

		return b
	}

	tests := []struct {
		name string
		data json.RawMessage
		want []addressed
	}{
		{name: "no data"},
		{name: "no list", data: raw(map[string]any{"slices": []any{}})},
		{name: "a list", data: raw(map[string]any{"addressed": []any{entry(2, "split it"), entry(1, nil)}}), want: []addressed{{finding: 2, note: "split it"}, {finding: 1}}},
		{name: "entries that do not read are skipped", data: raw(map[string]any{"addressed": []any{"x", entry(0, "zero"), entry(1.5, "half"), entry(3, "kept")}}), want: []addressed{{finding: 3, note: "kept"}}},
		{name: "not a list", data: raw(map[string]any{"addressed": "all"})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, addressedOf(store.Artifact{Data: tt.data}))
		})
	}
}

// A builder round after a review checks the findings its submit says it
// addressed; a run that follows no review checks no such list.
func TestReview_ARevisionNamesTheFindingsItAddressed(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner", "split it"))

	var (
		calls      atomic.Int32
		firstCheck atomic.Bool
		refused    atomic.Value // error
	)

	good := map[string]any{"addressed": []any{entry(2.0, "split into two")}}

	script["shape"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			firstCheck.Store(in.Validate != nil)

			return done("# Shape", nil), nil
		}

		bad := map[string]any{"addressed": []any{entry(3.0, "x")}}
		if err := in.Validate(ctx, stage.Artifact{Body: "# Shape 2", Data: bad}); err != nil {
			refused.Store(err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Shape 2", Data: good}); err != nil {
			return stage.Output{}, err
		}

		return done("# Shape 2", good), nil
	}

	c, st, _, _ := newGated(t, script)

	startGated(t, c, protocol.ArchitectModeDefault)

	assert.False(t, firstCheck.Load(), "the first round follows no review and checks no addressed list")

	err, _ := refused.Load().(error)
	require.Error(t, err, "a finding the round was not given is refused")
	assert.Contains(t, err.Error(), "finding 3 does not exist")

	art, err := st.LatestArtifact(context.Background(), "BP1", "shape")
	require.NoError(t, err)
	assert.Equal(t, 2, art.Version)
	assert.Equal(t, []addressed{{finding: 2, note: "split into two"}}, addressedOf(art))
	assert.NotContains(t, string(art.Data), "check_problems", "an accepted list leaves no problems")
}

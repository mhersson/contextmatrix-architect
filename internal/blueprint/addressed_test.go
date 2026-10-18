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
	given := []string{"R1", "R2", "R4"}

	tests := []struct {
		name string
		data map[string]any
		want []string // substrings, one per problem; nil means none
	}{
		{name: "no list", data: map[string]any{"slices": []any{}}},
		{name: "a null list", data: map[string]any{"addressed": nil}},
		{name: "findings the round was given", data: map[string]any{"addressed": []any{entry("R1", "named the owner"), entry("R4", "split it")}}},
		{name: "a note may be left out", data: map[string]any{"addressed": []any{entry("R2", nil)}}},
		{name: "a finding the round was not given", data: map[string]any{"addressed": []any{entry("R3", "x")}}, want: []string{"finding R3 was not given"}},
		{name: "a number, as before findings had ids", data: map[string]any{"addressed": []any{entry(1.0, "x")}}, want: []string{"addressed_shape"}},
		{name: "a blank id", data: map[string]any{"addressed": []any{entry(" ", "x")}}, want: []string{"addressed_shape"}},
		{name: "an entry that is not an object", data: map[string]any{"addressed": []any{"R1"}}, want: []string{"addressed_shape"}},
		{name: "a note that is not text", data: map[string]any{"addressed": []any{entry("R1", 7.0)}}, want: []string{"addressed_shape"}},
		{name: "not a list", data: map[string]any{"addressed": "all of them"}, want: []string{"addressed_shape"}},
		{name: "each bad entry is its own problem", data: map[string]any{"addressed": []any{entry("R9", "x"), entry("R1", "ok"), "x"}}, want: []string{"addressed[1]", "addressed[3]"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := addressedProblems(tt.data, given)

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
		{name: "a list", data: raw(map[string]any{"addressed": []any{entry("R2", "split it"), entry("R1", nil)}}), want: []addressed{{finding: "R2", note: "split it"}, {finding: "R1"}}},
		{name: "a revision stored before findings had ids", data: raw(map[string]any{"addressed": []any{entry(2, "split it")}}), want: []addressed{{finding: "R2", note: "split it"}}},
		{name: "entries that do not read are skipped", data: raw(map[string]any{"addressed": []any{"x", entry(0, "zero"), entry(1.5, "half"), entry("", "blank"), entry("Q1", "a question"), entry("R3", "kept")}}), want: []addressed{{finding: "R3", note: "kept"}}},
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
	script["shape.review"] = verdicts(scored(4, "name the owner", "split it"), fixed(5, "R1", "R2"))

	var (
		calls      atomic.Int32
		firstCheck atomic.Bool
		refused    atomic.Value // error
	)

	good := map[string]any{"addressed": []any{entry("R2", "split into two")}}

	script["shape"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		if calls.Add(1) == 1 {
			firstCheck.Store(in.Validate != nil)

			return done("# Shape", nil), nil
		}

		bad := map[string]any{"addressed": []any{entry("R3", "x")}}
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
	assert.Contains(t, err.Error(), "finding R3 was not given")

	art, err := st.LatestArtifact(context.Background(), "BP1", "shape")
	require.NoError(t, err)
	assert.Equal(t, 2, art.Version)
	assert.Equal(t, []addressed{{finding: "R2", note: "split into two"}}, addressedOf(art))
	assert.NotContains(t, string(art.Data), "check_problems", "an accepted list leaves no problems")
}

// artifactsOf returns the artifact events of stageID, oldest first.
func artifactsOf(rec *recorder, stageID string) []Event {
	var out []Event

	for _, e := range rec.all(EventArtifact) {
		if e.Payload["stage"] == stageID {
			out = append(out, e)
		}
	}

	return out
}

func TestArtifactPayload_NamesWhoDrewItAndWhatFor(t *testing.T) {
	script := gatedScript()
	script["shape.review"] = verdicts(scored(4, "name the owner", "split it"), fixed(5, "R1", "R2"))

	c, _, _, rec := newGated(t, script)

	startGated(t, c, protocol.ArchitectModeDefault)

	shapes := artifactsOf(rec, "shape")
	require.Len(t, shapes, 2)
	assert.Equal(t, "product_owner", shapes[0].Payload["persona"])
	assert.Equal(t, 1, shapes[0].Payload["round"])
	assert.Equal(t, 0, shapes[0].Payload["revised_for"])
	assert.Equal(t, 2, shapes[1].Payload["round"])
	assert.Equal(t, 2, shapes[1].Payload["revised_for"], "the revision was given two findings")
	assert.NotContains(t, shapes[1].Payload, "loop_round", "shape is no loop stage")
}

func TestArtifactPayload_ALoopStageCarriesItsRound(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	c.opts.ConnectCM = newFakeCM().connect

	driveIn(t, c, startInput("BP1"))

	ready := artifactsOf(rec, "readiness")
	require.NotEmpty(t, ready)
	assert.Equal(t, 1, ready[0].Payload["loop_round"])
	assert.Equal(t, 3, ready[0].Payload["max_rounds"])
	assert.NotContains(t, artifactsOf(rec, "decomposition")[0].Payload, "accepted_round", "cards that passed")
}

func TestArtifactPayload_CardsAcceptedWithFindingsSaySo(t *testing.T) {
	var submits int

	script := happyScript()
	script["decomposition"] = keepSubmitting(map[string]any{"cards": []any{}, "order": []any{}}, "no cards", &submits)

	c, _, _, rec, _ := newPipeline(t, script)

	driveIn(t, c, startInput("BP1"))

	cards := artifactsOf(rec, "decomposition")
	require.Len(t, cards, 1)
	assert.Equal(t, 3, cards[0].Payload["accepted_round"])
}

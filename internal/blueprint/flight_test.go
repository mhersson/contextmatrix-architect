package blueprint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

func TestFlight_Count(t *testing.T) {
	price := testPrice()

	tests := []struct {
		name   string
		lines  []string
		turns  int
		calls  int
		tokens int64
		spent  float64
	}{
		{name: "turns are the latest request's", lines: []string{`{"kind":"model_request","data":{"turn":1}}`, `{"kind":"model_request","data":{"turn":3}}`}, turns: 3},
		{name: "tool calls are counted", lines: []string{`{"kind":"tool_call","data":{}}`, `{"kind":"tool_call","data":{}}`}, calls: 2},
		{
			name:   "usage adds tokens and its priced cost",
			lines:  []string{`{"kind":"usage","data":{"prompt_tokens":100000,"completion_tokens":10000,"cache_read_tokens":0,"cache_creation_tokens":0,"cost_usd":0}}`},
			tokens: 110000, spent: 0.20,
		},
		{
			name:   "a reported cost above the priced one wins",
			lines:  []string{`{"kind":"usage","data":{"prompt_tokens":1000,"completion_tokens":0,"cost_usd":0.5}}`},
			tokens: 1000, spent: 0.5,
		},
		{name: "other lines and garbage count nothing", lines: []string{`{"kind":"thinking","data":{"turn":9}}`, `not json`}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &flight{price: price}
			for _, l := range tt.lines {
				f.count([]byte(l))
			}

			v := f.view()
			assert.Equal(t, tt.turns, v.turns)
			assert.Equal(t, tt.calls, v.toolCalls)
			assert.Equal(t, tt.tokens, v.tokens)
			assert.InDelta(t, tt.spent, v.spent, 1e-9)
		})
	}
}

func TestFlight_AChargeCountsOnAnUncappedRun(t *testing.T) {
	f := &flight{}
	s := &harnessSink{flight: f}

	s.charge(0.3)

	assert.InDelta(t, 0.3, f.view().charged, 1e-9)
}

func TestFlight_ARunIsInFlightUntilItEnds(t *testing.T) {
	release := make(chan struct{})
	counted := make(chan struct{}, 1)

	c, _, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		for _, l := range []string{
			`{"kind":"model_request","data":{"turn":1}}`,
			`{"kind":"tool_call","data":{"name":"read"}}`,
			`{"kind":"usage","data":{"prompt_tokens":100000,"completion_tokens":10000,"cost_usd":0}}`,
			`{"kind":"model_request","data":{"turn":2}}`,
		} {
			_, _ = in.Events.Write([]byte(l + "\n"))
		}

		signal(counted)
		parkUntil(ctx, release)

		return artifactOutput("# Need", in), nil
	})
	closeRelease := closer(t, release)
	ctx := context.Background()

	in := startInput("BP1")
	in.Price = testPrice()
	require.NoError(t, c.Start(ctx, in))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	receive(t, counted, "the harness lines")

	flights := c.inFlight("BP1")
	require.Len(t, flights, 1)

	f := flights[0]
	assert.Equal(t, "intake", f.stage)
	assert.Equal(t, "stage", f.kind)
	assert.Equal(t, "product_owner", f.persona)
	assert.Equal(t, "session/model", f.model)
	assert.Equal(t, 1, f.round)
	assert.True(t, f.priced)
	assert.Equal(t, 2, f.turns)
	assert.Equal(t, 1, f.toolCalls)
	assert.Equal(t, int64(110000), f.tokens)
	assert.InDelta(t, 0.20, f.spent, 1e-9)

	closeRelease()
	c.Wait("BP1")

	assert.Empty(t, c.inFlight("BP1"), "a run that ended is no longer in flight")
}

func TestSetState_IsStoredButNeverPublished(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, startInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")

	stored, err := st.ListEventsOfKind(ctx, "BP1", EventState)
	require.NoError(t, err)

	var states []string

	for _, e := range stored {
		var p struct {
			State string `json:"state"`
		}

		require.NoError(t, json.Unmarshal(e.Payload, &p))
		states = append(states, p.State)
	}

	assert.Contains(t, states, string(StateRunning))
	assert.Contains(t, states, string(StateIdle))
	assert.False(t, rec.has(EventState), "the panel never sees a state row")

	// The export writes every row, state rows included.
	c.opts.ExportDir = t.TempDir()
	require.NoError(t, c.Export(ctx, "BP1", "m2"))

	exported, ok := rec.first(EventExported)
	require.True(t, ok)

	path, _ := exported.Payload["path"].(string)
	lines, err := os.ReadFile(filepath.Join(path, "events.jsonl"))
	require.NoError(t, err)
	assert.Contains(t, string(lines), `"kind":"state"`)
}

package blueprint

import (
	"context"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

func unattendedStart(id string, on bool) StartInput {
	in := startInput(id)
	in.Unattended = on

	return in
}

func TestUnattended_TheFlagIsKeptFromCreationAndServed(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, unattendedStart("BP1", true)))

	created, ok := rec.first(EventCreated)
	require.True(t, ok)
	assert.Equal(t, true, created.Payload["unattended"])

	view, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, view.Unattended)

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, unattendedStart("BP1", false)))
	c.Wait("BP1")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, bp.Unattended, "a later start never switches it off")

	view, err = c.View(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, view.Unattended)
}

func TestUnattended_AReopenNeverSwitchesItOn(t *testing.T) {
	c, st, _, rec := newController(t, artifactRun("# Need"))
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, unattendedStart("BP1", false)))

	created, ok := rec.first(EventCreated)
	require.True(t, ok)
	assert.NotContains(t, created.Payload, "unattended")

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, unattendedStart("BP1", true)))
	c.Wait("BP1")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.False(t, bp.Unattended, "a later start never switches it on")

	view, err := c.View(ctx, "BP1")
	require.NoError(t, err)
	assert.False(t, view.Unattended)
}

func TestUnattended_EveryStageRunGetsTheOverlay(t *testing.T) {
	run := func(t *testing.T, on bool) *fakeRunner {
		t.Helper()

		c, _, runner, _ := newSliced(t, slicedScript())
		in := autoStart("BP1")
		in.Unattended = on
		driveIn(t, c, in)

		return runner
	}

	pk := slicedPack(t)
	require.NotEmpty(t, pk.UnattendedInstructions(), "the sliced test pack ships unattended.md")

	on, off := run(t, true), run(t, false)

	for _, key := range []string{"intake", "decomposition@SL1", "decomposition@SL2", "readiness"} {
		u, d := inputsOf(on, key), inputsOf(off, key)
		require.NotEmpty(t, u, key)
		require.NotEmpty(t, d, key)
		assert.NotEqual(t, d[0].Instructions, u[0].Instructions, key)
	}

	assert.Equal(t, pk.Instructions("readiness"), inputsOf(off, "readiness")[0].Instructions, "off, the instructions are the pack's")

	u, d := inputsOf(on, "slicing.review"), inputsOf(off, "slicing.review")
	require.NotEmpty(t, u)
	require.NotEmpty(t, d)
	assert.Equal(t, d[0].Instructions, u[0].Instructions, "a reviewer run never gets it")
}

func TestSignoff_AnOpenNeedlessStopNeverStopsTheAutoSignoff(t *testing.T) {
	const finding = "Only confirms the parser; D2 does not need it."

	script := twoCardScript()
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		v := readinessFor(true, "D1", "D2")
		v["needless_stops"] = []any{map[string]any{"id": "M1", "text": finding}}

		return done("# Ready", v), nil
	}

	c, _, runner, rec, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	in := unattendedStart("BP1", true)
	in.Mode = protocol.ArchitectModeYolo
	driveIn(t, c, in)

	assert.Equal(t, []string{"decomposition", "readiness", "decomposition", "readiness", "decomposition", "readiness"},
		stepRuns(runner, "decomposition", "readiness"), "every round below the cap sends the cards back")

	so, ok := rec.first(EventSignOffAuto)
	require.True(t, ok, "%v", rec.kinds())
	assert.False(t, rec.has(EventSignOff), "nothing waits for a person")
	assert.True(t, rec.has(EventEmitted))
	assert.Equal(t, []string{}, so.Payload["not_ready"], "a needless stop is no card finding")

	stops, ok := so.Payload["stops"].([]any)
	require.True(t, ok, "%T", so.Payload["stops"])
	require.Len(t, stops, 1)

	stop, ok := stops[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "M1", stop["id"])
	assert.Equal(t, "Check the dashboard", stop["text"])
	assert.Equal(t, finding, stop["needless"])
	assert.NotContains(t, stop, "note", "the entry has none")

	_, books := fake.inputs()
	require.Len(t, books, 1)
	assert.Equal(t, []string{"card", "manual", "card"}, entryTypes(books[0].Entries), "M1 stays a run stop")
}

func TestSignoff_AnUnattendedPayloadListsEveryStopInRunOrder(t *testing.T) {
	data := twoCards()
	data["manual"] = []any{
		map[string]any{"id": "M1", "text": "Approve the vendor contract", "note": "  The parser's vendor needs a signed contract first.  "},
		map[string]any{"id": "M2", "text": "Check the dashboard", "note": " "},
	}
	data["cards"] = append(data["cards"].([]any), map[string]any{
		"id": "D3", "project": "alpha", "title": "Show the parser's output", "type": "feature", "priority": "medium",
		"depends_on": []any{"D2"}, "body": cardBody("Show the parser's output"),
		"where": []any{map[string]any{"path": "internal/x.go", "symbol": "Thing"}},
	})
	data["order"] = []any{"D1", "M1", "D2", "M2", "D3"}

	script := twoCardScript()
	script["decomposition"] = returns("# Cards", data)
	script["readiness"] = returns("# Ready", readinessFor(true, "D1", "D2", "D3"))

	tests := []struct {
		name string
		on   bool
	}{
		{name: "unattended", on: true},
		{name: "not unattended", on: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _, _, rec, _ := newPipeline(t, script)
			driveIn(t, c, unattendedStart("BP1", tt.on))

			so, ok := rec.first(EventSignOff)
			require.True(t, ok)

			if !tt.on {
				assert.NotContains(t, so.Payload, "stops")

				return
			}

			assert.Equal(t, []any{
				map[string]any{"id": "M1", "text": "Approve the vendor contract", "note": "The parser's vendor needs a signed contract first."},
				map[string]any{"id": "M2", "text": "Check the dashboard"},
			}, so.Payload["stops"])
		})
	}
}

func TestSignoff_AnUnattendedSingleCardHasNoStops(t *testing.T) {
	data := goodCards()
	data["manual"] = []any{map[string]any{"id": "M1", "text": "Check the dashboard"}}
	data["order"] = []any{"M1", "D1"}

	script := happyScript()
	script["decomposition"] = returns("# Cards", data)

	c, _, _, rec, _ := newPipeline(t, script)
	driveIn(t, c, unattendedStart("BP1", true))

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []any{}, so.Payload["stops"], "one card has no playbook to stop")
}

// entryTypes lists a playbook's entry types in order.
func entryTypes(entries []cm.PlaybookEntryInput) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Type)
	}

	return out
}

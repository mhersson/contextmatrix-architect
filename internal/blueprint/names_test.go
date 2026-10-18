package blueprint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvents_TheSinkIsToldWhetherSlicesShow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		slicing map[string]any
		want    bool
	}{
		{"two slices", twoSlices(), true},
		{"one slice", oneSlice(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := slicedScript()
			script["slicing"] = returns("# Slices", tc.slicing)

			c, _, _, rec := newSliced(t, script)
			driveIn(t, c, autoStart("BP1"))

			seen := 0

			for _, e := range rec.all(EventArtifact) {
				switch e.Payload["stage"] {
				case "intake":
					assert.False(t, e.Sliced, "before slicing stores a version")
				case "solution_design@SL1":
					assert.Equal(t, tc.want, e.Sliced)

					seen++
				}
			}

			assert.Equal(t, 1, seen)
		})
	}
}

func TestEvents_ACanceledCallStillTellsTheSinkThatSlicesShow(t *testing.T) {
	c, st := newSlicedView(t)
	throughSliced(t, st, "slicing")

	var got []Event

	c.opts.Sink = func(e Event) { got = append(got, e) }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c.newAudit().event(ctx, Event{Kind: EventNote, BlueprintID: viewID, Payload: map[string]any{"text": "a note"}})

	require.Len(t, got, 1)
	assert.True(t, got[0].Sliced)
}

func TestSliced_APackThatRepeatsNothingPerSliceIsNeverSliced(t *testing.T) {
	ctx := context.Background()

	c, st := newViewController(t, "default", 0)
	put(t, st, "slicing", stageData("slicing"))

	assert.False(t, c.sliced(ctx, viewID), "two slices, but the dialog pack has no per-slice stage")
	assert.Empty(t, view(t, c).Slices)

	c.opts.Pack = slicedPack(t)

	assert.True(t, c.sliced(ctx, viewID), "the same two slices on a pack that repeats its stages")
	assert.Len(t, view(t, c).Slices, 2)
}

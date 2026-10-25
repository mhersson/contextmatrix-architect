package blueprint

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

package blueprint

import (
	"context"
	"errors"
	"testing"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// offerScript has a screen and a designer that fails, so a test sees
// whether the mockup step ran without depending on what it draws.
func offerScript() stageScript {
	script := withScreen(dialogScript(), true)
	script["mockup"] = func(context.Context, stage.Input) (stage.Output, error) {
		return stage.Output{}, errors.New("model down")
	}

	return script
}

// takeOffer accepts the offer that waits and lets the step run.
func takeOffer(t *testing.T, c *Controller) {
	t.Helper()

	require.NoError(t, c.Message(context.Background(), "BP1", "show mockup", "offer-1"))
	c.Wait("BP1")
}

// offerStates is the state of every offer event, oldest first.
func offerStates(rec *recorder) []any {
	var out []any

	for _, e := range rec.all(EventMockupOffer) {
		out = append(out, e.Payload["state"])
	}

	return out
}

func TestOffer_DefaultModeAsksBeforeTheFirstLook(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	drive(t, c, "BP1")

	assert.NotContains(t, stagesRun(runner), "mockup")

	v := view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, protocol.OpenPrompt{PromptID: offerPromptID, Kind: promptGate}, *v.Open)
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)

	shown, ok := rec.first(EventMockupOffer)
	require.True(t, ok)
	assert.Equal(t, "shown", shown.Payload["state"])
}

func TestOffer_ShowMockupRunsTheStep(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	drive(t, c, "BP1")
	takeOffer(t, c)

	assert.Contains(t, stagesRun(runner), "mockup")

	offers := rec.all(EventMockupOffer)
	assert.Equal(t, "accepted", offers[len(offers)-1].Payload["state"])
	assert.Equal(t, "offer-1", offers[len(offers)-1].Payload["message_id"])
}

func TestOffer_SkipMockupGoesOnWithoutTheStep(t *testing.T) {
	c, st, runner, rec, _ := newMockup(t, offerScript())
	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "skip mockup", "m2"))
	c.Wait("BP1")

	assert.NotContains(t, stagesRun(runner), "mockup")
	assert.Contains(t, stagesRun(runner), "slicing")
	assert.Equal(t, "skipped", stepOf(t, view(t, c), "mockup").Status)

	_, err := st.LatestArtifact(context.Background(), "BP1", "mockup")
	require.ErrorIs(t, err, store.ErrNotFound)

	offers := rec.all(EventMockupOffer)
	assert.Equal(t, "declined", offers[len(offers)-1].Payload["state"])
	assert.Equal(t, "m2", offers[len(offers)-1].Payload["message_id"])
}

func TestOffer_AnyOtherReplyIsANote(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "what would it show?", "m2"))
	c.Wait("BP1")

	notes := rec.all(EventNote)
	require.Len(t, notes, 1)
	assert.Equal(t, "what would it show?", notes[0].Payload["text"])
	assert.NotContains(t, notes[0].Payload, "retries", "a note at the offer runs nothing")

	assert.NotContains(t, stagesRun(runner), "mockup")
	assert.Equal(t, []any{"shown"}, offerStates(rec), "the offer stays open and is not posted again")

	v := view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, protocol.OpenPrompt{PromptID: offerPromptID, Kind: promptGate}, *v.Open)

	// The note left the run slot free, so the next reply takes the offer.
	takeOffer(t, c)
	assert.Contains(t, stagesRun(runner), "mockup")
}

// A person who took the offer can still go on without a mockup while no
// revision was shown, as when the designer's first run failed.
func TestOffer_SkipMockupLeavesAFirstLookThatFailed(t *testing.T) {
	c, st, runner, rec, _ := newMockup(t, offerScript())
	drive(t, c, "BP1")
	takeOffer(t, c)
	require.Equal(t, "failed", stepOf(t, view(t, c), "mockup").Status)

	require.NoError(t, c.Message(context.Background(), "BP1", "skip mockup", "m3"))
	c.Wait("BP1")

	offers := rec.all(EventMockupOffer)
	assert.Equal(t, "declined", offers[len(offers)-1].Payload["state"])
	assert.Equal(t, "m3", offers[len(offers)-1].Payload["message_id"])
	assert.Equal(t, "skipped", stepOf(t, view(t, c), "mockup").Status)
	assert.Contains(t, stagesRun(runner), "slicing")
	assert.Len(t, inputsOf(runner, "mockup"), 1, "the designer ran once")

	_, err := st.LatestArtifact(context.Background(), "BP1", "mockup")
	require.ErrorIs(t, err, store.ErrNotFound)
}

// Any other text after a first look that failed runs the designer again.
func TestOffer_ANoteAfterAFailedFirstLookRunsItAgain(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	drive(t, c, "BP1")
	takeOffer(t, c)

	require.NoError(t, c.Message(context.Background(), "BP1", "try once more", "m3"))
	c.Wait("BP1")

	assert.Len(t, inputsOf(runner, "mockup"), 2)
	assert.NotContains(t, stagesRun(runner), "slicing")
	assert.Equal(t, []any{"shown", "accepted"}, offerStates(rec))

	notes := rec.all(EventNote)
	require.NotEmpty(t, notes)
	assert.Equal(t, "mockup", notes[len(notes)-1].Payload["retries"])
	assert.Equal(t, firstLookHint, notes[len(notes)-1].Payload["hint"], "the note says skip mockup still goes on without one")
}

func TestOffer_AReopenShowsItAgain(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	ctx := context.Background()

	drive(t, c, "BP1")

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Equal(t, []any{"shown", "shown"}, offerStates(rec))
	assert.NotContains(t, stagesRun(runner), "mockup")

	v := view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, protocol.OpenPrompt{PromptID: offerPromptID, Kind: promptGate}, *v.Open)
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)
}

func TestOffer_ADeclineHoldsAcrossAReopen(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "skip mockup", "m2"))
	c.Wait("BP1")

	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	assert.Equal(t, []any{"shown", "declined"}, offerStates(rec), "a reopen asks nothing again")
	assert.NotContains(t, stagesRun(runner), "mockup")
	assert.Equal(t, "skipped", stepOf(t, view(t, c), "mockup").Status)
}

func TestOffer_ContinueLeavesItWaiting(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	drive(t, c, "BP1")
	require.NoError(t, c.Resume(context.Background(), "BP1", "m2"))
	c.Wait("BP1")

	resumed, ok := rec.first(EventResume)
	require.True(t, ok)
	assert.Equal(t, true, resumed.Payload["nothing"], "the offer waits for its reply, not a resume")
	assert.Equal(t, []any{"shown"}, offerStates(rec))
	assert.NotContains(t, stagesRun(runner), "mockup")
}

func TestOffer_AutoModesTakeNoOffer(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	driveIn(t, c, autoStart("BP1"))

	assert.Empty(t, rec.all(EventMockupOffer))
	assert.Contains(t, stagesRun(runner), "mockup")
}

func TestOffer_ASwitchToAutoTakesIt(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	drive(t, c, "BP1")
	require.NoError(t, c.SetMode(context.Background(), "BP1", protocol.ArchitectModeAuto, "m2"))
	c.Wait("BP1")

	offers := rec.all(EventMockupOffer)
	require.NotEmpty(t, offers)
	assert.Equal(t, "accepted", offers[len(offers)-1].Payload["state"])
	assert.Equal(t, "mode", offers[len(offers)-1].Payload["by"])
	assert.Contains(t, stagesRun(runner), "mockup")

	v := view(t, c)
	assert.Nil(t, v.Open, "the mode took the offer")
	assert.Equal(t, "failed", stepOf(t, v, "mockup").Status)
}

func TestOffer_AGateReplyLeavesTheShownOfferOpen(t *testing.T) {
	c, _, runner, rec, _ := newMockup(t, offerScript())
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.SetPolicy(ctx, "BP1", "mockup", "auto", "m2"))
	c.Wait("BP1")

	change, ok := rec.first(EventPolicy)
	require.True(t, ok)
	assert.Equal(t, "offer", change.Payload["open"])

	v := view(t, c)
	require.NotNil(t, v.Open)
	assert.Equal(t, protocol.OpenPrompt{PromptID: offerPromptID, Kind: promptGate}, *v.Open)
	assert.Equal(t, "waiting", stepOf(t, v, "mockup").Status)

	// A shown offer stays with the person across a reopen, whatever the
	// policy says now.
	require.NoError(t, c.End(ctx, "BP1"))
	require.NoError(t, c.Start(ctx, startInput("BP1")))
	c.Wait("BP1")

	require.NoError(t, c.Message(ctx, "BP1", "skip mockup", "m3"))
	c.Wait("BP1")

	assert.Equal(t, []any{"shown", "shown", "declined"}, offerStates(rec))
	assert.NotContains(t, stagesRun(runner), "mockup", "the person's skip holds")
	assert.Contains(t, stagesRun(runner), "slicing")
}

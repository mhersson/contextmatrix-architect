package blueprint

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// testHub is a hub over a fake view whose sequence a test sets; it counts
// its builds.
type testHub struct {
	*liveHub
	seq    atomic.Uint64
	builds atomic.Int32
}

func newTestHub(t *testing.T, buffer int, interval time.Duration) *testHub {
	t.Helper()

	th := &testHub{}
	th.liveHub = newLiveHub(func(_ context.Context, id string) (protocol.BlueprintView, error) {
		th.builds.Add(1)

		if id != viewID {
			return protocol.BlueprintView{}, ErrNotFound
		}

		return protocol.BlueprintView{ID: id, Seq: th.seq.Load(), Pipeline: []protocol.PipelineStep{}, Artifacts: []protocol.ArtifactRef{}}, nil
	}, slog.New(slog.DiscardHandler))
	th.buffer, th.interval = buffer, interval

	return th
}

func subscribed(t *testing.T, h *liveHub) (<-chan protocol.LiveFrame, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := h.subscribe(ctx, viewID)
	require.NoError(t, err)

	return ch, cancel
}

// nextFrame receives one frame, failing at a deadline or a closed channel.
func nextFrame(t *testing.T, ch <-chan protocol.LiveFrame) protocol.LiveFrame {
	t.Helper()

	select {
	case f, ok := <-ch:
		require.True(t, ok, "the stream closed")

		return f
	case <-time.After(5 * time.Second):
		t.Fatal("no frame came")

		return protocol.LiveFrame{}
	}
}

// framesUntil receives frames until stop says so, and returns them all.
func framesUntil(t *testing.T, ch <-chan protocol.LiveFrame, stop func(protocol.LiveFrame) bool) []protocol.LiveFrame {
	t.Helper()

	var out []protocol.LiveFrame

	for {
		f := nextFrame(t, ch)
		out = append(out, f)

		if stop(f) {
			return out
		}
	}
}

func draftFrame(offset int, text string) protocol.Draft {
	return protocol.Draft{Stage: "intake", Version: 1, Offset: offset, Text: text}
}

func TestLive_ASubscriptionStartsWithASnapshot(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)
	h.seq.Store(7)

	ch, _ := subscribed(t, h.liveHub)

	f := nextFrame(t, ch)
	assert.Equal(t, frameSnapshot, f.Type)
	require.NotNil(t, f.View)
	assert.Equal(t, uint64(7), f.View.Seq)
	assert.Nil(t, f.Draft)
}

func TestLive_AnUnknownBlueprintIsNotFound(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	_, err := h.subscribe(context.Background(), "nosuch")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLive_ALateSubscriberGetsTheWholeDraft(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	h.draft(viewID, draftFrame(0, "# Ne"))
	h.draft(viewID, draftFrame(4, "ed 日本"))

	ch, _ := subscribed(t, h.liveHub)

	f := nextFrame(t, ch)
	require.NotNil(t, f.Draft)
	assert.Equal(t, protocol.Draft{Stage: "intake", Version: 1, Offset: 0, Text: "# Need 日本"}, *f.Draft)
}

func TestLive_TouchesAreCoalesced(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	ch, _ := subscribed(t, h.liveHub)
	nextFrame(t, ch) // the subscription's snapshot
	nextFrame(t, ch) // the feed's first rebuild

	for range 50 {
		h.touch(viewID)
	}

	assert.Never(t, func() bool { return h.builds.Load() > 2 }, 200*time.Millisecond, 10*time.Millisecond,
		"one build per interval, however many touches")
}

func TestLive_ATouchAfterTheIntervalSendsAFreshSnapshot(t *testing.T) {
	h := newTestHub(t, 8, 20*time.Millisecond)

	ch, _ := subscribed(t, h.liveHub)
	nextFrame(t, ch) // the subscription's snapshot
	nextFrame(t, ch) // the feed's first rebuild

	h.seq.Store(42)
	h.touch(viewID)

	frames := framesUntil(t, ch, func(f protocol.LiveFrame) bool { return f.View != nil && f.View.Seq == 42 })
	assert.Equal(t, frameSnapshot, frames[len(frames)-1].Type)
}

func TestLive_AStoredArtifactIsHintedAndEndsItsDraft(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	h.draft(viewID, draftFrame(0, "# Need"))
	h.stored(viewID, "intake", 1)

	ch, _ := subscribed(t, h.liveHub)
	assert.Nil(t, nextFrame(t, ch).Draft, "the stored artifact replaced its draft")

	h.stored(viewID, "slicing", 2)

	frames := framesUntil(t, ch, func(f protocol.LiveFrame) bool { return f.Type == frameStored })
	last := frames[len(frames)-1]
	assert.Equal(t, "slicing", last.Stage)
	assert.Equal(t, 2, last.Version)
}

func TestLive_ARefusedDraftStaysUntilItsRunEnds(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	h.draft(viewID, draftFrame(0, "# Need"))
	h.draft(viewID, protocol.Draft{Stage: "intake", Version: 1, Offset: 6, Refused: true, Reason: "1 problem"})

	ch, _ := subscribed(t, h.liveHub)

	f := nextFrame(t, ch)
	require.NotNil(t, f.Draft)
	assert.True(t, f.Draft.Refused)
	assert.Equal(t, "# Need", f.Draft.Text)
	assert.Equal(t, "1 problem", f.Draft.Reason)

	h.clearDraft(viewID)

	ch2, _ := subscribed(t, h.liveHub)
	assert.Nil(t, nextFrame(t, ch2).Draft, "a snapshot without a draft: none in progress")
}

func TestLive_AnOverflowDropsDraftsAndSendsASnapshot(t *testing.T) {
	h := newTestHub(t, 4, time.Hour)

	ch, _ := subscribed(t, h.liveHub)

	// Nobody reads while ten draft frames and a hint arrive.
	var want strings.Builder

	for i := range 10 {
		text := string(rune('a' + i))
		h.draft(viewID, draftFrame(i, text))
		want.WriteString(text)
	}

	h.stored(viewID, "slicing", 1)

	frames := framesUntil(t, ch, func(f protocol.LiveFrame) bool { return f.Type == frameStored })

	assert.Equal(t, frameSnapshot, frames[0].Type, "a snapshot comes first")

	var (
		held   []rune
		drafts int
	)

	for _, f := range frames {
		switch f.Type {
		case frameSnapshot:
			held = nil
			if f.Draft != nil {
				held = []rune(f.Draft.Text)
			}
		case frameDraft:
			drafts++

			require.True(t, f.Draft.Offset == 0 || f.Draft.Offset == len(held), "no gap after a snapshot")

			if f.Draft.Offset == 0 {
				held = nil
			}

			held = append(held, []rune(f.Draft.Text)...)
		}
	}

	assert.Equal(t, want.String(), string(held), "the client ends with the whole draft")
	assert.Less(t, drafts, 10, "queued drafts were dropped")
}

func TestLive_PublishingNeverWaitsForAStalledSubscriber(t *testing.T) {
	h := newTestHub(t, 4, time.Hour)

	subscribed(t, h.liveHub) // never read

	done := make(chan struct{})

	go func() {
		defer close(done)

		for i := range 1000 {
			h.draft(viewID, draftFrame(i, "x"))
			h.stored(viewID, "slicing", i)
			h.touch(viewID)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a subscriber that does not read")
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for s := range h.feeds[viewID].subs {
		assert.LessOrEqual(t, len(s.queue), h.buffer, "the stalled subscriber's queue stays bounded")
	}
}

func TestLive_NoFrameAfterUnsubscribe(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	ch, cancel := subscribed(t, h.liveHub)
	nextFrame(t, ch) // the subscription's snapshot
	nextFrame(t, ch) // the feed's first rebuild

	cancel()

	h.draft(viewID, draftFrame(0, "late"))
	h.stored(viewID, "intake", 1)
	h.touch(viewID)

	select {
	case f, ok := <-ch:
		assert.False(t, ok, "no frame after unsubscribe, got %+v", f)
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never closed")
	}
}

func TestLive_OneSubscriberLeavingKeepsTheOthersFeed(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	a, cancelA := subscribed(t, h.liveHub)
	b, _ := subscribed(t, h.liveHub)

	assert.Equal(t, frameSnapshot, nextFrame(t, a).Type, "each subscriber gets its own snapshot")
	assert.Equal(t, frameSnapshot, nextFrame(t, b).Type)

	cancelA()

	for range a { // A's stream closes after A has left the feed
	}

	h.draft(viewID, draftFrame(0, "still here"))

	frames := framesUntil(t, b, func(f protocol.LiveFrame) bool { return f.Type == frameDraft })
	assert.Equal(t, "still here", frames[len(frames)-1].Draft.Text)
}

func TestSubscribe_FollowsARun(t *testing.T) {
	c, _, _, _ := newController(t, artifactRun("# Need"))
	c.live.interval = 10 * time.Millisecond

	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, c.Start(ctx, startInput("BP1")))

	ch, err := c.Subscribe(ctx, "BP1")
	require.NoError(t, err)

	first := nextFrame(t, ch)
	assert.Equal(t, frameSnapshot, first.Type)
	require.NotNil(t, first.View)
	assert.Equal(t, "BP1", first.View.ID)

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))

	frames := framesUntil(t, ch, func(f protocol.LiveFrame) bool { return f.Type == frameStored })
	stored := frames[len(frames)-1]
	assert.Equal(t, "intake", stored.Stage)
	assert.Equal(t, 1, stored.Version)

	// The run's end lands in a later snapshot.
	framesUntil(t, ch, func(f protocol.LiveFrame) bool {
		return f.View != nil && len(f.View.Pipeline) > 0 && f.View.Pipeline[0].Status == stepDone
	})
}

func TestSubscribe_AnUnknownBlueprintIsNotFound(t *testing.T) {
	c, _, _, _ := newController(t, artifactRun("# Need"))

	_, err := c.Subscribe(context.Background(), "nosuch")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestLive_AGapDraftFrameIsDropped(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	h.draft(viewID, draftFrame(3, "orphan")) // nothing held
	h.draft(viewID, draftFrame(0, "# Need"))
	h.draft(viewID, draftFrame(2, "gap")) // held is 6 code points
	h.draft(viewID, protocol.Draft{Stage: "other", Version: 1, Offset: 6, Text: "x"})
	h.draft(viewID, draftFrame(6, "!"))

	ch, _ := subscribed(t, h.liveHub)

	f := nextFrame(t, ch)
	require.NotNil(t, f.Draft)
	assert.Equal(t, "# Need!", f.Draft.Text)
}

func TestLive_AGapDraftFrameIsNotSentOn(t *testing.T) {
	h := newTestHub(t, 8, time.Hour)

	ch, _ := subscribed(t, h.liveHub)
	nextFrame(t, ch)
	nextFrame(t, ch)

	h.draft(viewID, draftFrame(9, "gap"))
	h.draft(viewID, draftFrame(0, "ok"))

	f := nextFrame(t, ch)
	assert.Equal(t, frameDraft, f.Type)
	assert.Equal(t, "ok", f.Draft.Text)
}

func TestSubscribe_AFailedRunEndsItsDraft(t *testing.T) {
	var c *Controller

	c, _, _, _ = newController(t, func(_ context.Context, _ stage.Input) (stage.Output, error) {
		c.live.draft("BP1", draftFrame(0, "# half"))

		return stage.Output{}, errors.New("the model gave up")
	})
	c.live.interval = 10 * time.Millisecond

	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, c.Start(ctx, startInput("BP1")))

	ch, err := c.Subscribe(ctx, "BP1")
	require.NoError(t, err)

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))

	var held bool

	framesUntil(t, ch, func(f protocol.LiveFrame) bool {
		held = held || f.Draft != nil

		return held && f.Type == frameSnapshot && f.Draft == nil
	})
}

func TestObserve_HintsAndDraftEnds(t *testing.T) {
	hints := []struct {
		name    string
		event   Event
		stage   string
		version int
	}{
		{"a stored artifact", Event{Kind: EventArtifact, Payload: map[string]any{"stage": "intake", "version": 2}}, "intake", 2},
		{"a sign-off", Event{Kind: EventSignOff, Payload: map[string]any{"version": float64(3)}}, signoffStage, 3},
		{"an automatic sign-off", Event{Kind: EventSignOffAuto, Payload: map[string]any{"version": 1}}, signoffStage, 1},
		{"the emitted board", Event{Kind: EventEmitted}, emissionStage, 1},
		{"a watch state", Event{Kind: EventWatchState}, emissionStage, 1},
		{"the watch's end", Event{Kind: EventWatchEnded}, emissionStage, 1},
	}

	for _, tc := range hints {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _, _ := newController(t, artifactRun("# Need"))
			c.live.interval = time.Hour

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			require.NoError(t, c.Start(ctx, startInput("BP1")))

			ch, err := c.Subscribe(ctx, "BP1")
			require.NoError(t, err)
			nextFrame(t, ch)

			tc.event.BlueprintID = "BP1"
			c.observe(tc.event)

			frames := framesUntil(t, ch, func(f protocol.LiveFrame) bool { return f.Type == frameStored })
			last := frames[len(frames)-1]
			assert.Equal(t, tc.stage, last.Stage)
			assert.Equal(t, tc.version, last.Version)
		})
	}

	ends := []struct {
		name  string
		event Event
		ends  bool
	}{
		{"a stage run starting", Event{Kind: EventStageStarted, Payload: map[string]any{"kind": store.RunKindStage}}, true},
		{"a stage run ending", Event{Kind: EventStageEnded, Payload: map[string]any{"kind": store.RunKindStage}}, true},
		{"the session ending", Event{Kind: EventEnded}, true},
		{"another kind of run starting", Event{Kind: EventStageStarted, Payload: map[string]any{"kind": "review"}}, false},
		{"a note", Event{Kind: EventNote}, false},
	}

	for _, tc := range ends {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _, _ := newController(t, artifactRun("# Need"))
			c.live.interval = time.Hour

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			require.NoError(t, c.Start(ctx, startInput("BP1")))

			c.live.draft("BP1", draftFrame(0, "# half"))

			tc.event.BlueprintID = "BP1"
			c.observe(tc.event)

			ch, err := c.Subscribe(ctx, "BP1")
			require.NoError(t, err)

			f := nextFrame(t, ch)
			assert.Equal(t, tc.ends, f.Draft == nil)
		})
	}
}

func TestRun_AStageRunsDraftReachesItsSubscribers(t *testing.T) {
	c, _, _, _ := newController(t, func(_ context.Context, in stage.Input) (stage.Output, error) {
		in.Draft(stage.DraftFrame{Offset: 0, Text: "# Ne"})
		in.Draft(stage.DraftFrame{Offset: 4, Text: "ed"})

		return artifactOutput("# Need", in), nil
	})
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, c.Start(ctx, startInput("BP1")))

	ch, err := c.Subscribe(ctx, "BP1")
	require.NoError(t, err)

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))

	frames := framesUntil(t, ch, func(f protocol.LiveFrame) bool { return f.Type == frameStored })

	var drafts []protocol.Draft

	for _, f := range frames {
		if f.Type == frameDraft {
			drafts = append(drafts, *f.Draft)
		}
	}

	assert.Equal(t, []protocol.Draft{
		{Stage: "intake", Version: 1, Offset: 0, Text: "# Ne"},
		{Stage: "intake", Version: 1, Offset: 4, Text: "ed"},
	}, drafts, "frames carry the stage and the version the artifact is stored as")
}

func TestRun_AVoidDraftLeavesTheSnapshotWithoutOne(t *testing.T) {
	release := make(chan struct{})

	c, _, _, _ := newController(t, func(ctx context.Context, in stage.Input) (stage.Output, error) {
		in.Draft(stage.DraftFrame{Offset: 0, Text: "# Ne"})
		in.Draft(stage.DraftFrame{Void: true})

		// The run stays open until the test has seen the snapshot the void
		// caused, so nothing but the void can have cleared the draft.
		parkUntil(ctx, release)

		return artifactOutput("# Need", in), nil
	})
	c.live.interval = 10 * time.Millisecond

	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })

	closeRelease := closer(t, release)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, c.Start(ctx, startInput("BP1")))

	ch, err := c.Subscribe(ctx, "BP1")
	require.NoError(t, err)

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))

	var sawDraft bool

	frames := framesUntil(t, ch, func(f protocol.LiveFrame) bool {
		if f.Type == frameDraft {
			sawDraft = true
		}

		return sawDraft && f.Type == frameSnapshot && f.Draft == nil
	})

	closeRelease()

	for _, f := range frames {
		if f.Type == frameDraft {
			assert.Equal(t, "# Ne", f.Draft.Text, "a void frame is never sent as a draft")
		}
	}

	framesUntil(t, ch, func(f protocol.LiveFrame) bool { return f.Type == frameStored })
}

func TestRun_ADraftCarriesTheNextVersion(t *testing.T) {
	script := gatedScript()
	script["shape"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		in.Draft(stage.DraftFrame{Offset: 0, Text: "# Sh"})

		return done("# Shape", nil), nil
	}

	c, _, _, _ := newGated(t, script)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	require.NoError(t, c.Start(ctx, startInput("BP1")))

	ch, err := c.Subscribe(ctx, "BP1")
	require.NoError(t, err)

	var versions []int

	storedShape := func(version int) func(protocol.LiveFrame) bool {
		return func(f protocol.LiveFrame) bool {
			if f.Type == frameDraft {
				versions = append(versions, f.Draft.Version)
			}

			return f.Type == frameStored && f.Stage == "shape" && f.Version == version
		}
	}

	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	framesUntil(t, ch, storedShape(1))
	c.Wait("BP1")

	// A rejection runs the stage again: its artifact is stored as version 2.
	require.NoError(t, c.Message(ctx, "BP1", "reject make it smaller", "m2"))
	framesUntil(t, ch, storedShape(2))

	assert.Equal(t, []int{1, 2}, versions)
}

func TestRun_AReviewDraftsNothing(t *testing.T) {
	c, _, runner, _ := newGated(t, gatedScript())

	startGated(t, c, protocol.ArchitectModeDefault)

	reviews := inputsOf(runner, "shape.review")
	require.NotEmpty(t, reviews)
	assert.Nil(t, reviews[0].Draft)
	assert.NotNil(t, inputsOf(runner, "shape")[0].Draft)
}

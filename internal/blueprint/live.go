package blueprint

import (
	"context"
	"log/slog"
	"sync"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Live frame types.
const (
	frameSnapshot = "snapshot"
	frameStored   = "artifact_stored"
	frameDraft    = "draft"
)

const (
	// liveInterval spaces a blueprint's snapshots: every event, state change
	// and harness line asks for one, and at most one is built per interval.
	liveInterval = 250 * time.Millisecond
	// liveBuffer bounds the frames queued for one subscriber.
	liveBuffer = 64
)

// liveHub sends each subscriber of a blueprint a snapshot of its view
// first, then a fresh snapshot after every change, a hint when an artifact
// is stored or changes, and each frame of the draft being written. A
// subscriber that falls behind loses its queued drafts and snapshots and
// gets one new snapshot, which carries the whole draft. Publishing never
// waits for a subscriber.
type liveHub struct {
	build    func(ctx context.Context, id string) (protocol.BlueprintView, error)
	logger   *slog.Logger
	interval time.Duration
	buffer   int

	// mu is a leaf lock: nothing that can block runs under it, and the view
	// is never built while holding it.
	mu    sync.Mutex
	feeds map[string]*feed
	// drafts are the whole current drafts, kept with or without
	// subscribers, so a late subscriber's snapshot carries one.
	drafts map[string]*protocol.Draft
}

// feed is one blueprint's live state while it has subscribers.
type feed struct {
	view    protocol.BlueprintView
	subs    map[*subscriber]struct{}
	touched chan struct{}
	stop    chan struct{}
}

// subscriber is one stream's queue; wake tells its delivery there is more.
type subscriber struct {
	queue []protocol.LiveFrame
	wake  chan struct{}
}

func newLiveHub(build func(context.Context, string) (protocol.BlueprintView, error), logger *slog.Logger) *liveHub {
	return &liveHub{
		build: build, logger: logger, interval: liveInterval, buffer: liveBuffer,
		feeds: map[string]*feed{}, drafts: map[string]*protocol.Draft{},
	}
}

// subscribe opens id's live stream: its first frame is a snapshot, and it
// closes when ctx ends, with no frame after.
func (h *liveHub) subscribe(ctx context.Context, id string) (<-chan protocol.LiveFrame, error) {
	view, err := h.build(ctx, id)
	if err != nil {
		return nil, err
	}

	s := &subscriber{wake: make(chan struct{}, 1)}

	h.mu.Lock()

	f, existed := h.feeds[id]

	switch {
	case !existed:
		f = &feed{view: view, subs: map[*subscriber]struct{}{}, touched: make(chan struct{}, 1), stop: make(chan struct{})}
		h.feeds[id] = f

		// The pump serves every subscriber of the feed, so it outlives any one
		// request's ctx and stops with the feed.
		go h.pump(id, f) //nolint:gosec // G118: see above
	case view.Seq >= f.view.Seq:
		f.view = view
	}

	f.subs[s] = struct{}{}
	s.queue = append(s.queue, h.snapshotLocked(id, f))
	h.mu.Unlock()

	// A change between the build and the feed's start reached no feed; one
	// more snapshot carries it.
	if !existed {
		h.touch(id)
	}

	out := make(chan protocol.LiveFrame)

	go h.deliver(ctx, id, f, s, out)

	return out, nil
}

// deliver hands s's frames to out in order until ctx ends.
func (h *liveHub) deliver(ctx context.Context, id string, f *feed, s *subscriber, out chan<- protocol.LiveFrame) {
	defer close(out)
	defer h.unsubscribe(id, f, s)

	for {
		h.mu.Lock()
		frames := s.queue
		s.queue = nil
		h.mu.Unlock()

		for _, fr := range frames {
			// Checked before each send: once ctx ends no frame goes out.
			if ctx.Err() != nil {
				return
			}

			select {
			case out <- fr:
			case <-ctx.Done():
				return
			}
		}

		select {
		case <-s.wake:
		case <-ctx.Done():
			return
		}
	}
}

func (h *liveHub) unsubscribe(id string, f *feed, s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(f.subs, s)

	if len(f.subs) == 0 && h.feeds[id] == f {
		delete(h.feeds, id)
		close(f.stop)
	}
}

// pump rebuilds f's view when touched, at most once per interval, and
// sends the snapshot to every subscriber.
func (h *liveHub) pump(id string, f *feed) {
	for {
		select {
		case <-f.touched:
		case <-f.stop:
			return
		}

		view, err := h.build(context.Background(), id)
		if err != nil {
			h.logger.Warn("blueprint: build the live view", "blueprint", id, "error", err)
		} else {
			h.mu.Lock()
			if view.Seq >= f.view.Seq {
				f.view = view
				h.broadcastLocked(id, f, h.snapshotLocked(id, f))
			}
			h.mu.Unlock()
		}

		t := time.NewTimer(h.interval)

		select {
		case <-t.C:
		case <-f.stop:
			t.Stop()

			return
		}
	}
}

// touch asks for a snapshot of id; it never blocks.
func (h *liveHub) touch(id string) {
	h.mu.Lock()
	f := h.feeds[id]
	h.mu.Unlock()

	if f == nil {
		return
	}

	select {
	case f.touched <- struct{}{}:
	default:
	}
}

// stored hints that key's version was stored or changed; the draft it
// replaces ends. The frame names key's stage and slice apart.
func (h *liveHub) stored(id, key string, version int) {
	stage, slice := pack.SplitStep(key)

	h.mu.Lock()
	defer h.mu.Unlock()

	if d := h.drafts[id]; d != nil && d.Stage == stage && d.Slice == slice && d.Version == version {
		delete(h.drafts, id)
	}

	if f := h.feeds[id]; f != nil {
		h.broadcastLocked(id, f, protocol.LiveFrame{Type: frameStored, Stage: stage, Slice: slice, Version: version})
	}
}

// draft applies one draft frame to id's whole draft and sends it on; a frame
// a client would drop as a gap is dropped here too.
func (h *liveHub) draft(id string, d protocol.Draft) {
	h.mu.Lock()
	defer h.mu.Unlock()

	next, ok := applyDraft(h.drafts[id], d)
	if !ok {
		return
	}

	h.drafts[id] = next

	if f := h.feeds[id]; f != nil {
		frame := d
		h.broadcastLocked(id, f, protocol.LiveFrame{Type: frameDraft, Draft: &frame})
	}
}

// clearDraft ends id's draft: a snapshot without one says none is in
// progress.
func (h *liveHub) clearDraft(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	delete(h.drafts, id)
}

// snapshotLocked is f's latest view with id's whole draft.
func (h *liveHub) snapshotLocked(id string, f *feed) protocol.LiveFrame {
	view := f.view
	frame := protocol.LiveFrame{Type: frameSnapshot, View: &view}

	if d := h.drafts[id]; d != nil {
		whole := *d
		frame.Draft = &whole
	}

	return frame
}

func (h *liveHub) broadcastLocked(id string, f *feed, fr protocol.LiveFrame) {
	for s := range f.subs {
		h.pushLocked(id, f, s, fr)

		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// pushLocked queues fr for s. A full queue drops its drafts and snapshots,
// which a new snapshot replaces, and keeps its newest hints.
func (h *liveHub) pushLocked(id string, f *feed, s *subscriber, fr protocol.LiveFrame) {
	if len(s.queue) < h.buffer {
		s.queue = append(s.queue, fr)

		return
	}

	kept := []protocol.LiveFrame{h.snapshotLocked(id, f)}

	for _, q := range s.queue {
		if q.Type == frameStored {
			kept = append(kept, q)
		}
	}

	if fr.Type == frameStored {
		kept = append(kept, fr)
	}

	if len(kept) > h.buffer {
		kept = append(kept[:1], kept[len(kept)-h.buffer+1:]...)
	}

	s.queue = kept
}

// applyDraft is the whole draft after frame d, as a client holding cur
// applies it: offset 0 replaces, an offset equal to cur's length in code
// points appends, and any other is a gap, reported as not ok.
func applyDraft(cur *protocol.Draft, d protocol.Draft) (*protocol.Draft, bool) {
	next := protocol.Draft{Stage: d.Stage, Slice: d.Slice, Version: d.Version, Text: d.Text, Refused: d.Refused, Reason: d.Reason}

	if d.Offset == 0 {
		return &next, true
	}

	if cur == nil || cur.Stage != d.Stage || cur.Slice != d.Slice || cur.Version != d.Version || d.Offset != len([]rune(cur.Text)) {
		return nil, false
	}

	next.Text = cur.Text + d.Text

	return &next, true
}

// Subscribe opens blueprint id's live stream: a snapshot of its view first,
// then a snapshot after each change, at most four a second; an
// artifact_stored hint when a stage's artifact, a sign-off or the board is
// stored or changes; and each frame of the draft being written. The channel
// closes when ctx ends. ErrNotFound for an unknown id.
func (c *Controller) Subscribe(ctx context.Context, id string) (<-chan protocol.LiveFrame, error) {
	return c.live.subscribe(ctx, id)
}

// observe keeps the live hub current with a stored controller event: a
// stage run's start or end ends its draft, a stored artifact, sign-off or
// board change is hinted, and every event asks for a snapshot. It only
// signals the hub, so a caller holding c.mu is safe.
func (c *Controller) observe(e Event) {
	id := e.BlueprintID
	kind, _ := e.Payload["kind"].(string)

	switch e.Kind {
	case EventStageStarted, EventStageEnded:
		if kind == store.RunKindStage {
			c.live.clearDraft(id)
		}
	case EventArtifact:
		stageID, _ := e.Payload["stage"].(string)
		c.live.stored(id, stageID, payloadInt(e.Payload["version"]))
	case EventSignOff, EventSignOffAuto:
		c.live.stored(id, signoffStage, payloadInt(e.Payload["version"]))
	case EventEmitted, EventWatchState, EventWatchEnded:
		c.live.stored(id, emissionStage, 1)
	case EventEnded:
		c.live.clearDraft(id)
	}

	c.live.touch(id)
}

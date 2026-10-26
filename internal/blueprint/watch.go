package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// watcher is one blueprint's watch goroutine.
type watcher struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startWatch polls an emitted blueprint's cards and playbook while sess is
// attached, posting a line per change. It is a no-op while one runs, after
// Shutdown, or for a session that is no longer the attached one.
func (c *Controller) startWatch(sess *session, id string) {
	c.mu.Lock()

	if c.closing || c.watches[id] != nil || c.sessions[id] != sess {
		c.mu.Unlock()

		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	w := &watcher{cancel: cancel, done: make(chan struct{})}
	c.watches[id] = w
	c.mu.Unlock()

	go func() {
		defer close(w.done)
		defer func() {
			c.mu.Lock()
			if c.watches[id] == w {
				delete(c.watches, id)
			}
			c.mu.Unlock()
		}()

		c.watchLoop(ctx, sess, id)
	}()
}

// watchLoop polls until the run ends or ctx does. Read errors are logged and
// retried on the next tick; a connection is redialled then. Only
// ContextMatrix's own answer that a card or the playbook is not found counts
// as a deletion (see snapshot). It starts from the snapshot the blueprint's
// watch last reported, so after a reopen or a restart it posts only what
// changed while nobody watched.
func (c *Controller) watchLoop(ctx context.Context, sess *session, id string) {
	bg := context.WithoutCancel(ctx)

	var client CMClient

	prev := c.reportedSnapshot(bg, sess.audit, id)
	offered := false

	defer func() {
		if client != nil {
			_ = client.Close()
		}
	}()

	tick := time.NewTicker(c.opts.WatchInterval)
	defer tick.Stop()

	for {
		if client == nil {
			cl, err := c.connectCM(ctx, sess)
			if err != nil && ctx.Err() == nil {
				c.opts.Logger.Warn("blueprint: watch connect", "blueprint", id, "error", sess.audit.text(err.Error()))
			}

			client = cl
		}

		if client != nil {
			snap, err := c.snapshot(ctx, client, id, prev)

			switch {
			case err != nil && ctx.Err() == nil:
				c.opts.Logger.Warn("blueprint: watch read", "blueprint", id, "error", sess.audit.text(err.Error()))
			case err == nil:
				lines := snap.changes(prev)
				for _, line := range lines {
					sess.audit.event(bg, Event{Kind: EventWatch, BlueprintID: id, Payload: map[string]any{"line": line}})
				}

				if len(lines) > 0 {
					c.reportSnapshot(bg, sess.audit, id, snap)
				}

				prev = &snap

				if !offered {
					offered = c.offerRetro(bg, sess, id, snap)
				}

				if snap.done {
					c.finish(bg, sess, id, snap)

					return
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// offerRetro posts the first-stop offer once per blueprint and reports
// whether no offer is still owed. The stored event is what survives a restart
// or a reopen; an offer starts nothing.
func (c *Controller) offerRetro(ctx context.Context, sess *session, id string, snap snapshot) bool {
	stopped := snap.stopped()
	if len(stopped) == 0 {
		return false
	}

	n, err := c.opts.Store.CountEvents(ctx, id, EventRetroStopOffered)
	if err != nil {
		c.opts.Logger.Warn("blueprint: count the retrospective offers", "blueprint", id, "error", sess.audit.text(err.Error()))

		return false
	}

	if n > 0 {
		return true
	}

	parts := make([]string, 0, len(stopped))

	for _, cv := range stopped {
		why := cv.State
		if cv.Worker == "parked" {
			why = "worker parked"
		}

		parts = append(parts, fmt.Sprintf("%s (%s)", cv.ID, why))
	}

	line := fmt.Sprintf("%s stopped. Reply `retrospective` to learn from the cards that struggled.", strings.Join(parts, ", "))
	sess.audit.event(ctx, Event{Kind: EventRetroStopOffered, BlueprintID: id, Payload: map[string]any{"line": line}})

	return true
}

// finish closes an ended run: the closing summary, posted once, then the
// done state. The summary goes first, so a crash between the two restarts
// the watch on the next reopen, which finds the summary stored and only
// sets the state; a reopen of a done blueprint starts no watch at all.
// Ending the session stays with the user.
func (c *Controller) finish(ctx context.Context, sess *session, id string, snap snapshot) {
	_, err := c.opts.Store.LatestEvent(ctx, id, EventWatchEnded)

	switch {
	case errors.Is(err, store.ErrNotFound):
		payload := snap.endPayload()
		payload["priced"] = sess.price != nil

		if cost, err := c.opts.Store.BlueprintCost(ctx, id); err != nil {
			c.opts.Logger.Warn("blueprint: the closing summary goes without the cost", "blueprint", id, "error", err)
		} else {
			payload["cost_usd"] = cost
		}

		sess.audit.event(ctx, Event{Kind: EventWatchEnded, BlueprintID: id, Payload: payload})
	case err != nil:
		// Without the record a second summary could follow; the blueprint
		// stays emitted, and the next reopen's watch tries again.
		c.opts.Logger.Error("blueprint: read the closing summary", "blueprint", id, "error", err)

		return
	}

	c.setState(ctx, id, StateDone)
}

// cardView is what the watch tracks of one emitted card. Gone marks a card
// deleted in ContextMatrix: it keeps what the watch last saw of it and is
// never read again.
type cardView struct {
	Draft   string `json:"draft"`
	Project string `json:"project"`
	ID      string `json:"id"`
	Title   string `json:"title"`
	State   string `json:"state"`
	Worker  string `json:"worker"`
	Phase   string `json:"phase"`
	PR      string `json:"pr"`
	Gone    bool   `json:"gone,omitempty"`
}

// stopped lists the cards whose worker is parked or whose state is stalled or blocked.
func (s snapshot) stopped() []cardView {
	var out []cardView

	for _, cv := range s.Cards {
		if cv.Worker == "parked" || cv.State == "stalled" || cv.State == "blocked" {
			out = append(out, cv)
		}
	}

	return out
}

// runView is what the watch tracks of the playbook's run.
type runView struct {
	Status string `json:"status"`
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

// snapshot is one poll of everything emitted. The watch_state event stores
// it, so its JSON names are a stored format: fields are only added, and an
// added one is omitted while unset, so an older snapshot still decodes.
type snapshot struct {
	Cards    []cardView `json:"cards"`
	Playbook string     `json:"playbook"`
	// PlaybookGone marks a playbook deleted in ContextMatrix, which ends the
	// watch.
	PlaybookGone bool     `json:"playbook_gone,omitempty"`
	Run          *runView `json:"run"`
	// Waiting is the manual entry's text when the run waits for a check-off.
	Waiting string            `json:"waiting"`
	Repos   []cm.PlaybookRepo `json:"repos"`
	// Manual is a single card's manual checks.
	Manual []string `json:"manual"`
	// Followups counts the follow-ups the playbook holds in ContextMatrix,
	// where people may add or remove them during the run.
	Followups int `json:"followups"`
	done      bool
}

// reportSnapshot stores snap as what the watch has reported. The payload is
// its exported fields, masked as every event payload is.
func (c *Controller) reportSnapshot(ctx context.Context, au audit, id string, snap snapshot) {
	var payload map[string]any

	b, err := json.Marshal(snap)
	if err == nil {
		err = json.Unmarshal(b, &payload)
	}

	if err != nil {
		c.opts.Logger.Warn("blueprint: encode the watch state", "blueprint", id, "error", err)

		return
	}

	au.event(ctx, Event{Kind: EventWatchState, BlueprintID: id, Payload: payload})
}

// reportedSnapshot is the snapshot the blueprint's watch last reported; nil
// when there is none or it cannot be read, and then everything is reported.
func (c *Controller) reportedSnapshot(ctx context.Context, au audit, id string) *snapshot {
	ev, err := c.opts.Store.LatestEvent(ctx, id, EventWatchState)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}

	if err != nil {
		c.opts.Logger.Warn("blueprint: read the watch state", "blueprint", id, "error", au.text(err.Error()))

		return nil
	}

	var snap snapshot
	if err := json.Unmarshal(ev.Payload, &snap); err != nil {
		c.opts.Logger.Warn("blueprint: decode the watch state", "blueprint", id, "error", err)

		return nil
	}

	return &snap
}

// snapshot reads every emitted card and the playbook. A card is gone when
// its playbook entry is flagged missing or ContextMatrix answers that it, or
// its project, is not found; prev's gone cards and playbook are not read
// again. Once the playbook is gone no card is read either, so no card read
// can hold the watch's end. Any other failed read fails the poll.
func (c *Controller) snapshot(ctx context.Context, client CMClient, id string, prev *snapshot) (snapshot, error) {
	recs, err := c.opts.Store.ListEmissions(ctx, id)
	if err != nil {
		return snapshot{}, fmt.Errorf("list emissions: %w", err)
	}

	draft, err := c.draft(ctx, id)
	if err != nil {
		return snapshot{}, err
	}

	titles := map[string]string{}
	for _, card := range draft.Cards {
		titles[card.ID] = card.Title
	}

	before := map[string]cardView{}

	if prev != nil {
		for _, cv := range prev.Cards {
			before[cv.ID] = cv
		}
	}

	var snap snapshot

	for _, e := range recs {
		if e.Kind == store.EmitPlaybook {
			snap.Playbook = e.CMID
		}
	}

	// The playbook is read first: its entries flag the cards deleted in
	// ContextMatrix.
	var (
		pb      cm.Playbook
		missing = map[string]bool{}
	)

	if snap.Playbook != "" {
		snap.PlaybookGone = prev != nil && prev.PlaybookGone

		if !snap.PlaybookGone {
			pb, err = client.GetPlaybook(ctx, snap.Playbook)

			switch {
			case cm.PlaybookNotFound(err):
				snap.PlaybookGone = true
			case err != nil:
				return snapshot{}, fmt.Errorf("read playbook %s: %w", snap.Playbook, err)
			}
		}

		for _, entry := range pb.Entries {
			if entry.Type == "card" && entry.Missing {
				missing[entry.Project+"/"+entry.Card] = true
			}
		}
	}

	for _, e := range recs {
		if e.Kind != store.EmitCard {
			continue
		}

		// A card that is not read keeps what the watch last saw of it.
		last := before[e.CMID]
		last.Draft, last.Project, last.ID, last.Title = e.DraftID, e.Project, e.CMID, titles[e.DraftID]

		if last.Gone || snap.PlaybookGone {
			snap.Cards = append(snap.Cards, last)

			continue
		}

		if missing[e.Project+"/"+e.CMID] {
			last.Gone = true
			snap.Cards = append(snap.Cards, last)

			continue
		}

		card, err := client.GetCard(ctx, e.Project, e.CMID)

		switch {
		case cm.CardNotFound(err):
			last.Gone = true
			snap.Cards = append(snap.Cards, last)
		case err != nil:
			return snapshot{}, fmt.Errorf("read %s: %w", e.CMID, err)
		default:
			snap.Cards = append(snap.Cards, cardView{
				Draft: e.DraftID, Project: e.Project, ID: e.CMID, Title: titles[e.DraftID],
				State: card.State, Worker: card.WorkerStatus, Phase: card.Phase, PR: card.PRURL,
			})
		}
	}

	if snap.Playbook == "" {
		for _, m := range draft.Manual {
			snap.Manual = append(snap.Manual, m.Text)
		}

		snap.done = len(snap.Cards) == 1 && (snap.Cards[0].Gone || snap.Cards[0].State == "done" || snap.Cards[0].State == "not_planned")

		return snap, nil
	}

	if snap.PlaybookGone {
		snap.done = true

		return snap, nil
	}

	snap.Repos = pb.Repos
	snap.Followups = len(pb.Followups)

	if pb.Run != nil {
		snap.Run = &runView{Status: pb.Run.Status, Entry: pb.Run.Entry, Reason: pb.Run.Reason}

		if pb.Run.Status == "waiting" {
			for _, entry := range pb.Entries {
				if entry.ID == pb.Run.Entry && entry.Type == "manual" {
					snap.Waiting = entry.Text
				}
			}
		}
	}

	// Every entry complete also ends the watch: a human may stop the run and
	// finish the entries by hand. A missing entry never completes, so a
	// playbook with a deleted card ends as ContextMatrix's run does.
	snap.done = (pb.Run != nil && pb.Run.Status == "completed") || (pb.Total > 0 && pb.Complete == pb.Total)

	return snap, nil
}

// changes are the lines that differ from prev; with no prev, everything.
func (s snapshot) changes(prev *snapshot) []string {
	before := map[string]cardView{}

	if prev != nil {
		for _, cv := range prev.Cards {
			before[cv.ID] = cv
		}
	}

	var lines []string

	for _, cv := range s.Cards {
		old, seen := before[cv.ID]

		if cv.Gone {
			if !seen || !old.Gone {
				lines = append(lines, cv.ID+" was deleted in ContextMatrix")
			}

			continue
		}

		// A card read from ContextMatrix always has a state; one without was
		// never read, behind a playbook deleted before the first read.
		if cv.State != "" && (!seen || old.State != cv.State || old.Worker != cv.Worker || old.Phase != cv.Phase) {
			lines = append(lines, cv.statusLine())
		}

		if cv.PR != "" && (!seen || old.PR != cv.PR) {
			lines = append(lines, fmt.Sprintf("%s: pull request %s", cv.ID, cv.PR))
		}
	}

	var (
		oldRun     runView
		oldWaiting string
	)

	if prev != nil {
		if prev.Run != nil {
			oldRun = *prev.Run
		}

		oldWaiting = prev.Waiting
	}

	// The run moving from one card to the next is no news: the cards' lines
	// say which one works. A wait at another entry is, even when two
	// check-offs read the same.
	if s.Run != nil {
		moved := s.Run.Status == "waiting" && s.Run.Entry != oldRun.Entry

		if s.Run.Status != oldRun.Status || s.Run.Reason != oldRun.Reason || s.Waiting != oldWaiting || moved {
			if line := s.runLine(); line != "" {
				lines = append(lines, line)
			}
		}
	}

	if s.PlaybookGone && (prev == nil || !prev.PlaybookGone) {
		lines = append(lines, fmt.Sprintf("Playbook %s was deleted in ContextMatrix.", s.Playbook))
	}

	return lines
}

func (cv cardView) statusLine() string {
	parts := []string{cv.State}

	if cv.Worker != "" {
		parts = append(parts, "worker "+cv.Worker)
	}

	if cv.Phase != "" {
		parts = append(parts, "phase "+cv.Phase)
	}

	return fmt.Sprintf("%s (%s) %s: %s", cv.ID, cv.Draft, cv.Title, strings.Join(parts, ", "))
}

// runLine describes the run's status; completed is the watch's end.
func (s snapshot) runLine() string {
	switch s.Run.Status {
	case "running":
		return fmt.Sprintf("Playbook %s is running.", s.Playbook)
	case "waiting":
		if s.Waiting != "" {
			return fmt.Sprintf("Playbook %s waits for a human to check off: %s", s.Playbook, s.Waiting)
		}

		return fmt.Sprintf("Playbook %s needs a human: %s. Fix it, then Stop and Run the playbook in ContextMatrix.", s.Playbook, s.Run.Reason)
	case "stopped":
		return fmt.Sprintf("Playbook %s was stopped in ContextMatrix.", s.Playbook)
	default:
		return ""
	}
}

// endPayload is the run's end: the playbook, its compare links, the cards'
// pull requests and a single card's manual checks, with the cards and the
// playbook deleted in ContextMatrix flagged.
func (s snapshot) endPayload() map[string]any {
	repos := make([]any, 0, len(s.Repos))
	for _, r := range s.Repos {
		repos = append(repos, map[string]any{"project": r.Project, "compare_url": r.CompareURL})
	}

	cards := make([]any, 0, len(s.Cards))
	for _, cv := range s.Cards {
		card := map[string]any{"card": cv.ID, "state": cv.State, "pr": cv.PR}
		if cv.Gone {
			card["deleted"] = true
		}

		cards = append(cards, card)
	}

	end := map[string]any{"playbook": s.Playbook, "repos": repos, "cards": cards, "manual": s.Manual}

	if s.PlaybookGone {
		end["playbook_deleted"] = true
	}

	if s.Playbook != "" && s.Followups > 0 {
		end["followups"] = s.Followups
	}

	return end
}

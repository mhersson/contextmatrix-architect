package blueprint

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func watcherOf(c *Controller, id string) *watcher {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.watches[id]
}

func watchLines(rec *recorder) []string {
	var out []string

	for _, e := range rec.all(EventWatch) {
		out = append(out, e.Payload["line"].(string))
	}

	return out
}

func TestWatch_ReportsASingleCardUntilItIsDone(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	fake.setCards("ALPHA-001",
		cm.Card{ID: "ALPHA-001", State: "todo"},
		cm.Card{ID: "ALPHA-001", State: "todo"},
		cm.Card{ID: "ALPHA-001", State: "in_progress", WorkerStatus: "running", Phase: "execute"},
		cm.Card{ID: "ALPHA-001", State: "review", WorkerStatus: "running", Phase: "pr_gates", PRURL: "https://example.com/pr/1"},
		cm.Card{ID: "ALPHA-001", State: "done", WorkerStatus: "completed", PRURL: "https://example.com/pr/1"},
	)

	rec.waitFor(t, EventWatchEnded)

	lines := strings.Join(watchLines(rec), "\n")
	assert.Equal(t, 1, strings.Count(lines, ": todo"), "an unchanged poll publishes nothing")
	assert.Contains(t, lines, "in_progress, worker running, phase execute")
	assert.Equal(t, 1, strings.Count(lines, "pull request https://example.com/pr/1"))

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateDone), bp.State)

	require.Eventually(t, func() bool { return watcherOf(c, "BP1") == nil }, time.Second, 5*time.Millisecond, "the watch ended")
}

func TestWatch_ReportsThePlaybookRunAndEndsWithTheCompareLinks(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	entries := []cm.PlaybookEntry{
		{ID: "e1", Type: "card", Card: "ALPHA-001"},
		{ID: "e2", Type: "manual", Text: "Check the dashboard"},
		{ID: "e3", Type: "card", Card: "ALPHA-002"},
	}
	repos := []cm.PlaybookRepo{{Project: "alpha", CompareURL: "https://example.com/compare/main...playbook/ship-the-parser"}}

	fake.setBooks("ship-the-parser",
		cm.Playbook{ID: "ship-the-parser", Entries: entries, Total: 3},
		cm.Playbook{ID: "ship-the-parser", Entries: entries, Total: 3, Run: &cm.PlaybookRun{Status: "running", Entry: "e1"}},
		cm.Playbook{ID: "ship-the-parser", Entries: entries, Total: 3, Run: &cm.PlaybookRun{Status: "waiting", Entry: "e2", Reason: "awaiting check-off"}},
		cm.Playbook{ID: "ship-the-parser", Entries: entries, Total: 3, Run: &cm.PlaybookRun{Status: "waiting", Entry: "e3", Reason: "ALPHA-002 worker failed"}},
		cm.Playbook{ID: "ship-the-parser", Entries: entries, Total: 3, Run: &cm.PlaybookRun{Status: "waiting", Entry: "e3", Reason: "ALPHA-002 worker failed"}},
		cm.Playbook{ID: "ship-the-parser", Entries: entries, Total: 3, Complete: 3, Repos: repos, Run: &cm.PlaybookRun{Status: "completed"}},
	)

	rec.waitFor(t, EventWatchEnded)

	lines := strings.Join(watchLines(rec), "\n")
	assert.Contains(t, lines, "ship-the-parser is running")
	assert.Contains(t, lines, "waits for a human to check off: Check the dashboard")
	assert.Equal(t, 1, strings.Count(lines, "needs a human: ALPHA-002 worker failed"), "a failed card is reported once")

	end, ok := rec.first(EventWatchEnded)
	require.True(t, ok)

	links := anyList(end.Payload["repos"])
	require.Len(t, links, 1)

	link, ok := links[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https://example.com/compare/main...playbook/ship-the-parser", link["compare_url"])

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateDone), bp.State)
}

func TestWatch_AStoppedRunIsReportedOnce(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	stopped := cm.Playbook{ID: "ship-the-parser", Total: 3, Run: &cm.PlaybookRun{Status: "stopped"}}
	fake.setBooks("ship-the-parser", stopped)

	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(watchLines(rec), "\n"), "was stopped in ContextMatrix")
	}, time.Second, 5*time.Millisecond)

	gets := fake.getCount()

	require.Eventually(t, func() bool { return fake.getCount() > gets+3 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, strings.Count(strings.Join(watchLines(rec), "\n"), "was stopped in ContextMatrix"))
}

func TestWatch_AReadErrorIsRetried(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond

	fake.mu.Lock()
	fake.failGets = 3
	fake.mu.Unlock()

	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	require.Eventually(t, func() bool { return len(watchLines(rec)) > 0 }, 2*time.Second, 5*time.Millisecond)
	assert.Contains(t, watchLines(rec)[0], "ALPHA-001")
	assert.GreaterOrEqual(t, fake.getCount(), 4, "the failed polls were retried")
}

func TestWatch_EndStopsIt(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	require.Eventually(t, func() bool { return fake.getCount() > 0 }, time.Second, 5*time.Millisecond)
	require.NoError(t, c.End(ctx, "BP1"))

	assert.Nil(t, watcherOf(c, "BP1"), "End stopped the watch")
}

func TestWatch_ShutdownWaitsForIt(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	require.Eventually(t, func() bool { return fake.getCount() > 0 }, time.Second, 5*time.Millisecond)
	require.NoError(t, c.Shutdown(ctx))

	assert.Nil(t, watcherOf(c, "BP1"), "Shutdown stopped the watch")
}

// A watch ended by End and restarted on a reopen posts only what changed
// while nobody watched.
func TestWatch_AReopenPostsOnlyWhatChanged(t *testing.T) {
	cases := []struct {
		name string
		now  cm.Card
		want []string
	}{
		{name: "unchanged", now: cm.Card{ID: "ALPHA-002", State: "todo"}, want: []string{}},
		{
			name: "a changed card",
			now:  cm.Card{ID: "ALPHA-002", State: "in_progress", WorkerStatus: "running", Phase: "execute"},
			want: []string{"ALPHA-002 (D2) Wire the parser: in_progress, worker running, phase execute"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _, rec, _ := newPipeline(t, twoCardScript())
			fake := newFakeCM()
			c.opts.ConnectCM = fake.connect
			c.opts.WatchInterval = 5 * time.Millisecond
			ctx := context.Background()

			drive(t, c, "BP1")
			require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
			c.Wait("BP1")

			require.Eventually(t, func() bool { return len(watchLines(rec)) >= 2 }, time.Second, 5*time.Millisecond, "both cards are posted")
			require.NoError(t, c.End(ctx, "BP1"))

			before := len(watchLines(rec))

			fake.setCards("ALPHA-002", tc.now)
			require.NoError(t, c.Start(ctx, startInput("BP1")))
			c.Wait("BP1")
			require.NotNil(t, watcherOf(c, "BP1"), "the reopen restarts the watch")

			gets := fake.getCount()

			require.Eventually(t, func() bool { return fake.getCount() > gets+6 }, time.Second, 5*time.Millisecond)

			assert.Equal(t, tc.want, watchLines(rec)[before:])
		})
	}
}

func TestWatch_ARunWaitingForAHumanIsPostedOnceAcrossARestart(t *testing.T) {
	c, st, _, rec, ws := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	fake.setBooks("ship-the-parser", cm.Playbook{
		ID: "ship-the-parser", Total: 3, Run: &cm.PlaybookRun{Status: "waiting", Entry: "e3", Reason: "ALPHA-002 worker failed"},
	})

	const needsAHuman = "needs a human: ALPHA-002 worker failed"

	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(watchLines(rec), "\n"), needsAHuman)
	}, time.Second, 5*time.Millisecond)

	c2, rec2 := reopen(t, c, st, ws, twoCardScript(), fake, startInput("BP1"))
	require.NotNil(t, watcherOf(c2, "BP1"), "the restart restarts the watch")

	gets := fake.getCount()

	require.Eventually(t, func() bool { return fake.getCount() > gets+6 }, time.Second, 5*time.Millisecond)

	lines := strings.Join(append(watchLines(rec), watchLines(rec2)...), "\n")
	assert.Equal(t, 1, strings.Count(lines, needsAHuman), "posted once in total")
	assert.Empty(t, watchLines(rec2), "nothing changed while nobody watched")
}

// The stored state is an append-only record, so its keys are fixed
// snake_case names, not Go's field names.
func TestWatch_StoresTheReportedStateUnderStableKeys(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	fake.setBooks("ship-the-parser", cm.Playbook{
		ID: "ship-the-parser", Total: 3, Repos: []cm.PlaybookRepo{{Project: "alpha", CompareURL: "https://example.com/compare"}},
		Run: &cm.PlaybookRun{Status: "waiting", Entry: "e3", Reason: "ALPHA-002 worker failed"},
	})

	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(watchLines(rec), "\n"), "needs a human")
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, c.End(ctx, "BP1"))

	ev, err := st.LatestEvent(ctx, "BP1", EventWatchState)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"cards": [
			{"draft": "D1", "project": "alpha", "id": "ALPHA-001", "title": "Add the parser", "state": "todo", "worker": "", "phase": "", "pr": ""},
			{"draft": "D2", "project": "alpha", "id": "ALPHA-002", "title": "Wire the parser", "state": "todo", "worker": "", "phase": "", "pr": ""}
		],
		"playbook": "ship-the-parser",
		"run": {"status": "waiting", "entry": "e3", "reason": "ALPHA-002 worker failed"},
		"waiting": "",
		"repos": [{"project": "alpha", "compare_url": "https://example.com/compare"}],
		"followups": 0,
		"manual": null
	}`, string(ev.Payload))
}

// anyList reads a list payload, typed or JSON-decoded.
func anyList(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []map[string]any:
		out := make([]any, len(s))
		for i, m := range s {
			out[i] = m
		}

		return out
	default:
		return nil
	}
}

func TestWatch_TheEndIsPostedOnceWithTheArchitectCost(t *testing.T) {
	script := happyScript()
	script["intake"] = func(context.Context, stage.Input) (stage.Output, error) {
		out := done("# Need", map[string]any{"slices": 1})
		out.Result.TotalCostUSD = 0.25

		return out, nil
	}

	c, st, _, rec, ws := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	fake.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "done", PRURL: "https://example.com/pr/1"})
	rec.waitFor(t, EventWatchEnded)

	end, ok := rec.first(EventWatchEnded)
	require.True(t, ok)
	assert.InDelta(t, 0.25, end.Payload["cost_usd"], 1e-9, "the ledger's sum over the architect's runs")
	assert.Equal(t, false, end.Payload["priced"], "the session model has no price")

	_, rec2 := reopen(t, c, st, ws, script, newFakeCM(), startInput("BP1"))
	assert.False(t, rec2.has(EventWatchEnded), "a reopen of a done blueprint posts nothing")

	stored, err := st.ListEvents(ctx, "BP1")
	require.NoError(t, err)

	n := 0

	for _, e := range stored {
		if e.Kind == EventWatchEnded {
			n++
		}
	}

	assert.Equal(t, 1, n)
}

func TestWatch_AnEndAlreadyPostedIsNotPostedAgain(t *testing.T) {
	c, st, _, _, ws := newPipeline(t, happyScript())
	c.opts.ConnectCM = newFakeCM().connect
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	// A crash fell between posting the summary and marking the blueprint done.
	require.NoError(t, st.AppendEvent(ctx, store.Event{
		Time: time.Now().UTC(), BlueprintID: "BP1", Kind: EventWatchEnded, Payload: []byte(`{"cards":[]}`),
	}))

	second := newFakeCM()
	second.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "done"})

	_, rec2 := reopen(t, c, st, ws, happyScript(), second, startInput("BP1"))

	require.Eventually(t, func() bool {
		bp, err := st.GetBlueprint(ctx, "BP1")

		return err == nil && bp.State == string(StateDone)
	}, 2*time.Second, 5*time.Millisecond, "the reopened watch sets the state")
	assert.False(t, rec2.has(EventWatchEnded), "the stored summary is not posted again")
}

// startWatchedTwoCards drives twoCardScript to its emission and returns the
// controller watching it.
func startWatchedTwoCards(t *testing.T) (*Controller, *store.Store, *fakeRunner, *recorder, *fakeWorkspace, *fakeCM) {
	t.Helper()

	c, st, runner, rec, ws := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m3"))
	c.Wait("BP1")

	return c, st, runner, rec, ws, fake
}

func TestWatch_OffersTheRetrospectiveOnceWhenACardStops(t *testing.T) {
	_, _, runner, rec, _, fake := startWatchedTwoCards(t)

	calls := runner.calls()

	fake.setCards("ALPHA-002", cm.Card{ID: "ALPHA-002", State: "in_progress", WorkerStatus: "parked", Phase: "execute"})

	rec.waitFor(t, EventRetroStopOffered)

	gets := fake.getCount()

	require.Eventually(t, func() bool { return fake.getCount() > gets+6 }, time.Second, 5*time.Millisecond)
	require.Equal(t, 1, rec.count(EventRetroStopOffered))

	offer, _ := rec.first(EventRetroStopOffered)
	assert.Contains(t, offer.Payload["line"], "ALPHA-002")
	assert.NotContains(t, offer.Payload["line"], "ALPHA-001")
	assert.Equal(t, calls, runner.calls(), "an offer starts nothing")
}

func TestWatch_TheOfferIsMadeOnceAcrossARestart(t *testing.T) {
	c, st, _, rec, ws, fake := startWatchedTwoCards(t)

	fake.setCards("ALPHA-002", cm.Card{ID: "ALPHA-002", State: "stalled"})
	rec.waitFor(t, EventRetroStopOffered)

	c2, rec2 := reopen(t, c, st, ws, twoCardScript(), fake, startInput("BP1"))
	require.NotNil(t, watcherOf(c2, "BP1"))

	gets := fake.getCount()

	require.Eventually(t, func() bool { return fake.getCount() > gets+6 }, time.Second, 5*time.Millisecond)

	n, err := st.CountEvents(context.Background(), "BP1", EventRetroStopOffered)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Zero(t, rec2.count(EventRetroStopOffered))
}

func TestWatch_OffersOnThePollThatEndsTheRun(t *testing.T) {
	_, _, _, rec, _, fake := startWatchedTwoCards(t)

	fake.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "blocked"})
	fake.setBooks("ship-the-parser", cm.Playbook{ID: "ship-the-parser", Total: 3, Complete: 3, Run: &cm.PlaybookRun{Status: "completed"}})

	rec.waitFor(t, EventWatchEnded)

	kinds := rec.kinds()
	offered, ended := -1, -1

	for i, k := range kinds {
		switch k {
		case EventRetroStopOffered:
			offered = i
		case EventWatchEnded:
			ended = i
		}
	}

	require.GreaterOrEqual(t, offered, 0, "the offer was made")
	assert.Less(t, offered, ended)
	assert.Equal(t, 1, rec.count(EventRetroStopOffered))
}

func TestWatch_ARunningCardIsNoStop(t *testing.T) {
	_, _, _, rec, _, fake := startWatchedTwoCards(t)

	fake.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "in_progress", WorkerStatus: "running", Phase: "execute"})
	fake.setCards("ALPHA-002", cm.Card{ID: "ALPHA-002", State: "done", WorkerStatus: "completed"})

	gets := fake.getCount()

	require.Eventually(t, func() bool { return fake.getCount() > gets+6 }, time.Second, 5*time.Millisecond)
	assert.Zero(t, rec.count(EventRetroStopOffered))
}

func TestWatch_TheEndCountsThePlaybooksFollowups(t *testing.T) {
	script := twoCardScript()
	script["decomposition"] = submitting(followupCards(""))

	c, _, _, rec, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	// The draft held one follow-up; a person added a second in
	// ContextMatrix during the run, so the end counts what the playbook holds.
	fake.setBooks("ship-the-parser", cm.Playbook{
		ID: "ship-the-parser", Total: 2, Complete: 2, Run: &cm.PlaybookRun{Status: "completed"},
		Followups: []cm.PlaybookFollowup{{ID: "f1", Text: "Check the dashboard"}, {ID: "f2", Text: "Read the exported report"}},
	})
	rec.waitFor(t, EventWatchEnded)

	end, ok := rec.first(EventWatchEnded)
	require.True(t, ok)
	assert.EqualValues(t, 2, end.Payload["followups"])
	assert.Empty(t, end.Payload["manual"], "a playbook's follow-ups are not listed")
}

func TestWatch_AnEndWithoutFollowupsCountsNone(t *testing.T) {
	// The draft holds a follow-up, but the playbook ContextMatrix serves has
	// none: an older ContextMatrix returns no followups, or a person removed
	// it.
	script := twoCardScript()
	script["decomposition"] = submitting(followupCards(""))

	c, _, _, rec, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	fake.setBooks("ship-the-parser", cm.Playbook{ID: "ship-the-parser", Total: 3, Complete: 3, Run: &cm.PlaybookRun{Status: "completed"}})
	rec.waitFor(t, EventWatchEnded)

	end, ok := rec.first(EventWatchEnded)
	require.True(t, ok)
	assert.NotContains(t, end.Payload, "followups")
}

// deletedCards are the end payload's cards flagged deleted.
func deletedCards(t *testing.T, end Event) []string {
	t.Helper()

	var out []string

	for _, raw := range anyList(end.Payload["cards"]) {
		card, ok := raw.(map[string]any)
		require.True(t, ok)

		if deleted, _ := card["deleted"].(bool); deleted {
			out = append(out, card["card"].(string))
		}
	}

	return out
}

func TestWatch_ACardDeletedFromThePlaybookIsReportedOnceAndNoLongerRead(t *testing.T) {
	_, st, _, rec, _, fake := startWatchedTwoCards(t)

	// The project's get_card answer is not "card not found" (the project
	// itself may be gone), so the entry's flag alone marks the card deleted.
	entries := []cm.PlaybookEntry{
		{ID: "e1", Type: "card", Project: "alpha", Card: "ALPHA-001", Complete: true},
		{ID: "e2", Type: "card", Project: "alpha", Card: "ALPHA-002", Missing: true},
	}
	fake.setBooks("ship-the-parser", cm.Playbook{
		ID: "ship-the-parser", Entries: entries, Total: 2, Complete: 1,
		Run: &cm.PlaybookRun{Status: "waiting", Entry: "e2", Reason: "alpha/ALPHA-002 no longer exists"},
	})

	const deleted = "ALPHA-002 was deleted in ContextMatrix"

	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(watchLines(rec), "\n"), deleted)
	}, time.Second, 5*time.Millisecond)

	reads := fake.cardReads("ALPHA-002")
	gets := fake.getCount()

	require.Eventually(t, func() bool { return fake.getCount() > gets+6 }, time.Second, 5*time.Millisecond)

	lines := strings.Join(watchLines(rec), "\n")
	assert.Equal(t, 1, strings.Count(lines, deleted), "a deleted card is reported once")
	assert.Equal(t, 1, strings.Count(lines, "needs a human: alpha/ALPHA-002 no longer exists"), "the run follows ContextMatrix's own status")
	assert.Equal(t, reads, fake.cardReads("ALPHA-002"), "a deleted card is not read again")

	// A person removes the entry in ContextMatrix, and the run completes.
	fake.setBooks("ship-the-parser", cm.Playbook{
		ID: "ship-the-parser", Entries: entries[:1], Total: 1, Complete: 1, Run: &cm.PlaybookRun{Status: "completed"},
	})
	rec.waitFor(t, EventWatchEnded)

	end, ok := rec.first(EventWatchEnded)
	require.True(t, ok)
	assert.Equal(t, []string{"ALPHA-002"}, deletedCards(t, end))
	assert.NotContains(t, end.Payload, "playbook_deleted")
	assert.Equal(t, reads, fake.cardReads("ALPHA-002"), "a card no longer in the playbook stays deleted")
	assert.Equal(t, string(StateDone), stateOf(t, st, "BP1"))
}

func TestWatch_ADeletedCardStaysDeletedAcrossARestart(t *testing.T) {
	c, st, _, rec, ws, fake := startWatchedTwoCards(t)

	fake.remove("ALPHA-002")

	const deleted = "ALPHA-002 was deleted in ContextMatrix"

	require.Eventually(t, func() bool {
		return strings.Contains(strings.Join(watchLines(rec), "\n"), deleted)
	}, time.Second, 5*time.Millisecond)

	c2, rec2 := reopen(t, c, st, ws, twoCardScript(), fake, startInput("BP1"))
	require.NotNil(t, watcherOf(c2, "BP1"), "the watch goes on")

	reads, kept := fake.cardReads("ALPHA-002"), fake.cardReads("ALPHA-001")

	require.Eventually(t, func() bool { return fake.cardReads("ALPHA-001") > kept+3 }, time.Second, 5*time.Millisecond)
	assert.Equal(t, reads, fake.cardReads("ALPHA-002"), "the stored state keeps it deleted")
	assert.Equal(t, 1, strings.Count(strings.Join(watchLines(rec), "\n"), deleted))
	assert.Empty(t, watchLines(rec2), "nothing changed while nobody watched")
}

func TestWatch_ADeletedSingleCardEndsTheWatch(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	drive(t, c, "BP1")
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	fake.remove("ALPHA-001")
	rec.waitFor(t, EventWatchEnded)

	lines := watchLines(rec)
	require.NotEmpty(t, lines)
	assert.Equal(t, "ALPHA-001 was deleted in ContextMatrix", lines[len(lines)-1], "the final line")

	end, ok := rec.first(EventWatchEnded)
	require.True(t, ok)
	assert.Equal(t, []string{"ALPHA-001"}, deletedCards(t, end))
	assert.Equal(t, string(StateDone), stateOf(t, st, "BP1"))
	require.Eventually(t, func() bool { return watcherOf(c, "BP1") == nil }, time.Second, 5*time.Millisecond, "the watch ended")
}

func TestWatch_ADeletedPlaybookEndsTheWatch(t *testing.T) {
	c, st, _, rec, _, fake := startWatchedTwoCards(t)

	fake.remove("ship-the-parser")
	rec.waitFor(t, EventWatchEnded)

	lines := watchLines(rec)
	require.NotEmpty(t, lines)
	assert.Equal(t, "Playbook ship-the-parser was deleted in ContextMatrix.", lines[len(lines)-1], "the final line")

	end, ok := rec.first(EventWatchEnded)
	require.True(t, ok)
	assert.Equal(t, true, end.Payload["playbook_deleted"])
	assert.Empty(t, deletedCards(t, end), "the cards are still there")
	assert.Equal(t, string(StateDone), stateOf(t, st, "BP1"))
	require.Eventually(t, func() bool { return watcherOf(c, "BP1") == nil }, time.Second, 5*time.Millisecond, "the watch ended")
}

// A read that fails for any other reason, such as ContextMatrix being down,
// is retried and never taken for a deletion.
func TestWatch_AFailedReadIsNoDeletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*fakeCM)
	}{
		{name: "a card read", fail: func(f *fakeCM) { f.failGets = 3 }},
		{name: "a playbook read", fail: func(f *fakeCM) { f.failBooks = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, rec, _, fake := startWatchedTwoCards(t)

			fake.mu.Lock()
			tc.fail(fake)
			fake.mu.Unlock()

			require.Eventually(t, func() bool {
				fake.mu.Lock()
				defer fake.mu.Unlock()

				return fake.failGets == 0 && fake.failBooks == 0
			}, time.Second, 5*time.Millisecond, "every failure was served")

			fake.setBooks("ship-the-parser", cm.Playbook{ID: "ship-the-parser", Total: 2, Complete: 2, Run: &cm.PlaybookRun{Status: "completed"}})
			rec.waitFor(t, EventWatchEnded)

			assert.NotContains(t, strings.Join(watchLines(rec), "\n"), "deleted")

			end, ok := rec.first(EventWatchEnded)
			require.True(t, ok)
			assert.NotContains(t, end.Payload, "playbook_deleted")
			assert.Empty(t, deletedCards(t, end))
		})
	}
}

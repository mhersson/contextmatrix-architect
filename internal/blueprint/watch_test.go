package blueprint

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
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

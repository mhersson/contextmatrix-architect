package blueprint

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

const needBody = "## Problem\nParsing is slow.\n## Value\nFaster builds.\n## Assumptions\n- The parser stays in Go.\n"

func cardBody(change string) string {
	return "## Context\nwhy\n## Where\ninternal/x.go\n## Change\n" + change + "\n## Acceptance criteria\n1. go test ./... passes.\n## Tests expected\nTestThing.\n## Out of scope\nrest.\n"
}

// twoCards is a draft of two alpha cards, the second depending on the
// first, with a manual check between them.
func twoCards() map[string]any {
	card := func(id, title string, deps ...any) map[string]any {
		return map[string]any{
			"id": id, "project": "alpha", "title": title, "type": "feature", "priority": "medium",
			"depends_on": append([]any{}, deps...), "body": cardBody(title),
			"where": []any{map[string]any{"path": "internal/x.go", "symbol": "Thing"}},
		}
	}

	return map[string]any{
		"title":  "Ship the parser",
		"cards":  []any{card("D1", "Add the parser"), card("D2", "Wire the parser", "D1")},
		"manual": []any{map[string]any{"id": "M1", "text": "Check the dashboard"}},
		"order":  []any{"D1", "M1", "D2"},
	}
}

func readinessFor(ready bool, ids ...string) map[string]any {
	cards := make([]any, 0, len(ids))
	for _, id := range ids {
		cards = append(cards, map[string]any{"id": id, "ready": ready, "findings": []any{}})
	}

	return map[string]any{"cards": cards}
}

func twoCardScript() stageScript {
	return stageScript{
		"intake":        returns(needBody, map[string]any{"slices": 1}),
		"grounding":     returns("# Grounding", nil),
		"decomposition": returns("# Cards", twoCards()),
		"readiness":     returns("# Ready", readinessFor(true, "D1", "D2")),
	}
}

func emitNow(t *testing.T, c *Controller, st *store.Store, id string) {
	t.Helper()

	bp, err := st.GetBlueprint(context.Background(), id)
	require.NoError(t, err)

	c.emit(context.Background(), sessionOf(t, c, id), bp)
}

func TestEmit_CreatesTheCardsThenThePlaybook(t *testing.T) {
	c, st, _, rec, ws := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")

	closesBefore := fake.closeCount()

	emitNow(t, c, st, "BP1")

	cards, books := fake.inputs()
	require.Len(t, cards, 2)
	assert.Equal(t, "Add the parser", cards[0].Title)
	assert.Equal(t, "Wire the parser", cards[1].Title)
	assert.Equal(t, []string{"ALPHA-001"}, cards[1].DependsOn, "depends_on carries the CM id")
	assert.Equal(t, "medium", cards[1].Priority)
	assert.Equal(t, "human:architect", cards[0].AgentID)
	assert.NotContains(t, cards[0].Body, "Parsing is slow.", "several cards: the summary goes in the playbook")

	require.Len(t, books, 1)
	assert.Equal(t, "Ship the parser", books[0].Title)
	assert.Equal(t, "human:architect", books[0].AgentID)
	assert.Equal(t, []cm.PlaybookEntryInput{
		{Type: "card", Project: "alpha", Card: "ALPHA-001"},
		{Type: "manual", Text: "Check the dashboard"},
		{Type: "card", Project: "alpha", Card: "ALPHA-002"},
	}, books[0].Entries)
	assert.Contains(t, books[0].Description, "Parsing is slow.")
	assert.Contains(t, books[0].Description, "The parser stays in Go.")

	recs, err := st.ListEmissions(context.Background(), "BP1")
	require.NoError(t, err)
	require.Len(t, recs, 3)
	assert.Equal(t, store.Emission{BlueprintID: "BP1", Kind: store.EmitPlaybook, CMID: "ship-the-parser", CreatedAt: recs[2].CreatedAt}, recs[2])

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateEmitted), bp.State)
	assert.Contains(t, ws.removed, "BP1", "the trees are removed at emission")

	ev, ok := rec.first(EventEmitted)
	require.True(t, ok)
	assert.Equal(t, "ship-the-parser", ev.Payload["playbook"])
	assert.Equal(t, closesBefore+1, fake.closeCount(), "the MCP session is closed")
}

func TestEmit_ResumesAfterAFailureWithoutDuplicates(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	fake.failCreate["Wire the parser"] = 1
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	emitNow(t, c, st, "BP1")

	assert.Equal(t, []string{"Add the parser"}, fake.titles())

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "Emission stopped")
	assert.False(t, rec.has(EventEmitted))

	emitNow(t, c, st, "BP1")

	assert.Equal(t, []string{"Add the parser", "Wire the parser"}, fake.titles(), "only the missing card is created")

	_, books := fake.inputs()
	assert.Len(t, books, 1)
	assert.True(t, rec.has(EventEmitted))
}

// ContextMatrix may finish a write whose caller gave up, so a cancel waits
// for the write in flight: its card is recorded, emission stops before the
// next call, and the next emission creates every card exactly once.
func TestEmit_ACancelWaitsForTheCreateInFlight(t *testing.T) {
	c, st, _, _, _ := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	hold := newHoldCM(t, fake)
	hold.title = "Wire the parser"
	c.opts.ConnectCM = hold.connect
	ctx := context.Background()

	drive(t, c, "BP1")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)

	sess := sessionOf(t, c, "BP1")
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		c.emit(runCtx, sess, bp)
	}()

	receive(t, hold.entered, "the held card")
	cancel()
	hold.free()
	receive(t, done, "the stopped emission")

	recs, err := st.ListEmissions(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, recs, 2, "the card the cancel waited for is recorded")
	assert.Equal(t, "D2", recs[1].DraftID)
	assert.Equal(t, "ALPHA-002", recs[1].CMID)

	_, books := fake.inputs()
	assert.Empty(t, books, "the cancel stops emission before the playbook")

	emitNow(t, c, st, "BP1")

	assert.Equal(t, []string{"Add the parser", "Wire the parser"}, fake.titles(), "each card exists once")

	_, books = fake.inputs()
	assert.Len(t, books, 1)
}

func TestEmit_AWarningIsReportedAndEmissionContinues(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	fake.warnings["Add the parser"] = []string{"reference to another project's repo \"acme/beta\""}
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	emitNow(t, c, st, "BP1")

	assert.Len(t, fake.titles(), 2, "emission continued")

	ev, ok := rec.first(EventCardCreated)
	require.True(t, ok)
	assert.Equal(t, []string{"reference to another project's repo \"acme/beta\""}, ev.Payload["warnings"])
}

func TestEmit_ASingleCardCarriesTheSummaryAndHasNoPlaybook(t *testing.T) {
	data := goodCards()
	data["manual"] = []any{map[string]any{"id": "M1", "text": "Check the dashboard"}}
	data["order"] = []any{"D1", "M1"}

	script := happyScript()
	script["intake"] = returns(needBody, map[string]any{"slices": 1})
	script["decomposition"] = returns("# Cards", data)

	c, st, _, rec, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	emitNow(t, c, st, "BP1")

	cards, books := fake.inputs()
	require.Len(t, cards, 1)
	assert.Empty(t, books)

	where := strings.Index(cards[0].Body, "## Where")
	require.Positive(t, where)

	ctxSection := cards[0].Body[:where]
	assert.Contains(t, ctxSection, "Parsing is slow.", "the problem goes in the Context section")
	assert.Contains(t, ctxSection, "The parser stays in Go.")

	ev, ok := rec.first(EventEmitted)
	require.True(t, ok)
	assert.Equal(t, []string{"Check the dashboard"}, ev.Payload["manual"])
}

func TestEmit_WithoutAClientIsAClearError(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	c.opts.ConnectCM = nil

	drive(t, c, "BP1")
	emitNow(t, c, st, "BP1")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "ContextMatrix")
}

func TestDraftProblems_ADependencyComesFirst(t *testing.T) {
	d := contract.Draft{
		Title: "T",
		Cards: []contract.Card{{ID: "D1", Project: "alpha", Title: "A"}, {ID: "D2", Project: "alpha", Title: "B", DependsOn: []string{"D1"}}},
		Order: []string{"D2", "D1"},
	}

	assert.Equal(t, []string{"D2 comes before its dependency D1 in order"}, draftProblems(d))
}

func TestDraftProblems_AMissingCardIsNotAlsoOutOfOrder(t *testing.T) {
	d := contract.Draft{
		Title: "T",
		Cards: []contract.Card{{ID: "D1", Project: "alpha", Title: "A"}, {ID: "D2", Project: "alpha", Title: "B", DependsOn: []string{"D1"}}},
		Order: []string{"D1"},
	}

	assert.Equal(t, []string{"D2 is missing from order"}, draftProblems(d))
}

func yoloInput(id string) StartInput {
	in := startInput(id)
	in.Mode = protocol.ArchitectModeYolo

	return in
}

func TestYolo_StartsOneCardWithRunCard(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	driveIn(t, c, yoloInput("BP1"))

	assert.Equal(t, []string{"alpha/ALPHA-001|human:architect"}, fake.runCards)
	assert.Empty(t, fake.runBooks)

	recs, err := st.ListEmissions(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, store.EmitRun, recs[len(recs)-1].Kind)
	assert.Equal(t, "ALPHA-001", recs[len(recs)-1].CMID)
	assert.True(t, rec.has(EventRunStarted))

	ev, ok := rec.first(EventEmitted)
	require.True(t, ok)
	assert.Equal(t, true, ev.Payload["run_follows"], "the run start follows the announcement")
}

func TestYolo_AnEmptyRepositoryEmitsTheCardsAndStartsNoRun(t *testing.T) {
	c, st, _, rec, ws := newPipeline(t, happyScript())
	ws.empty = map[string]bool{"alpha": true}
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	driveIn(t, c, yoloInput("BP1"))
	assert.Nil(t, watcherOf(c, "BP1"), "no watch while the run is held")

	assert.Len(t, fake.titles(), 1, "the card is created")
	assert.Empty(t, fake.cardRuns())
	assert.Empty(t, fake.runBooks)
	assert.False(t, rec.has(EventRunStarted))

	recs, err := st.ListEmissions(context.Background(), "BP1")
	require.NoError(t, err)

	for _, e := range recs {
		assert.NotEqual(t, store.EmitRun, e.Kind, "no run start is recorded")
	}

	ev, ok := rec.first(EventEmitted)
	require.True(t, ok)
	assert.Equal(t, false, ev.Payload["run_follows"])

	var prompt string

	for _, a := range rec.all(EventAwaitInput) {
		if p, _ := a.Payload["prompt"].(string); strings.Contains(p, "has no commits") {
			prompt = p
		}
	}

	assert.Contains(t, prompt, "https://example.com/alpha.git has no commits")

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateEmitted), bp.State)
}

func TestYolo_AFirstCommitThenApproveStartsTheRun(t *testing.T) {
	c, _, _, rec, ws := newPipeline(t, happyScript())
	ws.empty = map[string]bool{"alpha": true}
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	driveIn(t, c, yoloInput("BP1"))
	require.Empty(t, fake.cardRuns())
	assert.Nil(t, watcherOf(c, "BP1"), "no watch while the run is held")

	ws.mu.Lock()
	ws.heads = map[string]string{"https://example.com/alpha.git": "sha-first"}
	ws.mu.Unlock()

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	assert.Len(t, fake.cardRuns(), 1)
	assert.True(t, rec.has(EventRunStarted))
	assert.Len(t, fake.titles(), 1, "no card is created twice")
	assert.NotNil(t, watcherOf(c, "BP1"), "the recorded run start starts the watch")

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m4"))
	c.Wait("BP1")

	assert.Len(t, fake.cardRuns(), 1, "a started run is never started again")
}

func TestEmptyReposPrompt(t *testing.T) {
	tests := []struct {
		name  string
		repos []string
		want  string
	}{
		{
			name: "one repository", repos: []string{"a"},
			want: "a has no commits, so the run was not started; push an initial commit, then reply approve to start it, or start it in ContextMatrix and reply approve.",
		},
		{
			name: "several repositories", repos: []string{"a", "b"},
			want: "a, b have no commits, so the run was not started; push an initial commit to each, then reply approve to start it, or start it in ContextMatrix and reply approve.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, emptyReposPrompt(tt.repos))
		})
	}
}

func TestYolo_StartsSeveralCardsWithRunPlaybook(t *testing.T) {
	c, _, _, _, _ := newPipeline(t, twoCardScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	driveIn(t, c, yoloInput("BP1"))

	assert.Equal(t, []string{"ship-the-parser|human:architect"}, fake.runBooks)
	assert.Empty(t, fake.runCards)
}

func TestYolo_ARefusedStartIsReportedAndApproveRetriesItOnce(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	fake.runErr = errors.New("cm: run_card: no execution backend is configured")
	c.opts.ConnectCM = fake.connect
	c.opts.WatchInterval = 5 * time.Millisecond
	ctx := context.Background()

	driveIn(t, c, yoloInput("BP1"))
	assert.Nil(t, watcherOf(c, "BP1"), "a refused start starts no watch")

	ev, ok := rec.first(EventError)
	require.True(t, ok)
	assert.Contains(t, ev.Payload["message"], "no execution backend is configured")

	bp, err := st.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, string(StateEmitted), bp.State, "the cards exist")

	fake.mu.Lock()
	fake.runErr = nil
	fake.mu.Unlock()

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
	c.Wait("BP1")

	assert.NotNil(t, watcherOf(c, "BP1"), "the retry starts the watch")
	assert.Len(t, fake.runCards, 2, "the refused start, then the retry")
	assert.Len(t, fake.titles(), 1, "no card is created twice")

	require.NoError(t, c.Message(ctx, "BP1", "approve", "m4"))
	c.Wait("BP1")

	assert.Len(t, fake.runCards, 2, "a started run is never started again")
}

// ContextMatrix may start a run whose caller gave up, so a cancel waits for
// the run start in flight: the start is recorded and never made twice.
func TestYolo_AnAbortDuringTheRunStartRecordsItOnce(t *testing.T) {
	cases := []struct {
		name   string
		script stageScript
		target string
	}{
		{name: "run_card", script: happyScript(), target: "ALPHA-001"},
		{name: "run_playbook", script: twoCardScript(), target: "ship-the-parser"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _, rec, _ := newPipeline(t, tc.script)
			fake := newFakeCM()
			hold := newHoldCM(t, fake)
			hold.run = true
			c.opts.ConnectCM = hold.connect
			ctx := context.Background()

			require.NoError(t, c.Start(ctx, yoloInput("BP1")))
			require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
			c.Wait("BP1")
			require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
			receive(t, hold.entered, "the held run start")

			require.NoError(t, c.Abort(ctx, "BP1", "m3"))
			hold.free()
			c.Wait("BP1")

			recs, err := st.ListEmissions(ctx, "BP1")
			require.NoError(t, err)

			last := recs[len(recs)-1]
			assert.Equal(t, store.EmitRun, last.Kind, "the start the abort waited for is recorded")
			assert.Equal(t, tc.target, last.CMID)
			assert.True(t, rec.has(EventRunStarted))

			require.NoError(t, c.Message(ctx, "BP1", "approve", "m4"))
			c.Wait("BP1")

			assert.Len(t, slices.Concat(fake.runCards, fake.runBooks), 1, "the run is started once")
		})
	}
}

// After a refused start, a human may start the run in ContextMatrix; approve
// then records that run instead of starting another. A card whose trigger
// failed in todo, or a playbook run stopped there, is started again.
func TestYolo_ApproveRecordsARunAlreadyInContextMatrix(t *testing.T) {
	cases := []struct {
		name     string
		script   stageScript
		inCM     func(f *fakeCM)
		runs     int
		existing bool
	}{
		{
			name: "a queued card", script: happyScript(), runs: 1, existing: true,
			inCM: func(f *fakeCM) {
				f.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "todo", WorkerStatus: "queued"})
			},
		},
		{
			name: "a card past todo", script: happyScript(), runs: 1, existing: true,
			inCM: func(f *fakeCM) { f.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "review"}) },
		},
		{
			name: "a card whose trigger failed", script: happyScript(), runs: 2,
			inCM: func(f *fakeCM) {
				f.setCards("ALPHA-001", cm.Card{ID: "ALPHA-001", State: "todo", WorkerStatus: "failed"})
			},
		},
		{
			name: "a running playbook", script: twoCardScript(), runs: 1, existing: true,
			inCM: func(f *fakeCM) {
				f.setBooks("ship-the-parser", cm.Playbook{ID: "ship-the-parser", Total: 3, Run: &cm.PlaybookRun{Status: "running"}})
			},
		},
		{
			name: "a stopped playbook", script: twoCardScript(), runs: 2,
			inCM: func(f *fakeCM) {
				f.setBooks("ship-the-parser", cm.Playbook{ID: "ship-the-parser", Total: 3, Run: &cm.PlaybookRun{Status: "stopped"}})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _, rec, _ := newPipeline(t, tc.script)
			fake := newFakeCM()
			fake.runErr = errors.New("cm: run_card: no execution backend is configured")
			c.opts.ConnectCM = fake.connect
			ctx := context.Background()

			driveIn(t, c, yoloInput("BP1"))
			require.False(t, rec.has(EventRunStarted), "the first start is refused")

			fake.mu.Lock()
			fake.runErr = nil
			fake.mu.Unlock()
			tc.inCM(fake)

			require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
			c.Wait("BP1")

			assert.Len(t, slices.Concat(fake.runCards, fake.runBooks), tc.runs)

			recs, err := st.ListEmissions(ctx, "BP1")
			require.NoError(t, err)
			assert.Equal(t, store.EmitRun, recs[len(recs)-1].Kind, "the start is recorded")

			ev, ok := rec.first(EventRunStarted)
			require.True(t, ok)
			assert.Equal(t, tc.existing, ev.Payload["existing"])
			assert.NotNil(t, watcherOf(c, "BP1"), "the run is watched")
		})
	}
}

// resentCM starts the run, then fails the call: the MCP client re-sends a
// call after a dropped connection, and ContextMatrix refuses the re-send of
// a start it made.
type resentCM struct {
	*fakeCM
}

func (r resentCM) connect(context.Context, string) (CMClient, error) {
	return r, nil
}

func (r resentCM) RunCard(ctx context.Context, project, id, agentID string) (cm.Card, error) {
	card, err := r.fakeCM.RunCard(ctx, project, id, agentID)
	if err != nil {
		return cm.Card{}, err
	}

	r.setCards(id, card)

	return cm.Card{}, errors.New("cm: run_card: card is already being executed by a worker")
}

func (r resentCM) RunPlaybook(ctx context.Context, id, agentID string) error {
	if err := r.fakeCM.RunPlaybook(ctx, id, agentID); err != nil {
		return err
	}

	r.setBooks(id, cm.Playbook{ID: id, Total: 3, Run: &cm.PlaybookRun{Status: "running"}})

	return errors.New("cm: run_playbook: a running or waiting run is refused")
}

// A run start whose call fails after ContextMatrix started the run is the
// start: it is recorded once, not reported as a run that did not start.
func TestYolo_AStartThatFailsAfterTheRunStartedIsRecorded(t *testing.T) {
	cases := []struct {
		name   string
		script stageScript
		target string
	}{
		{name: "run_card", script: happyScript(), target: "ALPHA-001"},
		{name: "run_playbook", script: twoCardScript(), target: "ship-the-parser"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _, rec, _ := newPipeline(t, tc.script)
			fake := resentCM{fakeCM: newFakeCM()}
			c.opts.ConnectCM = fake.connect
			ctx := context.Background()

			driveIn(t, c, yoloInput("BP1"))

			assert.False(t, rec.has(EventError), "no failed start is reported")

			starts := rec.all(EventRunStarted)
			require.Len(t, starts, 1, "the start is recorded once")
			assert.Equal(t, tc.target, starts[0].Payload["target"])
			assert.Equal(t, true, starts[0].Payload["existing"])

			recs, err := st.ListEmissions(ctx, "BP1")
			require.NoError(t, err)

			runs := slices.DeleteFunc(recs, func(e store.Emission) bool { return e.Kind != store.EmitRun })
			require.Len(t, runs, 1)
			assert.Equal(t, tc.target, runs[0].CMID)
			assert.NotNil(t, watcherOf(c, "BP1"), "the run is watched")

			require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
			c.Wait("BP1")

			assert.Len(t, slices.Concat(fake.runCards, fake.runBooks), 1, "the run is started once")
		})
	}
}

func TestDefaultMode_NeverStartsARun(t *testing.T) {
	c, _, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m3"))
	c.Wait("BP1")

	assert.Len(t, fake.titles(), 1)
	assert.Empty(t, fake.runCards)

	ev, ok := rec.first(EventEmitted)
	require.True(t, ok)
	assert.Equal(t, false, ev.Payload["run_follows"])
}

// An emission stopped by abort or End says so; the cancel stays in the log.
// One a ContextMatrix error stopped shows the error.
func TestEmit_AStopSaysWhatStoppedIt(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name string
		stop func(t *testing.T, c *Controller, fake *fakeCM)
		want string
	}{
		{
			name: "an abort",
			stop: func(t *testing.T, c *Controller, _ *fakeCM) {
				t.Helper()

				require.NoError(t, c.Abort(ctx, "BP1", "m4"))
			},
			want: "Emission stopped by abort. Cards already created are kept; reply approve to resume.",
		},
		{
			name: "End",
			stop: func(t *testing.T, c *Controller, _ *fakeCM) {
				t.Helper()

				require.NoError(t, c.End(ctx, "BP1"))
			},
			want: "Emission stopped by abort. Cards already created are kept; reply approve to resume.",
		},
		{
			name: "a ContextMatrix error",
			stop: func(t *testing.T, _ *Controller, fake *fakeCM) {
				t.Helper()

				fake.mu.Lock()
				fake.failCreate["Wire the parser"] = 1
				fake.mu.Unlock()
			},
			want: "Emission stopped: create D2: cm: create_card: remote unreachable. Cards already created are kept; reply approve to resume.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, st, _, rec, _ := newPipeline(t, twoCardScript())
			fake := newFakeCM()
			hold := newHoldCM(t, fake)
			hold.title = "Add the parser"
			c.opts.ConnectCM = hold.connect

			drive(t, c, "BP1")
			require.NoError(t, c.Message(ctx, "BP1", "approve", "m3"))
			receive(t, hold.entered, "the held card")
			tc.stop(t, c, fake)
			hold.free()
			c.Wait("BP1")

			errs := rec.all(EventError)
			require.Len(t, errs, 1)
			assert.Equal(t, tc.want, errs[0].Payload["message"])
			assert.Equal(t, []string{"Add the parser"}, fake.titles())
			assert.Equal(t, string(StateIdle), stateOf(t, st, "BP1"))
		})
	}
}

// An abort that lands after the cards exist stops before the run start and
// says so; the cancel stays in the log.
func TestYolo_AnAbortBeforeTheRunStartSaysSo(t *testing.T) {
	c, st, _, rec, _ := newPipeline(t, happyScript())
	fake := newFakeCM()
	hold := newHoldCM(t, fake)
	hold.title = "Add the thing"
	c.opts.ConnectCM = hold.connect
	ctx := context.Background()

	require.NoError(t, c.Start(ctx, yoloInput("BP1")))
	require.NoError(t, c.Message(ctx, "BP1", "the need", "m1"))
	c.Wait("BP1")
	require.NoError(t, c.Message(ctx, "BP1", "the design", "m2"))
	receive(t, hold.entered, "the held card")
	require.NoError(t, c.Abort(ctx, "BP1", "m3"))
	hold.free()
	c.Wait("BP1")

	errs := rec.all(EventError)
	require.Len(t, errs, 1)
	assert.Equal(t, "The cards are emitted, but the run start was stopped by abort. Reply approve to retry the start, or start it in ContextMatrix and reply approve.", errs[0].Payload["message"])
	assert.Empty(t, fake.cardRuns(), "no run is started")
	assert.Equal(t, []string{"Add the thing"}, fake.titles())
	assert.Equal(t, string(StateEmitted), stateOf(t, st, "BP1"))
}

// The fake checkout records "sha-<tree>", so a repository line carries its
// first seven characters.
const (
	alphaSourceLine = "- S1: https://example.com/alpha.git at sha-alp"
	boardSourceLine = "- S2: the ContextMatrix board"
)

func TestEmit_CardBodiesSayWhatTheirSourceTagsMean(t *testing.T) {
	tests := []struct {
		name  string
		why   string // replaces "why" in the card's Context section
		has   []string
		lacks []string
	}{
		{name: "a repository tag", why: "why [S1 internal/x.go]", has: []string{"## Sources", alphaSourceLine}, lacks: []string{boardSourceLine}},
		{name: "two tags in one citation", why: "why [S1, S2]", has: []string{alphaSourceLine, boardSourceLine}},
		{name: "a tag that names no source", why: "why [S9]", lacks: []string{"## Sources"}},
		{name: "no tags", why: "why", lacks: []string{"## Sources"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			draft := goodCards()
			card := draft["cards"].([]any)[0].(map[string]any)
			card["body"] = strings.Replace(card["body"].(string), "why", tt.why, 1)

			script := happyScript()
			script["decomposition"] = returns("# Cards", draft)

			c, st, _, _, _ := newPipeline(t, script)
			fake := newFakeCM()
			c.opts.ConnectCM = fake.connect

			drive(t, c, "BP1")
			emitNow(t, c, st, "BP1")

			cards, _ := fake.inputs()
			require.Len(t, cards, 1)

			for _, s := range tt.has {
				assert.Contains(t, cards[0].Body, s)
			}

			for _, s := range tt.lacks {
				assert.NotContains(t, cards[0].Body, s)
			}
		})
	}
}

func TestEmit_OnlyACardThatCitesGetsASourcesSection(t *testing.T) {
	draft := twoCards()
	first := draft["cards"].([]any)[0].(map[string]any)
	first["body"] = strings.Replace(first["body"].(string), "why", "why [S1]", 1)

	script := twoCardScript()
	script["decomposition"] = returns("# Cards", draft)

	c, st, _, _, _ := newPipeline(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	emitNow(t, c, st, "BP1")

	cards, _ := fake.inputs()
	require.Len(t, cards, 2)
	assert.Contains(t, cards[0].Body, alphaSourceLine)
	assert.NotContains(t, cards[1].Body, "## Sources")
}

func TestEmit_AnotherProjectsRepositoryIsNotNamed(t *testing.T) {
	beta := cm.Project{Name: "beta", Repo: "https://example.com/beta.git", Verify: &cm.Verify{Command: "make test"}}

	draft := goodCards()
	card := draft["cards"].([]any)[0].(map[string]any)
	card["body"] = strings.Replace(card["body"].(string), "why", "why [S1], matching the client [S2]", 1)

	script := happyScript()
	script["decomposition"] = returns("# Cards", draft)

	c, st, _, _, _ := newPipeline(t, script)
	c.opts.Projects = func(context.Context, string) ([]cm.Project, error) {
		return []cm.Project{alphaProject(), beta}, nil
	}

	fake := newFakeCM()
	fake.projects = []cm.Project{alphaProject(), beta}
	c.opts.ConnectCM = fake.connect

	in := startInput("BP1")
	in.Projects = []string{"alpha", "beta"}
	driveIn(t, c, in)
	emitNow(t, c, st, "BP1")

	cards, _ := fake.inputs()
	require.Len(t, cards, 1)
	assert.Contains(t, cards[0].Body, alphaSourceLine)
	assert.Contains(t, cards[0].Body, "- S2: another project's repository at sha-bet")
	assert.NotContains(t, cards[0].Body, "https://example.com/beta.git", "the contract keeps other projects' repositories out of a card")
}

func TestSourceLine(t *testing.T) {
	own := []string{"https://example.com/alpha.git"}

	tests := []struct {
		name string
		src  store.Source
		want string
	}{
		{
			name: "own repository cut to seven characters",
			src:  store.Source{Number: 1, Kind: "repo", Identity: "https://example.com/alpha.git", Version: "0123456789abcdef0123456789abcdef01234567"},
			want: "S1: https://example.com/alpha.git at 0123456",
		},
		{
			name: "own repository with no commits",
			src:  store.Source{Number: 1, Kind: "repo", Identity: "https://example.com/alpha.git"},
			want: "S1: https://example.com/alpha.git with no commits",
		},
		{
			name: "another project's repository",
			src:  store.Source{Number: 2, Kind: "repo", Identity: "https://example.com/beta.git", Version: "0123456789abcdef"},
			want: "S2: another project's repository at 0123456",
		},
		{
			name: "the board",
			src:  store.Source{Number: 3, Kind: "cm"},
			want: "S3: the ContextMatrix board, as read while the cards were written",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sourceLine(tt.src, own))
		})
	}
}

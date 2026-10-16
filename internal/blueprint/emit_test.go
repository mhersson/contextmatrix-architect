package blueprint

import (
	"context"
	"strings"
	"testing"

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

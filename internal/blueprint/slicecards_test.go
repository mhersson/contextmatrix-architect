package blueprint

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// sliceCriteria is one slice's acceptance with one criterion, criterion.
func sliceCriteria(slice, criterion string) map[string]any {
	return map[string]any{"slices": []any{map[string]any{
		"id":       slice,
		"criteria": []any{map[string]any{"id": criterion, "project": "alpha", "text": "go test ./... passes with the export step"}},
		"manual":   []any{},
	}}}
}

// withManual is draft with a manual entry id appended to its entries and
// its order.
func withManual(draft map[string]any, id string) map[string]any {
	draft["manual"] = []any{map[string]any{"id": id, "text": "open an exported report"}}
	draft["order"] = append(draft["order"].([]any), id)

	return draft
}

// cardsJudge drives BP1 on the sliced pack to its sign-off, every slice's
// acceptance holding AC<n> and SL1's cards stored as sl1, and returns a
// judge: data as the first submit of a cards validator of its own for the
// step key names, so no case is ever accepted for reaching the cap on
// failing submits. noAgents leaves AGENTS.md out of the checkout.
func cardsJudge(t *testing.T, noAgents bool, sl1 map[string]any) func(*testing.T, string, map[string]any) (*validatorState, error) {
	t.Helper()

	script := slicedScript()
	script["acceptance"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		return done("# Acceptance", sliceCriteria(sliceOf(in), "AC"+strings.TrimPrefix(sliceOf(in), "SL"))), nil
	}
	script["decomposition"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		if sliceOf(in) == "SL1" {
			return done("# Cards SL1", sl1), nil
		}

		return done("# Cards SL2", draftOf("Export the reports", cardFor("D2", []any{"AC2"}, "D1"))), nil
	}

	c, st, _, _, ws := newPipeline(t, script)
	c.opts.Pack = slicedPack(t)
	ws.noAgents = noAgents

	driveIn(t, c, autoStart("BP1"))

	bp, err := st.GetBlueprint(context.Background(), "BP1")
	require.NoError(t, err)

	sess := sessionOf(t, c, "BP1")

	return func(t *testing.T, key string, data map[string]any) (*validatorState, error) {
		t.Helper()

		step, ok := c.stepAt(key)
		require.True(t, ok, "%s is a step of the sliced pack", key)

		validate, state := c.cardsValidator(sess, bp, step)

		return state, validate(context.Background(), stage.Artifact{Body: "# Cards", Data: data})
	}
}

func TestCards_ASliceIsCheckedInsideTheEarlierSlicesCards(t *testing.T) {
	// SL1's stored D1 names a path the checkout lacks: a finding on SL1's
	// own card, never a later slice's.
	gone := cardFor("D1", []any{"AC1"})
	gone["where"] = []any{map[string]any{"path": "internal/gone.go", "symbol": "Gone"}}

	created := cardFor("D1", []any{"AC1"})
	created["where"] = []any{map[string]any{"path": "internal/created.go", "new": true}}

	unmarked := cardFor("D1", []any{"AC1"})
	unmarked["where"] = []any{map[string]any{"path": "internal/created.go"}}

	// Without AGENTS.md in the checkout, SL1 names D1 as the card that writes it.
	instructed := draftOf("", cardFor("D1", []any{"AC1"}))
	instructed["setup"] = map[string]any{"alpha": map[string]any{"instruction_card": "D1"}}

	judges := map[bool]func(*testing.T, string, map[string]any) (*validatorState, error){
		false: cardsJudge(t, false, withManual(draftOf("", gone), "M1")),
		true:  cardsJudge(t, true, instructed),
	}

	sl2 := func(title string, deps ...any) map[string]any {
		return draftOf(title, cardFor("D2", []any{"AC2"}, deps...))
	}

	dangling := sl2("Export the reports", "D1")
	dangling["setup"] = map[string]any{"alpha": map[string]any{"instruction_card": "D9"}}

	// alpha has AGENTS.md, and SL1 set no setup for it.
	overwrites := sl2("Export the reports", "D1")
	overwrites["setup"] = map[string]any{"alpha": map[string]any{"instruction_card": "D1"}}

	tests := []struct {
		name     string
		noAgents bool
		key      string
		data     map[string]any
		refused  bool
		// names is a text the refusal carries.
		names string
	}{
		{
			name: "an earlier slice needs no title, even with two cards",
			key:  "decomposition@SL1", data: draftOf("", cardFor("D1", []any{"AC1"}), cardFor("D3", nil, "D1")),
		},
		{
			name: "a card that creates a file the checkout lacks, marked new",
			key:  "decomposition@SL1", data: draftOf("", created),
		},
		{
			name: "the same file not marked new",
			key:  "decomposition@SL1", data: draftOf("", unmarked), refused: true, names: "D1: where_exists:",
		},
		{
			name: "SL1's stored cards have a finding of their own",
			key:  "decomposition@SL1", data: withManual(draftOf("", gone), "M1"), refused: true, names: "D1: where_exists:",
		},
		{
			name: "a card depending on an earlier slice's card, which SL1's finding does not touch",
			key:  "decomposition@SL2", data: sl2("Export the reports", "D1"),
		},
		{
			name: "a card id an earlier slice holds",
			key:  "decomposition@SL2", data: draftOf("Export the reports", cardFor("D1", []any{"AC2"})), refused: true, names: "SL1",
		},
		{
			name: "a manual entry id an earlier slice holds",
			key:  "decomposition@SL2", data: withManual(sl2("Export the reports", "D1"), "M1"), refused: true, names: "SL1",
		},
		{
			name: "no playbook title on the last slice",
			key:  "decomposition@SL2", data: sl2("", "D1"), refused: true,
		},
		{
			name: "a card that carries none of its slice's criteria",
			key:  "decomposition@SL2", data: draftOf("Export the reports", cardFor("D2", nil, "D1")), refused: true, names: "AC2: criterion_unmapped:",
		},
		{
			name: "a setup naming a card no slice holds",
			key:  "decomposition@SL2", data: dangling, refused: true, names: "D9: setup_instruction:",
		},
		{
			name: "a setup naming an earlier slice's card to write an instruction file the project has",
			key:  "decomposition@SL2", data: overwrites, refused: true, names: "D1: setup_instruction:",
		},
		{
			name: "a card that skips the instruction card an earlier slice named", noAgents: true,
			key: "decomposition@SL2", data: sl2("Export the reports"), refused: true, names: "D2: setup_instruction:",
		},
		{
			name: "the same card depending on it", noAgents: true,
			key: "decomposition@SL2", data: sl2("Export the reports", "D1"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, err := judges[tt.noAgents](t, tt.key, tt.data)
			assert.Equal(t, 1, state.round(), "judged on its first submit, below the cap")

			if !tt.refused {
				require.NoError(t, err)
				assert.Empty(t, state.last(), "accepted with no problem")

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.names)
		})
	}
}

func TestCards_TheSignoffAndTheEmissionAreOnePlaybook(t *testing.T) {
	c, _, _, rec := newSliced(t, slicedScript())
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	driveIn(t, c, autoStart("BP1"))

	signoff, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.Equal(t, []string{"D1", "D2"}, signoff.Payload["order"], "every slice's cards, in slice order")
	assert.Equal(t, "Export the reports", signoff.Payload["title"])

	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")

	cards, books := fake.inputs()
	require.Len(t, cards, 2)
	assert.Equal(t, []string{"ALPHA-001"}, cards[1].DependsOn, "a later slice's card depends on the earlier one by its ContextMatrix id")
	assert.Len(t, books, 1, "one playbook across the slices")
}

func TestDraft_NoStoredCardsIsNotFound(t *testing.T) {
	sliced, _, _ := slicedBlueprint(t)

	plain, _, _, _, _ := newPipeline(t, happyScript())
	require.NoError(t, plain.Start(context.Background(), startInput("BP1")))

	for _, tc := range []struct {
		name string
		c    *Controller
	}{{"sliced", sliced}, {"unsliced", plain}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.c.draft(context.Background(), "BP1")
			assert.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestMergeDrafts(t *testing.T) {
	first := contract.Draft{
		Title: "the first slice's", Cards: []contract.Card{{ID: "D1"}}, Manual: []contract.Manual{{ID: "M1", Text: "check"}},
		Order: []string{"D1", "M1"}, Setup: map[string]contract.Setup{"alpha": {VerifyCommand: "make test"}},
	}
	second := contract.Draft{
		Title: "Export the reports", Cards: []contract.Card{{ID: "D2"}}, Order: []string{"D2"},
		Setup: map[string]contract.Setup{
			"alpha": {VerifyCommand: "make check", Replaces: "make all", InstructionCard: "D2"},
			"beta":  {VerifyCommand: "make verify"},
			"gamma": {VerifyCommand: "make verify", Replaces: "make all"},
		},
	}

	got := mergeDrafts([]contract.Draft{first, second})

	assert.Equal(t, "Export the reports", got.Title, "the last slice's title")
	assert.Equal(t, []string{"D1", "M1", "D2"}, got.Order)
	assert.Len(t, got.Cards, 2)
	assert.Len(t, got.Manual, 1)
	assert.Equal(t, contract.Setup{VerifyCommand: "make test", InstructionCard: "D2"}, got.Setup["alpha"], "per field, the earliest slice that sets it")
	assert.Equal(t, contract.Setup{VerifyCommand: "make verify"}, got.Setup["beta"])
	assert.Equal(t, contract.Setup{VerifyCommand: "make verify", Replaces: "make all"}, got.Setup["gamma"], "replaces goes with its verify command")
}

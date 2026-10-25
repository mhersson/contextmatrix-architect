package blueprint

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

func TestReadinessProblems(t *testing.T) {
	card := func(id string, ready bool, findings ...any) map[string]any {
		return map[string]any{"id": id, "ready": ready, "findings": append([]any{}, findings...)}
	}

	cross := func(id, severity, text string, refs ...any) map[string]any {
		return map[string]any{"id": id, "severity": severity, "text": text, "refs": append([]any{}, refs...)}
	}

	draft := []string{"D1", "D2"}

	tests := []struct {
		name string
		data map[string]any
		want []string // check names, in order
	}{
		{name: "it stands", data: map[string]any{
			"cards": []any{card("D1", true), card("D2", false, "Where names no file")},
			"cross": []any{cross("X1", "critical", "AC4 proves no outcome DEC2 promises.", "AC4", "DEC2", "O1")},
		}},
		{name: "no cross is fine", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}}},
		{name: "cards that are no list", data: map[string]any{"cards": "all ready"}, want: []string{"readiness_shape", "card_unjudged", "card_unjudged"}},
		{name: "an entry without an id", data: map[string]any{"cards": []any{card(" ", true), card("D1", true), card("D2", true)}}, want: []string{"card_entry"}},
		{name: "ready that is no flag", data: map[string]any{"cards": []any{map[string]any{"id": "D1", "ready": "yes"}, card("D2", true)}}, want: []string{"card_entry"}},
		{name: "ready with a finding", data: map[string]any{"cards": []any{card("D1", true, "x"), card("D2", true)}}, want: []string{"card_entry"}},
		{name: "not ready without one", data: map[string]any{"cards": []any{card("D1", false), card("D2", true)}}, want: []string{"card_entry"}},
		{name: "a card judged twice", data: map[string]any{"cards": []any{card("D1", true), card("D1", true), card("D2", true)}}, want: []string{"card_entry"}},
		{name: "a card left out", data: map[string]any{"cards": []any{card("D1", true)}}, want: []string{"card_unjudged"}},
		{name: "a card no draft holds", data: map[string]any{"cards": []any{card("D1", true), card("D2", true), card("D9", true)}}, want: []string{"card_unknown"}},
		{name: "an id with padding", data: map[string]any{"cards": []any{card(" D1", true), card("D2", true)}}, want: []string{"card_unknown", "card_unjudged"}},
		{name: "a card entry that is no object", data: map[string]any{"cards": []any{"D1", card("D2", true)}}, want: []string{"card_entry", "card_unjudged"}},
		{name: "a cross entry that is no object", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}, "cross": []any{"X1"}}, want: []string{"cross_shape"}},
		{name: "a severity in any case", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}, "cross": []any{cross("X1", "Critical", "x")}}, want: []string{"cross_severity"}},
		{name: "cross that is no list", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}, "cross": "none"}, want: []string{"cross_shape"}},
		{name: "a cross id of another form", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}, "cross": []any{cross("C1", "major", "x")}}, want: []string{"cross_id"}},
		{name: "a cross id twice", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}, "cross": []any{cross("X1", "major", "x"), cross("X1", "minor", "y")}}, want: []string{"cross_id"}},
		{name: "a severity outside the set", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}, "cross": []any{cross("X1", "blocker", "x")}}, want: []string{"cross_severity"}},
		{name: "no text", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}, "cross": []any{cross("X1", "minor", " ")}}, want: []string{"cross_text"}},
		{name: "refs that are no list of ids", data: map[string]any{"cards": []any{card("D1", true), card("D2", true)}, "cross": []any{map[string]any{"id": "X1", "severity": "minor", "text": "x", "refs": "AC4"}}}, want: []string{"cross_refs"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := readinessProblems(tt.data, draft)
			require.Len(t, got, len(tt.want), "%v", got)

			for i, name := range tt.want {
				assert.Contains(t, got[i], ": "+name+": ")
			}
		})
	}
}

func TestStopProblems(t *testing.T) {
	stop := func(id, text string) map[string]any { return map[string]any{"id": id, "text": text} }

	// M1 and M3 stop the run; M2 is a follow-up, D1 a card.
	stops := []string{"M1", "M3"}

	tests := []struct {
		name string
		v    any
		want []string // check names, in order
	}{
		{name: "absent is none", v: nil},
		{name: "an empty list is none", v: []any{}},
		{name: "not a list", v: "M1", want: []string{"stop_shape"}},
		{name: "an entry that is no object", v: []any{"M1"}, want: []string{"stop_shape"}},
		{name: "a card id", v: []any{stop("D1", "x")}, want: []string{"stop_id"}},
		{name: "a follow-up id", v: []any{stop("M2", "x")}, want: []string{"stop_id"}},
		{name: "empty text", v: []any{stop("M1", " ")}, want: []string{"stop_text"}},
		{name: "a stop named twice", v: []any{stop("M1", "x"), stop("M1", "y")}, want: []string{"stop_id"}},
		{name: "valid", v: []any{stop("M1", "the next card reads the key from the environment"), stop("M3", "it only confirms the result")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stopProblems(tt.v, stops)
			require.Len(t, got, len(tt.want), "%v", got)

			for i, name := range tt.want {
				assert.Contains(t, got[i], ": "+name+": ")
			}
		})
	}
}

// needlessSL2 is the sliced script with M1 a run stop between SL2's cards,
// D2 and D3, and readiness passing every card; needless says on which
// rounds it names M1 in needless_stops.
func needlessSL2(needless func(round int32) bool) stageScript {
	var rounds atomic.Int32

	script := slicedScript()
	script["decomposition"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		if sliceOf(in) == "SL1" {
			return done("# Cards SL1", draftOf("", cardFor("D1", nil))), nil
		}

		d := draftOf("Export the reports", cardFor("D2", nil, "D1"), cardFor("D3", nil, "D1"))
		d["manual"] = []any{map[string]any{"id": "M1", "text": "Open an exported report"}}
		d["order"] = []any{"D2", "M1", "D3"}

		return done("# Cards SL2", d), nil
	}
	script["readiness"] = func(context.Context, stage.Input) (stage.Output, error) {
		v := readinessFor(true, "D1", "D2", "D3")
		if needless(rounds.Add(1)) {
			v["needless_stops"] = []any{map[string]any{"id": "M1", "text": "Only confirms the export; no later card needs it."}}
		}

		return done("# Ready", v), nil
	}

	return script
}

func TestReadiness_ANeedlessStopSendsItsSliceBack(t *testing.T) {
	c, _, runner, rec := newSliced(t, needlessSL2(func(round int32) bool { return round == 1 }))

	in := autoStart("BP1")
	in.Unattended = true
	driveIn(t, c, in)

	assert.Equal(t, []string{
		"decomposition@SL1", "decomposition@SL2", "readiness",
		"decomposition@SL2", "readiness",
	}, stepRuns(runner, "decomposition", "readiness"), "the slice that holds the stop runs again, not the first")
	assert.True(t, rec.has(EventSignOff))
}

func TestReadiness_ANeedlessStopIsIgnoredWithoutUnattended(t *testing.T) {
	c, _, runner, rec := newSliced(t, needlessSL2(func(int32) bool { return true }))
	driveIn(t, c, autoStart("BP1"))

	assert.Equal(t, []string{"decomposition@SL1", "decomposition@SL2", "readiness"}, stepRuns(runner, "decomposition", "readiness"))

	so, ok := rec.first(EventSignOff)
	require.True(t, ok)
	assert.NotContains(t, so.Payload, "stops")
}

func TestReadiness_TheVerdictsStopsAreCheckedOnlyWhenUnattended(t *testing.T) {
	// oneCard has a manual entry before its only card: with one card there
	// is no playbook, so nothing stops the run.
	oneCard := goodCards()
	oneCard["manual"] = []any{map[string]any{"id": "M1", "text": "Check the dashboard"}}
	oneCard["order"] = []any{"M1", "D1"}

	tests := []struct {
		name       string
		draft      map[string]any // nil is twoCards, M1 between D1 and D2
		cards      []string       // nil is D1 and D2
		stop       string
		unattended bool
		refused    bool
	}{
		{name: "a card is no stop", stop: "D1", unattended: true, refused: true},
		{name: "without unattended the field is ignored", stop: "D1"},
		{name: "a follow-up is no stop", draft: followupCards(""), stop: "M1", unattended: true, refused: true},
		{name: "one card has no stops", draft: oneCard, cards: []string{"D1"}, stop: "M1", unattended: true, refused: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := twoCardScript()
			if tc.draft != nil {
				script["decomposition"] = submitting(tc.draft)
			}

			cards := tc.cards
			if cards == nil {
				cards = []string{"D1", "D2"}
			}

			var got error

			script["readiness"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
				v := readinessFor(true, cards...)
				v["needless_stops"] = []any{map[string]any{"id": tc.stop, "text": "the run need not stop there"}}
				got = in.Validate(ctx, stage.Artifact{Body: "# Ready", Data: v})

				return done("# Ready", readinessFor(true, cards...)), nil
			}

			c, _, _, _, _ := newPipeline(t, script)
			c.opts.Pack = withCheck(c.opts.Pack, "readiness", pack.CheckReadiness)

			in := startInput("BP1")
			in.Unattended = tc.unattended
			driveIn(t, c, in)

			if !tc.refused {
				assert.NoError(t, got)

				return
			}

			require.Error(t, got)
			assert.Contains(t, got.Error(), ": stop_id: ")
		})
	}
}

func TestReadinessProblems_WithoutADraftNoCardIsUnjudgedOrUnknown(t *testing.T) {
	got := readinessProblems(map[string]any{"cards": []any{map[string]any{"id": "D7", "ready": true, "findings": []any{}}}}, nil)
	assert.Empty(t, got)
}

func TestReadiness_ItsSubmitIsChecked(t *testing.T) {
	script := happyScript()

	var got error

	script["readiness"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		got = in.Validate(ctx, stage.Artifact{Body: "# Ready", Data: map[string]any{"cards": []any{}}})

		return done("# Ready", readiness(true)), nil
	}

	c, _, _, _, _ := newPipeline(t, script)
	c.opts.Pack = withCheck(c.opts.Pack, "readiness", pack.CheckReadiness)

	drive(t, c, "BP1")

	require.Error(t, got)
	assert.Contains(t, got.Error(), ": card_unjudged: ")
}

func TestReadiness_ReadsEverySlicesGroundingAsADigest(t *testing.T) {
	script := slicedScript()
	script["grounding"] = func(_ context.Context, in stage.Input) (stage.Output, error) {
		slice := sliceOf(in)

		return done("# Grounding body "+slice, map[string]any{
			"projects": map[string]any{"alpha": map[string]any{"sha": "sha-" + slice, "verify_command": "make test"}},
			"verdicts": []any{},
		}), nil
	}

	c, _, runner, _ := newSliced(t, script)
	driveIn(t, c, autoStart("BP1"))

	runs := inputsOf(runner, "readiness")
	require.NotEmpty(t, runs)

	for _, slice := range []string{"SL1", "SL2"} {
		assert.NotContains(t, runs[0].Task, "# Grounding body "+slice, "the body stays out")
		assert.Contains(t, runs[0].Task, "sha-"+slice, "the project facts reach readiness")
	}
}

package blueprint

import (
	"context"
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

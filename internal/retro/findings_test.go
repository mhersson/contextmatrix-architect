package retro

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
)

func TestParseFindings(t *testing.T) {
	struggled := []Struggle{
		{Signals: Signals{Card: "A", Body: "## Context\nThe card says\n   see the  doc above."}, Why: []string{WhyReviews}},
		{Signals: Signals{Card: "B", Body: "other body"}, Why: []string{WhyParked}},
	}

	lesson := func(m map[string]any) map[string]any {
		l := map[string]any{"text": "t", "rationale": "r", "scope": "pack"}
		for k, v := range m {
			l[k] = v
		}

		return l
	}
	finding := func(card string, m map[string]any) map[string]any {
		f := map[string]any{"card": card, "rule": "rule", "evidence": "other body", "at_fault": false, "lesson": nil}
		if card == "A" {
			f["evidence"] = "The card says"
		}

		for k, v := range m {
			f[k] = v
		}

		return f
	}
	data := func(fs ...any) map[string]any { return map[string]any{"findings": fs} }

	tests := []struct {
		name string
		data map[string]any
		errs []string // nil: accepted
	}{
		{"re-wrapped evidence", data(finding("A", map[string]any{"evidence": "The card says see the doc above."}), finding("B", nil)), nil},
		{"evidence not in the body", data(finding("A", map[string]any{"evidence": "nowhere"}), finding("B", nil)), []string{"A", "evidence"}},
		{"blank evidence", data(finding("A", map[string]any{"evidence": "  "}), finding("B", nil)), []string{"A", "evidence"}},
		{"a missing card", data(finding("A", nil)), []string{"B"}},
		{"a card named twice", data(finding("A", nil), finding("A", nil), finding("B", nil)), []string{"A", "twice"}},
		{"a foreign card", data(finding("A", nil), finding("B", nil), finding("Z", nil)), []string{"Z"}},
		{"a blank rule", data(finding("A", map[string]any{"rule": " "}), finding("B", nil)), []string{"A", "rule"}},
		{"a lesson on a card not at fault", data(finding("A", map[string]any{"lesson": lesson(nil)}), finding("B", nil)), []string{"A", "lesson"}},
		{"no lesson on a card at fault", data(finding("A", map[string]any{"at_fault": true}), finding("B", nil)), []string{"A", "lesson"}},
		{"a blank lesson text", data(finding("A", map[string]any{"at_fault": true, "lesson": lesson(map[string]any{"text": " "})}), finding("B", nil)), []string{"A", "text"}},
		{"an unknown scope", data(finding("A", map[string]any{"at_fault": true, "lesson": lesson(map[string]any{"scope": "world"})}), finding("B", nil)), []string{"A", "scope"}},
		{"at_fault not a boolean", data(finding("A", map[string]any{"at_fault": "yes"}), finding("B", nil)), []string{"A", "at_fault"}},
		{"an entry that is not an object", data("x", finding("A", nil), finding("B", nil)), []string{"1"}},
		{"no findings array", map[string]any{}, []string{"findings"}},
		{
			"an overlay key beyond the three lists",
			data(finding("A", map[string]any{"at_fault": true, "lesson": lesson(map[string]any{"overlay": map[string]any{"required_sections": []any{"x"}}})}), finding("B", nil)),
			[]string{"A", "overlay"},
		},
		{
			"a forbidden reference with a blank pattern",
			data(finding("A", map[string]any{"at_fault": true, "lesson": lesson(map[string]any{"overlay": map[string]any{"forbidden_references": []any{map[string]any{"name": "n", "pattern": " "}}}})}), finding("B", nil)),
			[]string{"A", "forbidden"},
		},
		{"two problems in one error", data(finding("A", map[string]any{"rule": ""}), finding("B", map[string]any{"evidence": "nowhere"})), []string{"A", "rule", "B", "evidence"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseFindings(tt.data, struggled)
			if tt.errs == nil {
				require.NoError(t, err)
				assert.Len(t, got, 2)

				return
			}

			require.Error(t, err)

			for _, part := range tt.errs {
				assert.Contains(t, err.Error(), part)
			}
		})
	}

	t.Run("accepted findings in struggled order", func(t *testing.T) {
		lessonA := lesson(map[string]any{
			"scope": "project",
			"overlay": map[string]any{
				"edge_cases":           []any{"Clock skew"},
				"dangling_references":  []any{"see above"},
				"forbidden_references": []any{map[string]any{"name": "tmp", "pattern": "^/tmp"}},
			},
		})
		got, err := ParseFindings(data(finding("B", nil), finding("A", map[string]any{"at_fault": true, "lesson": lessonA})), struggled)
		require.NoError(t, err)
		require.Len(t, got, 2)

		assert.Equal(t, "A", got[0].Card)
		assert.Equal(t, "rule", got[0].Rule)
		assert.Equal(t, "The card says", got[0].Evidence)
		assert.True(t, got[0].AtFault)
		require.NotNil(t, got[0].Lesson)
		assert.Equal(t, "t", got[0].Lesson.Text)
		assert.Equal(t, "r", got[0].Lesson.Rationale)
		assert.Equal(t, "project", got[0].Lesson.Scope)
		assert.Equal(t, contract.Overlay{
			EdgeCases:           []string{"Clock skew"},
			DanglingReferences:  []string{"see above"},
			ForbiddenReferences: []contract.Pattern{{Name: "tmp", Pattern: "^/tmp"}},
		}, got[0].Lesson.Overlay)

		assert.Equal(t, "B", got[1].Card)
		assert.False(t, got[1].AtFault)
		assert.Nil(t, got[1].Lesson)
	})
}

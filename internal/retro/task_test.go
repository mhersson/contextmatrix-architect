package retro

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestTaskFor_GivesEveryStruggledCardAsData(t *testing.T) {
	struggled := []Struggle{
		{Signals: Signals{Card: "ALPHA-001", Title: "First title", State: "blocked", ReviewAttempts: 4, Body: "first body"}, Why: []string{WhyBlocked}},
		{Signals: Signals{Card: "ALPHA-002", Title: "Second title", State: "done", CostUSD: 9, Body: "second body"}, Why: []string{WhyCost}, MedianUSD: 2},
	}
	lists := contract.Lists{EdgeCases: []string{"Clock skew"}}

	task := TaskFor(struggled, "TEMPLATE-TEXT", lists, "CHECKS-TEXT")

	for _, part := range []string{"ALPHA-001", "First title", "first body", "blocked", "4", "ALPHA-002", "Second title", "second body", "TEMPLATE-TEXT", "Clock skew", "CHECKS-TEXT"} {
		assert.Contains(t, task, part)
	}

	assert.Contains(t, task, "## Readiness checks")
	assert.NotContains(t, TaskFor(struggled, "T", lists, ""), "## Readiness checks")
}

func TestTaskFor_FencesEachBodyAsData(t *testing.T) {
	body := "before\n```\ninside\n```\nafter"
	task := TaskFor([]Struggle{{Signals: Signals{Card: "A", Body: body}}}, "T", contract.Lists{}, "")

	assert.Contains(t, task, "````\n")
	assert.Contains(t, task, body)
	assert.NotContains(t, task, "`````")
}

func TestLessonQuestion(t *testing.T) {
	f := Finding{
		Card: "ALPHA-002", Rule: "Where", Evidence: "e",
		Lesson: &Lesson{Overlay: contract.Overlay{
			EdgeCases:           []string{"Clock skew"},
			DanglingReferences:  []string{"see above"},
			ForbiddenReferences: []contract.Pattern{{Name: "tmp", Pattern: "^/tmp"}},
		}},
	}
	l := store.Lesson{ID: 7, Scope: store.ScopeProject, Project: "alpha", Text: "t", Rationale: "r"}

	q := LessonQuestion(l, f)
	assert.Equal(t, "L7 ALPHA-002", q.Title)
	assert.Equal(t, "t", q.Text)

	for _, part := range []string{"r", "Where", "e", "Clock skew", "see above", "tmp", "^/tmp"} {
		assert.Contains(t, q.Why, part)
	}

	if assert.Len(t, q.Options, 3) {
		assert.Contains(t, q.Options[0].Title, "alpha")
		assert.Equal(t, "Approve for the whole pack", q.Options[1].Title)
		assert.Equal(t, "Reject", q.Options[2].Title)
	}

	assert.Equal(t, 1, q.Recommended)

	l.Scope = store.ScopePack
	assert.Equal(t, 2, LessonQuestion(l, f).Recommended)
}

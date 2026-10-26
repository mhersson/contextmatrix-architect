package retro

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
)

func TestStruggled(t *testing.T) {
	card := func(id string, f func(*Signals)) Signals {
		s := Signals{Card: id, State: "done"}
		if f != nil {
			f(&s)
		}

		return s
	}
	cost := func(v float64) func(*Signals) { return func(s *Signals) { s.CostUSD = v } }

	tests := []struct {
		name   string
		cards  []Signals
		want   map[string][]string
		median float64
	}{
		{"one review attempt is no struggle", []Signals{card("A", func(s *Signals) { s.ReviewAttempts = 1 })}, map[string][]string{}, 0},
		{"two review attempts", []Signals{card("A", func(s *Signals) { s.ReviewAttempts = 2 })}, map[string][]string{"A": {WhyReviews}}, 0},
		{"parked", []Signals{card("A", func(s *Signals) { s.Worker = "parked" })}, map[string][]string{"A": {WhyParked}}, 0},
		{"a failed worker is no struggle", []Signals{card("A", func(s *Signals) { s.Worker = "failed" })}, map[string][]string{}, 0},
		{
			"stalled, blocked and not planned",
			[]Signals{card("A", func(s *Signals) { s.State = "stalled" }), card("B", func(s *Signals) { s.State = "blocked" }), card("C", func(s *Signals) { s.State = "not_planned" })},
			map[string][]string{"A": {WhyStalled}, "B": {WhyBlocked}, "C": {WhyNotPlanned}},
			0,
		},
		{
			"every reason that holds, in order",
			[]Signals{card("A", func(s *Signals) { s.ReviewAttempts = 3; s.Worker = "parked"; s.State = "blocked" })},
			map[string][]string{"A": {WhyReviews, WhyParked, WhyBlocked}},
			0,
		},
		{"cost needs three costed cards", []Signals{card("A", cost(1)), card("B", cost(9))}, map[string][]string{}, 0},
		{"cost at three times the median", []Signals{card("A", cost(1)), card("B", cost(1)), card("C", cost(3))}, map[string][]string{"C": {WhyCost}}, 1},
		{
			"an even count's median is the mean of the middle two",
			[]Signals{card("A", cost(1)), card("B", cost(2)), card("C", cost(3)), card("D", cost(7.5))},
			map[string][]string{"D": {WhyCost}},
			2.5,
		},
		{
			"an uncosted card is not counted",
			[]Signals{card("A", cost(1)), card("B", cost(1)), card("C", nil), card("D", cost(3))},
			map[string][]string{"D": {WhyCost}},
			1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Struggled(tt.cards)

			var ids []string

			for _, g := range got {
				ids = append(ids, g.Card)
				assert.Equal(t, tt.want[g.Card], g.Why, g.Card)

				if len(g.Why) > 0 && g.Why[len(g.Why)-1] == WhyCost {
					assert.InDelta(t, tt.median, g.MedianUSD, 1e-9)
				}
			}

			var wantIDs []string

			for _, c := range tt.cards {
				if _, ok := tt.want[c.Card]; ok {
					wantIDs = append(wantIDs, c.Card)
				}
			}

			assert.Equal(t, wantIDs, ids)
		})
	}
}

func TestSignalsOf(t *testing.T) {
	t.Run("a parked card's newest parked message", func(t *testing.T) {
		c := cm.EndedCard{
			Card: cm.Card{ID: "A-1", Title: "t", State: "in_progress", WorkerStatus: "parked", ReviewAttempts: 2},
			ActivityLog: []cm.ActivityEntry{
				{Action: "parked", Message: "first"},
				{Action: "note", Message: "x"},
				{Action: "parked", Message: "second"},
				{Action: "parked"},
			},
			Subtasks: []string{"s1", "s2"},
			Body:     "body",
		}
		s := SignalsOf(c, "alpha")
		assert.Equal(t, "second", s.Reason)
		assert.Equal(t, "alpha", s.Project)
		assert.Equal(t, "A-1", s.Card)
		assert.Equal(t, "parked", s.Worker)
		assert.Equal(t, 2, s.ReviewAttempts)
		assert.Equal(t, 2, s.Subtasks)
		assert.Equal(t, "body", s.Body)
	})

	t.Run("a stalled card", func(t *testing.T) {
		s := SignalsOf(cm.EndedCard{Card: cm.Card{State: "stalled"}}, "p")
		assert.Equal(t, "stalled", s.Reason)
	})

	t.Run("cost", func(t *testing.T) {
		s := SignalsOf(cm.EndedCard{TokenUsage: &cm.TokenUsage{EstimatedCostUSD: 1.5}, SubtaskCostUSD: 0.25}, "p")
		assert.InDelta(t, 1.75, s.CostUSD, 1e-9)

		s = SignalsOf(cm.EndedCard{SubtaskCostUSD: 0.25}, "p")
		assert.InDelta(t, 0.25, s.CostUSD, 1e-9)
		require.Empty(t, s.Reason)
	})
}

// Package retro holds the retrospective's pure parts: which emitted cards
// struggled, the findings a retro run returns, its task and the lesson
// questions.
package retro

import (
	"slices"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
)

// Why codes: a struggled card's reasons, in this order.
const (
	WhyReviews    = "review_attempts"
	WhyParked     = "parked"
	WhyStalled    = "stalled"
	WhyBlocked    = "blocked"
	WhyNotPlanned = "not_planned"
	WhyCost       = "cost"
)

const (
	reviewAttemptsFloor = 2
	costedCardsFloor    = 3
	costMultiple        = 3
)

// Signals is what one emitted card's run left, as the rules read it.
type Signals struct {
	Card, Project, Title, State, Worker string
	ReviewAttempts                      int
	CostUSD                             float64 // estimated cost plus subtask_cost_usd
	Subtasks                            int
	Reason                              string // a parked card's newest parked message; "stalled" for a stalled card
	Body                                string
}

// Struggle is a card that struggled and the reasons why.
type Struggle struct {
	Signals

	Why       []string // Why codes
	MedianUSD float64  // the costed cards' median, set when Why holds WhyCost
}

// SignalsOf reads a card's signals; the activity log is oldest first.
func SignalsOf(c cm.EndedCard, project string) Signals {
	s := Signals{
		Card:           c.ID,
		Project:        project,
		Title:          c.Title,
		State:          c.State,
		Worker:         c.WorkerStatus,
		ReviewAttempts: c.ReviewAttempts,
		CostUSD:        c.SubtaskCostUSD,
		Subtasks:       len(c.Subtasks),
		Body:           c.Body,
	}

	if c.TokenUsage != nil {
		s.CostUSD += c.TokenUsage.EstimatedCostUSD
	}

	if c.WorkerStatus == "parked" {
		for _, e := range slices.Backward(c.ActivityLog) {
			if e.Action == "parked" && e.Message != "" {
				s.Reason = e.Message

				break
			}
		}
	}

	if c.State == "stalled" && s.Reason == "" {
		s.Reason = "stalled"
	}

	return s
}

// Struggled returns the cards that struggled, in the order given.
func Struggled(cards []Signals) []Struggle {
	var costs []float64

	for _, c := range cards {
		if c.CostUSD > 0 {
			costs = append(costs, c.CostUSD)
		}
	}

	var median float64

	if len(costs) >= costedCardsFloor {
		median = medianOf(costs)
	}

	out := []Struggle{}

	for _, c := range cards {
		var why []string

		if c.ReviewAttempts >= reviewAttemptsFloor {
			why = append(why, WhyReviews)
		}

		if c.Worker == "parked" {
			why = append(why, WhyParked)
		}

		switch c.State {
		case "stalled":
			why = append(why, WhyStalled)
		case "blocked":
			why = append(why, WhyBlocked)
		case "not_planned":
			why = append(why, WhyNotPlanned)
		}

		st := Struggle{Signals: c}

		if median > 0 && c.CostUSD >= costMultiple*median {
			why = append(why, WhyCost)
			st.MedianUSD = median
		}

		if len(why) == 0 {
			continue
		}

		st.Why = why
		out = append(out, st)
	}

	return out
}

func medianOf(v []float64) float64 {
	s := slices.Clone(v)
	slices.Sort(s)

	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}

	return (s[n/2-1] + s[n/2]) / 2
}

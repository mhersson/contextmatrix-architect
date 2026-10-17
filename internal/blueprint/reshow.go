package blueprint

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// openQuestion is the payload of the question open on r; nil when none is.
// c.mu must be held.
func openQuestion(r *run) map[string]any {
	if r == nil || r.answer == nil {
		return nil
	}

	return r.pending
}

// reshowQuestion publishes a question the architect still waits on again,
// with the same payload and so the same id and actions. CM's panel closes a
// free-text question on any later row, so a reply the architect took as a
// command would otherwise leave the question unanswerable by a click. A gate
// or the sign-off stays open on CM's side across a reply that matches none
// of its actions and is never published again.
func (c *Controller) reshowQuestion(ctx context.Context, au audit, id string, asked map[string]any) {
	if asked == nil {
		return
	}

	au.event(ctx, Event{Kind: EventReshow, BlueprintID: id, Payload: map[string]any{"kind": EventQuestion, "payload": asked}})
}

// leftOpenQuestion is a human's question a run cut short never had answered:
// its event payload, to show again as it was, and the fields it records.
type leftOpenQuestion struct {
	runID   int64
	payload map[string]any
	asked   struct {
		Number    int      `json:"number"`
		Text      string   `json:"text"`
		Options   []string `json:"options"`
		Context   string   `json:"context"`
		Responder string   `json:"responder"`
	}
}

// leftOpen returns the human's questions that runs of stageID asked and End
// or a restart cut short before an answer, since the stage last completed,
// oldest first. An abort ends the question open at the time, as it always
// has, even when it lands while a reopen waits for the ended run.
func (c *Controller) leftOpen(ctx context.Context, id, stageID string) ([]leftOpenQuestion, error) {
	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list runs: %w", err)
	}

	cut := map[int64]bool{}

	for _, r := range runs {
		if r.Stage != stageID || r.Kind != store.RunKindStage {
			continue
		}

		switch {
		case r.Status == store.RunInterrupted, r.Status == store.RunCanceled && r.Reason == reasonSessionEnded:
			cut[r.ID] = true
		case r.Status == store.RunDone:
			clear(cut)
		}
	}

	if len(cut) == 0 {
		return nil, nil
	}

	asked, err := c.opts.Store.ListEventsOfKind(ctx, id, EventQuestion)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list questions asked: %w", err)
	}

	aborts, err := c.opts.Store.ListEventsOfKind(ctx, id, EventAborted)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list aborts: %w", err)
	}

	var lastAbort int64
	if len(aborts) > 0 {
		lastAbort = aborts[len(aborts)-1].ID
	}

	answered, err := c.opts.Store.ListQuestions(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list answers: %w", err)
	}

	done := map[int]bool{}
	for _, q := range answered {
		done[q.Number] = true
	}

	var out []leftOpenQuestion

	for _, e := range asked {
		if !cut[e.RunID] || e.ID < lastAbort {
			continue
		}

		q := leftOpenQuestion{runID: e.RunID}

		if err := json.Unmarshal(e.Payload, &q.payload); err != nil {
			return nil, fmt.Errorf("blueprint: decode question event %d: %w", e.ID, err)
		}

		if err := json.Unmarshal(e.Payload, &q.asked); err != nil {
			return nil, fmt.Errorf("blueprint: decode question event %d: %w", e.ID, err)
		}

		if q.asked.Responder != store.ResponderHuman || done[q.asked.Number] {
			continue
		}

		out = append(out, q)
	}

	return out, nil
}

// askLeftOpen puts every question stageID left open to the human again, as
// the same Q<n>, and records each answer under the run that asked it, so a
// re-run is given the answer and never decides the question alone.
func (c *Controller) askLeftOpen(ctx context.Context, r *run, au audit, bp store.Blueprint, stageID string) ([]store.Question, error) {
	bg := context.WithoutCancel(ctx)

	open, err := c.leftOpen(bg, bp.ID, stageID)
	if err != nil {
		return nil, err
	}

	var out []store.Question

	for _, q := range open {
		ans, err := c.awaitAnswer(ctx, r, bp.ID, q.asked.Number, q.payload, func() {
			c.reshowQuestion(bg, au, bp.ID, q.payload)
		})
		if err != nil {
			return nil, err
		}

		row := store.Question{
			BlueprintID: bp.ID, RunID: q.runID, Stage: stageID, Number: q.asked.Number,
			Text: q.asked.Text, Options: q.asked.Options, Context: q.asked.Context,
			Answer: au.text(ans.text), Responder: store.ResponderHuman, MessageID: ans.msgID, AnsweredAt: c.opts.Now().UTC(),
		}

		if err := c.opts.Store.AddQuestion(bg, row); err != nil {
			return nil, fmt.Errorf("blueprint: record the answer to Q%d: %w", q.asked.Number, err)
		}

		out = append(out, row)
	}

	return out, nil
}

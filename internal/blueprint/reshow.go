package blueprint

import "context"

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

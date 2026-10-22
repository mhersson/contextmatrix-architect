package blueprint

import (
	"context"
	"errors"
	"fmt"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Lessons lists every lesson of every blueprint in id order. It needs only an
// attached session, works in any state and takes no run slot.
func (c *Controller) Lessons(ctx context.Context, id, msgID string) error {
	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	lessons, err := c.opts.Store.ListLessons(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}

	if err != nil {
		return fmt.Errorf("blueprint: list lessons: %w", err)
	}

	items := make([]map[string]any, 0, len(lessons))

	for _, l := range lessons {
		evidence := l.Evidence
		if evidence == nil {
			evidence = []string{}
		}

		items = append(items, map[string]any{
			"lesson": l.ID, "blueprint": l.BlueprintID, "state": l.State, "scope": l.Scope, "project": l.Project,
			"text": l.Text, "rationale": l.Rationale, "evidence": evidence, "decided_by": l.DecidedBy,
		})
	}

	sess.audit.event(ctx, Event{Kind: EventLessonList, BlueprintID: id, Payload: map[string]any{
		"lessons": items, "message_id": msgID, "during_run": c.running(id),
	}})

	return nil
}

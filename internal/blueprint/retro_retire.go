package blueprint

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// Retire retires an active lesson of any blueprint. It needs an attached
// session, works in any state and while a run is in flight, and takes no run
// slot. Blueprints created before keep the lesson in their set; later ones do
// not get it.
func (c *Controller) Retire(ctx context.Context, id, ref, msgID string) error {
	c.mu.Lock()
	sess, ok := c.sessions[id]
	c.mu.Unlock()

	if !ok {
		return ErrNotFound
	}

	digits := strings.TrimPrefix(ref, "l")
	payload := map[string]any{"message_id": msgID, "during_run": c.running(id)}

	lessonID, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		payload["refused"] = fmt.Sprintf("There is no lesson L%s.", digits)
		sess.audit.event(ctx, Event{Kind: EventLessonRetire, BlueprintID: id, Payload: payload})

		return nil
	}

	retired, err := c.opts.Store.RetireLesson(ctx, lessonID)
	if err != nil {
		return fmt.Errorf("blueprint: retire lesson %d: %w", lessonID, err)
	}

	l, err := c.opts.Store.GetLesson(ctx, lessonID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		payload["refused"] = fmt.Sprintf("There is no lesson L%d.", lessonID)
	case err != nil:
		return fmt.Errorf("blueprint: read lesson %d: %w", lessonID, err)
	case retired:
		payload["lesson"], payload["state"], payload["text"] = l.ID, store.LessonRetired, l.Text
	default:
		payload["lesson"] = l.ID
		payload["refused"] = fmt.Sprintf("L%d is %s; only an active lesson can be retired.", l.ID, l.State)
	}

	sess.audit.event(ctx, Event{Kind: EventLessonRetire, BlueprintID: id, Payload: payload})

	return nil
}

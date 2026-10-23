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

	if err := c.retire(ctx, digits, payload); err != nil {
		return err
	}

	sess.audit.event(ctx, Event{Kind: EventLessonRetire, BlueprintID: id, Payload: payload})

	return nil
}

// retire fills payload with the outcome of retiring the lesson numbered
// digits. The lesson is read first so a failed read changes nothing; an
// active lesson's text is fixed, so the text read before the retire is its text.
func (c *Controller) retire(ctx context.Context, digits string, payload map[string]any) error {
	lessonID, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		payload["refused"] = fmt.Sprintf("There is no lesson L%s.", digits)

		return nil
	}

	l, err := c.opts.Store.GetLesson(ctx, lessonID)
	if errors.Is(err, store.ErrNotFound) {
		payload["refused"] = fmt.Sprintf("There is no lesson L%d.", lessonID)

		return nil
	}

	if err != nil {
		return fmt.Errorf("blueprint: read lesson %d: %w", lessonID, err)
	}

	retired, err := c.opts.Store.RetireLesson(ctx, lessonID)
	if err != nil {
		return fmt.Errorf("blueprint: retire lesson %d: %w", lessonID, err)
	}

	payload["lesson"] = l.ID

	if retired {
		payload["state"], payload["text"] = store.LessonRetired, l.Text

		return nil
	}

	now, err := c.opts.Store.GetLesson(ctx, lessonID)
	if err != nil {
		return fmt.Errorf("blueprint: read lesson %d: %w", lessonID, err)
	}

	payload["refused"] = fmt.Sprintf("L%d is %s; only an active lesson can be retired.", now.ID, now.State)

	return nil
}

package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// listsFor is the blueprint's lists: the pack's merged with the overlays of its
// lesson set, in lesson id order. Nothing is cached or stored; Pack.Lists
// stays the pack's own.
func (c *Controller) listsFor(ctx context.Context, bp store.Blueprint) (contract.Lists, error) {
	set, err := c.opts.Store.BlueprintLessons(ctx, bp.ID)
	if err != nil {
		return contract.Lists{}, fmt.Errorf("read the lesson set: %w", err)
	}

	overlays := make([]contract.Overlay, 0, len(set))

	for _, l := range set {
		var ov contract.Overlay
		if err := json.Unmarshal(l.Overlay, &ov); err != nil {
			return contract.Lists{}, fmt.Errorf("decode the overlay of lesson %s: %w", l.Tag(), err)
		}

		overlays = append(overlays, ov)
	}

	lists, err := c.opts.Pack.MergeLists(overlays)
	if err != nil {
		return contract.Lists{}, fmt.Errorf("merge the lessons' lists: %w", err)
	}

	return lists, nil
}

// lessonsReach reports whether a run of st gets the blueprint's lessons and
// the Unattended overlay: st is
// the stage that checks cards or the one that checks readiness. Stages are
// found by their checks, as everywhere else, never by id.
func (c *Controller) lessonsReach(st pipelineStep) bool {
	id := st.Stage.ID

	return id != "" && (id == c.cardsStage() || id == c.readinessStage())
}

// withLessons is instructions with a section listing lessons, in id order;
// instructions unchanged for none.
func withLessons(instructions string, lessons []store.Lesson) string {
	if len(lessons) == 0 {
		return instructions
	}

	var b strings.Builder

	b.WriteString(instructions)
	b.WriteString("\n\n# Lessons from earlier blueprints\n\n")
	b.WriteString("The operator approved these lessons from cards of earlier blueprints that struggled. Apply each where it fits these cards.\n\n")

	for _, l := range lessons {
		fmt.Fprintf(&b, "- %s: %s\n", l.Tag(), l.Text)
	}

	return b.String()
}

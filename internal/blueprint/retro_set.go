package blueprint

import (
	"context"
	"encoding/json"
	"fmt"

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

package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// slicesValidator refuses slices that do not stand, as boundedValidator
// bounds it.
func (c *Controller) slicesValidator(bp store.Blueprint) (func(context.Context, stage.Artifact) error, *validatorState) {
	return boundedValidator("slices", func(_ context.Context, a stage.Artifact) ([]string, error) {
		return sliceProblems(a.Data, bp.Projects), nil
	})
}

// sliceProblems checks slicing's submit: at least one slice, each named
// SL1, SL2, ... in its order, a title each, target projects only.
func sliceProblems(data map[string]any, targets []string) []string {
	var d struct {
		Slices []struct {
			ID       string   `json:"id"`
			Title    string   `json:"title"`
			Projects []string `json:"projects"`
		} `json:"slices"`
	}

	raw, err := json.Marshal(data)
	if err == nil {
		err = json.Unmarshal(raw, &d)
	}

	if err != nil {
		return []string{problem("slicing", "shape", "the data is not a list of slices: "+err.Error())}
	}

	if len(d.Slices) == 0 {
		return []string{problem("slicing", "no_slices", "slicing needs at least one slice")}
	}

	var out []string

	seen := map[string]bool{}

	for i, s := range d.Slices {
		id := strings.TrimSpace(s.ID)
		where := id

		if where == "" {
			where = fmt.Sprintf("slice %d", i+1)
		}

		switch {
		case id == "":
			out = append(out, problem(where, "slice_id", "a slice needs an id"))
		case seen[id]:
			out = append(out, problem(where, "slice_id", "the id is used twice"))
		case id != sliceID(i+1):
			out = append(out, problem(where, "slice_id", fmt.Sprintf("the id must be %s: slices are named SL1, SL2, ... in slicing's order", sliceID(i+1))))
		}

		seen[id] = true

		if strings.TrimSpace(s.Title) == "" {
			out = append(out, problem(where, "slice_title", "a slice needs a title"))
		}

		for _, p := range s.Projects {
			if !slices.Contains(targets, p) {
				out = append(out, problem(where, "slice_project", fmt.Sprintf("%q is not a target project of this blueprint", p)))
			}
		}
	}

	return out
}

// sliceIDs reads the slice ids of an artifact with slices, in order.
func sliceIDs(art store.Artifact) []string {
	var d struct {
		Slices []struct {
			ID string `json:"id"`
		} `json:"slices"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &d) != nil {
		return nil
	}

	ids := make([]string, 0, len(d.Slices))
	for _, s := range d.Slices {
		ids = append(ids, strings.TrimSpace(s.ID))
	}

	return ids
}

// slicedBy is the id of the run that built the latest artifact of a slicing
// stageID consumes, 0 when there is none. Run ids rise, so a question with a
// higher run id was asked about the slices as they are now.
func (c *Controller) slicedBy(ctx context.Context, id, stageID string) (int64, error) {
	st, ok := c.stepAt(stageID)
	if !ok {
		return 0, nil
	}

	runs, err := c.opts.Store.ListRuns(ctx, id)
	if err != nil {
		return 0, fmt.Errorf("list runs: %w", err)
	}

	var by int64

	for _, consumed := range st.Consumes {
		cs, _ := c.opts.Pack.Stage(consumed)
		if !slices.Contains(cs.Checks, pack.CheckSlices) {
			continue
		}

		for _, r := range runs {
			if r.Kind == store.RunKindStage && r.Stage == consumed && r.Status == store.RunDone && r.ID > by {
				by = r.ID
			}
		}
	}

	return by, nil
}

package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// acceptanceData is acceptance's structured block.
type acceptanceData struct {
	Slices []struct {
		ID       string `json:"id"`
		Criteria []struct {
			ID      string `json:"id"`
			Project string `json:"project"`
			Text    string `json:"text"`
		} `json:"criteria"`
		Manual []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"manual"`
	} `json:"slices"`
}

// criteriaValidator refuses acceptance that does not stand, as
// boundedValidator bounds it.
func (c *Controller) criteriaValidator(bp store.Blueprint, stageID string) (func(context.Context, stage.Artifact) error, *validatorState) {
	return boundedValidator("criteria", func(ctx context.Context, a stage.Artifact) ([]string, error) {
		return c.criteriaProblems(ctx, bp, stageID, a.Data)
	})
}

// criteriaProblems checks acceptance: slices, every slice of a consumed
// slicing among them, or a per-slice acceptance's own slice alone; every
// criterion with a unique id, a target project and text; every manual check
// with a unique id and text. A per-slice acceptance takes no id an earlier
// slice's holds.
func (c *Controller) criteriaProblems(ctx context.Context, bp store.Blueprint, stageID string, data map[string]any) ([]string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode the criteria: %w", err)
	}

	var d acceptanceData
	if err := json.Unmarshal(raw, &d); err != nil {
		return []string{problem("acceptance", "shape", "the data is not slices of criteria: "+err.Error())}, nil
	}

	taken, err := c.takenIDs(ctx, bp.ID, stageID)
	if err != nil {
		return nil, err
	}

	var out []string

	if len(d.Slices) == 0 {
		out = append(out, problem("acceptance", "no_slices", "acceptance has no slices"))
	}

	present := map[string]bool{}
	ids := map[string]bool{}

	for _, s := range d.Slices {
		present[strings.TrimSpace(s.ID)] = true

		if len(s.Criteria) == 0 {
			out = append(out, problem(strings.TrimSpace(s.ID), "slice_empty", "a slice needs at least one criterion; manual checks alone do not prove it"))
		}

		for _, cr := range s.Criteria {
			id := strings.TrimSpace(cr.ID)

			switch {
			case id == "" || ids[id]:
				out = append(out, problem(strings.TrimSpace(s.ID+" "+id), "criterion_id", "a criterion needs an id of its own"))
			case taken[id] != "":
				out = append(out, problem(id, "criterion_id", taken[id]+" already holds this id; ids run on across slices"))
			case !slices.Contains(bp.Projects, cr.Project):
				out = append(out, problem(id, "criterion_project", fmt.Sprintf("%q is not a target project of this blueprint", cr.Project)))
			}

			if strings.TrimSpace(cr.Text) == "" {
				out = append(out, problem(id, "criterion_text", "a criterion needs its text"))
			}

			ids[id] = true
		}

		for _, m := range s.Manual {
			id := strings.TrimSpace(m.ID)

			switch {
			case id == "" || ids[id]:
				out = append(out, problem(strings.TrimSpace(s.ID+" "+id), "manual_id", "a manual check needs an id of its own"))
			case taken[id] != "":
				out = append(out, problem(id, "manual_id", taken[id]+" already holds this id; ids run on across slices"))
			}

			if strings.TrimSpace(m.Text) == "" {
				out = append(out, problem(id, "manual_text", "a manual check needs its text"))
			}

			ids[id] = true
		}
	}

	missing, err := c.sliceCoverage(ctx, bp, stageID, present)
	if err != nil {
		return nil, err
	}

	return append(out, missing...), nil
}

// takenIDs maps every criterion and manual check id the earlier slices'
// latest acceptance holds to its slice; nil for a step that is no slice's.
func (c *Controller) takenIDs(ctx context.Context, id, key string) (map[string]string, error) {
	st, ok := c.stepAt(key)
	if !ok || st.Slice == "" {
		return nil, nil
	}

	steps, _, err := c.steps(ctx, id)
	if err != nil {
		return nil, err
	}

	taken := map[string]string{}

	for _, s := range earlierSteps(steps, st) {
		art, err := c.opts.Store.LatestArtifact(ctx, id, s.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", c.stepName(ctx, id, s.ID), err)
		}

		var d acceptanceData
		if json.Unmarshal(art.Data, &d) != nil {
			continue
		}

		for _, sl := range d.Slices {
			for _, cr := range sl.Criteria {
				taken[strings.TrimSpace(cr.ID)] = s.Slice
			}

			for _, m := range sl.Manual {
				taken[strings.TrimSpace(m.ID)] = s.Slice
			}
		}
	}

	return taken, nil
}

// acceptance is what the latest acceptance artifact defines for the
// contract; unreadable is set when its data does not decode.
type acceptance struct {
	criteria   []contract.Criterion
	manual     []string
	unreadable error
}

// acceptanceOf reads the criteria and manual checks of the latest artifact
// of the stage that checks criteria, of slice, when that stage runs per
// slice; found is false when the pack has no such stage or it has no
// artifact yet. Data that does not decode is not an error: retrying cannot
// fix it, so it comes back as unreadable for the contract to report. Only a
// store read fails.
func (c *Controller) acceptanceOf(ctx context.Context, id, slice string) (acc acceptance, stageID string, found bool, err error) {
	for _, st := range c.opts.Pack.Stages {
		if slices.Contains(st.Checks, pack.CheckCriteria) {
			stageID = st.ID
			if st.PerSlice {
				stageID = pack.StepKey(st.ID, slice)
			}

			break
		}
	}

	if stageID == "" {
		return acceptance{}, "", false, nil
	}

	art, err := c.opts.Store.LatestArtifact(ctx, id, stageID)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return acceptance{}, stageID, false, nil
	case err != nil:
		return acceptance{}, stageID, false, fmt.Errorf("read %s: %w", pack.StageName(stageID, false), err)
	}

	var d acceptanceData
	if err := json.Unmarshal(art.Data, &d); err != nil {
		return acceptance{unreadable: err}, stageID, true, nil
	}

	for _, s := range d.Slices {
		for _, cr := range s.Criteria {
			acc.criteria = append(acc.criteria, contract.Criterion{ID: strings.TrimSpace(cr.ID), Slice: s.ID, Project: cr.Project})
		}

		for _, m := range s.Manual {
			acc.manual = append(acc.manual, strings.TrimSpace(m.ID))
		}
	}

	return acc, stageID, true, nil
}

package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
// fatalValidator bounds it: acceptance of another shape, or a slice without
// criteria, leaves the cards nothing to carry. poc is the target projects the
// run was told are proofs of concept.
func (c *Controller) criteriaValidator(bp store.Blueprint, stageID string, poc []string) (func(context.Context, stage.Artifact) error, *validatorState) {
	return fatalValidator("criteria", []string{"shape", "no_slices", "slice_missing", "slice_empty"}, func(ctx context.Context, a stage.Artifact) ([]string, error) {
		return c.criteriaProblems(ctx, bp, stageID, a.Data, poc)
	})
}

// criteriaProblems checks acceptance: slices, every slice of a consumed
// slicing among them, or a per-slice acceptance's own slice alone; every
// criterion with a unique id, a target project and text; every manual check
// with a unique id and text. A per-slice acceptance takes no id an earlier
// slice's holds. Then the traces traceProblems checks.
func (c *Controller) criteriaProblems(ctx context.Context, bp store.Blueprint, stageID string, data map[string]any, poc []string) ([]string, error) {
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

	trace, err := c.traceProblems(ctx, bp, stageID, raw, poc)
	if err != nil {
		return nil, err
	}

	return append(append(out, missing...), trace...), nil
}

// traces is what acceptance's data says beyond its criteria: the outcomes
// each criterion and manual check proves, each criterion's project and each
// slice's edge-case sweep. It is read apart from acceptanceData, so a
// malformed trace is a problem of its own, never the shape problem that
// fails the run.
type traces struct {
	Slices []struct {
		ID       string `json:"id"`
		Criteria []struct {
			ID       string          `json:"id"`
			Project  string          `json:"project"`
			Outcomes json.RawMessage `json:"outcomes"`
		} `json:"criteria"`
		Manual []struct {
			ID       string          `json:"id"`
			Outcomes json.RawMessage `json:"outcomes"`
		} `json:"manual"`
		Sweep json.RawMessage `json:"sweep"`
	} `json:"slices"`
}

// citedOutcomes reads an entry's outcomes, a list of O<n> ids, trimmed; ok
// is false when it is set and not a list of strings.
func citedOutcomes(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}

	var ids []string
	if json.Unmarshal(raw, &ids) != nil {
		return nil, false
	}

	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	return ids, true
}

// traceProblems checks acceptance's traces. When an earlier step states an
// outcome, every criterion cites at least one, a manual check may, and none
// cites an unknown one; on the last slice's acceptance, or one for the whole
// blueprint, every outcome is cited by some slice's criterion or manual
// check. When the pack lists edge cases, every slice sweeps each once, as
// sweepProblems judges a slice that touches a POC project.
func (c *Controller) traceProblems(ctx context.Context, bp store.Blueprint, key string, raw []byte, poc []string) ([]string, error) {
	var d traces
	if json.Unmarshal(raw, &d) != nil {
		// acceptanceData decoded the same data; a trace cannot fail it.
		return nil, nil
	}

	known, err := c.outcomesBefore(ctx, bp.ID, key)
	if err != nil {
		return nil, err
	}

	isKnown := map[string]bool{}
	for _, o := range known {
		isKnown[o.ID] = true
	}

	cited := map[string]bool{}
	mark := func(ids []string) {
		for _, id := range ids {
			cited[id] = true
		}
	}
	cite := func(trace json.RawMessage) {
		ids, _ := citedOutcomes(trace)
		mark(ids)
	}

	lists, err := c.listsFor(ctx, bp)
	if err != nil {
		return nil, err
	}

	cases := lists.EdgeCases

	isPOC := map[string]bool{}
	for _, name := range poc {
		isPOC[name] = true
	}

	var touched map[string][]string

	// Only a blueprint with a POC target needs what slicing says each slice
	// touches; without one every slice sweeps.
	if len(cases) > 0 && len(poc) > 0 {
		if touched, err = c.sliceProjects(ctx, bp.ID); err != nil {
			return nil, err
		}
	}

	var out []string

	check := func(where string, trace json.RawMessage, required bool) {
		ids, ok := citedOutcomes(trace)

		switch {
		case len(known) == 0:
		case !ok:
			out = append(out, problem(where, "criterion_outcomes", `outcomes is a list of outcome ids, such as ["O1"]`))
		case required && len(ids) == 0:
			out = append(out, problem(where, "criterion_outcomes", `a criterion cites the outcomes it proves, such as ["O1"]`))
		default:
			for _, id := range ids {
				if !isKnown[id] {
					out = append(out, problem(where, "criterion_outcomes", id+" is no outcome an earlier step states"))
				}
			}

			mark(ids)
		}
	}

	for _, s := range d.Slices {
		slice := strings.TrimSpace(s.ID)
		ids := map[string]bool{}
		onPOC := map[string]bool{}
		projects := slices.Clone(touched[slice])

		for _, cr := range s.Criteria {
			id := strings.TrimSpace(cr.ID)
			ids[id] = true

			projects = append(projects, cr.Project)

			if isPOC[cr.Project] {
				onPOC[id] = true
			}

			check(id, cr.Outcomes, true)
		}

		for _, m := range s.Manual {
			ids[strings.TrimSpace(m.ID)] = true
			check(strings.TrimSpace(m.ID), m.Outcomes, false)
		}

		if len(cases) > 0 {
			out = append(out, sweepProblems(slice, s.Sweep, cases, ids, onPOC, onlyPOC(projects, isPOC))...)
		}
	}

	if len(known) == 0 {
		return out, nil
	}

	steps, pieces, err := c.steps(ctx, bp.ID)
	if err != nil {
		return nil, err
	}

	st, _ := c.stepAt(key)
	if st.Slice != "" && (len(pieces) == 0 || pieces[len(pieces)-1].id != st.Slice) {
		return out, nil
	}

	for _, s := range earlierSteps(steps, st) {
		art, err := c.opts.Store.LatestArtifact(ctx, bp.ID, s.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", c.stepName(ctx, bp.ID, s.ID), err)
		}

		var earlier traces
		if json.Unmarshal(art.Data, &earlier) != nil {
			continue
		}

		for _, es := range earlier.Slices {
			for _, cr := range es.Criteria {
				cite(cr.Outcomes)
			}

			for _, m := range es.Manual {
				cite(m.Outcomes)
			}
		}
	}

	for _, o := range known {
		if !cited[o.ID] {
			out = append(out, problem(o.ID, "outcome_uncovered", "no criterion or manual check of any slice proves this outcome"))
		}
	}

	return out, nil
}

// sweepProblems checks one slice's sweep against the pack's edge cases:
// one entry per case, matched case-insensitively after trimming, each
// settled by ids of the slice's own criteria or manual checks, or not
// applicable with a one-sentence reason, never both. A criterion on a POC
// project (onPOC) settles nothing: the case would reach that project's card
// as an edge-case test, the demand a POC is spared; a manual check reaches
// no card and still settles. A slice whose projects are all POC (allPOC)
// may leave its sweep out, null, empty or short of cases; an entry it does
// give is judged as any other.
func sweepProblems(slice string, raw json.RawMessage, cases []string, ids, onPOC map[string]bool, allPOC bool) []string {
	absent := len(raw) == 0 || string(raw) == "null"
	if absent && allPOC {
		return nil
	}

	var entries []map[string]json.RawMessage

	if absent || json.Unmarshal(raw, &entries) != nil {
		return []string{problem(slice, "sweep_missing",
			`the slice needs a sweep, one {"condition": "<case>", "settled_by": ["<id>"]} or {"condition": "<case>", "not_applicable": "<why>"} entry for each of: `+strings.Join(cases, ", "))}
	}

	listed := map[string]string{}
	for _, edge := range cases {
		listed[strings.ToLower(strings.TrimSpace(edge))] = edge
	}

	var out []string

	seen := map[string]bool{}

	for i, e := range entries {
		where := slice + " sweep " + strconv.Itoa(i+1)

		var condition string

		textual := json.Unmarshal(e["condition"], &condition) == nil

		name, ok := listed[strings.ToLower(strings.TrimSpace(condition))]

		switch {
		case !textual:
			out = append(out, problem(where, "sweep_unknown", "the condition is the edge case's name as text, one of: "+strings.Join(cases, ", ")))

			continue
		case !ok:
			out = append(out, problem(where, "sweep_unknown", fmt.Sprintf("%q is no edge case of the list: %s", condition, strings.Join(cases, ", "))))

			continue
		case seen[name]:
			out = append(out, problem(where, "sweep_duplicate", name+" is swept twice; give it one entry"))

			continue
		}

		seen[name] = true

		var (
			settled []string
			reason  string
		)

		settledOK := len(e["settled_by"]) == 0 || json.Unmarshal(e["settled_by"], &settled) == nil
		reasonOK := len(e["not_applicable"]) == 0 || json.Unmarshal(e["not_applicable"], &reason) == nil

		if !settledOK || !reasonOK || (len(settled) > 0) == (strings.TrimSpace(reason) != "") {
			out = append(out, problem(where, "sweep_settled", name+` is settled_by this slice's criteria or manual checks, such as ["AC4", "MC2"], or not_applicable with a one-sentence reason, never both`))

			continue
		}

		for _, id := range settled {
			switch key := strings.TrimSpace(id); {
			case !ids[key]:
				out = append(out, problem(where, "sweep_settled", fmt.Sprintf("%s is no criterion or manual check of %s", id, slice)))
			case onPOC[key]:
				out = append(out, problem(where, "sweep_poc", fmt.Sprintf(
					`%s is a criterion on a POC project, so it settles no case: settle %s with a criterion on another project or a manual check, or record it not_applicable with the reason "POC project"`, id, name)))
			}
		}
	}

	if allPOC {
		return out
	}

	for _, edge := range cases {
		if !seen[edge] {
			out = append(out, problem(slice, "sweep_missing", edge+" is not swept: settle it or record it not applicable with a reason"))
		}
	}

	return out
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
		return acceptance{}, stageID, false, fmt.Errorf("read %s: %w", c.stepName(ctx, id, stageID), err)
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

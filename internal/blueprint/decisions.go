package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// design is a design's structured block, as far as its checks read it: a
// slice design's decisions per slice, or a shared stage's one list.
type design struct {
	Decisions []decision `json:"decisions"`
	Slices    []struct {
		ID        string     `json:"id"`
		Decisions []decision `json:"decisions"`
	} `json:"slices"`
}

// decision is one decided question of a design.
type decision struct {
	ID       string           `json:"id"`
	Chosen   string           `json:"chosen"`
	Rejected []rejectedOption `json:"rejected"`
	Basis    string           `json:"basis"`
}

// rejectedOption is an option a decision turned down, and why.
type rejectedOption struct {
	Option string `json:"option"`
	Why    string `json:"why"`
}

// decisionsValidator refuses a design whose decisions do not stand, as
// fatalValidator bounds it: a design of another shape, or without its
// slices, leaves nothing to ground; its gate and reviewer judge it from there.
func (c *Controller) decisionsValidator(bp store.Blueprint, stageID string) (func(context.Context, stage.Artifact) error, *validatorState) {
	return fatalValidator("decisions", []string{"shape", "no_slices", "slice_missing"}, func(ctx context.Context, a stage.Artifact) ([]string, error) {
		return c.decisionProblems(ctx, bp, stageID, a.Data)
	})
}

// decisionProblems checks a design: it has slices and covers the slices
// sliceCoverage asks for; a shared stage's is one decisions list with at
// least one decision. Every decision has a chosen option, a rejected option
// and a basis the blueprint knows (an answered question, an accepted
// assumption, an earlier step's stated assumption, a recorded source or
// "assumption"). Every question the stage got answered and every assumption
// the user accepted on its tabs since the slicing was built, in any attempt,
// is the basis of some decision or stated assumption, so none is dropped.
// One from before a re-slice stays citable. On a plain-language blueprint an
// answer a re-ask superseded is owed by none and no decision rests on it:
// the answer that superseded it stands.
func (c *Controller) decisionProblems(ctx context.Context, bp store.Blueprint, stageID string, data map[string]any) ([]string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode the design: %w", err)
	}

	var d design
	if err := json.Unmarshal(raw, &d); err != nil {
		return []string{problem("design", "shape", "the data is not slices of decisions: "+err.Error())}, nil
	}

	// A shared stage never has a slices key, whether empty or not.
	_, hasSlices := data["slices"]
	st, _ := c.stepAt(stageID)

	questions, err := c.opts.Store.ListQuestions(ctx, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("read the questions: %w", err)
	}

	sources, err := c.opts.Store.ListSources(ctx, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("read the sources: %w", err)
	}

	accepted, err := c.acceptedAssumptions(ctx, bp.ID)
	if err != nil {
		return nil, err
	}

	since, err := c.slicedBy(ctx, bp.ID, stageID)
	if err != nil {
		return nil, err
	}

	superseded, err := c.supersededAnswers(ctx, bp)
	if err != nil {
		return nil, err
	}

	known := map[string]bool{}

	// owed is a basis the stage must use, with what made it one.
	type owed struct{ tag, what string }

	var own []owed

	// stands maps a superseded answer's tag to the newest answer of its re-asks.
	stands := map[string]string{}

	for _, q := range questions {
		known[q.Tag()] = true

		if m := superseded[q.Number]; m > 0 {
			for superseded[m] > 0 {
				m = superseded[m]
			}

			stands[q.Tag()] = fmt.Sprintf("Q%d", m)

			continue
		}

		if q.Stage == stageID && q.RunID > since {
			own = append(own, owed{q.Tag(), "the question was answered"})
		}
	}

	for _, a := range accepted {
		// A row an older close wrote is owed above, as its question.
		if known[a.Tag] {
			continue
		}

		known[a.Tag] = true

		if a.Stage == stageID && a.RunID > since {
			what := "the user accepted the assumption"
			if a.Plain {
				what = "the person accepted what the assumption means for them"
			}

			own = append(own, owed{a.Tag, what})
		}
	}

	for _, s := range sources {
		known[s.Tag()] = true
	}

	// A decision may rest on an assumption an earlier step states.
	stated, err := c.existingAssumptions(ctx, bp.ID, stageID, nil)
	if err != nil {
		return nil, err
	}

	for _, a := range stated {
		known[a.id] = true
	}

	var out []string

	// group is a list of decisions and where it sits, a slice id or shared.
	type group struct {
		where     string
		decisions []decision
	}

	var groups []group

	if st.Shared {
		if hasSlices {
			out = append(out, problem("design", "shared_slices", "shared decisions are one decisions list for every slice, not slices"))
		}

		if len(d.Decisions) == 0 {
			out = append(out, problem("design", "no_decisions", "the shared decisions name none"))
		}

		if bad := contractsProblem(data); bad != "" {
			out = append(out, bad)
		}

		groups = append(groups, group{"shared", d.Decisions})
	} else {
		if len(d.Slices) == 0 {
			out = append(out, problem("design", "no_slices", "the design has no slices"))
		}

		for _, s := range d.Slices {
			groups = append(groups, group{strings.TrimSpace(s.ID), s.Decisions})
		}
	}

	used := map[string]bool{}

	for _, a := range statedAssumptions(store.Artifact{Data: raw}) {
		used[a.Basis] = true
	}

	for _, g := range groups {
		for _, dec := range g.decisions {
			where := strings.TrimSpace(g.where + " " + dec.ID)

			if strings.TrimSpace(dec.Chosen) == "" {
				out = append(out, problem(where, "chosen", "the decision names no chosen option"))
			}

			if !slices.ContainsFunc(dec.Rejected, func(r rejectedOption) bool { return strings.TrimSpace(r.Option) != "" }) {
				out = append(out, problem(where, "rejected", "the decision names no option it rejected"))
			}

			switch basis := normalTag(dec.Basis); {
			case basis == "ASSUMPTION":
			case stands[basis] != "":
				out = append(out, problem(where, "superseded", fmt.Sprintf("%s's answer was turned down and asked again; rest the decision on %s, whose answer stands", basis, stands[basis])))
			case known[basis]:
				used[basis] = true
			default:
				out = append(out, problem(where, "basis", fmt.Sprintf("%q is no answered question (Q<n>), accepted assumption (round<n>.A<k>), assumption an earlier step states (A<n>), recorded source (S<n>) or assumption", dec.Basis)))
			}
		}
	}

	if !st.Shared {
		designed := map[string]bool{}
		for _, s := range d.Slices {
			designed[strings.TrimSpace(s.ID)] = true
		}

		missing, err := c.sliceCoverage(ctx, bp, stageID, designed)
		if err != nil {
			return nil, err
		}

		out = append(out, missing...)
	}

	for _, o := range own {
		if !used[o.tag] {
			out = append(out, problem(o.tag, "unused_answer", o.what+", but no decision or stated assumption rests on it"))
		}
	}

	return out, nil
}

// missingSlices reports every slice of a slicing stageID consumes that
// present lacks; a slicing without an artifact has none.
func (c *Controller) missingSlices(ctx context.Context, bp store.Blueprint, stageID string, present map[string]bool) ([]string, error) {
	st, ok := c.stepAt(stageID)
	if !ok {
		return nil, nil
	}

	var out []string

	for _, id := range st.Consumes {
		consumed, _ := c.opts.Pack.Stage(id)
		if !slices.Contains(consumed.Checks, pack.CheckSlices) {
			continue
		}

		art, err := c.opts.Store.LatestArtifact(ctx, bp.ID, id)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", id, err)
		}

		for _, sliceID := range sliceIDs(art) {
			if !present[sliceID] {
				out = append(out, problem(sliceID, "slice_missing", id+"'s slice is missing here"))
			}
		}
	}

	return out, nil
}

// sliceCoverage reports what a design's or acceptance's slices leave out or
// add: a per-slice step covers its own slice and no other; any other step
// covers every slice of the slicing it consumes (missingSlices).
func (c *Controller) sliceCoverage(ctx context.Context, bp store.Blueprint, key string, present map[string]bool) ([]string, error) {
	st, ok := c.stepAt(key)
	if !ok || st.Slice == "" {
		return c.missingSlices(ctx, bp, key, present)
	}

	var out []string

	if !present[st.Slice] {
		out = append(out, problem(st.Slice, "slice_missing", "this step's slice is missing here"))
	}

	for _, id := range slices.Sorted(maps.Keys(present)) {
		if id != st.Slice {
			out = append(out, problem(id, "slice_extra", fmt.Sprintf("this step covers %s alone; leave %s out", st.Slice, id)))
		}
	}

	return out, nil
}

// contractsProblem refuses a shared stage's contracts that are not a list of
// non-empty strings; "" when absent or well formed.
func contractsProblem(data map[string]any) string {
	v, ok := data["contracts"]
	if !ok {
		return ""
	}

	list, isList := v.([]any)
	if !isList {
		return problem("design", "contracts", "contracts are a list of strings")
	}

	for _, item := range list {
		if text, isText := item.(string); !isText || strings.TrimSpace(text) == "" {
			return problem("design", "contracts", "every contract is a non-empty string")
		}
	}

	return ""
}

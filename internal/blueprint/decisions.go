package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// design is a solution design's structured block, as far as its checks
// read it.
type design struct {
	Slices []struct {
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
// boundedValidator bounds it; its gate and reviewer judge it from there.
func (c *Controller) decisionsValidator(bp store.Blueprint, stageID string) (func(context.Context, stage.Artifact) error, *validatorState) {
	return boundedValidator("decisions", func(ctx context.Context, a stage.Artifact) ([]string, error) {
		return c.decisionProblems(ctx, bp, stageID, a.Data)
	})
}

// decisionProblems checks a design: it has slices; every decision has a
// chosen option, a rejected option and a basis the blueprint knows (an
// answered question, a recorded source or "assumption"); every slice of a
// consumed slicing is designed; and every question the stage got answered
// since that slicing was built, in any attempt, is the basis of some
// decision or stated assumption, so no answer is dropped: an assumption
// accepted on a round's tab is an answered question its statement rests
// on. An answer from before a re-slice stays citable.
func (c *Controller) decisionProblems(ctx context.Context, bp store.Blueprint, stageID string, data map[string]any) ([]string, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode the design: %w", err)
	}

	var d design
	if err := json.Unmarshal(raw, &d); err != nil {
		return []string{problem("design", "shape", "the data is not slices of decisions: "+err.Error())}, nil
	}

	questions, err := c.opts.Store.ListQuestions(ctx, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("read the questions: %w", err)
	}

	sources, err := c.opts.Store.ListSources(ctx, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("read the sources: %w", err)
	}

	since, err := c.slicedBy(ctx, bp.ID, stageID)
	if err != nil {
		return nil, err
	}

	known := map[string]bool{}

	var own []string

	for _, q := range questions {
		known[q.Tag()] = true

		if q.Stage == stageID && q.RunID > since {
			own = append(own, q.Tag())
		}
	}

	for _, s := range sources {
		known[s.Tag()] = true
	}

	var out []string

	if len(d.Slices) == 0 {
		out = append(out, problem("design", "no_slices", "the design has no slices"))
	}

	used := map[string]bool{}

	for _, a := range statedAssumptions(store.Artifact{Data: raw}) {
		used[a.Basis] = true
	}

	for _, s := range d.Slices {
		for _, dec := range s.Decisions {
			where := strings.TrimSpace(s.ID + " " + dec.ID)

			if strings.TrimSpace(dec.Chosen) == "" {
				out = append(out, problem(where, "chosen", "the decision names no chosen option"))
			}

			if !slices.ContainsFunc(dec.Rejected, func(r rejectedOption) bool { return strings.TrimSpace(r.Option) != "" }) {
				out = append(out, problem(where, "rejected", "the decision names no option it rejected"))
			}

			switch basis := normalTag(dec.Basis); {
			case basis == "ASSUMPTION":
			case known[basis]:
				used[basis] = true
			default:
				out = append(out, problem(where, "basis", fmt.Sprintf("%q is no answered question (Q<n>), recorded source (S<n>) or assumption", dec.Basis)))
			}
		}
	}

	designed := map[string]bool{}
	for _, s := range d.Slices {
		designed[strings.TrimSpace(s.ID)] = true
	}

	missing, err := c.missingSlices(ctx, bp, stageID, designed)
	if err != nil {
		return nil, err
	}

	out = append(out, missing...)

	for _, tag := range own {
		if !used[tag] {
			out = append(out, problem(tag, "unused_answer", "the question was answered, but no decision or stated assumption rests on it"))
		}
	}

	return out, nil
}

// missingSlices reports every slice of a slicing stageID consumes that
// present lacks; a slicing without an artifact has none.
func (c *Controller) missingSlices(ctx context.Context, bp store.Blueprint, stageID string, present map[string]bool) ([]string, error) {
	st, ok := c.opts.Pack.Stage(stageID)
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

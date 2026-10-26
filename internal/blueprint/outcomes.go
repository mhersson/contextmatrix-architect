package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// outcomeEntry is one measurable outcome a stage states: O<n> and its
// sentence. It sits at the top level of the data, beside the assumptions.
type outcomeEntry struct {
	ID, Text string
}

var outcomeForm = regexp.MustCompile(`^O([1-9][0-9]*)$`)

// outcomeNumber is n of an id O<n>; 0 for any other text.
func outcomeNumber(id string) int {
	m := outcomeForm.FindStringSubmatch(id)
	if m == nil {
		return 0
	}

	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}

	return n
}

// outcomesOf reads the outcomes an artifact states. The data is a model's,
// accepted after its check's last round, so an entry that carries no id is
// skipped and one of another shape keeps what reads.
func outcomesOf(art store.Artifact) []outcomeEntry {
	var data struct {
		Outcomes []any `json:"outcomes"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &data) != nil {
		return nil
	}

	var out []outcomeEntry

	for _, item := range data.Outcomes {
		e, isObject := item.(map[string]any)
		if !isObject {
			continue
		}

		// Each field is read as text when it is text, so an entry with a
		// stray type still keeps its id reserved.
		id, _ := e["id"].(string)
		text, _ := e["text"].(string)

		if id = strings.TrimSpace(id); id != "" {
			out = append(out, outcomeEntry{ID: id, Text: strings.TrimSpace(text)})
		}
	}

	return out
}

// outcomesBefore lists the outcomes the steps before key state, in pipeline
// order: the latest artifact of each step whose stage checks outcomes. A
// re-run's own earlier version and a step after key, stale or not, never
// count. A key the pipeline does not hold reads none.
func (c *Controller) outcomesBefore(ctx context.Context, id, key string) ([]outcomeEntry, error) {
	steps, _, err := c.steps(ctx, id)
	if err != nil {
		return nil, err
	}

	i := stepIndex(steps, key)
	if i < 0 {
		return nil, nil
	}

	var out []outcomeEntry

	for _, s := range steps[:i] {
		if s.Collapsed || !slices.Contains(s.Checks, pack.CheckOutcomes) {
			continue
		}

		art, err := c.opts.Store.LatestArtifact(ctx, id, s.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("read %s: %w", c.stepName(ctx, id, s.ID), err)
		}

		out = append(out, outcomesOf(art)...)
	}

	return out, nil
}

// outcomesValidator refuses outcomes that do not stand, as boundedValidator
// bounds it; the first pack stage that checks outcomes, intake in the
// default pack, states at least one.
func (c *Controller) outcomesValidator(bp store.Blueprint, key string) (func(context.Context, stage.Artifact) error, *validatorState) {
	st, _ := c.stepAt(key)
	first := slices.IndexFunc(c.opts.Pack.Stages, func(s pack.Stage) bool { return slices.Contains(s.Checks, pack.CheckOutcomes) })
	isFirst := first >= 0 && c.opts.Pack.Stages[first].ID == st.Stage.ID

	return boundedValidator("outcomes", func(ctx context.Context, a stage.Artifact) ([]string, error) {
		earlier, err := c.outcomesBefore(ctx, bp.ID, key)
		if err != nil {
			return nil, err
		}

		return outcomeProblems(a.Data, earlier, isFirst), nil
	})
}

const outcomeShape = `{"id": "O<n>", "text": "<one measurable sentence>", "quality": "<optional: the quality it states>"}`

// outcomeProblems checks a submit's outcomes against those the earlier steps
// state: each an object with an id O<n> no other outcome holds, after the
// highest an earlier step holds, its sentence and, when set, a quality word;
// first asks for at least one.
func outcomeProblems(data map[string]any, earlier []outcomeEntry, first bool) []string {
	highest, taken := 0, map[string]bool{}

	for _, o := range earlier {
		taken[o.ID] = true
		highest = max(highest, outcomeNumber(o.ID))
	}

	raw, set := data["outcomes"]
	items, isList := raw.([]any)

	var out []string

	// A design's outcomes sit at the top level of its data, never in a slice
	// entry, where nothing downstream reads them.
	sliceEntries, _ := data["slices"].([]any)
	for i, item := range sliceEntries {
		if e, ok := item.(map[string]any); ok {
			if _, nested := e["outcomes"]; nested {
				out = append(out, problem(fmt.Sprintf("slice %d", i+1), "outcome_shape", "outcomes sit at the top level of the data, never inside a slice entry"))
			}
		}
	}

	switch {
	case (!set || raw == nil || (isList && len(items) == 0)) && first:
		return append(out, problem("outcomes", "no_outcomes", "state every measurable outcome of the need as "+outcomeShape))
	case !set || raw == nil:
		return out
	case !isList:
		return append(out, problem("outcomes", "outcome_shape", "outcomes must be a list of "+outcomeShape+" entries"))
	}

	for i, item := range items {
		where := fmt.Sprintf("outcome %d", i+1)

		e, isObject := item.(map[string]any)
		id, isID := e["id"].(string)
		text, _ := e["text"].(string)
		quality, hasQuality := e["quality"]
		_, qualityIsText := quality.(string)

		if !isObject || !isID || (hasQuality && quality != nil && !qualityIsText) {
			out = append(out, problem(where, "outcome_shape", "each outcome is "+outcomeShape))

			continue
		}

		if id = strings.TrimSpace(id); id != "" {
			where = id
		}

		switch n := outcomeNumber(id); {
		case n == 0:
			out = append(out, problem(where, "outcome_id", "an outcome's id is O<n>, such as O1"))
		case taken[id]:
			out = append(out, problem(where, "outcome_id", "the id is taken; ids run on across the blueprint"))
		case n <= highest:
			out = append(out, problem(where, "outcome_id", fmt.Sprintf("an earlier step holds up to O%d, so this step's ids start after it", highest)))
		}

		taken[id] = true

		if strings.TrimSpace(text) == "" {
			out = append(out, problem(where, "outcome_text", "an outcome needs its measurable sentence"))
		}
	}

	return out
}

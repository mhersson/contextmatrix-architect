package blueprint

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// shownParts is how many of a refused run's biggest inputs its message
// names.
const shownParts = 3

// contextStop reports a run its window stopped, or one refused because its
// opening prompt would not fit, in words a person reads: the run, the window
// and, for a refusal, its biggest inputs by step name. The blueprint is
// failed, as after any failed run, and continue runs it again. false when
// err is neither.
func (c *Controller) contextStop(ctx context.Context, au audit, id string, runID int64, kind, key string, err error) bool {
	var (
		big  *stage.TooBigError
		full *stage.ContextFullError
	)

	what := c.stepName(ctx, id, key)

	if kind == store.RunKindReviewer {
		what = "The review of " + what
	}

	payload := map[string]any{"stage": key, "kind": kind}

	switch {
	case errors.As(err, &big):
		// A failure measuring never hides the stop: the message then names
		// no inputs.
		sizes, serr := c.inputSizes(ctx, id, key, kind)
		if serr != nil {
			c.opts.Logger.Warn("blueprint: measure a refused run's inputs", "blueprint", id, "stage", key, "error", serr)
		}

		// []any, not []map[string]any: the audit masks the strings of []any
		// and maps only.
		parts := make([]any, 0, shownParts)
		names := make([]string, 0, shownParts)

		// The rest is what the named parts leave of the estimate, so the
		// numbers a person reads add up; inputs beyond the named ones fall
		// into it.
		rest := big.Estimate

		for _, p := range sizes[:min(len(sizes), shownParts)] {
			rest -= p.Tokens

			parts = append(parts, map[string]any{"name": p.Name, "tokens": p.Tokens})
			names = append(names, fmt.Sprintf("%s (about %d tokens)", c.stepName(ctx, id, p.Name), p.Tokens))
		}

		rest = max(rest, 0)

		payload["reason"], payload["window"], payload["limit"] = stage.ReasonTooBig, big.Window, big.Limit
		payload["estimate"], payload["parts"], payload["rest_tokens"] = big.Estimate, parts, rest

		biggest := ""
		if len(names) > 0 {
			biggest = " The biggest inputs: " + strings.Join(names, ", ") + "; the rest of the prompt is about " + fmt.Sprint(rest) + " tokens."
		}

		payload["message"] = fmt.Sprintf("%s did not start: its opening prompt is about %d tokens, and a run on this model's %d-token window "+
			"must open below %d.%s", what, big.Estimate, big.Window, big.Limit, biggest)
	case errors.As(err, &full):
		payload["reason"], payload["window"], payload["limit"] = stage.ReasonContextLimit, full.Window, full.Limit
		payload["message"] = fmt.Sprintf("%s stopped: its conversation reached the %d tokens this model's %d-token window allows "+
			"and could not be summarized below it.", what, full.Limit, full.Window)
	default:
		return false
	}

	c.opts.Logger.Error("blueprint: stage stopped by its context window", "blueprint", id, "run", runID, "stage", key, "kind", kind,
		"reason", payload["reason"], "window", payload["window"], "limit", payload["limit"], "estimate", payload["estimate"])

	c.setState(ctx, id, StateFailed)
	au.event(ctx, Event{Kind: EventError, BlueprintID: id, RunID: runID, Payload: payload})

	return true
}

// inputSizes is what key's task carries, in estimated tokens, biggest first:
// each artifact it reads as the task gives it, and for a reviewer the
// artifact it judges. The rest of the prompt is not split.
func (c *Controller) inputSizes(ctx context.Context, id, key, kind string) ([]stage.Part, error) {
	st, ok := c.stepAt(key)
	if !ok {
		return nil, nil
	}

	steps, pieces, err := c.steps(ctx, id)
	if err != nil {
		return nil, err
	}

	var parts []stage.Part

	for _, in := range consumedSteps(steps, st) {
		var b strings.Builder
		if err := c.writeInput(ctx, &b, id, st, in, len(pieces) > 1, "size"); err != nil {
			return nil, err
		}

		parts = append(parts, stage.Part{Name: in.ID, Tokens: stage.EstimateTokens(b.String())})
	}

	if kind == store.RunKindReviewer {
		art, err := c.opts.Store.LatestArtifact(ctx, id, key)
		if err != nil {
			return nil, fmt.Errorf("blueprint: the judged artifact: %w", err)
		}

		var b strings.Builder

		writeArtifact(&b, key, art)
		parts = append(parts, stage.Part{Name: key, Tokens: stage.EstimateTokens(b.String())})
	}

	slices.SortStableFunc(parts, func(a, b stage.Part) int { return cmp.Compare(b.Tokens, a.Tokens) })

	return parts, nil
}

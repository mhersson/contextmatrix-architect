package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// folding is a stage's artifact folding into a later gate, decided when
// the pipeline passes the stage.
type folding struct {
	stage   string
	version int
	into    string
}

// foldTarget is the gate step key folds into: the next gated step after it
// that is no shared stage's; "" when there is none.
func foldTarget(steps []pipelineStep, key string) string {
	i := stepIndex(steps, key)
	if i < 0 {
		return ""
	}

	for _, s := range steps[i+1:] {
		if s.Gate && !s.Shared {
			return s.ID
		}
	}

	return ""
}

// foldersOf lists the steps that fold into key's gate.
func foldersOf(steps []pipelineStep, key string) []pipelineStep {
	var out []pipelineStep

	for _, s := range steps {
		if s.Fold != nil && foldTarget(steps, s.ID) == key {
			out = append(out, s)
		}
	}

	return out
}

// foldsInto returns the gate st's artifact folds into, or "": its fold
// field holds at most the pack's max entries, its attempt has no verdict
// yet, and the gate it folds into has the same gate policy, so neither a
// human nor the auto policy loses a decision. A policy changed after the
// fold is openGate's: the folded gate stays a human's while any stage it
// judges is.
func (c *Controller) foldsInto(ctx context.Context, bp store.Blueprint, st pipelineStep, art store.Artifact) (string, error) {
	if st.Fold == nil {
		return "", nil
	}

	steps, _, err := c.steps(ctx, bp.ID)
	if err != nil {
		return "", err
	}

	into := foldTarget(steps, st.ID)
	if into == "" {
		return "", nil
	}

	// Once st's own reviewer has judged a version of this attempt, its gate
	// stays its own, whatever the policies say now and whatever a later
	// version of the attempt holds.
	built, err := c.latestStageRun(ctx, bp.ID, st.ID)
	if err != nil {
		return "", err
	}

	past, err := c.attemptReviews(ctx, bp.ID, st.ID, built.Attempt)
	if err != nil || len(past) > 0 {
		return "", err
	}

	var data map[string]any
	if len(art.Data) == 0 || json.Unmarshal(art.Data, &data) != nil {
		return "", nil
	}

	items, ok := data[st.Fold.Field].([]any)
	if !ok || len(items) > st.Fold.Max {
		return "", nil
	}

	mine, err := c.gatePolicy(ctx, bp, st.ID)
	if err != nil {
		return "", err
	}

	theirs, err := c.gatePolicy(ctx, bp, into)
	if err != nil {
		return "", err
	}

	if mine != theirs {
		return "", nil
	}

	return into, nil
}

// recordFold records a fold as a decision on the folded stage's version, so
// a later policy change cannot unfold it; false when it could not be stored,
// which fails the blueprint. A collapse is a fold at version 0, which no
// artifact holds.
func (c *Controller) recordFold(ctx context.Context, au audit, id string, f *folding) bool {
	return c.decide(ctx, au, id, f.stage, f.version, store.GateFold, f.into, store.ResponderAuto, "")
}

// folded returns the latest artifact of every stage folded into stageID's
// gate: its latest version's decision is the fold.
func (c *Controller) folded(ctx context.Context, id, stageID string) ([]store.Artifact, error) {
	return c.foldersDecided(ctx, id, stageID, store.GateFold)
}

// rejectedAtGate returns the step a rejection at st's gate runs again, and
// everything after it: a stage folded into the gate, or slicing or a shared
// stage the gate offers to reject, whose latest version was rejected.
func (c *Controller) rejectedAtGate(ctx context.Context, id string, st pipelineStep) (string, error) {
	rejected, err := c.foldersDecided(ctx, id, st.ID, store.GateReject)
	if err != nil {
		return "", err
	}

	if len(rejected) > 0 {
		return rejected[0].Stage, nil
	}

	earlier, err := c.earlierRejects(ctx, id, st.ID)
	if err != nil {
		return "", err
	}

	return c.rejectedTarget(ctx, id, earlier)
}

// rejectedTarget returns the target of the first of actions whose latest
// version's latest decision is a rejection, which stands until the target
// runs again and stores a new version; "" when there is none. A target with
// no version was never rejected.
func (c *Controller) rejectedTarget(ctx context.Context, id string, actions []gateAction) (string, error) {
	for _, a := range actions {
		art, err := c.opts.Store.LatestArtifact(ctx, id, a.Target)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return "", fmt.Errorf("blueprint: read %s: %w", c.stepName(ctx, id, a.Target), err)
		}

		d, err := c.gateDecision(ctx, id, a.Target, art.Version)
		if err != nil {
			return "", err
		}

		if d == store.GateReject {
			return a.Target, nil
		}
	}

	return "", nil
}

// earlierRejects are the replies that reject an earlier step at key's gate,
// when key is its slice's first gated step, the slice's design: slicing,
// unless it folded into this gate, whose fold offers it already, and every
// shared stage that ran. Each runs that step again, and everything after
// it. A collapsed shared stage is not run, whatever versions it stored
// before a re-slice, so one slice offers no shared reject.
func (c *Controller) earlierRejects(ctx context.Context, id, key string) ([]gateAction, error) {
	steps, _, err := c.steps(ctx, id)
	if err != nil {
		return nil, err
	}

	i := stepIndex(steps, key)
	if i < 0 || !firstGated(steps, steps[i]) {
		return nil, nil
	}

	var out []gateAction

	for _, s := range steps[:i] {
		slicing := slices.Contains(s.Checks, pack.CheckSlices)
		if !slicing && (!s.Shared || s.Collapsed) {
			continue
		}

		art, err := c.opts.Store.LatestArtifact(ctx, id, s.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("blueprint: read %s: %w", c.stepName(ctx, id, s.ID), err)
		}

		if slicing {
			d, err := c.gateDecision(ctx, id, s.ID, art.Version)
			if err != nil {
				return nil, err
			}

			if d == store.GateFold {
				continue
			}
		}

		out = append(out, gateAction{
			Label: "Reject " + strings.ReplaceAll(s.ID, "_", " "), Reply: store.GateReject + " " + s.ID, Feedback: true,
			Decision: store.GateReject, Target: s.ID,
		})
	}

	return out, nil
}

// firstGated reports whether st is the first gated step of its slice, the
// slice's design; false for a whole-blueprint step.
func firstGated(steps []pipelineStep, st pipelineStep) bool {
	if st.Slice == "" {
		return false
	}

	for _, s := range steps {
		if s.Slice == st.Slice && s.Gate {
			return s.ID == st.ID
		}
	}

	return false
}

// foldersDecided returns the latest artifact of every stage that folds into
// stageID's gate whose latest decision is decision, in pipeline order.
func (c *Controller) foldersDecided(ctx context.Context, id, stageID, decision string) ([]store.Artifact, error) {
	steps, _, err := c.steps(ctx, id)
	if err != nil {
		return nil, err
	}

	var out []store.Artifact

	for _, f := range foldersOf(steps, stageID) {
		art, err := c.opts.Store.LatestArtifact(ctx, id, f.ID)

		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fmt.Errorf("blueprint: the folded %s: %w", f.ID, err)
		}

		d, err := c.gateDecision(ctx, id, f.ID, art.Version)
		if err != nil {
			return nil, err
		}

		if d == decision {
			out = append(out, art)
		}
	}

	return out, nil
}

// gateActions are the replies stageID's gate takes: approve and reject, and
// for every stage folded into it, "reject <stage> <feedback>", which runs
// that stage again and everything after it, and at a slice's design gate,
// reject slicing <feedback> and reject <shared stage> <feedback> for every
// shared stage that ran. It returns the folded artifacts too.
func (c *Controller) gateActions(ctx context.Context, id, stageID string) ([]gateAction, []store.Artifact, error) {
	folded, err := c.folded(ctx, id, stageID)
	if err != nil {
		return nil, nil, err
	}

	actions := stageGateActions(stageID)

	for _, f := range folded {
		actions = append(actions, gateAction{
			Label: "Reject " + f.Stage, Reply: store.GateReject + " " + f.Stage, Feedback: true,
			Decision: store.GateReject, Target: f.Stage,
		})
	}

	earlier, err := c.earlierRejects(ctx, id, stageID)
	if err != nil {
		return nil, nil, err
	}

	actions = append(actions, earlier...)

	return actions, folded, nil
}

package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

// foldedRejected returns the stage folding into st's gate whose latest
// artifact was rejected there; it runs again, and everything after it.
func (c *Controller) foldedRejected(ctx context.Context, id string, st pipelineStep) (string, error) {
	rejected, err := c.foldersDecided(ctx, id, st.ID, store.GateReject)
	if err != nil || len(rejected) == 0 {
		return "", err
	}

	return rejected[0].Stage, nil
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
// that stage again and everything after it. It returns the folded
// artifacts too.
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

	return actions, folded, nil
}

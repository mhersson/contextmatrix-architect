package blueprint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// board is the emission pseudo-artifact's data: the created cards with
// their ContextMatrix ids, draft type and priority and, once the watch
// has seen them, their state and pull request; the playbook, the compare
// links, the manual checks, steps and notices, and the architect's total
// cost.
type board struct {
	Cards    []boardCard       `json:"cards"`
	Playbook string            `json:"playbook,omitempty"`
	Repos    []cm.PlaybookRepo `json:"repos"`
	Manual   []string          `json:"manual"`
	Steps    []string          `json:"steps"`
	Notices  []string          `json:"notices"`
	CostUSD  float64           `json:"cost_usd"`
	Priced   bool              `json:"priced"`
}

type boardCard struct {
	Draft    string `json:"draft"`
	Project  string `json:"project"`
	Card     string `json:"card"`
	Title    string `json:"title"`
	Type     string `json:"type,omitempty"`
	Priority string `json:"priority,omitempty"`
	State    string `json:"state,omitempty"`
	PR       string `json:"pr,omitempty"`
}

// Artifact is one version of an artifact on blueprint id's sheet: a pack
// stage's, with the problems its checks accepted it with, and for the
// mockup's version "Use this design" took its chosen settings; the sign-off of a
// version shown or recorded as approved automatically, its data the
// sign-off's payload, with the reason a person approved it over a critical
// finding; or the board (emission, version 1) once the cards exist. The need
// as typed and the reviewer verdicts are not served here: verdicts travel in
// the view. slice names a per-slice stage's slice; ErrNotFound for an unknown
// blueprint, stage or version, and for a slice the record or the stage does
// not hold.
func (c *Controller) Artifact(ctx context.Context, id, stageID, slice string, version int) (protocol.BlueprintArtifact, error) {
	if _, err := c.opts.Store.GetBlueprint(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return protocol.BlueprintArtifact{}, ErrNotFound
		}

		return protocol.BlueprintArtifact{}, fmt.Errorf("blueprint: artifact: %w", err)
	}

	switch {
	case slice != "" && (stageID == signoffStage || stageID == emissionStage):
		return protocol.BlueprintArtifact{}, ErrNotFound
	case stageID == signoffStage:
		return c.signoffArtifact(ctx, id, version)
	case stageID == emissionStage && version == 1:
		return c.boardArtifact(ctx, id)
	case stageID == emissionStage:
		return protocol.BlueprintArtifact{}, ErrNotFound
	}

	key := pack.StepKey(stageID, slice)
	if _, ok := c.stepAt(key); !ok {
		return protocol.BlueprintArtifact{}, ErrNotFound
	}

	a, err := c.opts.Store.GetArtifact(ctx, id, key, version)
	if errors.Is(err, store.ErrNotFound) {
		return protocol.BlueprintArtifact{}, ErrNotFound
	}

	if err != nil {
		return protocol.BlueprintArtifact{}, fmt.Errorf("blueprint: artifact: %w", err)
	}

	data := a.Data

	if stageID == c.opts.Pack.MockupStage() {
		if data, err = c.withChosen(ctx, id, a); err != nil {
			return protocol.BlueprintArtifact{}, fmt.Errorf("blueprint: artifact: %w", err)
		}
	}

	return protocol.BlueprintArtifact{
		Stage: stageID, Slice: slice, Version: a.Version, Body: a.Body, Data: data, CheckProblems: problemsOf(a),
	}, nil
}

// signoffArtifact is the latest payload recorded for the sign-off of
// version, shown or approved automatically.
func (c *Controller) signoffArtifact(ctx context.Context, id string, version int) (protocol.BlueprintArtifact, error) {
	events, err := c.signoffEvents(ctx, id)
	if err != nil {
		return protocol.BlueprintArtifact{}, err
	}

	var found *store.Event

	for i := range events {
		if signoffVersion(events[i]) == version {
			found = &events[i]
		}
	}

	if found == nil {
		return protocol.BlueprintArtifact{}, ErrNotFound
	}

	data := found.Payload

	reason, err := c.overrideReason(ctx, id, version)
	if err != nil {
		return protocol.BlueprintArtifact{}, err
	}

	if reason != "" {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return protocol.BlueprintArtifact{}, fmt.Errorf("blueprint: decode the sign-off: %w", err)
		}

		payload["override_reason"] = reason

		if data, err = json.Marshal(payload); err != nil {
			return protocol.BlueprintArtifact{}, fmt.Errorf("blueprint: encode the sign-off: %w", err)
		}
	}

	return protocol.BlueprintArtifact{Stage: signoffStage, Version: version, Data: data}, nil
}

// overrideReason is the reason a person gave to approve the sign-off of
// version over a critical finding: the approval's feedback; "" when it was
// approved without one or not approved.
func (c *Controller) overrideReason(ctx context.Context, id string, version int) (string, error) {
	gates, err := c.opts.Store.ListGates(ctx, id, signoffStage)
	if err != nil {
		return "", fmt.Errorf("blueprint: read the sign-off's decisions: %w", err)
	}

	reason := ""

	for _, g := range gates {
		if g.Version == version && g.Decision == store.GateApprove {
			reason = g.Feedback
		}
	}

	return reason, nil
}

// boardArtifact is the board: the emitted cards, with what the watch last
// saw of each, and the total cost.
func (c *Controller) boardArtifact(ctx context.Context, id string) (protocol.BlueprintArtifact, error) {
	emitted, err := c.latestEvent(ctx, id, EventEmitted)
	if err != nil {
		return protocol.BlueprintArtifact{}, err
	}

	if emitted == nil {
		return protocol.BlueprintArtifact{}, ErrNotFound
	}

	var p struct {
		Cards    []boardCard `json:"cards"`
		Playbook string      `json:"playbook"`
		Manual   []string    `json:"manual"`
		Steps    []string    `json:"steps"`
		Notices  []string    `json:"notices"`
	}

	if err := json.Unmarshal(emitted.Payload, &p); err != nil {
		return protocol.BlueprintArtifact{}, fmt.Errorf("blueprint: read the emitted cards: %w", err)
	}

	b := board{Cards: p.Cards, Playbook: p.Playbook, Repos: []cm.PlaybookRepo{}, Manual: p.Manual, Steps: p.Steps, Notices: p.Notices}

	if b.Cards == nil {
		b.Cards = []boardCard{}
	}

	if b.Manual == nil {
		b.Manual = []string{}
	}

	if b.Steps == nil {
		b.Steps = []string{}
	}

	if b.Notices == nil {
		b.Notices = []string{}
	}

	watched, err := c.latestEvent(ctx, id, EventWatchState)
	if err != nil {
		return protocol.BlueprintArtifact{}, err
	}

	// The watch's last poll gives each card's state and pull request.
	var snap snapshot
	if watched != nil && json.Unmarshal(watched.Payload, &snap) == nil {
		seen := map[string]cardView{}
		for _, cv := range snap.Cards {
			seen[cv.Draft] = cv
		}

		for i := range b.Cards {
			if cv, ok := seen[b.Cards[i].Draft]; ok {
				b.Cards[i].State, b.Cards[i].PR = cv.State, cv.PR
			}
		}

		if snap.Repos != nil {
			b.Repos = snap.Repos
		}
	}

	if b.CostUSD, err = c.opts.Store.BlueprintCost(ctx, id); err != nil {
		return protocol.BlueprintArtifact{}, fmt.Errorf("blueprint: board cost: %w", err)
	}

	unpriced, err := c.anyUnpriced(ctx, id)
	if err != nil {
		return protocol.BlueprintArtifact{}, err
	}

	b.Priced = !unpriced

	data, err := json.Marshal(b)
	if err != nil {
		return protocol.BlueprintArtifact{}, fmt.Errorf("blueprint: encode the board: %w", err)
	}

	return protocol.BlueprintArtifact{Stage: emissionStage, Version: 1, Data: data}, nil
}

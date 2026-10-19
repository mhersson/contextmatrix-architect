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

// cardShaping reports whether a run of st writes or judges what a card
// demands: the stage that checks criteria, the stage that checks cards and a
// stage that judges them, by its readiness check or its loop to them. Only
// such a run learns which projects are proofs of concept; the design stages
// design as they would for any project.
func (c *Controller) cardShaping(st pipelineStep) bool {
	if slices.Contains(st.Checks, pack.CheckCriteria) || slices.Contains(st.Checks, pack.CheckCards) ||
		slices.Contains(st.Checks, pack.CheckReadiness) {
		return true
	}

	return st.LoopsTo != "" && st.LoopsTo == c.cardsStage()
}

// pocProjects is, for a run of st that shapes cards, the blueprint's target
// projects ContextMatrix flags as proofs of concept, in target order. It is
// read again as each run starts, so a flag changed during a blueprint
// reaches the runs after it and never the ones before. Any other run, and a
// controller without a project lister, gets none. A read that fails is the
// run's error: guessing either way would lower a production bar or spend
// the sweep the operator turned off.
func (c *Controller) pocProjects(ctx context.Context, sess *session, bp store.Blueprint, st pipelineStep) ([]string, error) {
	if !c.cardShaping(st) || c.opts.Projects == nil {
		return nil, nil
	}

	projects, err := c.opts.Projects(ctx, sess.mcpKey)
	if err != nil {
		return nil, fmt.Errorf("blueprint: read which projects are proofs of concept: %w", err)
	}

	flagged := map[string]bool{}

	for _, p := range projects {
		if p.POC {
			flagged[p.Name] = true
		}
	}

	var out []string

	for _, name := range bp.Projects {
		if flagged[name] {
			out = append(out, name)
		}
	}

	return out, nil
}

// writePOC names the target projects that are proofs of concept; nothing
// when there are none, so a production blueprint's task reads as before.
func writePOC(b *strings.Builder, poc []string) {
	if len(poc) == 0 {
		return
	}

	b.WriteString("## POC projects\n\nThese target projects are proofs of concept (POC); the instructions say what that changes:\n\n")

	for _, name := range poc {
		b.WriteString("- " + name + "\n")
	}

	b.WriteString("\n")
}

// sliceProjects maps each slice of slicing's newest version, SL<n> by its
// position as piecesOf names slices, to the projects slicing says it
// touches; nil when the pack has no slicing stage or slicing has no version
// yet, and then a slice's criteria alone say what it touches. Data that does
// not decode reads as no slices: slicing's own check refuses it.
func (c *Controller) sliceProjects(ctx context.Context, id string) (map[string][]string, error) {
	sid := c.opts.Pack.SlicingStage()
	if sid == "" {
		return nil, nil
	}

	art, err := c.opts.Store.LatestArtifact(ctx, id, sid)

	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", c.stepName(ctx, id, sid), err)
	}

	var d struct {
		Slices []struct {
			Projects []string `json:"projects"`
		} `json:"slices"`
	}

	if len(art.Data) == 0 || json.Unmarshal(art.Data, &d) != nil {
		return nil, nil
	}

	out := map[string][]string{}
	for i, s := range d.Slices {
		out[sliceID(i+1)] = s.Projects
	}

	return out, nil
}

// onlyPOC reports whether a slice touches at least one project and every
// one is a proof of concept. A project the flags do not hold counts as
// production, the safe side.
func onlyPOC(projects []string, isPOC map[string]bool) bool {
	return len(projects) > 0 && !slices.ContainsFunc(projects, func(p string) bool { return !isPOC[p] })
}

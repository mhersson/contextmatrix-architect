package blueprint

import (
	"context"
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

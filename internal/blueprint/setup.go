package blueprint

import (
	"context"
	"fmt"
	"slices"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// groundingStage is the first stage in pack order with the code tools, the
// one that checks the trees out; "" when the pack has none.
func (c *Controller) groundingStage() string {
	for _, st := range c.opts.Pack.Stages {
		if slices.Contains(st.Tools, pack.ToolCode) {
			return st.ID
		}
	}

	return ""
}

// reportSetup tells the session, once grounding ends and before the paid
// stages, what each target project lacks for an agent run. Nothing is
// published when no project lacks anything. A read that fails is logged and
// skipped: the report is advice, and decomposition plans the setup either
// way.
func (c *Controller) reportSetup(ctx context.Context, sess *session, bp store.Blueprint) {
	// An aborted run tells the session nothing more.
	if ctx.Err() != nil {
		return
	}

	gaps, err := c.setupGaps(ctx, sess, bp)
	if err != nil {
		c.opts.Logger.Warn("blueprint: setup gaps not reported", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))

		return
	}

	if len(gaps) == 0 {
		return
	}

	sess.audit.event(context.WithoutCancel(ctx), Event{Kind: EventSetupGaps, BlueprintID: bp.ID, Payload: map[string]any{"projects": gaps}})
}

// setupGaps reads, per target project with a repository and in target
// order, whether ContextMatrix declares a verify command, read again here
// so a fix made during grounding is not reported, whether its primary tree
// has an instruction file, and whether that repository has no commits, which
// grounding recorded as a source with no version. A project lacking none of
// them is left out.
func (c *Controller) setupGaps(ctx context.Context, sess *session, bp store.Blueprint) ([]any, error) {
	projects, err := c.opts.Projects(ctx, sess.mcpKey)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	view, err := contractView(ctx, projects, bp.Projects, c.opts.Workspace.TreeRoot(bp.ID))
	if err != nil {
		return nil, err
	}

	sources, err := c.opts.Store.ListSources(ctx, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}

	versions := map[string]string{}

	for _, s := range sources {
		if s.Kind == "repo" {
			versions[s.Identity] = s.Version
		}
	}

	byName := map[string]cm.Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}

	var gaps []any

	for _, name := range bp.Projects {
		cp, ok := view[name]
		if !ok || !cp.HasRepo {
			continue
		}

		ts, err := treesOf(byName[name])
		if err != nil {
			return nil, err
		}

		version, recorded := versions[code.CleanURL(ts[0].url)]
		empty := recorded && version == ""

		if cp.HasVerify && cp.HasInstructionFile && !empty {
			continue
		}

		gaps = append(gaps, map[string]any{
			"project": name, "has_verify": cp.HasVerify, "has_instruction_file": cp.HasInstructionFile, "empty": empty,
		})
	}

	return gaps, nil
}

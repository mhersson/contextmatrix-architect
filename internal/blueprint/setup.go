package blueprint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/pack"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// checkoutStage is the first stage in pack order with the code tools, the one
// that reports what the trees lack; "" when the pack has none.
func (c *Controller) checkoutStage() string {
	for _, st := range c.opts.Pack.Stages {
		if slices.Contains(st.Tools, pack.ToolCode) {
			return st.ID
		}
	}

	return ""
}

// reportSetup tells the session, once the checkout stage's first step ends
// (SL1's for a per-slice stage) and before the paid stages, what each
// target project lacks for an agent run. Nothing is published when no
// project lacks anything. A read that fails is logged and skipped: the
// report is advice, and decomposition plans the setup either way.
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

	// The stored mode, not bp's: a chain of runs holds bp from its start. The
	// plain note names a person only for a gap a yolo run does not leave
	// to one.
	mode := bp.Mode

	if cur, err := c.opts.Store.GetBlueprint(ctx, bp.ID); err != nil {
		c.opts.Logger.Warn("blueprint: read the mode for the setup report", "blueprint", bp.ID, "error", sess.audit.text(err.Error()))
	} else {
		mode = cur.Mode
	}

	payload := map[string]any{"projects": gaps, "mode": mode}

	// A gate reject runs the stage again; the same gaps are not told twice.
	if c.sameAsLastReport(ctx, sess, bp.ID, payload) {
		return
	}

	sess.audit.event(context.WithoutCancel(ctx), Event{Kind: EventSetupGaps, BlueprintID: bp.ID, Payload: payload})
}

// sameAsLastReport reports whether the blueprint's latest setup-gap event
// carries payload as stored. A read that fails reports false: a repeat beats
// a missing report.
func (c *Controller) sameAsLastReport(ctx context.Context, sess *session, id string, payload map[string]any) bool {
	last, err := c.opts.Store.LatestEvent(ctx, id, EventSetupGaps)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			c.opts.Logger.Warn("blueprint: read the last setup report", "blueprint", id, "error", err)
		}

		return false
	}

	now, err := marshalPayload(sess.audit.maskFields(payload))

	return err == nil && bytes.Equal(last.Payload, now)
}

// setupGaps reads, per target project with a repository and in target
// order, whether ContextMatrix declares a verify command, read again here
// so a fix made during the stage is not reported, whether its primary tree
// has an instruction file, and whether that repository has no commits, which
// the checkout recorded as a source with no version. A project lacking none of
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

		if cp.VerifyCommand != "" && cp.HasInstructionFile && !empty {
			continue
		}

		gaps = append(gaps, map[string]any{
			"project": name, "has_verify": cp.VerifyCommand != "", "has_instruction_file": cp.HasInstructionFile, "empty": empty,
		})
	}

	return gaps, nil
}

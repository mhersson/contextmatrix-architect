package blueprint

import (
	"context"
	"fmt"
	"slices"
	"strings"

	designpkg "github.com/mhersson/contextmatrix-architect/internal/design"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// sourceKindPackage is the kind of a source that is a package a stage with
// the design key kept: identity packageIdentityPrefix and the package name,
// version the locked version.
const (
	sourceKindPackage     = "package"
	packageIdentityPrefix = "npm:"
)

// reasonDesign is the reason of the error a design gather that failed posts;
// the run goes on without the material.
const reasonDesign = "design"

// keptPackage is a package kept under the tool root: later code stages grep
// the whole root, where the files still sit, and cite them by Tag.
type keptPackage struct{ Name, Version, Tag string }

// packageTags maps the directory under the tool root each package source is
// kept in to the package.
func packageTags(sources []store.Source) map[string]keptPackage {
	out := map[string]keptPackage{}

	for _, s := range sources {
		if name, ok := strings.CutPrefix(s.Identity, packageIdentityPrefix); ok && s.Kind == sourceKindPackage {
			out[designpkg.PackageDir(name, s.Version)] = keptPackage{Name: name, Version: s.Version, Tag: s.Tag()}
		}
	}

	return out
}

// prepareDesign gathers the design material of facts' trees for a run of
// stageID and records each kept package as a source. It returns the brief
// that opens the run's task, sized by window, the run's model window in
// tokens (0 when unknown), and each kept package by its directory. A failure
// is logged and posted and leaves the run without the material; it never
// fails the run.
func (c *Controller) prepareDesign(ctx context.Context, sess *session, bp store.Blueprint, stageID string, facts *codeFacts, window int) (string, map[string]keptPackage) {
	if c.opts.Design == nil {
		return "", nil
	}

	req := designpkg.Request{Root: facts.root, ImageCap: stage.MaxImageBytes}

	for _, r := range facts.repos {
		if !r.Empty {
			req.Trees = append(req.Trees, designpkg.Tree{Project: r.Project, Dir: r.Dir})
		}
	}

	m, err := c.opts.Design.Gather(ctx, req)
	if err != nil {
		c.designFailed(ctx, sess, bp.ID, stageID, err)

		return "", nil
	}

	tags, err := c.recordPackages(ctx, sess, bp, stageID, m.Packages)
	if err != nil {
		c.designFailed(ctx, sess, bp.ID, stageID, err)

		return "", nil
	}

	brief := designpkg.Brief(m, tagsByDir(tags), stage.WindowBytes(window), stage.MaxImageBytes)
	if brief == "" {
		return "", tags
	}

	return brief + "\n", tags
}

// recordPackages finds or adds the source of every kept package and returns
// their tags by directory. A package recorded at the same version is reused,
// so a revision's run cites what the first run did. Packages added are
// announced together, even when a later one fails to record.
func (c *Controller) recordPackages(ctx context.Context, sess *session, bp store.Blueprint, stageID string, pkgs []designpkg.Package) (map[string]keptPackage, error) {
	bg := context.WithoutCancel(ctx)

	known, err := c.opts.Store.ListSources(bg, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list sources: %w", err)
	}

	var added []any

	defer func() {
		if len(added) > 0 {
			sess.audit.event(bg, Event{Kind: EventFetched, BlueprintID: bp.ID, Payload: map[string]any{"stage": stageID, "packages": added}})
		}
	}()

	tags := make(map[string]keptPackage, len(pkgs))

	for _, p := range pkgs {
		identity := packageIdentityPrefix + p.Name

		if i := slices.IndexFunc(known, func(s store.Source) bool {
			return s.Kind == sourceKindPackage && s.Identity == identity && s.Version == p.Version
		}); i >= 0 {
			tags[p.Dir] = keptPackage{Name: p.Name, Version: p.Version, Tag: known[i].Tag()}

			continue
		}

		src, err := c.opts.Store.AddSource(bg, store.Source{
			BlueprintID: bp.ID, Kind: sourceKindPackage, Identity: identity, Version: p.Version,
			FetchedAt: c.opts.Now().UTC(), FirstUsedBy: stageID,
		})
		if err != nil {
			return nil, fmt.Errorf("blueprint: record source: %w", err)
		}

		tags[p.Dir] = keptPackage{Name: p.Name, Version: p.Version, Tag: src.Tag()}
		added = append(added, map[string]any{
			"tag": src.Tag(), "name": p.Name, "version": p.Version, "registry": p.Registry, "verified": p.Verified,
		})
	}

	return tags, nil
}

// designFailed logs and posts a design gather that failed. A cancelled run
// posts nothing: it is no failure, and Shutdown leaves the store untouched.
func (c *Controller) designFailed(ctx context.Context, sess *session, id, stageID string, err error) {
	if ctx.Err() != nil {
		return
	}

	c.opts.Logger.Warn("blueprint: gather design material", "blueprint", id, "stage", stageID, "error", sess.audit.text(err.Error()))
	sess.audit.event(ctx, Event{Kind: EventError, BlueprintID: id, Payload: map[string]any{
		"stage": stageID, "reason": reasonDesign,
		"message": fmt.Sprintf("The design material could not be gathered (%v), so the run goes on with the checkouts alone.", err),
	}})
}

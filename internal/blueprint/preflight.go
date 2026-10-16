package blueprint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// preflightResult is what preflight found. Blocking problems refuse
// emission until they are fixed and the human approves again. Warnings need
// a human: an approve after they were shown overrides them, and an emitting
// mode stops for a human instead of approving.
type preflightResult struct {
	Blocking []string
	Warnings []string
}

// preflight checks what emission needs: every card's project in
// ContextMatrix with a repository, a verify command and an instruction
// file; every repository's default branch where grounding read it; and no
// draft title already a card in another runnable playbook.
func (c *Controller) preflight(ctx context.Context, sess *session, bp store.Blueprint, draft contract.Draft, client CMClient) (preflightResult, error) {
	var res preflightResult

	projects, err := client.ListProjects(ctx)
	if err != nil {
		return res, fmt.Errorf("list projects: %w", err)
	}

	byName := map[string]cm.Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}

	var names []string

	for _, card := range draft.Cards {
		if !slices.Contains(names, card.Project) {
			names = append(names, card.Project)
		}
	}

	for _, name := range names {
		p, ok := byName[name]
		if !ok {
			res.Blocking = append(res.Blocking, fmt.Sprintf("project %s is not in ContextMatrix", name))

			continue
		}

		if len(p.RepoURLs()) == 0 {
			res.Blocking = append(res.Blocking, fmt.Sprintf("project %s declares no repository", name))
		}

		if p.Verify == nil || strings.TrimSpace(p.Verify.Command) == "" {
			res.Blocking = append(res.Blocking, fmt.Sprintf("project %s declares no verify command; the architect emits only into projects with one", name))
		}

		if c.opts.Workspace == nil {
			continue
		}

		dir := filepath.Join(c.opts.Workspace.TreeRoot(bp.ID), name)

		if _, err := os.Stat(dir); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("the checkout of %s is gone, so its instruction file was not re-checked", name))

			continue
		}

		info, err := code.Inspect(ctx, dir)
		if err != nil {
			return res, fmt.Errorf("inspect %s: %w", name, err)
		}

		if info.InstructionFile == "" {
			res.Blocking = append(res.Blocking, fmt.Sprintf("project %s has no AGENTS.md or CLAUDE.md at the root of its repository", name))
		}
	}

	moved, err := c.movedRepos(ctx, sess, bp)
	if err != nil {
		return res, err
	}

	taken, err := c.takenTitles(ctx, bp, draft, client)
	if err != nil {
		return res, err
	}

	res.Warnings = append(append(res.Warnings, moved...), taken...)

	return res, nil
}

// movedRepos warns about every repository whose default branch moved since
// grounding recorded it, or could not be read.
func (c *Controller) movedRepos(ctx context.Context, sess *session, bp store.Blueprint) ([]string, error) {
	if c.opts.Workspace == nil {
		return nil, nil
	}

	sources, err := c.opts.Store.ListSources(ctx, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}

	latest := map[string]store.Source{}

	var order []string

	for _, s := range sources {
		if s.Kind != "repo" {
			continue
		}

		if _, seen := latest[s.Identity]; !seen {
			order = append(order, s.Identity)
		}

		latest[s.Identity] = s
	}

	var creds code.Credentials
	if c.opts.Credentials != nil && sess.gitToken != "" {
		creds = c.opts.Credentials(sess.gitToken)
	}

	var out []string

	for _, identity := range order {
		src := latest[identity]

		_, sha, err := c.opts.Workspace.RemoteHead(ctx, identity, creds)

		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("could not read the default branch of %s [%s]: %v", identity, src.Tag(), err))
		case sha != src.Version:
			out = append(out, fmt.Sprintf("the default branch of %s [%s] moved since grounding, from %s to %s", identity, src.Tag(), short(src.Version), short(sha)))
		}
	}

	return out, nil
}

// takenTitles warns about every draft card whose title is already a card in
// another runnable playbook, a likely duplicate.
func (c *Controller) takenTitles(ctx context.Context, bp store.Blueprint, draft contract.Draft, client CMClient) ([]string, error) {
	recs, err := c.opts.Store.ListEmissions(ctx, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("list emissions: %w", err)
	}

	own := ""

	for _, e := range recs {
		if e.Kind == store.EmitPlaybook {
			own = e.CMID
		}
	}

	summaries, err := client.ListPlaybooks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list playbooks: %w", err)
	}

	var out []string

	for _, s := range summaries {
		if !s.Runnable || s.ID == own {
			continue
		}

		pb, err := client.GetPlaybook(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("read playbook %s: %w", s.ID, err)
		}

		for _, e := range pb.Entries {
			if e.Type != "card" {
				continue
			}

			for _, card := range draft.Cards {
				if strings.EqualFold(strings.TrimSpace(card.Title), strings.TrimSpace(e.CardTitle)) {
					out = append(out, fmt.Sprintf("%s has the title of %s in runnable playbook %s", card.ID, e.Card, s.ID))
				}
			}
		}
	}

	return out, nil
}

// short is a commit's first 12 characters.
func short(sha string) string {
	const n = 12

	if len(sha) > n {
		return sha[:n]
	}

	return sha
}

package blueprint

import (
	"context"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/mhersson/contextmatrix-protocol"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// preflightResult is what preflight found. Blocking problems refuse
// emission until they are fixed and the human approves again. Warnings need
// a human: an approve after they were shown overrides them, and an emitting
// mode stops for a human instead of approving. Steps are the repository
// setup only a human can do before the cards run; they are shown but
// neither block emission nor stop an emitting mode, because creating the
// cards does not need them. Verify are the projects that declare no verify
// command while the cards propose one: setup turns them into steps or
// notices, because when the run starts at once no human can set it.
// EmptyRepos are the identities (clean urls) of the repositories of card
// projects with no commits, in source order; a yolo run waits for them.
type preflightResult struct {
	Blocking   []string
	Warnings   []string
	Steps      []string
	Verify     []verifyGap
	EmptyRepos []string
}

// verifyGap is a project with no verify command that the cards propose one
// for.
type verifyGap struct {
	Project, Command string
}

// runsAtOnce is whether the cards run with no human in between: only yolo
// does, and a repository with no commits holds its run start for one.
func (r preflightResult) runsAtOnce(mode string) bool {
	return mode == protocol.ArchitectModeYolo && len(r.EmptyRepos) == 0
}

// setup is what a human is told about setup. A verify gap is a step while a
// person can still set the command before the cards run, and a notice of
// what happens without it when the run starts at once. Neither list is nil.
func (r preflightResult) setup(runsAtOnce bool) (steps, notices []string) {
	steps, notices = []string{}, []string{}

	if runsAtOnce {
		for _, g := range r.Verify {
			notices = append(notices, verifyNotice(g.Project, g.Command))
		}
	} else {
		for _, g := range r.Verify {
			steps = append(steps, fmt.Sprintf("set verify command `%s` on project %s in ContextMatrix", g.Command, g.Project))
		}
	}

	return append(steps, r.Steps...), notices
}

// verifyNotice says what a run does for a project with no verify command:
// the agent's own search, then a model proposal, then none.
func verifyNotice(project, command string) string {
	return fmt.Sprintf("project %s declares no verify command, so each card's agent looks for one in the repository as the cards build it (a test target or the project's test command), else uses one its model proposes, else runs unverified. The cards propose `%s`; set it on project %s in ContextMatrix to pin it.", project, command, project)
}

// preflight checks what emission needs: every card's project in ContextMatrix
// with a repository; every repository's default branch where the checkout read
// it; and no draft title already a card in another runnable playbook. A project
// ContextMatrix declares no verify command for is a verify gap when the draft
// proposes one and a warning when it proposes none. A missing instruction file
// is no finding: the contract makes a card write it.
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

	targets := map[string]bool{}

	for _, name := range names {
		for _, u := range byName[name].RepoURLs() {
			targets[code.CleanURL(u)] = true
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

		if p.Verify != nil && strings.TrimSpace(p.Verify.Command) != "" {
			continue
		}

		if command := draft.Setup[name].VerifyCommand; command != "" {
			res.Verify = append(res.Verify, verifyGap{Project: name, Command: command})
		} else {
			res.Warnings = append(res.Warnings, fmt.Sprintf("project %s declares no verify command and the cards propose none", name))
		}
	}

	moved, empty, err := c.movedRepos(ctx, sess, bp, targets)
	if err != nil {
		return res, err
	}

	taken, err := c.takenTitles(ctx, bp, draft, client)
	if err != nil {
		return res, err
	}

	res.Warnings = append(append(res.Warnings, moved...), taken...)
	for _, identity := range empty {
		res.Steps = append(res.Steps, fmt.Sprintf("push an initial commit to %s's default branch before running the cards; the agent needs a base branch", identity))
	}

	res.EmptyRepos = empty

	return res, nil
}

// movedRepos warns about every repository whose default branch moved since
// the checkout recorded it or could not be read, and returns the identity of
// every repository with no commits that a card runs in (targets, by clean
// url): the agent needs a base branch to work from. A first commit to a
// repository that was empty at the checkout is no move: it is the push step
// done.
func (c *Controller) movedRepos(ctx context.Context, sess *session, bp store.Blueprint, targets map[string]bool) ([]string, []string, error) {
	if c.opts.Workspace == nil {
		return nil, nil, nil
	}

	sources, err := c.opts.Store.ListSources(ctx, bp.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("list sources: %w", err)
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

	var warnings, empty []string

	for _, identity := range order {
		src := latest[identity]

		_, sha, err := c.opts.Workspace.RemoteHead(ctx, identity, creds)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("could not read the default branch of %s [%s]: %v", identity, src.Tag(), err))

			continue
		}

		if sha != src.Version && src.Version != "" {
			warnings = append(warnings, fmt.Sprintf("the default branch of %s [%s] moved since the checkout, from %s to %s", identity, src.Tag(), commitOf(src.Version), commitOf(sha)))
		}

		if sha == "" && targets[identity] {
			empty = append(empty, identity)
		}
	}

	return warnings, empty, nil
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

// commitOf names a commit in a message: its short sha, or an empty
// repository for no commit.
func commitOf(sha string) string {
	if sha == "" {
		return "an empty repository"
	}

	return short(sha)
}

// short is a commit's first 12 characters.
func short(sha string) string {
	const n = 12

	if len(sha) > n {
		return sha[:n]
	}

	return sha
}

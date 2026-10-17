package blueprint

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mhersson/contextmatrix-architect/internal/cm"
	"github.com/mhersson/contextmatrix-architect/internal/code"
	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

var errNoCodeSupport = errors.New("blueprint: code stages need a workspace and the project list")

var errNoCMSupport = errors.New("blueprint: cm stages need the ContextMatrix tool source")

// cmSourceIdentity is the ContextMatrix board as a source: one per
// blueprint, because CM state carries no version a read could cite.
const cmSourceIdentity = "contextmatrix"

// cmSource returns the blueprint's ContextMatrix source, recording it the
// first time a run of a stage with the cm tools prepares them: the tag must
// exist before the tools open, since every result carries it.
func (c *Controller) cmSource(ctx context.Context, bp store.Blueprint, stageID string) (store.Source, error) {
	known, err := c.opts.Store.ListSources(ctx, bp.ID)
	if err != nil {
		return store.Source{}, fmt.Errorf("blueprint: list sources: %w", err)
	}

	for _, s := range known {
		if s.Kind == "cm" && s.Identity == cmSourceIdentity {
			return s, nil
		}
	}

	src, err := c.opts.Store.AddSource(ctx, store.Source{
		BlueprintID: bp.ID, Kind: "cm", Identity: cmSourceIdentity,
		FetchedAt: c.opts.Now().UTC(), FirstUsedBy: stageID,
	})
	if err != nil {
		return store.Source{}, fmt.Errorf("blueprint: record source: %w", err)
	}

	return src, nil
}

// codeFacts is what a code stage knows: the jailed tool root, the facts
// repo_info reports, and the contract view of every target project.
type codeFacts struct {
	root     string
	repos    []stage.RepoFacts
	projects map[string]contract.Project
}

// tree is one repository of a target project and the tree it is checked
// out as under the blueprint's root.
type tree struct {
	project string
	name    string
	url     string
	primary bool
}

// treesOf names a project's trees, primary first. The repository flagged
// primary, else the first with a url, is the tree <project>; every other one
// is <project>@<repo name>, so a path's first segment names the tree and no
// tree nests in another. Repositories without a url are skipped; a project
// without a repos list has its single repo as the primary.
func treesOf(p cm.Project) ([]tree, error) {
	if !safeSegment(p.Name) {
		return nil, fmt.Errorf("blueprint: project %q is not a safe directory name", p.Name)
	}

	if len(p.Repos) == 0 {
		if strings.TrimSpace(p.Repo) == "" {
			return nil, nil
		}

		return []tree{{project: p.Name, name: p.Name, url: p.Repo, primary: true}}, nil
	}

	var repos []cm.Repo

	for _, r := range p.Repos {
		if strings.TrimSpace(r.URL) != "" {
			repos = append(repos, r)
		}
	}

	if len(repos) == 0 {
		return nil, nil
	}

	primary := 0

	for i, r := range repos {
		if r.Primary {
			primary = i

			break
		}
	}

	out := []tree{{project: p.Name, name: p.Name, url: repos[primary].URL, primary: true}}

	for i, r := range repos {
		if i == primary {
			continue
		}

		if !safeSegment(r.Name) {
			return nil, fmt.Errorf("blueprint: project %q: repository name %q is not a safe directory name", p.Name, r.Name)
		}

		out = append(out, tree{project: p.Name, name: p.Name + "@" + r.Name, url: r.URL})
	}

	return out, nil
}

// safeSegment admits one path segment that cannot climb out of or nest
// inside the tree root; the workspace applies the same rule to tree names.
func safeSegment(s string) bool {
	return s != "" && s != "." && !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
}

// contractView is the card contract's view of each target project: from a
// project list, what ContextMatrix declares (repository, verify command,
// card types and priorities, every other project's repositories), and from
// the blueprint's trees under root, the primary tree the where check reads
// and whether it has an instruction file. A target missing from the list is
// left out, so the contract reports its cards.
func contractView(ctx context.Context, projects []cm.Project, targets []string, root string) (map[string]contract.Project, error) {
	byName := map[string]cm.Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}

	out := map[string]contract.Project{}

	for _, name := range targets {
		p, ok := byName[name]
		if !ok {
			continue
		}

		ts, err := treesOf(p)
		if err != nil {
			return nil, err
		}

		cp := contract.Project{
			HasVerify: p.Verify != nil && strings.TrimSpace(p.Verify.Command) != "",
			Types:     p.Types, Priorities: p.Priorities,
			ForeignRepos: foreignRepos(projects, name),
		}

		if len(ts) > 0 {
			cp.HasRepo = true
			cp.Root = filepath.Join(root, ts[0].name)

			info, err := code.Inspect(ctx, cp.Root)
			if err != nil {
				return nil, fmt.Errorf("blueprint: inspect %s: %w", ts[0].name, err)
			}

			cp.HasInstructionFile = info.InstructionFile != ""
		}

		out[name] = cp
	}

	return out, nil
}

// prepareCode makes sure every target project's repositories are checked
// out for the blueprint and recorded as sources. A tree checked out by an
// earlier stage is reused at its recorded sha, so later stages and a
// restarted process read the commit the checkout recorded; a missing one is
// checked out again and recorded as a new source version when its sha
// moved. Sources added here are announced to the panel. Sources are keyed
// on the clean url, so a credential in a project's url never reaches the
// record.
func (c *Controller) prepareCode(ctx context.Context, sess *session, bp store.Blueprint, stageID string) (*codeFacts, error) {
	if c.opts.Workspace == nil || c.opts.Projects == nil {
		return nil, errNoCodeSupport
	}

	// The fetches end with the run; what they wrote is recorded regardless.
	bg := context.WithoutCancel(ctx)

	projects, err := c.opts.Projects(ctx, sess.mcpKey)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list projects: %w", err)
	}

	byName := map[string]cm.Project{}
	for _, p := range projects {
		byName[p.Name] = p
	}

	// Every tree is named before anything is fetched, so a bad name fails
	// the stage without a checkout.
	facts := &codeFacts{root: c.opts.Workspace.TreeRoot(bp.ID)}
	owners := map[string]string{}
	verifies := map[string]string{}

	var trees []tree

	for _, name := range bp.Projects {
		p, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("blueprint: target project %q is not in ContextMatrix", name)
		}

		verify := ""
		if p.Verify != nil {
			verify = strings.TrimSpace(p.Verify.Command)
		}

		ts, err := treesOf(p)
		if err != nil {
			return nil, err
		}

		verifies[name] = verify

		for _, t := range ts {
			switch owner, taken := owners[t.name]; {
			case taken && owner == name:
				return nil, fmt.Errorf("blueprint: project %q names the tree %q twice", name, t.name)
			case taken:
				return nil, fmt.Errorf("blueprint: projects %q and %q both name the tree %q", owner, name, t.name)
			}

			owners[t.name] = name
		}

		trees = append(trees, ts...)
	}

	known, err := c.opts.Store.ListSources(bg, bp.ID)
	if err != nil {
		return nil, fmt.Errorf("blueprint: list sources: %w", err)
	}

	byIdentity := map[string]store.Source{}
	for _, s := range known {
		byIdentity[s.Identity] = s
	}

	var creds code.Credentials
	if c.opts.Credentials != nil && sess.gitToken != "" {
		creds = c.opts.Credentials(sess.gitToken)
	}

	// Sources recorded here are announced even when a later checkout fails,
	// since a retry reuses them without announcing them again.
	var added []any

	defer func() {
		if len(added) > 0 {
			sess.audit.event(bg, Event{Kind: EventGrounded, BlueprintID: bp.ID, Payload: map[string]any{"stage": stageID, "sources": added}})
		}
	}()

	for _, t := range trees {
		clean := code.CleanURL(t.url)
		dir := filepath.Join(facts.root, t.name)
		src, recorded := byIdentity[clean]
		branch := ""

		if _, statErr := os.Stat(dir); statErr != nil || !recorded {
			co, err := c.opts.Workspace.Checkout(ctx, bp.ID, t.name, clean, creds)
			if err != nil {
				return nil, fmt.Errorf("blueprint: check out %s: %w", t.name, err)
			}

			branch = co.DefaultBranch

			if !recorded || src.Version != co.SHA {
				src, err = c.opts.Store.AddSource(bg, store.Source{
					BlueprintID: bp.ID, Kind: "repo", Identity: co.RepoURL, Version: co.SHA,
					FetchedAt: c.opts.Now().UTC(), FirstUsedBy: stageID,
				})
				if err != nil {
					return nil, fmt.Errorf("blueprint: record source: %w", err)
				}

				byIdentity[co.RepoURL] = src
				added = append(added, map[string]any{"tag": src.Tag(), "project": t.project, "identity": co.RepoURL, "sha": co.SHA})
			}
		}

		info, err := code.Inspect(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("blueprint: inspect %s: %w", t.name, err)
		}

		facts.repos = append(facts.repos, stage.RepoFacts{
			Project: t.project, Dir: t.name, DefaultBranch: branch, SHA: src.Version,
			InstructionFile: info.InstructionFile, VerifyCommand: verifies[t.project], SourceTag: src.Tag(),
			Empty: src.Version == "",
		})
	}

	facts.projects, err = contractView(ctx, projects, bp.Projects, facts.root)
	if err != nil {
		return nil, err
	}

	return facts, nil
}

// foreignRepos returns the repository urls of every CM project but the named
// one, collected as CM's self-containment lint collects them: in CM's
// project order, the legacy repo when set and then every repos url, so the
// findings match CM's warnings and come in the same order on every run.
func foreignRepos(projects []cm.Project, name string) []string {
	var out []string

	for _, p := range projects {
		if p.Name == name {
			continue
		}

		if p.Repo != "" {
			out = append(out, p.Repo)
		}

		for _, r := range p.Repos {
			out = append(out, r.URL)
		}
	}

	return out
}

// checkoutLines describes the trees for a stage's task, then the card types
// and priorities each target project declares, since the card contract
// accepts only those.
func checkoutLines(facts *codeFacts) string {
	if facts == nil || len(facts.repos) == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteString("Checkouts under the tool root (cite the tag for anything read there):\n")

	for _, r := range facts.repos {
		if r.Empty {
			fmt.Fprintf(&b, "- %s/ is project %s, an empty repository with no commits [%s]\n", r.Dir, r.Project, r.SourceTag)

			continue
		}

		fmt.Fprintf(&b, "- %s/ is project %s at %s [%s]\n", r.Dir, r.Project, r.SHA, r.SourceTag)
	}

	for _, name := range slices.Sorted(maps.Keys(facts.projects)) {
		p := facts.projects[name]

		var parts []string

		if len(p.Types) > 0 {
			parts = append(parts, "card types "+strings.Join(p.Types, ", "))
		}

		if len(p.Priorities) > 0 {
			parts = append(parts, "priorities "+strings.Join(p.Priorities, ", "))
		}

		if len(parts) > 0 {
			fmt.Fprintf(&b, "- project %s declares %s; use only these.\n", name, strings.Join(parts, " and "))
		}
	}

	b.WriteString("\n")

	return b.String()
}

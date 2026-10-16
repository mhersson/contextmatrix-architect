package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Project is what the checks know about a target project: whether CM
// declares a repository and a verify command for it, and the checkout root
// the where check reads. An empty Root skips the where check.
type Project struct {
	HasRepo   bool
	HasVerify bool
	Root      string
}

// Finding is one failed check on one card.
type Finding struct {
	CardID  string
	Check   string
	Message string
}

func (f Finding) String() string { return f.CardID + ": " + f.Check + ": " + f.Message }

// Check runs every mechanical check over a draft and returns the findings,
// in card order. It is pure: the only I/O is reading the checkout under a
// project's Root for the where check.
func Check(d Draft, lists Lists, projects map[string]Project) []Finding {
	var out []Finding

	ids := map[string]string{}
	for _, c := range d.Cards {
		ids[c.ID] = c.Project
	}

	for _, c := range d.Cards {
		out = append(out, checkBody(c, lists)...)
		out = append(out, checkTitle(c, lists)...)
		out = append(out, checkDependsOn(c, ids)...)

		p, known := projects[c.Project]
		if !known {
			out = append(out, Finding{c.ID, "project", fmt.Sprintf("project %q is not a target project", c.Project)})

			continue
		}

		if !p.HasRepo {
			out = append(out, Finding{c.ID, "project_repo", fmt.Sprintf("project %q declares no repository", c.Project)})
		}

		if !p.HasVerify {
			out = append(out, Finding{c.ID, "project_verify", fmt.Sprintf("project %q declares no verify command", c.Project)})
		}

		if p.Root != "" {
			out = append(out, checkWhere(c, p.Root)...)
		}
	}

	out = append(out, checkAcyclic(d.Cards)...)

	return out
}

// headings returns the "## " headings of a body, trimmed.
func headings(body string) []string {
	var out []string

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "## ")))
		}
	}

	return out
}

// section returns the non-blank lines under the first "## <name>" heading,
// up to the next "## " heading.
func section(body, name string) []string {
	var (
		out []string
		in  bool
	)

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			in = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "## ")), name)

			continue
		}

		if in && strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}

	return out
}

var numbered = regexp.MustCompile(`^\s*\d+[.)]\s+\S`)

func checkBody(c Card, lists Lists) []Finding {
	var out []Finding

	hs := headings(c.Body)

	for _, want := range lists.RequiredSections {
		found := false

		for _, h := range hs {
			if strings.EqualFold(h, want) {
				found = true

				break
			}
		}

		if !found {
			out = append(out, Finding{c.ID, "required_section", fmt.Sprintf("missing section %q", want)})
		}
	}

	for _, h := range hs {
		if isStripped(h, lists) {
			out = append(out, Finding{c.ID, "stripped_heading", fmt.Sprintf("heading %q is stripped by the agent", h)})
		}
	}

	// A continuation line of a numbered criterion is indented; anything else
	// under the heading must open with a number.
	for i, line := range section(c.Body, "Acceptance criteria") {
		if !numbered.MatchString(line) && !strings.HasPrefix(line, "   ") {
			out = append(out, Finding{c.ID, "criteria_numbered", fmt.Sprintf("criterion line %d is not numbered: %q", i+1, strings.TrimSpace(line))})

			break
		}
	}

	if !isBug(c) {
		lower := strings.ToLower(c.Title + "\n" + c.Body)

		for _, phrase := range lists.DefectPhrases {
			if strings.Contains(lower, strings.ToLower(phrase)) {
				out = append(out, Finding{c.ID, "defect_phrase", fmt.Sprintf("%q routes a feature card to the bug path", phrase)})
			}
		}
	}

	for _, pat := range lists.ForbiddenReferences {
		re, err := regexp.Compile(pat.Pattern)
		if err != nil {
			continue // the pack loader rejects a bad pattern; nothing to do here
		}

		if m := re.FindString(c.Body); m != "" {
			out = append(out, Finding{c.ID, "forbidden_reference", fmt.Sprintf("%s: %q", pat.Name, strings.TrimSpace(m))})
		}
	}

	return out
}

// isStripped reports whether the agent removes a heading before prompting:
// it matches a stripped heading exactly or starts with a stripped prefix.
func isStripped(h string, lists Lists) bool {
	for _, s := range lists.StrippedHeadings {
		if strings.EqualFold(h, s) {
			return true
		}
	}

	for _, prefix := range lists.StrippedHeadingPrefixes {
		if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return true
		}
	}

	return false
}

// isBug reports whether a card takes the bug path. Only a bug-typed card may
// name a defect in its body; a feature card must not, whatever its title says.
func isBug(c Card) bool {
	return strings.EqualFold(c.Type, "bug")
}

func checkTitle(c Card, lists Lists) []Finding {
	words := strings.Fields(c.Title)
	if len(words) == 0 {
		return []Finding{{c.ID, "title", "title is empty"}}
	}

	for _, verb := range lists.MaintenanceVerbs {
		if !strings.EqualFold(words[0], verb) {
			continue
		}

		for _, label := range c.Labels {
			for _, allowed := range lists.MaintenanceLabels {
				if strings.EqualFold(label, allowed) {
					return nil
				}
			}
		}

		return []Finding{{c.ID, "maintenance_label", fmt.Sprintf("title verb %q needs one of the labels %v", words[0], lists.MaintenanceLabels)}}
	}

	return nil
}

func checkDependsOn(c Card, ids map[string]string) []Finding {
	var out []Finding

	for _, dep := range c.DependsOn {
		project, ok := ids[dep]

		switch {
		case !ok:
			out = append(out, Finding{c.ID, "depends_on_known", fmt.Sprintf("depends on unknown card %q", dep)})
		case project != c.Project:
			out = append(out, Finding{c.ID, "depends_on_same_project", fmt.Sprintf("depends on %q in project %q; depends_on stays within a project", dep, project)})
		}
	}

	return out
}

// checkAcyclic reports the cards that sit on a depends_on cycle. A card that
// merely depends on a cycle is not reported.
func checkAcyclic(cards []Card) []Finding {
	deps := map[string][]string{}
	for _, c := range cards {
		deps[c.ID] = c.DependsOn
	}

	var out []Finding

	for _, c := range cards {
		if reaches(deps, c.DependsOn, c.ID) {
			out = append(out, Finding{c.ID, "depends_on_acyclic", "depends_on forms a cycle"})
		}
	}

	return out
}

// reaches reports whether target is reachable by following depends_on from
// any of the start ids.
func reaches(deps map[string][]string, start []string, target string) bool {
	seen := map[string]bool{}
	stack := slices.Clone(start)

	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if id == target {
			return true
		}

		if seen[id] {
			continue
		}

		seen[id] = true
		stack = append(stack, deps[id]...)
	}

	return false
}

// checkWhere verifies every path exists under root and every named symbol
// occurs in its file. Paths escaping root are findings, never reads.
func checkWhere(c Card, root string) []Finding {
	var out []Finding

	for _, w := range c.Where {
		clean := filepath.Clean(filepath.FromSlash(w.Path))
		if w.Path == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			out = append(out, Finding{c.ID, "where_exists", fmt.Sprintf("path %q is not inside the repository", w.Path)})

			continue
		}

		full := filepath.Join(root, clean)

		data, err := os.ReadFile(full)
		if err != nil {
			if info, statErr := os.Stat(full); statErr == nil && info.IsDir() && w.Symbol == "" {
				continue
			}

			out = append(out, Finding{c.ID, "where_exists", fmt.Sprintf("path %q does not exist at the recorded commit", w.Path)})

			continue
		}

		if w.Symbol != "" && !strings.Contains(string(data), w.Symbol) {
			out = append(out, Finding{c.ID, "where_exists", fmt.Sprintf("symbol %q is not in %q at the recorded commit", w.Symbol, w.Path)})
		}
	}

	return out
}

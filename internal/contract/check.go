package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Project is what the checks know about a target project: whether CM
// declares a repository and a verify command for it, the checkout root the
// where check reads (empty skips it and the instruction check), whether that
// checkout has an instruction file, the card types and priorities it
// declares (empty skips that check), and the repository urls of every other
// CM project, which no card may name.
type Project struct {
	HasRepo            bool
	HasVerify          bool
	HasInstructionFile bool
	Root               string
	Types              []string
	Priorities         []string
	ForeignRepos       []string
}

// Finding is one failed check on one card.
type Finding struct {
	CardID  string
	Check   string
	Message string
}

func (f Finding) String() string { return f.CardID + ": " + f.Check + ": " + f.Message }

// Check runs every mechanical check over a draft and returns the findings,
// in card order, then the instruction setup and cycle findings. It is pure:
// the only I/O is reading the checkout under a project's Root for the where
// check.
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

		out = append(out, checkFields(c, p)...)
		out = append(out, checkForeignRepos(c, p.ForeignRepos)...)
		out = append(out, checkVerifySetup(c, p, d.Setup[c.Project], lists)...)

		if p.Root != "" {
			out = append(out, checkWhere(c, p.Root)...)
		}
	}

	out = append(out, checkInstructionSetup(d, projects)...)
	out = append(out, checkAcyclic(d.Cards)...)

	return out
}

// checkVerifySetup applies to a card whose project declares no verify
// command: the draft proposes one in the project's setup, and the card's
// acceptance criteria run it, written exactly as proposed. A proposal for a
// project that declares one is ignored; the sign-off reads CM itself.
func checkVerifySetup(c Card, p Project, s Setup, lists Lists) []Finding {
	if p.HasVerify {
		return nil
	}

	if s.VerifyCommand == "" {
		return []Finding{{c.ID, "setup_verify", fmt.Sprintf(
			"project %q declares no verify command; propose one in setup.%s.verify_command and run it in this card's acceptance criteria", c.Project, c.Project)}}
	}

	if !strings.Contains(strings.Join(section(c.Body, "Acceptance criteria", lists), "\n"), s.VerifyCommand) {
		return []Finding{{c.ID, "setup_verify", fmt.Sprintf("acceptance criteria do not run the proposed verify command %q", s.VerifyCommand)}}
	}

	return nil
}

// checkInstructionSetup applies to every target project with a checkout. One
// without an instruction file names one of its cards in its setup as the
// card that also writes AGENTS.md, and every other card of the project
// depends on that card, directly or through others, so the agent reads
// AGENTS.md in all of them; the order and cycle checks then make it the
// project's first card. One with an instruction file names no such card,
// which would overwrite it.
func checkInstructionSetup(d Draft, projects map[string]Project) []Finding {
	deps := map[string][]string{}
	byProject := map[string][]Card{}

	var names []string

	for _, c := range d.Cards {
		deps[c.ID] = c.DependsOn

		if _, seen := byProject[c.Project]; !seen {
			names = append(names, c.Project)
		}

		byProject[c.Project] = append(byProject[c.Project], c)
	}

	var out []Finding

	for _, name := range names {
		p, known := projects[name]
		if !known || p.Root == "" {
			continue
		}

		cards := byProject[name]
		named := d.Setup[name].InstructionCard

		if p.HasInstructionFile {
			if named != "" {
				out = append(out, Finding{named, "setup_instruction", fmt.Sprintf(
					"project %q already has an instruction file, which this card would overwrite; drop setup.%s.instruction_card", name, name)})
			}

			continue
		}

		if !slices.ContainsFunc(cards, func(c Card) bool { return c.ID == named }) {
			out = append(out, Finding{cards[0].ID, "setup_instruction", fmt.Sprintf(
				"project %q has no AGENTS.md or CLAUDE.md; its first card also writes AGENTS.md (stack, verify command, layout, conventions), and setup.%s.instruction_card names that card", name, name)})

			continue
		}

		for _, c := range cards {
			if c.ID != named && !reaches(deps, c.DependsOn, named) {
				out = append(out, Finding{c.ID, "setup_instruction", fmt.Sprintf("does not depend on %s, which writes AGENTS.md for project %q", named, name)})
			}
		}
	}

	return out
}

// checkFields requires a type and a priority the project declares, matched
// exactly as CM's create_card does; each is checked only when the project
// declares any.
func checkFields(c Card, p Project) []Finding {
	var out []Finding

	if len(p.Types) > 0 && !slices.Contains(p.Types, c.Type) {
		out = append(out, Finding{c.ID, "type", fmt.Sprintf("type %q is not one of the project's types %v", c.Type, p.Types)})
	}

	switch {
	case strings.TrimSpace(c.Priority) == "" && len(p.Priorities) > 0:
		out = append(out, Finding{c.ID, "priority", fmt.Sprintf("priority is required, one of %v", p.Priorities)})
	case len(p.Priorities) > 0 && !slices.Contains(p.Priorities, c.Priority):
		out = append(out, Finding{c.ID, "priority", fmt.Sprintf("priority %q is not one of the project's priorities %v", c.Priority, p.Priorities)})
	}

	return out
}

// heading returns the title of a "## " heading line. A column-0 heading
// always counts; an indented one counts only when the agent strips it, since
// the agent starts a stripped section at an indented heading too.
func heading(line string, lists Lists) (string, bool) {
	if title, ok := strings.CutPrefix(line, "## "); ok {
		return strings.TrimSpace(title), true
	}

	if title, ok := strings.CutPrefix(strings.TrimSpace(line), "## "); ok {
		title = strings.TrimSpace(title)

		return title, isStripped(title, lists)
	}

	return "", false
}

// headings returns the headings of a body, trimmed.
func headings(body string, lists Lists) []string {
	var out []string

	for _, line := range strings.Split(body, "\n") {
		if h, ok := heading(line, lists); ok {
			out = append(out, h)
		}
	}

	return out
}

// section returns the non-blank lines under the first "## <name>" heading,
// up to the next heading, an indented stripped one included.
func section(body, name string, lists Lists) []string {
	var (
		out []string
		in  bool
	)

	for _, line := range strings.Split(body, "\n") {
		if h, ok := heading(line, lists); ok {
			in = strings.EqualFold(h, name)

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

	hs := headings(c.Body, lists)

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

	out = append(out, checkCriteria(c, hs, lists)...)

	// The phrase scans skip mockup blocks, as the agent's classifier and CM's
	// lint do; the structural ones above read the whole body, as the agent's
	// section parser does not track fences.
	text := WithoutMockups(c.Body)

	if !isBug(c) {
		lower := strings.ToLower(c.Title + "\n" + text)

		for _, phrase := range lists.DefectPhrases {
			if strings.Contains(lower, strings.ToLower(phrase)) {
				out = append(out, Finding{c.ID, "defect_phrase", fmt.Sprintf("%q routes a feature card to the bug path", phrase)})
			}
		}
	}

	if phrase := DanglingReference(text, lists); phrase != "" {
		out = append(out, Finding{c.ID, "dangling_reference", fmt.Sprintf("%q points at text this card does not carry; write the fact into the card", phrase)})
	}

	for _, pat := range lists.ForbiddenReferences {
		re, err := regexp.Compile(pat.Pattern)
		if err != nil {
			continue // the pack loader rejects a bad pattern; nothing to do here
		}

		if m := re.FindString(c.Title + "\n" + text); m != "" {
			out = append(out, Finding{c.ID, "forbidden_reference", fmt.Sprintf("%s: %q", pat.Name, strings.TrimSpace(m))})
		}
	}

	return out
}

// DanglingReference returns the first phrase of the dangling references
// list in text, matched case-insensitively as whole words ("as above" is not
// in "was above"); "" when there is none.
func DanglingReference(text string, lists Lists) string {
	lower := strings.ToLower(text)

	for _, phrase := range lists.DanglingReferences {
		if phrase != "" && containsWords(lower, strings.ToLower(phrase)) {
			return phrase
		}
	}

	return ""
}

// containsWords reports whether phrase is in text with no letter, digit,
// hyphen or underscore just before or after it.
func containsWords(text, phrase string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], phrase)
		if i < 0 {
			return false
		}

		start, end := from+i, from+i+len(phrase)

		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])

		if (start == 0 || !isWordRune(before)) && (end == len(text) || !isWordRune(after)) {
			return true
		}

		from = start + 1
	}
}

// isWordRune counts a hyphen and an underscore as part of a word, so
// "as above" is not found in "such as above-the-fold".
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_'
}

// checkCriteria requires the acceptance criteria to be numbered: at least
// one numbered line, and every line either numbered or an indented
// continuation of one.
func checkCriteria(c Card, hs []string, lists Lists) []Finding {
	const name = "Acceptance criteria"

	if !slices.ContainsFunc(hs, func(h string) bool { return strings.EqualFold(h, name) }) {
		return nil // required_section reports the missing section
	}

	count := 0

	for i, line := range section(c.Body, name, lists) {
		switch {
		case numbered.MatchString(line):
			count++
		case !strings.HasPrefix(line, "   "):
			return []Finding{{c.ID, "criteria_numbered", fmt.Sprintf("criterion line %d is not numbered: %q", i+1, strings.TrimSpace(line))}}
		}
	}

	if count == 0 {
		return []Finding{{c.ID, "criteria_numbered", "no numbered criterion"}}
	}

	return nil
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

// firstWord is the agent's routing word: the title's first word without
// trailing punctuation.
func firstWord(title string) string {
	words := strings.Fields(title)
	if len(words) == 0 {
		return ""
	}

	return strings.TrimRight(words[0], ".:,!?")
}

func checkTitle(c Card, lists Lists) []Finding {
	first := firstWord(c.Title)
	if first == "" {
		return []Finding{{c.ID, "title", "title is empty"}}
	}

	if !isBug(c) {
		for _, verb := range lists.BugTitleVerbs {
			if strings.EqualFold(first, verb) {
				return []Finding{{c.ID, "bug_title_verb", fmt.Sprintf("title verb %q routes a %s card to the bug path", first, c.Type)}}
			}
		}
	}

	for _, verb := range lists.MaintenanceVerbs {
		if !strings.EqualFold(first, verb) {
			continue
		}

		for _, label := range c.Labels {
			for _, allowed := range lists.MaintenanceLabels {
				if strings.EqualFold(label, allowed) {
					return nil
				}
			}
		}

		return []Finding{{c.ID, "maintenance_label", fmt.Sprintf("title verb %q needs one of the labels %v", first, lists.MaintenanceLabels)}}
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

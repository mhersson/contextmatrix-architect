package retro

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// TaskFor is the retro run's task: the card template, the lists, the
// readiness checks and every struggled card as fenced data.
func TaskFor(struggled []Struggle, cardTemplate string, lists contract.Lists, checks string) string {
	var b strings.Builder

	b.WriteString("## Card template\n\n" + cardTemplate + "\n\n")

	// A Lists is plain data and always encodes.
	encoded, _ := yaml.Marshal(lists)

	b.WriteString("## Lists\n\n" + fence("yaml", string(encoded)))

	if checks != "" {
		b.WriteString("\n## Readiness checks\n\n" + checks + "\n")
	}

	b.WriteString("\n## Cards that struggled\n")

	for _, s := range struggled {
		fmt.Fprintf(&b, "\n### %s: %s\n\n", s.Card, s.Title)
		fmt.Fprintf(&b, "- project: %s\n", s.Project)
		fmt.Fprintf(&b, "- state: %s\n", s.State)
		fmt.Fprintf(&b, "- worker: %s\n", s.Worker)
		fmt.Fprintf(&b, "- review attempts: %d\n", s.ReviewAttempts)
		fmt.Fprintf(&b, "- cost: $%.2f\n", s.CostUSD)

		if slices.Contains(s.Why, WhyCost) {
			fmt.Fprintf(&b, "- median cost of the costed cards: $%.2f\n", s.MedianUSD)
		}

		fmt.Fprintf(&b, "- subtasks: %d\n", s.Subtasks)

		if s.Reason != "" {
			fmt.Fprintf(&b, "- reason: %s\n", s.Reason)
		}

		fmt.Fprintf(&b, "- why it struggled: %s\n\n", strings.Join(s.Why, ", "))
		b.WriteString(fence("", s.Body))
	}

	return b.String()
}

// LessonQuestion is the round question that asks the operator to decide one
// proposed lesson.
func LessonQuestion(l store.Lesson, f Finding) stage.RoundQuestion {
	why := []string{l.Rationale, "Rule: " + f.Rule, "Evidence: " + f.Evidence}

	if f.Lesson != nil {
		for _, x := range f.Lesson.Overlay.EdgeCases {
			why = append(why, "edge case: "+x)
		}

		for _, x := range f.Lesson.Overlay.DanglingReferences {
			why = append(why, "dangling reference: "+x)
		}

		for _, p := range f.Lesson.Overlay.ForbiddenReferences {
			why = append(why, "forbidden reference "+p.Name+": "+p.Pattern)
		}
	}

	recommended := 1
	if l.Scope == store.ScopePack {
		recommended = 2
	}

	return stage.RoundQuestion{
		Title: l.Tag() + " " + f.Card,
		Text:  l.Text,
		Why:   strings.Join(why, "\n"),
		Options: []stage.RoundOption{
			{Title: "Approve for " + l.Project},
			{Title: "Approve for the whole pack"},
			{Title: "Reject"},
		},
		Recommended: recommended,
	}
}

// fence wraps content in a code fence one backtick longer than the longest
// run of backticks in it, at least three, so no line of it closes the block.
func fence(info, content string) string {
	longest, run := 0, 0

	for i := range len(content) {
		if content[i] != '`' {
			run = 0

			continue
		}

		run++
		longest = max(longest, run)
	}

	f := strings.Repeat("`", max(3, longest+1))

	var b strings.Builder

	b.WriteString(f + info + "\n" + content)

	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}

	b.WriteString(f + "\n")

	return b.String()
}

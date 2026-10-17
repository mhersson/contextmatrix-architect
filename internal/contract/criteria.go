package contract

import (
	"fmt"
	"strings"
)

// Criterion is one acceptance criterion: its id, the slice it belongs to and
// the project whose repository checks it.
type Criterion struct {
	ID      string
	Slice   string
	Project string
}

// CheckCriteria checks the draft against acceptance: every criterion is
// carried by exactly one card of its project, a card names only criteria
// acceptance defines, and every manual check is carried by a manual entry
// that names only checks acceptance defines. Findings come per card, then
// per criterion, then per manual entry and check.
func CheckCriteria(d Draft, criteria []Criterion, manual []string) []Finding {
	byID := map[string]Criterion{}
	for _, c := range criteria {
		byID[c.ID] = c
	}

	var out []Finding

	carriers := map[string][]string{}

	for _, card := range d.Cards {
		for _, id := range dedup(card.Criteria) {
			c, known := byID[id]

			switch {
			case !known:
				out = append(out, Finding{card.ID, "criterion_unknown", fmt.Sprintf("%s is not an acceptance criterion", id)})
			case c.Project != card.Project:
				out = append(out, Finding{card.ID, "criterion_project", fmt.Sprintf("%s is checked in project %q, not in this card's %q", id, c.Project, card.Project)})
			default:
				carriers[id] = append(carriers[id], card.ID)
			}
		}
	}

	for _, c := range criteria {
		switch n := len(carriers[c.ID]); {
		case n == 0:
			out = append(out, Finding{c.ID, "criterion_unmapped", fmt.Sprintf("slice %s's criterion is carried by no card", c.Slice)})
		case n > 1:
			out = append(out, Finding{c.ID, "criterion_shared", fmt.Sprintf("carried by %s; exactly one card carries a criterion", strings.Join(carriers[c.ID], " and "))})
		}
	}

	defined := map[string]bool{}
	for _, id := range manual {
		defined[id] = true
	}

	carried := map[string]bool{}

	for _, m := range d.Manual {
		for _, id := range m.Criteria {
			if !defined[id] {
				out = append(out, Finding{m.ID, "manual_unknown", fmt.Sprintf("%s is not a manual check of acceptance", id)})

				continue
			}

			carried[id] = true
		}
	}

	for _, id := range manual {
		if !carried[id] {
			out = append(out, Finding{id, "manual_unmapped", "the manual check is carried by no manual entry"})
		}
	}

	return out
}

// dedup returns ids without repeats, in order, so a card naming a criterion
// twice carries it once.
func dedup(ids []string) []string {
	seen := map[string]bool{}

	var out []string

	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}

	return out
}

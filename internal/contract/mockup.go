package contract

import (
	"fmt"
	"slices"
	"strings"
)

// WithoutMockups is text without its fenced mockup blocks: a mockup's HTML
// source holds the screen's own copy, such as "error:" or a path, which is
// no statement of the card. The grammar is shared with CM's card lint and
// the agent's card classifier: an opening line of three or more backticks
// at column 0 and the info string exactly mockup, closed by a line of only
// backticks, at least as many, else running to the end; trailing spaces and
// tabs are allowed on both. Tilde and indented fences are not mockup blocks.
// Every other line stays byte for byte.
func WithoutMockups(text string) string {
	var b strings.Builder

	fence := 0

	for _, line := range strings.SplitAfter(text, "\n") {
		bare := strings.TrimRight(strings.TrimSuffix(line, "\n"), " \t")

		if fence > 0 {
			if len(bare) >= fence && strings.Trim(bare, "`") == "" {
				fence = 0
			}

			continue
		}

		if rest := strings.TrimLeft(bare, "`"); rest == "mockup" && len(bare)-len(rest) >= 3 {
			fence = len(bare) - len(rest)

			continue
		}

		b.WriteString(line)
	}

	return b.String()
}

// CheckMockups checks the screens each card names against the approved
// mockup: with none approved a card names no screen (mockup_none), and
// with one every screen it names is one the mockup shows (mockup_screen).
func CheckMockups(d Draft, screens []string, approved bool) []Finding {
	var out []Finding

	for _, c := range d.Cards {
		if len(c.Mockup) == 0 {
			continue
		}

		if !approved {
			out = append(out, Finding{c.ID, "mockup_none", fmt.Sprintf("names the screens %v, but no mockup was approved; name none", c.Mockup)})

			continue
		}

		for _, id := range c.Mockup {
			if !slices.Contains(screens, id) {
				out = append(out, Finding{c.ID, "mockup_screen", fmt.Sprintf("screen %q is not in the approved mockup, whose screens are %v", id, screens)})
			}
		}
	}

	return out
}

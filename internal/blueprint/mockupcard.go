package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/net/html"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
)

// approvedMockup is blueprint id's mockup when one was used and its step
// still stands, with the used revision's data; used is false after a skip,
// a declined offer, a need without a screen or with no mockup step.
func (c *Controller) approvedMockup(ctx context.Context, id string) (m mockupResult, d mockupData, used bool, err error) {
	res, err := c.mockupResult(ctx, id)
	if err != nil || res.Outcome != mockupUsed {
		return mockupResult{}, mockupData{}, false, err
	}

	collapsed, err := c.mockupCollapsed(ctx, id)
	if err != nil || collapsed {
		return mockupResult{}, mockupData{}, false, err
	}

	if err := json.Unmarshal(res.Art.Data, &d); err != nil {
		return mockupResult{}, mockupData{}, false, fmt.Errorf("blueprint: decode mockup revision %d: %w", res.Revision, err)
	}

	return res, d, true, nil
}

// screenIDs is the ids of the screens d shows.
func (d mockupData) screenIDs() []string {
	out := make([]string, 0, len(d.Screens))
	for _, s := range d.Screens {
		out = append(out, s.ID)
	}

	return out
}

// chosenValue is the value a use chose for suggestion id, or the revision's
// opening value for an id the use does not hold.
func chosenValue(chosen, opening map[string]string, id string) string {
	if v, ok := chosen[id]; ok {
		return v
	}

	return opening[id]
}

// mockupCarriers is the card ids that carry the mockup, given one draft per
// slice: every card of a slice whose draft names a screen, the cards behind
// those screens included, and no card of a slice that names none.
func mockupCarriers(drafts []contract.Draft) map[string]bool {
	out := map[string]bool{}

	for _, d := range drafts {
		if !slices.ContainsFunc(d.Cards, func(c contract.Card) bool { return len(c.Mockup) > 0 }) {
			continue
		}

		for _, c := range d.Cards {
			out[c.ID] = true
		}
	}

	return out
}

// mockupCarriersOf is mockupCarriers over blueprint id's drafts, one per
// step of the stage that checks cards, as emission reads them.
func (c *Controller) mockupCarriersOf(ctx context.Context, id string) (map[string]bool, error) {
	drafts, err := c.stepDrafts(ctx, id)
	if err != nil {
		return nil, err
	}

	return mockupCarriers(drafts), nil
}

// withMockup appends a Mockup section to a card body: two fixed sentences
// and the source of m's revision, whose data is d, in a mockup block, its
// settings fixed to the chosen ones. screens are the ids of the screens the
// card builds; with none the card is told it builds none and the design is
// its target.
func withMockup(body string, m mockupResult, d mockupData, screens []string) string {
	fixed := make(map[string]string, len(d.Suggestions))

	var words strings.Builder

	fmt.Fprintf(&words, "Revision %d of the approved mockup.\nThe chosen settings:\n", m.Revision)

	for _, s := range d.Suggestions {
		v := chosenValue(m.Chosen, d.Settings, s.ID)
		fixed[s.ID] = v
		fmt.Fprintf(&words, "  %s: %s\n", s.Title, s.Label(v))
	}

	if len(d.Suggestions) == 0 {
		words.WriteString("  none\n")
	}

	var b strings.Builder

	b.WriteString(strings.TrimRight(body, "\n"))
	b.WriteString("\n\n## Mockup\n\n")

	if len(screens) == 0 {
		words.WriteString("This card builds none of the screens.")
		b.WriteString("This card builds none of the screens of the design the person approved, one HTML document whose first comment lists the chosen settings; that design is the target its work serves.")
	} else {
		words.WriteString("The screens this card builds:")

		for _, id := range screens {
			title := id
			if i := slices.IndexFunc(d.Screens, func(s stage.MockupScreen) bool { return s.ID == id }); i >= 0 {
				title = d.Screens[i].Title
			}

			words.WriteString("\n  " + title)
		}

		b.WriteString("This is the design the person approved for the screens this card builds, as one HTML document whose first comment lists the chosen settings and those screens.")
	}

	b.WriteString(" Build for the chosen variant only: the source also holds code for variants nobody chose.\n\n")
	b.WriteString(mockupFence(bakeMockup(d.Source, fixed, words.String())))

	return b.String()
}

// bakeMockup is source with comment before it, as an HTML comment no text
// can close early, and the chosen settings fixed in a script before the
// source's first script element that runs as it loads, so that script
// reads them. A source without one gets no settings.
func bakeMockup(source string, chosen map[string]string, comment string) string {
	for strings.Contains(comment, "--") {
		comment = strings.ReplaceAll(comment, "--", "- -")
	}

	var b strings.Builder

	b.WriteString("<!-- " + comment + " -->\n")

	at := firstScript(source)
	if at < 0 {
		b.WriteString(source)

		return b.String()
	}

	raw, _ := json.Marshal(chosen) // A map of strings always encodes, with < and > escaped.

	b.WriteString(source[:at])
	b.WriteString("<script>window.mockupSettings = " + string(raw) + ";</script>")
	b.WriteString(source[at:])

	return b.String()
}

// firstScript is the byte offset of source's first script start tag as the
// HTML tokenizer reads it, so a tag inside a comment, a style or a text
// area does not count, outside any template, whose scripts do not run as
// the document loads; -1 when there is none.
func firstScript(source string) int {
	z := html.NewTokenizer(strings.NewReader(source))
	at, templates := 0, 0

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return -1
		}

		n := len(z.Raw())
		name, _ := z.TagName()

		switch {
		case tt == html.EndTagToken && string(name) == "template" && templates > 0:
			templates--
		case tt != html.StartTagToken && tt != html.SelfClosingTagToken:
		case string(name) == "template":
			templates++
		case string(name) == "script" && templates == 0:
			return at
		}

		at += n
	}
}

// mockupFence fences content as a mockup block: one backtick more than its
// longest run of backticks, at least three, so no line of it closes the
// block, with the info string, the content and the closing fence each on
// its own line.
func mockupFence(content string) string {
	longest, run := 0, 0

	for i := range len(content) {
		if content[i] != '`' {
			run = 0

			continue
		}

		run++
		longest = max(longest, run)
	}

	fence := strings.Repeat("`", max(3, longest+1))

	var b strings.Builder

	b.WriteString(fence + "mockup\n" + content)

	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}

	b.WriteString(fence + "\n")

	return b.String()
}

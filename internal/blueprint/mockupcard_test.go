package blueprint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/contract"
	"github.com/mhersson/contextmatrix-architect/internal/stage"
	"github.com/mhersson/contextmatrix-architect/internal/store"
)

// memberSource imitates a club's member list in a desktop window.
const memberSource = "<!doctype html><html><head><style>main{font:13px system-ui}</style></head><body><main id=\"m\"></main>" +
	"<script>function draw(s){document.getElementById(\"m\").textContent = s.sort === \"joined\" ? \"By date joined\" : \"By name\"}" +
	"draw(window.mockupSettings);window.addEventListener(\"mockup:settings\",function(e){draw(e.detail)})</script></body></html>"

// memberSuggestions sort the member list by name or by the date joined.
func memberSuggestions() []stage.MockupSuggestion {
	return []stage.MockupSuggestion{{
		ID: "sort", Title: "Sort members by", Kind: stage.MockupChoice, Why: "New members are easy to find at the top.", Default: "name",
		Options: []stage.MockupOption{{Value: "name", Label: "Name"}, {Value: "joined", Label: "Date joined"}},
	}}
}

// memberMockup is a used mockup of memberSource that shows the member list,
// sorted by the date joined, and its revision's data.
func memberMockup(t *testing.T) (mockupResult, mockupData) {
	t.Helper()

	d := mockupData{
		Revision: 1, Screens: []stage.MockupScreen{{ID: "members", Title: "The members"}},
		Suggestions: memberSuggestions(), Settings: map[string]string{"sort": "name"}, Source: memberSource,
	}

	raw, err := json.Marshal(d)
	require.NoError(t, err)

	return mockupResult{
		Outcome: mockupUsed, Revision: 1, Chosen: map[string]string{"sort": "joined"},
		Art: store.Artifact{Stage: "mockup", Version: 1, Body: "The members, newest first.", Data: raw},
	}, d
}

// stripLeadingComment cuts everything up to the first comment end and the
// newline after it.
func stripLeadingComment(s string) string {
	_, rest, ok := strings.Cut(s, "-->")
	if !ok {
		return s
	}

	return strings.TrimPrefix(rest, "\n")
}

// mockupBlocks is the content of every mockup block of body, read with the
// grammar the contract shares.
func mockupBlocks(body string) []string {
	var (
		out   []string
		cur   []string
		fence string
	)

	for _, line := range strings.Split(body, "\n") {
		bare := strings.TrimRight(line, " \t")

		if fence != "" {
			if len(bare) >= len(fence) && strings.Trim(bare, "`") == "" {
				out, fence = append(out, strings.Join(cur, "\n")), ""

				continue
			}

			cur = append(cur, line)

			continue
		}

		if rest := strings.TrimLeft(bare, "`"); rest == "mockup" && len(bare)-len(rest) >= 3 {
			fence, cur = bare[:len(bare)-len(rest)], nil
		}
	}

	if fence != "" {
		out = append(out, strings.Join(cur, "\n"))
	}

	return out
}

func TestBakeMockup_FixesTheChosenSettings(t *testing.T) {
	baked := bakeMockup(memberSource, map[string]string{"sort": "joined"}, "Sort members by: Date joined")

	fixed := `<script>window.mockupSettings = {"sort":"joined"};</script>`
	assert.Less(t, strings.Index(baked, fixed), strings.Index(baked, "<script>function draw"), "fixed before the source's first script")
	assert.True(t, strings.HasPrefix(baked, "<!--"), "the settings in words come first")
	assert.Equal(t, memberSource, strings.Replace(stripLeadingComment(baked), fixed, "", 1), "nothing else of the source changes")

	plain := "<!doctype html><p>no script</p>"
	assert.NotContains(t, bakeMockup(plain, map[string]string{"sort": "joined"}, "x"), "window.mockupSettings")

	assert.NotContains(t, bakeMockup(memberSource, map[string]string{}, "words -- with --> in them"), "-->"+" in them", "the comment cannot close early")
}

func TestBakeMockup_TheFirstScriptIsTheFirstElement(t *testing.T) {
	// A script tag inside a comment, a style or a text area is no script
	// element, and one inside a template never runs as the page loads; the
	// fixed settings go before the first real one.
	source := "<!doctype html><!-- <script>old</script> --><style>p::after{content:\"<script>\"}</style>" +
		"<textarea><script>typed</script></textarea><template><p><script>later()</script></p></template>" +
		"<SCRIPT>draw(window.mockupSettings)</SCRIPT>"

	baked := stripLeadingComment(bakeMockup(source, map[string]string{"sort": "name"}, "x"))
	fixed := `<script>window.mockupSettings = {"sort":"name"};</script>`

	assert.Equal(t, strings.Replace(source, "<SCRIPT>", fixed+"<SCRIPT>", 1), baked)
}

func TestBakeMockup_TheCommentNeverCloses(t *testing.T) {
	for _, words := range []string{"a -- b", "a --> b", "a ---> b", "a --!> b", "ends in -", "ends in --"} {
		baked := bakeMockup("<p>x</p>", map[string]string{}, words)

		end := strings.Index(baked, "-->")
		require.GreaterOrEqual(t, end, 0, words)
		assert.Equal(t, "\n<p>x</p>", baked[end+len("-->"):], "%q: the comment ends where the source starts", words)
		assert.NotContains(t, baked[:end], "--!>", words)
	}
}

func TestMockupFence_OutrunsTheSource(t *testing.T) {
	content := "<pre>\n````\n</pre>"
	block := mockupFence(content)

	assert.True(t, strings.HasPrefix(block, "`````mockup\n"))
	assert.Empty(t, strings.TrimSpace(contract.WithoutMockups(block)), "the shared grammar reads it as one block")
	assert.Equal(t, []string{content}, mockupBlocks(block))

	assert.True(t, strings.HasPrefix(mockupFence("<p>plain</p>"), "```mockup\n"), "at least three backticks")
}

func TestMockupCarriers_EveryCardOfASliceThatNamesAScreen(t *testing.T) {
	draft := func(cards ...contract.Card) contract.Draft { return contract.Draft{Cards: cards} }
	card := func(id string, screens ...string) contract.Card { return contract.Card{ID: id, Mockup: screens} }

	got := mockupCarriers([]contract.Draft{
		draft(card("D1", "members"), card("D2")), // the list and the API behind it
		draft(card("D3")),                        // a slice with no screen
		draft(card("D4"), card("D5", "edit")),
	})

	assert.Equal(t, map[string]bool{"D1": true, "D2": true, "D4": true, "D5": true}, got)
	assert.Empty(t, mockupCarriers([]contract.Draft{draft(card("D1"), card("D2"))}))
}

func TestWithMockup_ACardThatBuildsNoScreenStillSeesTheTarget(t *testing.T) {
	m, d := memberMockup(t)
	body := "## Context\nwhy\n"

	builds, serves := withMockup(body, m, d, []string{"members"}), withMockup(body, m, d, nil)

	for _, got := range []string{builds, serves} {
		assert.True(t, strings.HasPrefix(got, body))
		assert.NotEqual(t, got, contract.WithoutMockups(got), "it holds a mockup block")
		assert.Contains(t, got, `window.mockupSettings = {"sort":"joined"}`)
	}

	assert.NotEqual(t, builds, serves, "a card that builds no screen is told so")
}

func TestWithMockup_AFullSourceStaysUnderTheCardCap(t *testing.T) {
	m, d := memberMockup(t)

	// The worst case for the fence: a source that is one run of backticks.
	d.Source = memberSource + "<pre>" + strings.Repeat("`", stage.MaxMockupBytes-len(memberSource)-len("<pre></pre>")) + "</pre>"
	require.Len(t, d.Source, stage.MaxMockupBytes)

	const cardCap = 512 << 10

	added := len(withMockup("", m, d, []string{"members"}))
	assert.Less(t, added, cardCap/2, "the mockup leaves the card's own sections more than half of the cap")
}

// memberRevision is a club's member list in a desktop window and the form
// that edits a member; its source holds an error message the screen shows
// and a bracketed locker label.
func memberRevision() stage.MockupRevision {
	return stage.MockupRevision{
		Notes:  "The members, newest first, and the form that edits one.",
		Source: strings.Replace(memberSource, "<main", "<p>error: that name is taken</p><p>Locker [S2] is free</p><main", 1),
		Screens: []stage.MockupScreen{
			{ID: "members", Title: "The members"}, {ID: "edit", Title: "Edit a member"},
		},
		Suggestions: memberSuggestions(),
	}
}

// memberCards is the two cards of the one slice: D1 builds screens and
// cites S1, D2 is the work behind the list and builds none. D1 carries the
// slice's criterion.
func memberCards(screens ...string) map[string]any {
	card := func(id, title, why string) map[string]any {
		return map[string]any{
			"id": id, "project": "alpha", "title": title, "type": "feature", "depends_on": []any{},
			"body":  "## Context\n" + why + "\n## Where\ninternal/x.go\n## Solution\nadd\n## Acceptance criteria\n1. go test ./... passes.\n## Tests expected\nTestThing.\n## Out of scope\nrest.\n",
			"where": []any{map[string]any{"path": "internal/x.go", "symbol": "Thing"}},
		}
	}

	d1 := card("D1", "Add the member list", "why [S1]")
	d1["criteria"] = []any{"AC1"}

	if len(screens) > 0 {
		names := make([]any, 0, len(screens))
		for _, s := range screens {
			names = append(names, s)
		}

		d1["mockup"] = names
	}

	return map[string]any{
		"title": "Show the members",
		"cards": []any{d1, card("D2", "Add the member query", "why")},
		"order": []any{"D1", "D2"},
	}
}

// photoCards is goodCards carrying the slice's criterion and naming the
// screens of the photo renamer it builds.
func photoCards(screens ...any) map[string]any {
	cards := goodCards()
	card := cards["cards"].([]any)[0].(map[string]any)
	card["criteria"] = []any{"AC1"}
	card["mockup"] = screens

	return cards
}

func TestCards_AScreenTheMockupDoesNotShowIsRefused(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: photoCards("settings")})
		if err == nil || !strings.Contains(err.Error(), "D1: mockup_screen") {
			return stage.Output{}, fmt.Errorf("a screen the mockup does not show passed: %v", err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: photoCards("run")}); err != nil {
			return stage.Output{}, fmt.Errorf("a screen the mockup shows is refused: %w", err)
		}

		return done("# Cards", photoCards("run")), nil
	}

	c, st, _, rec, _ := newMockup(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	takeOffer(t, c)
	useMockup(t, c, map[string]string{"preview": "on"})
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	cards, err := st.LatestArtifact(ctx, "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(cards), "the second submit passed")
	assert.True(t, rec.has(EventSignOff))
}

func TestCards_AScreenWithoutAnApprovedMockupIsRefused(t *testing.T) {
	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(photoRevision())
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: photoCards("run")})
		if err == nil || !strings.Contains(err.Error(), "D1: mockup_none") {
			return stage.Output{}, fmt.Errorf("a screen named after a skip passed: %v", err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: photoCards()}); err != nil {
			return stage.Output{}, fmt.Errorf("a card that names no screen is refused: %w", err)
		}

		return done("# Cards", photoCards()), nil
	}

	c, st, _, rec, _ := newMockup(t, script)
	ctx := context.Background()

	drive(t, c, "BP1")
	takeOffer(t, c)
	skipMockup(t, c)
	require.NoError(t, c.Message(ctx, "BP1", "approve", "m2"))
	c.Wait("BP1")

	cards, err := st.LatestArtifact(ctx, "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(cards), "the second submit passed")
	assert.True(t, rec.has(EventSignOff))
}

// emitMembers runs the member list to its sign-off with the cards that name
// screens and emits them on a fake ContextMatrix; it returns the created
// cards and the pack's lists.
func emitMembers(t *testing.T, screens ...string) ([]string, contract.Lists) {
	t.Helper()

	script := withScreen(dialogScript(), true)
	script["mockup"] = showing(memberRevision())
	script["decomposition"] = returns("# Cards", memberCards(screens...))
	script["readiness"] = returns("# Ready", readinessFor(true, "D1", "D2"))

	c, st, _, rec, _ := newMockup(t, script)
	fake := newFakeCM()
	c.opts.ConnectCM = fake.connect

	drive(t, c, "BP1")
	takeOffer(t, c)
	useMockup(t, c, map[string]string{"sort": "joined"})
	require.NoError(t, c.Message(context.Background(), "BP1", "approve", "m2"))
	c.Wait("BP1")
	require.True(t, rec.has(EventSignOff))

	emitNow(t, c, st, "BP1")

	cards, _ := fake.inputs()
	require.Len(t, cards, 2)

	bodies := make([]string, 0, len(cards))
	for _, card := range cards {
		bodies = append(bodies, card.Body)
	}

	return bodies, c.opts.Pack.Lists()
}

// mockupSection is body's Mockup section, up to the next section.
func mockupSection(t *testing.T, body string) string {
	t.Helper()

	_, rest, ok := strings.Cut(body, "\n## Mockup\n")
	require.True(t, ok, "the body has a Mockup section")

	section, _, _ := strings.Cut(rest, "\n## Sources\n")

	return section
}

func TestEmit_EveryCardOfASliceThatBuildsAScreenCarriesTheMockup(t *testing.T) {
	bodies, lists := emitMembers(t, "members")

	for i, body := range bodies {
		blocks := mockupBlocks(body)
		require.Len(t, blocks, 1, "card %d holds one mockup block", i+1)
		assert.Contains(t, blocks[0], `window.mockupSettings = {"sort":"joined"}`)
		assert.Contains(t, blocks[0], `function draw(s)`)
		assert.Contains(t, blocks[0], "error: that name is taken", "the source made it in")

		card := contract.Card{ID: "D1", Project: "alpha", Title: "Add the member list", Type: "feature", Body: body}
		for _, f := range contract.Check(contract.Draft{Cards: []contract.Card{card}}, lists, map[string]contract.Project{"alpha": {HasRepo: true, VerifyCommand: "make test"}}) {
			assert.NotEqual(t, "defect_phrase", f.Check, "card %d: %s", i+1, f)
		}
	}

	d1 := bodies[0]
	assert.Less(t, strings.Index(d1, "\n## Mockup\n"), strings.Index(d1, "\n## Sources\n"), "the mockup comes before the sources")

	_, sources, _ := strings.Cut(d1, "\n## Sources\n")

	var tags []string

	for _, line := range strings.Split(strings.TrimSpace(sources), "\n") {
		tag, _, _ := strings.Cut(strings.TrimPrefix(line, "- "), ":")
		tags = append(tags, tag)
	}

	assert.Equal(t, []string{"S1"}, tags, "the locker label in the mockup cites nothing")
	assert.NotContains(t, bodies[1], "\n## Sources\n")

	assert.NotEqual(t, mockupSection(t, bodies[0]), mockupSection(t, bodies[1]), "D2 builds none of the screens")

	bodies, _ = emitMembers(t)

	for i, body := range bodies {
		assert.NotContains(t, body, "```mockup", "no card names a screen, so card %d carries no mockup", i+1)
	}
}

func TestCards_AMockupBlockInADraftIsRefused(t *testing.T) {
	fenced := goodCards()
	card := fenced["cards"].([]any)[0].(map[string]any)
	card["body"] = card["body"].(string) + "\n```mockup\n<p>error: the list is gone</p>\n```\n"

	script := happyScript()
	script["decomposition"] = func(ctx context.Context, in stage.Input) (stage.Output, error) {
		err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: fenced})
		if err == nil || !strings.Contains(err.Error(), "D1: mockup_block") {
			return stage.Output{}, fmt.Errorf("a draft's own mockup block passed: %v", err)
		}

		if err := in.Validate(ctx, stage.Artifact{Body: "# Cards", Data: goodCards()}); err != nil {
			return stage.Output{}, fmt.Errorf("the draft without the block is refused: %w", err)
		}

		return done("# Cards", goodCards()), nil
	}

	c, st, _, _, _ := newPipeline(t, script)
	drive(t, c, "BP1")

	cards, err := st.LatestArtifact(context.Background(), "BP1", "decomposition")
	require.NoError(t, err)
	assert.False(t, artifactStopped(cards), "the second submit passed")
}

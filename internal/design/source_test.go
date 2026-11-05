package design

import (
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// window128k is a 128k-token window in bytes, the default a run gets.
const window128k = 320_000

// frameRoot returns a root holding the given frame files.
func frameRoot(name string, frame ...Asset) Root {
	return Root{Project: name, Path: name + "/web", Frame: frame}
}

// withLogo returns r with a logo SVG of the given text.
func withLogo(r Root, text string) Root {
	r.Brand = []Asset{{Path: r.Path + "/logo.svg", Size: int64(len(text)), Text: text}}

	return r
}

// textless returns m with every Text cleared, on copies.
func textless(m Material) Material {
	m.Roots = append([]Root(nil), m.Roots...)

	for i := range m.Roots {
		m.Roots[i].Frame = append([]Asset(nil), m.Roots[i].Frame...)
		m.Roots[i].Brand = append([]Asset(nil), m.Roots[i].Brand...)

		for j := range m.Roots[i].Frame {
			m.Roots[i].Frame[j].Text = ""
		}

		for j := range m.Roots[i].Brand {
			m.Roots[i].Brand[j].Text = ""
		}
	}

	return m
}

// marked is n bytes of one letter, a text one marker tells from another.
func marked(letter string, n int) string {
	return strings.Repeat(letter, n)
}

// headings returns the lines of s that a card's reader may take for a section
// heading: trimmed, one or more # then white space or the end.
func headings(s string) []string {
	var out []string

	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)

		rest := strings.TrimLeft(t, "#")
		if len(rest) == len(t) {
			continue
		}

		if rest == "" || unicode.IsSpace([]rune(rest)[0]) {
			out = append(out, t)
		}
	}

	return out
}

func TestBrief_FrameSource(t *testing.T) {
	m := briefMaterial()
	m.Roots[0].Frame = []Asset{
		{Path: "portal/frontend/src/a.tsx", Size: 21, Text: "export const Nav = 1"},
		{Path: "portal/frontend/src/b.css", Size: 6, Text: ".nav{}"},
		{Path: "portal/frontend/src/big.tsx", Size: 40000},
	}
	m.Roots[0].Brand = []Asset{{Path: "portal/frontend/public/logo.svg", Size: 17, Text: `<svg id="logo"/>`}}

	out := Brief(m, nil, window128k)

	logo := strings.Index(out, "```svg\n<svg id=\"logo\"/>\n```")
	a := strings.Index(out, "```tsx\nexport const Nav = 1\n```")
	b := strings.Index(out, "```css\n.nav{}\n```")

	require.GreaterOrEqual(t, logo, 0, "logo block")
	require.GreaterOrEqual(t, a, 0, "a.tsx block")
	require.GreaterOrEqual(t, b, 0, "b.css block")
	assert.Less(t, logo, a)
	assert.Less(t, a, b)
	assert.Greater(t, strings.LastIndex(out, "portal/frontend/src/big.tsx"), b)
	assert.Contains(t, out, "File portal/frontend/src/a.tsx:")

	bare := Brief(textless(m), nil, window128k)

	assert.True(t, strings.HasPrefix(out, bare))
	assert.NotContains(t, bare, "```")
}

func TestBrief_FrameSourceNeedsSomethingIncluded(t *testing.T) {
	tests := []struct {
		name   string
		roots  []Root
		window int
	}{
		{"no roots", nil, window128k},
		{"frame files without text", []Root{frameRoot("p", Asset{Path: "p/web/a.tsx", Size: 3})}, window128k},
		{"logo without text", []Root{{Path: "p/web", Brand: []Asset{{Path: "p/web/logo.svg", Size: 3}}}}, window128k},
		{"a frame file over its root's cap", []Root{frameRoot("p", Asset{Path: "p/web/a.tsx", Text: marked("a", maxFrameSourceBytes)})}, window128k},
		{"a logo over its root's cap", []Root{withLogo(Root{Path: "p/web"}, marked("a", maxLogoSourceBytes))}, window128k},
		{"no window", []Root{withLogo(frameRoot("p", Asset{Path: "p/web/a.tsx", Text: "x"}), "<svg/>")}, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Empty(t, frameSource(tt.roots, tt.window))
		})
	}
}

func TestBrief_FrameSourceLeavesOutARootWithNothingIncluded(t *testing.T) {
	out := frameSource([]Root{
		frameRoot("a", Asset{Path: "a/web/big.tsx", Text: marked("a", maxFrameSourceBytes)}),
		withLogo(Root{Project: "b", Path: "b/web"}, marked("b", maxLogoSourceBytes)),
		frameRoot("c", Asset{Path: "c/web/x.tsx", Text: "cc"}),
	}, window128k)

	assert.NotContains(t, out, "### Frame source: a a/web")
	assert.NotContains(t, out, "a/web/big.tsx")
	assert.NotContains(t, out, "### Frame source: b b/web")
	assert.NotContains(t, out, "b/web/logo.svg")
	assert.Contains(t, out, "### Frame source: c c/web")
	assert.Contains(t, out, "```tsx\ncc\n```")
}

func TestBrief_FrameSourceLogoWithoutFrameText(t *testing.T) {
	r := withLogo(frameRoot("p", Asset{Path: "p/web/a.tsx", Size: 3}), "<svg/>")

	out := frameSource([]Root{r}, window128k)

	assert.Contains(t, out, "```svg\n<svg/>\n```")
	assert.NotContains(t, out, "File p/web/a.tsx")
	assert.Contains(t, out, "- p/web/a.tsx\n")
}

func TestBrief_FrameSourceBudget(t *testing.T) {
	var frame []Asset

	for i, letter := range []string{"a", "b", "c", "d"} {
		frame = append(frame, Asset{Path: "p/web/f" + string(rune('1'+i)) + ".tsx", Text: marked(letter, 10<<10)})
	}

	frame = append(frame, Asset{Path: "p/web/f5.tsx", Text: "small"})

	out := frameSource([]Root{frameRoot("p", frame...)}, window128k)

	for _, letter := range []string{"a", "b", "c"} {
		assert.Contains(t, out, marked(letter, 10<<10))
	}

	assert.NotContains(t, out, marked("d", 100))
	assert.Contains(t, out, "```tsx\nsmall\n```")
	assert.Contains(t, out, "- p/web/f4.tsx\n")
	assert.NotContains(t, out, "- p/web/f5.tsx")
}

func TestBrief_FrameSourceCountsTheWholeBlock(t *testing.T) {
	// The text alone fits the cap; its label and fences do not.
	r := frameRoot("p",
		Asset{Path: "p/web/a.tsx", Text: marked("a", maxFrameSourceBytes)},
		Asset{Path: "p/web/b.tsx", Text: "small"},
	)

	out := frameSource([]Root{r}, window128k)

	assert.NotContains(t, out, marked("a", 100))
	assert.Contains(t, out, "- p/web/a.tsx\n")
	assert.Contains(t, out, "```tsx\nsmall\n```")
}

func TestBrief_FrameSourceShare(t *testing.T) {
	letters := []string{"w", "x", "y", "z"}

	// Each root needs four 10 KiB blocks, more than its 32 KiB cap: it takes
	// three at most.
	roots := func(n int) []Root {
		var out []Root

		for i := range n {
			name := "r" + string(rune('1'+i))

			var frame []Asset
			for _, letter := range letters {
				frame = append(frame, Asset{Path: name + "/web/" + letter + ".tsx", Text: marked(letter, 10<<10)})
			}

			out = append(out, frameRoot(name, frame...))
		}

		return out
	}

	tests := []struct {
		name   string
		window int
		roots  int
		blocks int // the blocks each root gets
	}{
		{"a 64k window gives 30,000 bytes", window128k / 2, 1, 2},
		{"a 128k window gives 60,000 bytes, one root stops at its cap", window128k, 1, 3},
		{"two roots split them", window128k, 2, 2},
		{"four roots split them", window128k, 4, 1},
		{"a million-token window gives 96 KiB", 2_500_000, 4, 2},
		{"two roots stop at their cap", 2_500_000, 2, 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := frameSource(roots(tt.roots), tt.window)

			for i, letter := range letters {
				want := 0
				if i < tt.blocks {
					want = tt.roots
				}

				assert.Equal(t, want, strings.Count(out, marked(letter, 10<<10)), letter)
			}

			for i := range tt.roots {
				name := "r" + string(rune('1'+i))
				assert.Contains(t, out, "- "+name+"/web/z.tsx\n")
			}
		})
	}
}

func TestBrief_FrameSourceSharedByNeed(t *testing.T) {
	// 36,000 bytes of frame files: an equal split gives the big root one block.
	const window = 192_000

	big := frameRoot("big",
		Asset{Path: "big/web/w.tsx", Text: marked("w", 10<<10)},
		Asset{Path: "big/web/x.tsx", Text: marked("x", 10<<10)},
		Asset{Path: "big/web/y.tsx", Text: marked("y", 10<<10)},
		Asset{Path: "big/web/z.tsx", Text: marked("z", 10<<10)},
	)
	small := frameRoot("small", Asset{Path: "small/web/s.tsx", Text: marked("s", 2<<10)})

	out := frameSource([]Root{big, small}, window)

	assert.Contains(t, out, marked("s", 2<<10))

	for _, letter := range []string{"w", "x", "y"} {
		assert.Contains(t, out, marked(letter, 10<<10), letter)
	}

	assert.NotContains(t, out, marked("z", 100))
	assert.Contains(t, out, "- big/web/z.tsx\n")
}

func TestShareByNeed(t *testing.T) {
	tests := []struct {
		name  string
		needs []int
		total int
		want  []int
	}{
		{"a small need leaves the rest", []int{2000, 40000}, 36000, []int{2000, 34000}},
		{"in any order", []int{40000, 2000}, 36000, []int{34000, 2000}},
		{"equal needs split equally", []int{30, 30, 30}, 21, []int{7, 7, 7}},
		{"the last takes the remainder", []int{100, 100, 100}, 10, []int{3, 3, 4}},
		{"no need takes nothing", []int{0, 50}, 30, []int{0, 30}},
		{"needs that fit are met", []int{5, 6}, 100, []int{5, 6}},
		{"nothing to share", []int{5}, 0, []int{0}},
		{"no needs", nil, 10, []int{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, shareByNeed(tt.needs, tt.total))
		})
	}
}

func TestBrief_FrameSourceShareCountsOnlyRootsWithFrameText(t *testing.T) {
	roots := []Root{
		frameRoot("a", Asset{Path: "a/web/x.tsx", Text: marked("x", 20<<10)}),
		withLogo(Root{Project: "b", Path: "b/web"}, "<svg/>"),
		frameRoot("c", Asset{Path: "c/web/x.tsx", Size: 9}),
	}

	assert.Contains(t, frameSource(roots, window128k), marked("x", 20<<10))
}

func TestBrief_LogoShare(t *testing.T) {
	// A 128k window gives logos 20,000 bytes.
	t.Run("a small logo leaves the rest to a big one", func(t *testing.T) {
		out := frameSource([]Root{
			withLogo(Root{Project: "r1", Path: "r1/web"}, marked("a", 12<<10)),
			withLogo(Root{Project: "r2", Path: "r2/web"}, marked("b", 1<<10)),
		}, window128k)

		assert.Contains(t, out, marked("a", 12<<10))
		assert.Contains(t, out, marked("b", 1<<10))
	})

	t.Run("a logo over its share is listed by path", func(t *testing.T) {
		var roots []Root

		for i := range 3 {
			name := "r" + string(rune('1'+i))
			roots = append(roots, withLogo(frameRoot(name, Asset{Path: name + "/web/x.tsx", Text: "x"}), marked(string(rune('a'+i)), 12<<10)))
		}

		out := frameSource(roots, window128k)

		for i, letter := range []string{"a", "b", "c"} {
			assert.NotContains(t, out, marked(letter, 100))
			assert.Contains(t, out, "- r"+string(rune('1'+i))+"/web/logo.svg\n")
		}
	})
}

func TestBrief_FrameSourceScalesWithTheWindow(t *testing.T) {
	m := briefMaterial()

	for _, letter := range []string{"a", "b", "c", "d"} {
		m.Roots[0].Frame = append(m.Roots[0].Frame, Asset{Path: "portal/frontend/src/" + letter + ".tsx", Text: marked(letter, 10<<10)})
	}

	small := Brief(m, nil, window128k/2)
	big := Brief(m, nil, window128k)

	assert.Less(t, len(small), len(big))
	assert.Contains(t, small, marked("b", 10<<10))
	assert.NotContains(t, small, marked("c", 100))
	assert.Contains(t, big, marked("c", 10<<10))
}

func TestBrief_FrameSourceFence(t *testing.T) {
	out := frameSource([]Root{frameRoot("p", Asset{Path: "p/web/x.tsx", Text: "a ``` b\n````\nc"})}, window128k)

	assert.Contains(t, out, "`````tsx\na ``` b\n````\nc\n`````\n")
}

func TestFence(t *testing.T) {
	tests := []struct {
		text string
		want string
	}{
		{"", "```"},
		{"plain", "```"},
		{"`one` ``two``", "```"},
		{"```", "````"},
		{"a\n`````\nb ``", "``````"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, fence(tt.text), tt.text)
	}
}

func TestEscapeSource(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "a\nb", "a\nb"},
		{"every line break becomes a newline", "a\r\nb\rc\u0085d\u2028e\u2029f", "a\nb\nc\nd\ne\nf"},
		{"trailing breaks go", "a\n\r\n\u2028", "a"},
		{"heading", "## Plan", "&#35;# Plan"},
		{"one hash", "# Title", "&#35; Title"},
		{"indented", "  \t## Plan", "  \t&#35;# Plan"},
		{"bare hashes", "##", "&#35;#"},
		{"hash then tab", "#\tx", "&#35;\tx"},
		{"hash then no-break space", "#\u00a0x", "&#35;\u00a0x"},
		{"hash then ideographic space", "#\u3000x", "&#35;\u3000x"},
		{"after a form feed", "\f## x", "\f&#35;# x"},
		{"after a line separator", "a\u2028## x", "a\n&#35;# x"},
		{"id selector", "#header { color: red }", "#header { color: red }"},
		{"shebang", "#!/bin/sh", "#!/bin/sh"},
		{"hash mid line", "a ## b", "a ## b"},
		{"a hash then a word", "##x", "##x"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, escapeSource(tt.in))
		})
	}
}

func TestBrief_FrameSourceCannotForgeASection(t *testing.T) {
	const text = "a\r\n## Mockup\r  # Title ### x #\u0085##\tz\u2028## y\u2029#\n#header { color: red }\n#!/bin/sh"

	m := Material{Roots: []Root{
		withLogo(frameRoot("p", Asset{Path: "p/web/x.tsx", Text: text}), "<svg>\n## Logo\n</svg>"),
	}}

	out := Brief(m, nil, window128k)

	for _, r := range []string{"\r", "\u0085", "\u2028", "\u2029"} {
		assert.NotContains(t, out, r)
	}

	m.Roots[0].Frame[0].Text = "safe"
	m.Roots[0].Brand[0].Text = "<svg/>"

	assert.Equal(t, headings(Brief(m, nil, window128k)), headings(out))

	assert.Contains(t, out, "&#35;# Mockup")
	assert.Contains(t, out, "  &#35; Title")
	assert.Contains(t, out, "&#35;#\tz")
	assert.Contains(t, out, "#header { color: red }")
	assert.Contains(t, out, "#!/bin/sh")
	assert.Contains(t, out, "&#35;# Logo")
}

func TestBrief_FrameSourcePathsStayOnOneLine(t *testing.T) {
	m := Material{Roots: []Root{{
		Project: "po\n## rtal",
		Path:    "portal/web",
		Frame:   []Asset{{Path: "portal/web/a\n## x.tsx", Text: "x"}, {Path: "portal/web/b\n## y.tsx", Size: 3}},
		Brand:   []Asset{{Path: "portal/web/l\n## z.svg", Text: "<svg/>"}},
	}}}

	out := Brief(m, nil, window128k)

	for _, line := range strings.Split(out, "\n") {
		assert.False(t, strings.HasPrefix(line, "## x"), line)
		assert.False(t, strings.HasPrefix(line, "## y"), line)
		assert.False(t, strings.HasPrefix(line, "## z"), line)
		assert.False(t, strings.HasPrefix(line, "## rtal"), line)
	}

	assert.Contains(t, out, "File portal/web/a ## x.tsx:")
}

func TestBrief_FrameSourceLanguage(t *testing.T) {
	tests := map[string]string{
		"a.tsx":    "tsx",
		"a.jsx":    "jsx",
		"a.ts":     "ts",
		"a.js":     "js",
		"a.vue":    "vue",
		"a.svelte": "svelte",
		"a.astro":  "astro",
		"a.html":   "html",
		"a.css":    "css",
		"a.svg":    "svg",
		"A.TSX":    "tsx",
		"a.scss":   "",
		"a":        "",
	}

	for file, lang := range tests {
		out := frameSource([]Root{frameRoot("p", Asset{Path: "p/web/" + file, Text: "x"})}, window128k)

		assert.Contains(t, out, "\n```"+lang+"\nx\n```\n", file)
	}
}

func TestBrief_FrameSourceOrderOfRoots(t *testing.T) {
	out := frameSource([]Root{
		frameRoot("b", Asset{Path: "b/web/x.tsx", Text: "bb"}),
		frameRoot("a", Asset{Path: "a/web/x.tsx", Text: "aa"}),
	}, window128k)

	assert.Less(t, strings.Index(out, "### Frame source: b b/web"), strings.Index(out, "### Frame source: a a/web"))
}

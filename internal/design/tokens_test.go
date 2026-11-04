package design

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRootDecls(t *testing.T) {
	tests := []struct {
		name string
		css  string
		want []decl
	}{
		{
			name: "root, theme, layered html; media, other selectors and host lists",
			css: `:root{--a:#007a7a}@theme{--b:var(--a)}@layer base{html{--c:1rem}}` +
				`@media (max-width:40em){:root{--a:red}}.x{--d:blue}:root, :host{--e:2px}`,
			want: []decl{{"--a", "#007a7a"}, {"--b", "var(--a)"}, {"--c", "1rem"}, {"--e", "2px"}},
		},
		{
			name: "theme with any suffix",
			css:  `@theme inline{--a:1}@theme static{--b:2}@theme default inline{--c:3}@themes{--d:4}`,
			want: []decl{{"--a", "1"}, {"--b", "2"}, {"--c", "3"}},
		},
		{
			name: "several root blocks keep source order and repeats",
			css:  `:root{--a:1}html{--b:2}:root{--a:3}`,
			want: []decl{{"--a", "1"}, {"--b", "2"}, {"--a", "3"}},
		},
		{
			name: "selector list item must be exactly root or html",
			css: `html.dark{--a:1}:root:not(.x){--b:2}.a :root{--c:3}body{--d:4}` +
				`@media screen, html{--e:5}.a,html,.b{--f:6}HTML{--g:7}`,
			want: []decl{{"--f", "6"}, {"--g", "7"}},
		},
		{
			name: "every outer block must be a layer",
			css: `@layer a{@layer b{:root{--a:1}}}@layer a{@media x{:root{--b:2}}:root{--c:3}}` +
				`@layer{:root{--d:4}}@supports (a:b){@layer c{:root{--e:5}}}`,
			want: []decl{{"--a", "1"}, {"--c", "3"}, {"--d", "4"}},
		},
		{
			name: "a root selector nested in another rule is ignored",
			css:  `.a{:root{--x:1}}:root{--b:2;.c{--y:3}--d:4;& :root{--z:5}}`,
			want: []decl{{"--b", "2"}, {"--d", "4"}},
		},
		{
			name: "important is dropped and whitespace trimmed",
			css:  ":root{ --a :  red !important ;--b:blue ! IMPORTANT;--c:1px!Important}",
			want: []decl{{"--a", "red"}, {"--b", "blue"}, {"--c", "1px"}},
		},
		{
			name: "only custom properties",
			css:  `:root{color:red;--a:1;font-family:x;--:2;--b}`,
			want: []decl{{"--a", "1"}},
		},
		{
			name: "comments are not structure",
			css: `/* :root{--no:1} */:root /* } */ {/* --x:1; */--a:1;/* ; */--b: 2 /* c */;` +
				`--c:3/* } */}/* {*/ .y /* , :root */{--d:4}`,
			want: []decl{{"--a", "1"}, {"--b", "2"}, {"--c", "3"}},
		},
		{
			name: "strings keep braces, semicolons and comment openers",
			css:  `:root{--a:"x;}{";--b:'/*';--c:"\";}"}:root{--d:1}`,
			want: []decl{{"--a", `"x;}{"`}, {"--b", `'/*'`}, {"--c", `"\";}"`}, {"--d", "1"}},
		},
		{
			name: "backslash escapes are not structure",
			css:  `.a\{b{--no:1}.c\}d{--no2:2}:root{--a:\;x;--b:\};--c:\'}:root{--d:4}`,
			want: []decl{{"--a", `\;x`}, {"--b", `\}`}, {"--c", `\'`}, {"--d", "4"}},
		},
		{
			name: "unquoted url body keeps its semicolon",
			css:  `:root{--i:url(data:image/svg+xml;base64,AAA=);--b:2}`,
			want: []decl{{"--i", "url(data:image/svg+xml;base64,AAA=)"}, {"--b", "2"}},
		},
		{
			name: "reflowed rules",
			css:  reflow(`:root{--a:1;--b:var(--a)}@layer x{html{--c:3}}`),
			want: []decl{{"--a", "1"}, {"--b", "var(--a)"}, {"--c", "3"}},
		},
		{
			name: "empty value and a name containing an escape",
			css:  `:root{--a:;--b\.c:1}`,
			want: []decl{{"--a", ""}, {`--b\.c`, "1"}},
		},
		{
			name: "block closed by the end of input keeps its last declaration",
			css:  `:root{--a:1;--b:2`,
			want: []decl{{"--a", "1"}, {"--b", "2"}},
		},
		{
			name: "stray closing braces and unterminated comments",
			css:  `}}:root{--a:1}}@media x{:root{--b:2}}/* :root{--c:3}`,
			want: []decl{{"--a", "1"}},
		},
		{
			name: "empty",
			css:  "",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, rootDecls(tt.css))
		})
	}
}

func TestRootDecls_DeepNestingIsLinear(t *testing.T) {
	css := strings.Repeat("@media x{", 2_000_000) + ":root{--a:1}" + strings.Repeat("}", 2_000_000) + ":root{--b:2}"

	done := make(chan []decl, 1)

	go func() { done <- rootDecls(css) }()

	select {
	case got := <-done:
		assert.Equal(t, []decl{{"--b", "2"}}, got)
	case <-time.After(10 * time.Second):
		t.Fatal("rootDecls did not finish on deeply nested input")
	}
}

func TestReferences(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "distinct and sorted",
			src:  `a{color:var(--b);background:var(--a)}c{border:1px solid var(--b)}`,
			want: []string{"--a", "--b"},
		},
		{
			name: "fallbacks and nested references",
			src:  `x{margin:var(--m, var(--n, 1px))}`,
			want: []string{"--m", "--n"},
		},
		{
			name: "whitespace and unusual characters",
			src:  "x{a:var(\n  --acme-green_600 );b:var(--färg);c:var(--a\\.b)}",
			want: []string{`--a\.b`, "--acme-green_600", "--färg"},
		},
		{
			name: "inside a script string",
			src:  "const s = { color: 'var(--text-main)' }; `calc(var(--gap) * 2)`",
			want: []string{"--gap", "--text-main"},
		},
		{
			name: "not a reference",
			src:  "var(foo) var(--) var(--$x) var( ) varnish(--x) --solo",
			want: nil,
		},
		{
			name: "empty",
			src:  "",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, references(tt.src))
		})
	}
}

func TestResolveTokens(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		decls []decl
		want  []Token
	}{
		{
			name:  "chain",
			names: []string{"--b"},
			decls: []decl{{"--b", "var(--a)"}, {"--a", "#007a7a"}},
			want:  []Token{{Name: "--b", Value: "#007a7a"}},
		},
		{
			name:  "composite value",
			names: []string{"--shadow"},
			decls: []decl{{"--a", "#007a7a"}, {"--shadow", "0 1px var(--a)"}},
			want:  []Token{{Name: "--shadow", Value: "0 1px #007a7a"}},
		},
		{
			name:  "several references in one value",
			names: []string{"--s"},
			decls: []decl{{"--x", "1px"}, {"--y", "2px"}, {"--s", "var(--x) var(--y) var(--x)"}},
			want:  []Token{{Name: "--s", Value: "1px 2px 1px"}},
		},
		{
			name:  "fallback for a missing name",
			names: []string{"--b"},
			decls: []decl{{"--b", "var(--missing, 4px)"}},
			want:  []Token{{Name: "--b", Value: "4px"}},
		},
		{
			name:  "fallback with a comma and parentheses",
			names: []string{"--b"},
			decls: []decl{{"--b", "var(--missing, rgb(0, 1, 2) url(a,b))"}},
			want:  []Token{{Name: "--b", Value: "rgb(0, 1, 2) url(a,b)"}},
		},
		{
			name:  "empty fallback",
			names: []string{"--b"},
			decls: []decl{{"--b", "a var(--missing,) b"}},
			want:  []Token{{Name: "--b", Value: "a  b"}},
		},
		{
			name:  "nested fallback resolves a declared name",
			names: []string{"--b"},
			decls: []decl{{"--a", "red"}, {"--b", "var(--missing, var(--a, blue))"}},
			want:  []Token{{Name: "--b", Value: "red"}},
		},
		{
			name:  "declared name is preferred over its fallback",
			names: []string{"--b"},
			decls: []decl{{"--a", "red"}, {"--b", "var(--a, blue)"}},
			want:  []Token{{Name: "--b", Value: "red"}},
		},
		{
			name:  "fallback stands in for a name that cannot be resolved",
			names: []string{"--b"},
			decls: []decl{{"--a", "var(--missing)"}, {"--b", "var(--a, 4px)"}},
			want:  []Token{{Name: "--b", Value: "4px"}},
		},
		{
			name:  "missing name without a fallback stays as written",
			names: []string{"--b"},
			decls: []decl{{"--b", "1px var(--missing)"}},
			want:  []Token{{Name: "--b", Value: "1px var(--missing)", Unresolved: true}},
		},
		{
			name:  "resolvable parts are replaced next to an unresolvable one",
			names: []string{"--b"},
			decls: []decl{{"--a", "#007a7a"}, {"--b", "0 1px var(--a), 0 2px var(--missing)"}},
			want:  []Token{{Name: "--b", Value: "0 1px #007a7a, 0 2px var(--missing)", Unresolved: true}},
		},
		{
			name:  "a requested name nobody declares",
			names: []string{"--nope"},
			decls: []decl{{"--a", "1"}},
			want:  []Token{{Name: "--nope", Value: "var(--nope)", Unresolved: true}},
		},
		{
			name:  "cycle",
			names: []string{"--x", "--y"},
			decls: []decl{{"--x", "var(--y)"}, {"--y", "var(--x)"}},
			want: []Token{
				{Name: "--x", Value: "var(--y)", Unresolved: true},
				{Name: "--y", Value: "var(--x)", Unresolved: true},
			},
		},
		{
			name:  "self reference is a cycle even with a fallback",
			names: []string{"--x"},
			decls: []decl{{"--x", "var(--x, 1px)"}},
			want:  []Token{{Name: "--x", Value: "var(--x, 1px)", Unresolved: true}},
		},
		{
			name:  "a cycle does not stop a reference with a fallback to it",
			names: []string{"--c"},
			decls: []decl{{"--a", "var(--b)"}, {"--b", "var(--a)"}, {"--c", "var(--a, red)"}},
			want:  []Token{{Name: "--c", Value: "red"}},
		},
		{
			name:  "later declaration wins",
			names: []string{"--a", "--b"},
			decls: []decl{{"--a", "red"}, {"--b", "var(--a)"}, {"--a", "blue"}},
			want:  []Token{{Name: "--a", Value: "blue"}, {Name: "--b", Value: "blue"}},
		},
		{
			name:  "result is sorted by name and names are distinct",
			names: []string{"--z", "--a", "--z", "--m"},
			decls: []decl{{"--a", "1"}, {"--m", "2"}, {"--z", "3"}},
			want:  []Token{{Name: "--a", Value: "1"}, {Name: "--m", Value: "2"}, {Name: "--z", Value: "3"}},
		},
		{
			name:  "unrequested names are not returned",
			names: []string{"--a"},
			decls: []decl{{"--a", "1"}, {"--b", "2"}},
			want:  []Token{{Name: "--a", Value: "1"}},
		},
		{
			name:  "a reference inside a string is text",
			names: []string{"--a"},
			decls: []decl{{"--b", "1"}, {"--a", `"var(--b)" var(--b)`}},
			want:  []Token{{Name: "--a", Value: `"var(--b)" 1`}},
		},
		{
			name:  "a literal value is returned as is",
			names: []string{"--a"},
			decls: []decl{{"--a", "url(data:image/png;base64,AAA=)"}},
			want:  []Token{{Name: "--a", Value: "url(data:image/png;base64,AAA=)"}},
		},
		{
			name:  "malformed reference stays as written",
			names: []string{"--a"},
			decls: []decl{{"--a", "var(--b"}},
			want:  []Token{{Name: "--a", Value: "var(--b", Unresolved: true}},
		},
		{
			name:  "no names",
			names: nil,
			decls: []decl{{"--a", "1"}},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveTokens(tt.names, tt.decls))
		})
	}
}

func TestResolveTokens_DepthLimit(t *testing.T) {
	chain := func(hops int) []decl {
		decls := []decl{{"--n0", "#007a7a"}}
		for i := 1; i <= hops; i++ {
			decls = append(decls, decl{fmt.Sprintf("--n%d", i), fmt.Sprintf("var(--n%d)", i-1)})
		}

		return decls
	}

	got := resolveTokens([]string{"--n10"}, chain(10))
	require.Len(t, got, 1)
	assert.Equal(t, Token{Name: "--n10", Value: "#007a7a"}, got[0], "ten references deep resolves")

	got = resolveTokens([]string{"--n11"}, chain(11))
	require.Len(t, got, 1)
	assert.True(t, got[0].Unresolved, "eleven references deep does not")
}

func TestResolveTokens_ExpansionBomb(t *testing.T) {
	// Each level references the one below it 300 times, ten levels deep:
	// unbounded expansion would never finish.
	decls := []decl{{"--l0", "x"}}
	for i := 1; i <= 10; i++ {
		decls = append(decls, decl{fmt.Sprintf("--l%d", i), strings.Repeat(fmt.Sprintf("var(--l%d) ", i-1), 300)})
	}

	done := make(chan []Token, 1)

	go func() { done <- resolveTokens([]string{"--l10", "--l9"}, decls) }()

	select {
	case got := <-done:
		require.Len(t, got, 2)

		for _, tok := range got {
			assert.True(t, tok.Unresolved, tok.Name)
			assert.Less(t, len(tok.Value), 1<<20, tok.Name)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolveTokens did not finish on an expansion bomb")
	}
}

func TestResolveTokens_ManyNamesOneBigValue(t *testing.T) {
	big := strings.Repeat("a", 4<<20)
	decls := []decl{{"--big", big}}
	names := make([]string, 0, 2000)

	for i := range 2000 {
		name := fmt.Sprintf("--n%d", i)
		decls = append(decls, decl{name, "var(--big)"})
		names = append(names, name)
	}

	done := make(chan []Token, 1)

	go func() { done <- resolveTokens(names, decls) }()

	select {
	case got := <-done:
		require.Len(t, got, len(names))

		for _, tok := range got {
			assert.True(t, tok.Unresolved, tok.Name)
			assert.Equal(t, "var(--big)", tok.Value, tok.Name)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("resolveTokens did not finish")
	}
}

func TestResolveTokens_UnclosedCallsAreLinear(t *testing.T) {
	// A thousand unclosed fallbacks: rescanning the rest of the value for each
	// one is quadratic and, over thousands of names, never finishes.
	decls := []decl{{"--m", strings.Repeat("var(--a,", 1000)}}
	names := make([]string, 0, 5000)

	for i := range 5000 {
		name := fmt.Sprintf("--n%d", i)
		decls = append(decls, decl{name, "var(--m)"})
		names = append(names, name)
	}

	done := make(chan []Token, 1)

	go func() { done <- resolveTokens(names, decls) }()

	select {
	case got := <-done:
		require.Len(t, got, len(names))
		assert.True(t, got[0].Unresolved)
	case <-time.After(10 * time.Second):
		t.Fatal("resolveTokens did not finish")
	}
}

func TestFonts(t *testing.T) {
	tests := []struct {
		name    string
		decls   []decl
		ownCSS  []string
		want    []Font
		wantLen int
	}{
		{
			name:  "default stack draws with its generic family",
			decls: []decl{{"--font-family-default", `"Acme Sans", sans-serif`}},
			want:  []Font{{Stack: `"Acme Sans", sans-serif`, DrawWith: "sans-serif"}},
		},
		{
			name:  "a named system family wins over the generic after it",
			decls: []decl{{"--font-family-mono", `"AcmeMono","Menlo",monospace`}},
			want:  []Font{{Stack: `"AcmeMono","Menlo",monospace`, DrawWith: "Menlo"}},
		},
		{
			name:  "no drawable family falls back to sans-serif",
			decls: []decl{{"--font-family-brand", `"Acme Display", Fraunces`}},
			want:  []Font{{Stack: `"Acme Display", Fraunces`, DrawWith: "sans-serif"}},
		},
		{
			name: "quotes are ignored, case is ignored, the family is returned unquoted",
			decls: []decl{
				{"--font-family-a", `'acme', 'segoe ui', Arial`},
				{"--font-family-b", `-apple-system, BlinkMacSystemFont, Roboto`},
				{"--font-family-c", `"Times New Roman", serif`},
			},
			want: []Font{
				{Stack: `'acme', 'segoe ui', Arial`, DrawWith: "segoe ui"},
				{Stack: `-apple-system, BlinkMacSystemFont, Roboto`, DrawWith: "-apple-system"},
				{Stack: `"Times New Roman", serif`, DrawWith: "Times New Roman"},
			},
		},
		{
			name: "a quoted family may hold a comma",
			decls: []decl{
				{"--font-family-a", `"Acme, Menlo", Verdana`},
			},
			want: []Font{{Stack: `"Acme, Menlo", Verdana`, DrawWith: "Verdana"}},
		},
		{
			name: "declarations resolve through var and unresolved ones are skipped",
			decls: []decl{
				{"--acme-mono", `"AcmeMono", ui-monospace`},
				{"--font-family-mono", "var(--acme-mono)"},
				{"--font-family-broken", "var(--missing), serif"},
				{"--color-text", "black"},
			},
			want: []Font{{Stack: `"AcmeMono", ui-monospace`, DrawWith: "ui-monospace"}},
		},
		{
			name: "own css html, body and root rules, not other selectors",
			ownCSS: []string{
				`html{font-family:Georgia,serif}body{font-family:"Acme Text",Verdana;color:red}` +
					`:root{font-family:Menlo,monospace}.card{font-family:Arial}` +
					`@media print{body{font-family:Courier New}}`,
			},
			want: []Font{
				{Stack: "Georgia,serif", DrawWith: "Georgia"},
				{Stack: `"Acme Text",Verdana`, DrawWith: "Verdana"},
				{Stack: "Menlo,monospace", DrawWith: "Menlo"},
			},
		},
		{
			name:   "own css value resolves through the declarations",
			decls:  []decl{{"--font-sans", "Acme, system-ui"}},
			ownCSS: []string{"body { font-family: var(--font-sans) !important; }", "html{font-family:var(--missing)}"},
			want:   []Font{{Stack: "Acme, system-ui", DrawWith: "system-ui"}},
		},
		{
			name:   "stacks are distinct across declarations and own css",
			decls:  []decl{{"--font-family-a", "Arial, sans-serif"}, {"--font-family-b", "Arial, sans-serif"}},
			ownCSS: []string{"body{font-family:Arial, sans-serif}body{font-family:Arial,sans-serif}"},
			want: []Font{
				{Stack: "Arial, sans-serif", DrawWith: "Arial"},
				{Stack: "Arial,sans-serif", DrawWith: "Arial"},
			},
		},
		{
			name:   "declaration stacks come first, in name order, then own css",
			decls:  []decl{{"--font-family-b", "Verdana"}, {"--font-family-a", "Georgia"}},
			ownCSS: []string{"body{font-family:Menlo}"},
			want: []Font{
				{Stack: "Georgia", DrawWith: "Georgia"},
				{Stack: "Verdana", DrawWith: "Verdana"},
				{Stack: "Menlo", DrawWith: "Menlo"},
			},
		},
		{
			name: "at most six",
			decls: []decl{
				{"--font-family-1", "Arial"},
				{"--font-family-2", "Verdana"},
				{"--font-family-3", "Georgia"},
				{"--font-family-4", "Menlo"},
				{"--font-family-5", "Consolas"},
				{"--font-family-6", "Helvetica"},
				{"--font-family-7", "Courier New"},
			},
			wantLen: 6,
		},
		{
			name: "nothing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fonts(tt.decls, tt.ownCSS)
			if tt.wantLen > 0 {
				assert.Len(t, got, tt.wantLen)

				return
			}

			assert.Equal(t, tt.want, got)
		})
	}
}

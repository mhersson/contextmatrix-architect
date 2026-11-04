package design

import (
	"iter"
	"slices"
	"strings"
	"unicode"
)

const (
	// maxVarHops is how many var() references deep a value is followed.
	maxVarHops = 10

	// maxExpandBytes bounds the bytes one token's expansion may touch, so
	// fan-out or a huge shared value cannot blow up time or memory.
	maxExpandBytes = 8 << 10

	maxFonts          = 6
	maxFontCandidates = 64
)

// decl is one declaration: a property name and its value, trimmed of
// surrounding whitespace and of a trailing !important.
type decl struct{ Name, Value string }

// rootDecls returns the custom properties declared where a design system
// keeps its tokens, in source order: inside an @theme block, or inside a rule
// whose selector list has an item that is exactly :root or html, with every
// outer block an @layer. Declarations under @media, @supports, other
// selectors, or a root selector nested in another rule are not tokens.
func rootDecls(css string) []decl {
	var out []decl

	for _, d := range blockDecls(css, isRootPrelude) {
		if strings.HasPrefix(d.Name, "--") && len(d.Name) > 2 {
			out = append(out, d)
		}
	}

	return out
}

func isRootPrelude(prelude string) bool {
	return isAtRule(prelude, "@theme") || selectorHas(prelude, ":root", "html")
}

func isFontRulePrelude(prelude string) bool {
	return selectorHas(prelude, ":root", "html", "body")
}

// blockDecls returns every declaration directly inside a block whose prelude
// satisfies match, in source order, when each outer block is an @layer. It is
// one linear pass that keeps counters rather than a stack, so nesting depth
// costs nothing.
func blockDecls(css string, match func(prelude string) bool) []decl {
	var (
		out    []decl
		depth  int // open blocks
		layers int // open blocks that are @layer, all of them outermost
		target int // depth of the open matching block; 0 when none
		start  int // where the current prelude or declaration began
	)

	add := func(end int) {
		if d, ok := parseDecl(css[start:end]); ok {
			out = append(out, d)
		}
	}

	for i := 0; i < len(css); {
		if end := lexEnd(css, i); end > i {
			i = end

			continue
		}

		switch css[i] {
		case '{':
			if depth == layers {
				prelude := strings.TrimSpace(stripComments(css[start:i]))

				switch {
				case isAtRule(prelude, "@layer"):
					layers++
				case match(prelude):
					target = depth + 1
				}
			}

			depth++
			start = i + 1
		case '}':
			if depth > 0 {
				switch depth {
				case target:
					add(i)

					target = 0
				case layers:
					layers--
				}

				depth--
			}

			start = i + 1
		case ';':
			if target > 0 && depth == target {
				add(i)
			}

			start = i + 1
		}

		i++
	}

	if target > 0 && depth == target {
		add(len(css))
	}

	return out
}

// parseDecl splits one declaration statement at its first colon.
func parseDecl(stmt string) (decl, bool) {
	stmt = stripComments(stmt)

	colon := -1

	for i := 0; i < len(stmt); i++ {
		if stmt[i] == '\\' {
			i++

			continue
		}

		if stmt[i] == ':' {
			colon = i

			break
		}
	}

	if colon < 0 {
		return decl{}, false
	}

	name := strings.TrimSpace(stmt[:colon])
	if name == "" || strings.ContainsFunc(name, unicode.IsSpace) {
		return decl{}, false
	}

	return decl{Name: name, Value: trimValue(stmt[colon+1:])}, true
}

func trimValue(v string) string {
	const important = "important"

	v = strings.TrimSpace(v)
	if len(v) < len(important) || !strings.EqualFold(v[len(v)-len(important):], important) {
		return v
	}

	rest := strings.TrimSpace(v[:len(v)-len(important)])
	if rest, ok := strings.CutSuffix(rest, "!"); ok {
		return strings.TrimSpace(rest)
	}

	return v
}

// lexEnd returns the index after the comment, string, unquoted url() body or
// backslash escape that starts at i, or i when none does. The parenthesis of a
// url() with a quoted body is structure, so it is not skipped.
func lexEnd(css string, i int) int {
	switch c := css[i]; {
	case c == '/' && strings.HasPrefix(css[i:], "/*"):
		return commentEnd(css, i)
	case c == '"' || c == '\'':
		return stringEnd(css, i)
	case c == '(' && i >= 3 && strings.EqualFold(css[i-3:i], "url"):
		if end := urlEnd(css, i); end > i+1 {
			return end
		}
	case c == '\\':
		return min(i+2, len(css))
	}

	return i
}

// stripComments replaces each comment with a space; strings, url() bodies and
// escapes are left alone.
func stripComments(s string) string {
	if !strings.Contains(s, "/*") {
		return s
	}

	var b strings.Builder

	b.Grow(len(s))

	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "/*") {
			b.WriteByte(' ')

			i = commentEnd(s, i)

			continue
		}

		end := max(lexEnd(s, i), i+1)
		b.WriteString(s[i:end])

		i = end
	}

	return b.String()
}

// isAtRule reports whether prelude is the at-rule name, alone or followed by
// whitespace and a suffix.
func isAtRule(prelude, name string) bool {
	if len(prelude) < len(name) || !strings.EqualFold(prelude[:len(name)], name) {
		return false
	}

	return len(prelude) == len(name) || isCSSSpace(prelude[len(name)])
}

// selectorHas reports whether prelude is a selector list with an item that is
// exactly one of names.
func selectorHas(prelude string, names ...string) bool {
	if strings.HasPrefix(prelude, "@") {
		return false
	}

	for item := range strings.SplitSeq(prelude, ",") {
		item = strings.TrimSpace(item)

		if slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(item, n) }) {
			return true
		}
	}

	return false
}

// references returns the distinct custom property names that src passes to
// var(), sorted. src may be CSS or script source, so strings are not skipped.
func references(src string) []string {
	var names []string

	seen := map[string]bool{}

	for i := 0; i+len("var(") < len(src); i++ {
		if src[i] != 'v' || !strings.HasPrefix(src[i:], "var(") {
			continue
		}

		name, end, ok := varName(src, i+len("var("))
		if !ok {
			continue
		}

		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}

		i = end - 1
	}

	slices.Sort(names)

	return names
}

// varName reads the custom property name that starts at i, after optional
// whitespace. end is the index after it.
func varName(s string, i int) (name string, end int, ok bool) {
	for i < len(s) && isCSSSpace(s[i]) {
		i++
	}

	start := i
	if !strings.HasPrefix(s[i:], "--") {
		return "", i, false
	}

	i += 2

	for i < len(s) {
		if s[i] == '\\' && i+1 < len(s) {
			i += 2

			continue
		}

		if !isIdentByte(s[i]) {
			break
		}

		i++
	}

	if i-start <= 2 {
		return "", i, false
	}

	return s[start:i], i, true
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c >= 0x80
}

// Token is a resolved design token. When Unresolved is set, Value still holds
// each reference that could not be resolved as it was written.
type Token struct {
	Name, Value string
	Unresolved  bool
}

// resolveTokens returns the named tokens sorted by name, the last declaration
// of a name winning and var() references replaced up to ten deep. A missing
// name is replaced by the reference's fallback when it has one; a reference
// that closes a cycle, or runs past the depth or size limit, stays as written.
func resolveTokens(names []string, decls []decl) []Token {
	sorted := slices.Clone(names)
	slices.Sort(sorted)

	r := newResolver(decls)

	var out []Token

	for _, name := range slices.Compact(sorted) {
		out = append(out, r.token(name))
	}

	return out
}

type resolver struct {
	values map[string]string
	path   []string // names being resolved, outermost first
	budget int
}

func newResolver(decls []decl) *resolver {
	values := make(map[string]string, len(decls))
	for _, d := range decls {
		values[d.Name] = d.Value
	}

	return &resolver{values: values}
}

func (r *resolver) token(name string) Token {
	v, ok := r.values[name]
	if !ok {
		return Token{Name: name, Value: "var(" + name + ")", Unresolved: true}
	}

	value, resolved := r.value(name, v)

	return Token{Name: name, Value: value, Unresolved: !resolved}
}

// value expands the value v of the token root ("" for a value no token owns).
// A value without var() is returned untouched, however long.
func (r *resolver) value(root, v string) (string, bool) {
	if !strings.Contains(v, "var(") {
		return v, true
	}

	r.path = append(r.path[:0], root)
	r.budget = maxExpandBytes

	return r.expand(v)
}

// expand replaces every resolvable var() in v. ok is false when any stays.
func (r *resolver) expand(v string) (out string, ok bool) {
	if len(v) > r.budget {
		return v, false
	}

	r.budget -= len(v)

	var b strings.Builder

	ok = true
	from := 0

	for i := 0; i < len(v); {
		if end := lexEnd(v, i); end > i {
			i = end

			continue
		}

		if v[i] != 'v' || !strings.HasPrefix(v[i:], "var(") {
			i++

			continue
		}

		call, parsed := parseVarCall(v[i:])
		if !parsed {
			ok = false
			i += max(call.n, len("var("))

			continue
		}

		val, resolved, useFallback := r.lookup(call.name)
		if !resolved && useFallback && call.hasFallback {
			val, resolved = r.expand(call.fallback)
		}

		if !resolved {
			ok = false
			i += call.n

			continue
		}

		b.WriteString(v[from:i])
		b.WriteString(val)

		i += call.n
		from = i
	}

	if from == 0 {
		return v, ok
	}

	b.WriteString(v[from:])

	return b.String(), ok
}

// lookup resolves a declared name. useFallback is false for a cycle or the
// depth limit, where a var() fallback does not stand in.
func (r *resolver) lookup(name string) (val string, resolved, useFallback bool) {
	if slices.Contains(r.path, name) || len(r.path) > maxVarHops {
		return "", false, false
	}

	v, ok := r.values[name]
	if !ok {
		return "", false, true
	}

	r.path = append(r.path, name)
	val, resolved = r.expand(v)
	r.path = r.path[:len(r.path)-1]

	return val, resolved, true
}

type varCall struct {
	name, fallback string
	hasFallback    bool
	n              int // length of the whole call; the rest of the text for an unclosed one
}

// parseVarCall reads the var() call at the start of s.
func parseVarCall(s string) (varCall, bool) {
	name, i, ok := varName(s, len("var("))
	if !ok {
		return varCall{}, false
	}

	for i < len(s) && isCSSSpace(s[i]) {
		i++
	}

	if i >= len(s) {
		return varCall{}, false
	}

	switch s[i] {
	case ')':
		return varCall{name: name, n: i + 1}, true
	case ',':
		end, closed := parenEnd(s, i+1)
		if !closed {
			// Nothing after an unclosed call can close it: skip to the end
			// rather than rescan the rest for every call that follows.
			return varCall{n: len(s)}, false
		}

		return varCall{name: name, fallback: strings.TrimSpace(s[i+1 : end]), hasFallback: true, n: end + 1}, true
	}

	return varCall{}, false
}

// parenEnd returns the index of the parenthesis that closes the one open
// before from.
func parenEnd(s string, from int) (int, bool) {
	depth := 1

	for i := from; i < len(s); {
		if end := lexEnd(s, i); end > i {
			i = end

			continue
		}

		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i, true
			}
		}

		i++
	}

	return 0, false
}

// Font is a font stack found in the design material and the family a mockup
// draws it with.
type Font struct{ Stack, DrawWith string }

// fonts returns up to six distinct stacks: the resolved declarations whose
// name contains font-family, in name order, then the font-family values of the
// html, body and :root rules of ownCSS. Stacks that do not resolve are left
// out. Each source is examined for at most maxFontCandidates declarations, so
// a flood of them in untrusted CSS costs a fixed amount of work.
func fonts(decls []decl, ownCSS []string) []Font {
	r := newResolver(decls)

	var (
		out  []Font
		seen = map[string]bool{}
	)

	// add reports whether out is full.
	add := func(stack string) bool {
		if stack != "" && !seen[stack] {
			seen[stack] = true

			out = append(out, Font{Stack: stack, DrawWith: drawWith(stack)})
		}

		return len(out) == maxFonts
	}

	var names []string

	for _, d := range decls {
		if strings.Contains(d.Name, "font-family") {
			names = append(names, d.Name)
		}
	}

	slices.Sort(names)

	names = slices.Compact(names)
	for _, name := range names[:min(len(names), maxFontCandidates)] {
		if t := r.token(name); !t.Unresolved && add(t.Value) {
			return out
		}
	}

	examined := 0

	for _, css := range ownCSS {
		for _, d := range blockDecls(css, isFontRulePrelude) {
			if !strings.EqualFold(d.Name, "font-family") {
				continue
			}

			if examined == maxFontCandidates {
				return out
			}

			examined++

			if value, ok := r.value("", d.Value); ok && add(value) {
				return out
			}
		}
	}

	return out
}

// drawWith returns the first family of stack that a mockup can draw without
// loading anything, or sans-serif.
func drawWith(stack string) string {
	for family := range families(stack) {
		family = strings.TrimSpace(family)
		if n := len(family); n >= 2 && (family[0] == '"' || family[0] == '\'') && family[n-1] == family[0] {
			family = strings.TrimSpace(family[1 : n-1])
		}

		switch strings.ToLower(family) {
		case "serif", "sans-serif", "monospace", "system-ui", "ui-sans-serif", "ui-serif", "ui-monospace",
			"cursive", "fantasy",
			"-apple-system", "blinkmacsystemfont", "segoe ui", "helvetica neue", "helvetica", "arial",
			"verdana", "georgia", "times new roman", "courier new", "menlo", "consolas":
			return family
		}
	}

	return "sans-serif"
}

// families yields the families of a font stack, split at the commas outside
// quotes.
func families(stack string) iter.Seq[string] {
	return func(yield func(string) bool) {
		start := 0

		for i := 0; i < len(stack); {
			switch stack[i] {
			case ',':
				if !yield(stack[start:i]) {
					return
				}

				start = i + 1
				i++
			case '"', '\'':
				i = stringEnd(stack, i)
			default:
				i++
			}
		}

		yield(stack[start:])
	}
}

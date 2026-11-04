package design

import "strings"

// reflow puts a newline after every `{`, `}` and `;` that is outside strings,
// comments and unquoted url() bodies, and drops the whitespace and newlines
// that followed the break. Nothing else changes.
func reflow(css string) string {
	var b strings.Builder

	b.Grow(len(css) + len(css)/4)

	for i := 0; i < len(css); {
		var end int

		switch c := css[i]; {
		case c == '/' && strings.HasPrefix(css[i:], "/*"):
			end = commentEnd(css, i)
		case c == '"' || c == '\'':
			end = stringEnd(css, i)
		case c == '(' && i >= 3 && strings.EqualFold(css[i-3:i], "url"):
			end = urlEnd(css, i)
		case c == '{' || c == '}' || c == ';':
			b.WriteByte(c)
			b.WriteByte('\n')

			i++
			for i < len(css) && isCSSSpace(css[i]) {
				i++
			}

			continue
		default:
			end = i + 1
		}

		b.WriteString(css[i:end])

		i = end
	}

	return b.String()
}

// commentEnd returns the index after the comment starting at i.
func commentEnd(css string, i int) int {
	idx := strings.Index(css[i+2:], "*/")
	if idx < 0 {
		return len(css)
	}

	return i + 2 + idx + 2
}

// stringEnd returns the index after the string starting at i. An unescaped
// newline ends a string that has no closing quote, and stays outside it.
func stringEnd(css string, i int) int {
	quote := css[i]

	for j := i + 1; j < len(css); j++ {
		switch css[j] {
		case '\\':
			j++
		case quote:
			return j + 1
		case '\n', '\r':
			return j
		}
	}

	return len(css)
}

// urlEnd returns the index after the url( at open when its body is unquoted
// and so runs to the closing parenthesis; for a quoted body it returns the
// index after the opening parenthesis.
func urlEnd(css string, open int) int {
	j := open + 1
	for j < len(css) && isCSSSpace(css[j]) {
		j++
	}

	if j < len(css) && (css[j] == '"' || css[j] == '\'') {
		return open + 1
	}

	for j = open + 1; j < len(css); j++ {
		switch css[j] {
		case '\\':
			j++
		case ')':
			return j + 1
		}
	}

	return len(css)
}

func isCSSSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

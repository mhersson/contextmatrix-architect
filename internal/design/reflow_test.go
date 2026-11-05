package design

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReflow(t *testing.T) {
	tests := []struct {
		name string
		css  string
		want string
	}{
		{
			name: "break after brace and semicolon",
			css:  `a{b:c;d:e}f{g:h}`,
			want: "a{\nb:c;\nd:e}\nf{\ng:h}\n",
		},
		{
			name: "braces and semicolons inside a string stay",
			css:  `a{content:"}{;"}`,
			want: "a{\ncontent:\"}{;\"}\n",
		},
		{
			name: "single quoted string",
			css:  `a{content:'}{;'}`,
			want: "a{\ncontent:'}{;'}\n",
		},
		{
			name: "escaped quote does not end the string",
			css:  `a{content:"\";}"}`,
			want: "a{\ncontent:\"\\\";}\"}\n",
		},
		{
			name: "comment is untouched",
			css:  `a{b:c}/* } ; { */d{e:f}`,
			want: "a{\nb:c}\n/* } ; { */d{\ne:f}\n",
		},
		{
			name: "multiline comment is untouched",
			css:  "a{b:c}\n/* x;\n} */\nd{e:f}",
			want: "a{\nb:c}\n/* x;\n} */\nd{\ne:f}\n",
		},
		{
			name: "unterminated comment runs to the end",
			css:  `a{b:c}/* d{e:f}`,
			want: "a{\nb:c}\n/* d{e:f}",
		},
		{
			name: "whitespace and newlines after a break are dropped",
			css:  "a {\n  b: c;\n\n  d: e;\n}\n\nf {\r\n\tg: h }",
			want: "a {\nb: c;\nd: e;\n}\nf {\ng: h }\n",
		},
		{
			name: "other whitespace is kept",
			css:  "a\n,\tb  {c : d}",
			want: "a\n,\tb  {\nc : d}\n",
		},
		{
			name: "unquoted url keeps its semicolons",
			css:  `a{b:url(data:image/png;base64,AAA=);c:d}`,
			want: "a{\nb:url(data:image/png;base64,AAA=);\nc:d}\n",
		},
		{
			name: "unquoted url with an escaped parenthesis",
			css:  `a{b:url(x\);y.png);c:d}`,
			want: "a{\nb:url(x\\);y.png);\nc:d}\n",
		},
		{
			name: "quoted url is an ordinary string",
			css:  `a{b:url( "x;y.png" );c:d}`,
			want: "a{\nb:url( \"x;y.png\" );\nc:d}\n",
		},
		{
			name: "url function name is case insensitive",
			css:  `a{b:URL(data:a;b);c:d}`,
			want: "a{\nb:URL(data:a;b);\nc:d}\n",
		},
		{
			name: "newline ends an unterminated string",
			css:  "a{b:\"x\n}c{d:e}",
			want: "a{\nb:\"x\n}\nc{\nd:e}\n",
		},
		{
			name: "non-ASCII text is kept",
			css:  `a{content:"é;ü"}b{c:d}`,
			want: "a{\ncontent:\"é;ü\"}\nb{\nc:d}\n",
		},
		{
			name: "escaped quotes outside a string do not open one",
			css:  `.x\:content-\[\'\'\]{--tw-content:'';}.y{content:'a; b'}`,
			want: ".x\\:content-\\[\\'\\'\\]{\n--tw-content:'';\n}\n.y{\ncontent:'a; b'}\n",
		},
		{
			name: "escaped semicolon and brace in a selector",
			css:  `.a\;b{c:d}.a\{b{e:f}.a\}b{g:h}`,
			want: ".a\\;b{\nc:d}\n.a\\{b{\ne:f}\n.a\\}b{\ng:h}\n",
		},
		{
			name: "escaped slash does not open a comment",
			css:  `.a\/*b{c:d}`,
			want: ".a\\/*b{\nc:d}\n",
		},
		{
			name: "trailing backslash",
			css:  `a{b:c}\`,
			want: "a{\nb:c}\n\\",
		},
		{
			name: "empty",
			css:  "",
			want: "",
		},
		{
			name: "no break characters",
			css:  "plain text",
			want: "plain text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reflow(tt.css)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, got, reflow(got), "reflow is idempotent")
		})
	}
}

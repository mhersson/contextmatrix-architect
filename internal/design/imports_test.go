package design

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCSSImports(t *testing.T) {
	tests := []struct {
		name string
		css  string
		want []string
	}{
		{
			name: "single quoted with layer",
			css:  `@import '@acme/ui/style/core.css' layer(base);`,
			want: []string{"@acme/ui/style/core.css"},
		},
		{
			name: "double quoted",
			css:  `@import "@acme/ui/a.css";`,
			want: []string{"@acme/ui/a.css"},
		},
		{
			name: "url with quotes and media",
			css:  `@import url("./a.css") screen;`,
			want: []string{"./a.css"},
		},
		{
			name: "url single quoted",
			css:  `@import url('./b.css');`,
			want: []string{"./b.css"},
		},
		{
			name: "url unquoted",
			css:  `@import url(./c.css);`,
			want: []string{"./c.css"},
		},
		{
			name: "url with padding",
			css:  `@import url(  "./d.css"  );`,
			want: []string{"./d.css"},
		},
		{
			name: "supports and media list",
			css:  `@import "e.css" supports(display: grid) screen and (min-width: 600px), print;`,
			want: []string{"e.css"},
		},
		{
			name: "commented import ignored",
			css:  "/* @import 'old.css'; */\n@import 'new.css';",
			want: []string{"new.css"},
		},
		{
			name: "multiline comment ignored",
			css:  "/*\n@import 'old.css';\n*/@import 'new.css';",
			want: []string{"new.css"},
		},
		{
			name: "two on one minified line",
			css:  `@import "a.css";@import url(b.css);body{color:red}`,
			want: []string{"a.css", "b.css"},
		},
		{
			name: "uppercase at-rule",
			css:  `@IMPORT "a.css";`,
			want: []string{"a.css"},
		},
		{
			name: "empty specifier skipped",
			css:  `@import ""; @import url(); @import 'a.css';`,
			want: []string{"a.css"},
		},
		{
			name: "no imports",
			css:  `body { color: red; } .a { background: url(x.png); }`,
			want: nil,
		},
		{
			name: "empty",
			css:  ``,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, cssImports(tt.css))
		})
	}
}

func TestJSCSSImports(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "single and double quoted",
			src:  "import '@acme/ui/theme.css';\nimport \"./local.css\"\n",
			want: []string{"@acme/ui/theme.css", "./local.css"},
		},
		{
			name: "default import is not a side effect",
			src:  `import x from '@acme/ui'`,
			want: nil,
		},
		{
			name: "side effect js import ignored",
			src:  `import '@acme/ui/x.js'`,
			want: nil,
		},
		{
			name: "named import from css ignored",
			src:  `import styles from './a.module.css'`,
			want: nil,
		},
		{
			name: "mixed with code",
			src:  "import React from 'react';\nimport '@acme/ui/theme.css';\nconst a = 1;\n",
			want: []string{"@acme/ui/theme.css"},
		},
		{
			name: "minified",
			src:  `import"a.css";import'b.css';`,
			want: []string{"a.css", "b.css"},
		},
		{
			name: "dynamic import ignored",
			src:  `const m = import('a.css')`,
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, jsCSSImports(tt.src))
		})
	}
}

func TestSplitBare(t *testing.T) {
	tests := []struct {
		spec     string
		name     string
		subpath  string
		wantBare bool
	}{
		{"@acme/ui/style/core.css", "@acme/ui", "style/core.css", true},
		{"@acme/ui", "@acme/ui", "", true},
		{"tailwindcss", "tailwindcss", "", true},
		{"tailwindcss/base.css", "tailwindcss", "base.css", true},
		{"~@acme/ui/a.css", "@acme/ui", "a.css", true},
		{"@acme", "", "", false},
		{"@acme/", "", "", false},
		{"@/styles/global.css", "", "", false},
		{"~@/styles/global.css", "", "", false},
		{"", "", "", false},
		{"~", "", "", false},
		{"./a.css", "", "", false},
		{"../a.css", "", "", false},
		{"~./a.css", "", "", false},
		{"/a.css", "", "", false},
		{"theme.css", "", "", false},
		{"https://cdn.example.test/a.css", "", "", false},
		{"http://cdn.example.test/a.css", "", "", false},
		{"data:text/css;base64,AAAA", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			name, subpath, ok := splitBare(tt.spec)
			assert.Equal(t, tt.wantBare, ok)
			assert.Equal(t, tt.name, name)
			assert.Equal(t, tt.subpath, subpath)
		})
	}
}

func TestHTMLStylesheets(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "double quoted",
			src:  `<link rel="stylesheet" href="/a.css">`,
			want: []string{"/a.css"},
		},
		{
			name: "single quoted",
			src:  `<link rel='stylesheet' href='/a.css'>`,
			want: []string{"/a.css"},
		},
		{
			name: "unquoted",
			src:  `<link rel=stylesheet href=/a.css>`,
			want: []string{"/a.css"},
		},
		{
			name: "href first, self-closing, upper case",
			src:  `<LINK HREF="/a.css" REL="StyleSheet" />`,
			want: []string{"/a.css"},
		},
		{
			name: "stylesheet among other tokens",
			src:  `<link rel="preload stylesheet" href="/a.css">`,
			want: []string{"/a.css"},
		},
		{
			name: "an alternate stylesheet skipped",
			src:  `<link rel="alternate stylesheet" href="/dark.css" title="Dark">`,
			want: nil,
		},
		{
			name: "preload and icon skipped",
			src:  `<link rel="preload" href="/a.css" as="style"><link rel="icon" href="/favicon.ico">`,
			want: nil,
		},
		{
			name: "a data attribute is not the href",
			src:  `<link rel="stylesheet" data-href="/a.css">`,
			want: nil,
		},
		{
			name: "remote hrefs kept",
			src:  `<link rel="stylesheet" href="https://cdn.example.test/a.css"><link rel="stylesheet" href="//cdn.example.test/b.css">`,
			want: []string{"https://cdn.example.test/a.css", "//cdn.example.test/b.css"},
		},
		{
			name: "document order",
			src:  "<head>\n<link rel=\"stylesheet\" href=\"b.css\">\n<link\n  rel=\"stylesheet\"\n  href=\"a.css\">\n</head>",
			want: []string{"b.css", "a.css"},
		},
		{
			name: "commented link ignored",
			src:  `<!-- <link rel="stylesheet" href="/old.css"> --><link rel="stylesheet" href="/new.css">`,
			want: []string{"/new.css"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, htmlStylesheets(tt.src))
		})
	}
}

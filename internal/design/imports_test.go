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

func TestLocalImports(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{name: "default", src: `import Menu from './Menu'`, want: []string{"./Menu"}},
		{name: "named", src: `import { Item, Link } from './Items'`, want: []string{"./Items"}},
		{name: "namespace", src: `import * as Icons from './Icons'`, want: []string{"./Icons"}},
		{name: "default and named", src: `import Menu, { Item } from './Menu'`, want: []string{"./Menu"}},
		{name: "side effect", src: `import './Menu'`, want: []string{"./Menu"}},
		{name: "type only", src: `import type { Props } from './types'`, want: []string{"./types"}},
		{name: "re-export", src: `export { default as Menu } from './Menu'`, want: []string{"./Menu"}},
		{name: "re-export all", src: `export * from './Menu'`, want: []string{"./Menu"}},
		{name: "re-export namespace", src: `export * as Menu from './Menu'`, want: []string{"./Menu"}},
		{
			name: "multi-line list",
			src:  "import {\n  Item,\n  Link,\n} from './Items'\n",
			want: []string{"./Items"},
		},
		{name: "double quotes", src: `import Menu from "./Menu"`, want: []string{"./Menu"}},
		{name: "minified", src: `import Menu from'./Menu';import'./Brand';`, want: []string{"./Menu", "./Brand"}},
		{name: "nested path", src: `import { A } from './Menu/Items'`, want: []string{"./Menu/Items"}},
		{
			name: "source order",
			src:  "import B from './B'\nimport A from './A'\nexport { C } from './C'\n",
			want: []string{"./B", "./A", "./C"},
		},
		{
			name: "duplicates once",
			src:  "import A from './A'\nimport { B } from './A'\nexport * from './A'\n",
			want: []string{"./A"},
		},
		{
			name: "block comment ignored",
			src:  "/* import Old from './Old' */\nimport New from './New'\n",
			want: []string{"./New"},
		},
		{
			name: "multi-line block comment ignored",
			src:  "/*\n * import Old from './Old'\n */\nimport New from './New'\n",
			want: []string{"./New"},
		},
		{
			name: "commented-out import ignored",
			src:  "// import Old from './Old'\n  // import Older from './Older'\nimport New from './New'\n",
			want: []string{"./New"},
		},
		{
			name: "block comment opener inside a line comment",
			src:  "// for /docs/*\nimport A from './A'\n/** @returns the bar */\nimport B from './B'\n",
			want: []string{"./A", "./B"},
		},
		{
			name: "line comment inside a block comment",
			src:  "/*\n// import Old from './Old'\n*/\nimport New from './New'\n",
			want: []string{"./New"},
		},
		{
			name: "block comment closer inside a line comment",
			src:  "/* import Old from './Old'\n// done */\nimport New from './New'\n",
			want: []string{"./New"},
		},
		{name: "parent directory", src: `import Footer from '../Footer'`},
		{name: "parent directory inside", src: `import Menu from './a/../Menu'`},
		{name: "alias", src: `import Menu from '@/components/Menu'`},
		{name: "tilde alias", src: `import Menu from '~/components/Menu'`},
		{name: "package", src: `import React from 'react'`},
		{name: "scoped package", src: `import { Button } from '@acme/ui'`},
		{name: "bare directory", src: `import Menu from './'`},
		{name: "query", src: `import raw from './Menu?raw'`},
		{name: "dynamic import", src: `const Menu = lazy(() => import('./Menu'))`},
		{name: "require", src: `const Menu = require('./Menu')`},
		{name: "default export", src: "export default function NavBar() {\n  return <nav className=\"bar\" />\n}\n"},
		{name: "string holding the word import", src: `const hint = 'import it from ./here'`},
		{
			name: "mixed",
			src: "import React from 'react'\nimport { Menu } from './Menu'\nimport Footer from '../Footer'\n" +
				"import styles from './index.module.css'\nimport '@/styles.css'\n",
			want: []string{"./Menu", "./index.module.css"},
		},
		{
			name: "vue script",
			src: "<template><nav><Menu /></nav></template>\n" +
				"<script setup lang=\"ts\">\nimport Menu from './Menu.vue'\nimport { ref } from 'vue'\n</script>\n",
			want: []string{"./Menu.vue"},
		},
		{
			name: "svelte script",
			src:  "<script>\n  import Menu from './Menu.svelte';\n</script>\n<Menu />\n",
			want: []string{"./Menu.svelte"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, localImports(tt.src))
		})
	}
}

func TestImageSpecs(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		src  string
		want []string
	}{
		{name: "default import", ext: ".tsx", src: `import logo from './logo.svg'`, want: []string{"./logo.svg"}},
		{name: "import with a react query", ext: ".tsx", src: `import Logo from './logo.svg?react'`, want: []string{"./logo.svg"}},
		{name: "alias import with a url query", ext: ".ts", src: `import url from '@/img/x.svg?url'`, want: []string{"@/img/x.svg"}},
		{name: "tilde alias", ext: ".js", src: `import hero from "~/img/hero.webp"`, want: []string{"~/img/hero.webp"}},
		{name: "side-effect import", ext: ".js", src: `import './sprite.svg'`, want: []string{"./sprite.svg"}},
		{name: "namespace import", ext: ".ts", src: `import * as art from "../art/a.png"`, want: []string{"../art/a.png"}},
		{name: "require", ext: ".js", src: `const hero = require('./hero.jpg')`, want: []string{"./hero.jpg"}},
		{name: "re-export", ext: ".ts", src: `export { default as Mark } from './mark.gif'`, want: []string{"./mark.gif"}},
		{name: "fragment", ext: ".js", src: `import sprite from './sprite.svg#icon'`, want: []string{"./sprite.svg"}},
		{name: "upper-case extension", ext: ".js", src: `import a from './A.PNG'`, want: []string{"./A.PNG"}},
		{name: "avif", ext: ".js", src: `import a from './a.avif'`, want: []string{"./a.avif"}},
		{name: "jpeg", ext: ".js", src: `import a from './a.jpeg'`, want: []string{"./a.jpeg"}},
		{
			name: "not an image",
			ext:  ".tsx",
			src: "import Menu from './Menu'\nimport s from './a.css'\nimport m from './logo.svg.map'\n" +
				"import p from './logo.svgz'\nimport d from './data.json'\nimport i from './img.png/index'\n",
		},
		{name: "package", ext: ".tsx", src: `import icon from 'pkg/icon.svg'`, want: []string{"pkg/icon.svg"}},
		{name: "remote and data", ext: ".js", src: `import a from 'https://cdn.example.test/a.png'` + "\n" + `import b from 'data:image/png;base64,AAAA'`},
		{name: "dynamic import", ext: ".js", src: `const a = await import('./a.png')`},
		{name: "once each, in order", ext: ".js", src: "import b from './b.png'\nimport a from './a.png'\nimport b2 from './b.png'\n", want: []string{"./b.png", "./a.png"}},
		{name: "line comment", ext: ".js", src: "// import old from './old.png'\nimport a from './a.png'", want: []string{"./a.png"}},
		{name: "block comment", ext: ".js", src: "/* import old from './old.png' */\nimport a from './a.png'", want: []string{"./a.png"}},

		{name: "css url double quoted", ext: ".css", src: `a{background:url("../../img/bg.png")}`, want: []string{"../../img/bg.png"}},
		{name: "css url single quoted", ext: ".css", src: `a{background:url('./bg.png')}`, want: []string{"./bg.png"}},
		{name: "css url bare", ext: ".css", src: `a{background:url(/bg/wave.svg) no-repeat}`, want: []string{"/bg/wave.svg"}},
		{name: "css url padded", ext: ".css", src: `a{background:URL(  "./a.webp"  )}`, want: []string{"./a.webp"}},
		{name: "css url with a query", ext: ".css", src: `a{background:url(./f.png?v=2)}`, want: []string{"./f.png"}},
		{name: "css url of a fragment", ext: ".css", src: `a{fill:url(#grad)}`},
		{name: "css url of a font", ext: ".css", src: `@font-face{src:url(./f.woff2) format("woff2")}`},
		{name: "css url remote", ext: ".css", src: `a{background:url(//cdn.example.test/a.png)} b{background:url(http://x.test/b.png)}`},
		{name: "css url data", ext: ".css", src: `a{background:url(data:image/svg+xml;utf8,<svg/>)}`},
		{name: "css two urls", ext: ".css", src: `a{background:url(./a.png),url(./b.png)}`, want: []string{"./a.png", "./b.png"}},
		{name: "css comment", ext: ".css", src: "/* a{background:url(./old.png)} */\nb{background:url(./b.png)}", want: []string{"./b.png"}},
		{name: "css has no script imports", ext: ".css", src: `import a from './a.png'`},

		{name: "html src double quoted", ext: ".html", src: `<img src="./a.png">`, want: []string{"./a.png"}},
		{name: "html src single quoted", ext: ".html", src: `<img alt="x" src='/img/a.svg'>`, want: []string{"/img/a.svg"}},
		{name: "html script src", ext: ".html", src: `<script src="./main.js"></script>`},
		{name: "html srcset is not src", ext: ".html", src: `<img srcset="./a.png 1x">`},
		{name: "html remote", ext: ".html", src: `<img src="https://x.test/a.png"><img src="//x.test/b.png"><img src="data:image/png;base64,AA">`},
		{name: "html comment", ext: ".html", src: `<!-- <img src="./old.png"> --><img src="./a.png">`, want: []string{"./a.png"}},
		{name: "html style url", ext: ".html", src: `<div style="background:url(./a.png)"></div>`, want: []string{"./a.png"}},
		{name: "jsx src is not read", ext: ".tsx", src: `<img src="./a.png" />`},
		{
			name: "vue in source order",
			ext:  ".vue",
			src: "<template><img src=\"./b.png\"></template>\n" +
				"<script setup>\nimport a from './a.svg'\n</script>\n" +
				"<style>.hero{background:url(../c.avif)}</style>\n",
			want: []string{"./b.png", "./a.svg", "../c.avif"},
		},
		{
			name: "svelte",
			ext:  ".svelte",
			src:  "<script>\n  import logo from './logo.svg?url';\n</script>\n<img src=\"./logo.svg\" alt=\"\"><img src='./b.gif'>\n",
			want: []string{"./logo.svg", "./b.gif"},
		},
		{name: "astro", ext: ".astro", src: "---\nimport hero from '../hero.png'\n---\n<img src=\"./a.png\">\n", want: []string{"../hero.png", "./a.png"}},
		{name: "unknown extension", ext: ".md", src: `import a from './a.png'`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, imageSpecs(tt.src, tt.ext))
		})
	}
}

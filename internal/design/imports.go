package design

import (
	"cmp"
	"path"
	"regexp"
	"slices"
	"strings"
)

var (
	blockCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

	// A block comment or a line whose first non-space characters are `//`, in
	// one pass so that whichever opens first hides the other's marks.
	jsCommentRE = regexp.MustCompile(`(?ms)/\*.*?\*/|^[ \t]*//[^\n]*`)

	// Groups: 1-3 url(...) as double-quoted, single-quoted, bare; 4-5 a plain
	// string as double-quoted, single-quoted.
	cssImportRE = regexp.MustCompile(`(?i)@import\s*(?:url\(\s*(?:"([^"]*)"|'([^']*)'|([^)\s'"]*))\s*\)|"([^"]*)"|'([^']*)')`)

	// Groups: 1 double-quoted, 2 single-quoted. Requiring a quote straight
	// after `import` leaves out default, named and dynamic imports.
	jsCSSImportRE = regexp.MustCompile(`\bimport\s*(?:"([^"]+\.css)"|'([^']+\.css)')`)

	// Groups: 1 double-quoted, 2 single-quoted. A static import or re-export:
	// the binding list, if any, ends in `from` and holds no quote or semicolon,
	// which leaves out a dynamic import.
	staticImportRE = regexp.MustCompile(`\b(?:import|export)\b(?:[^;'"]*?\bfrom\s*)?\s*(?:"([^"\n]*)"|'([^'\n]*)')`)

	// Groups: 1 `type` for a type-only statement; 2 the braced list; 3-4 the
	// specifier as double-quoted, single-quoted.
	namedImportRE = regexp.MustCompile(`\bimport\b\s*(type\b\s*)?(?:[\p{L}_$][\p{L}\p{N}_$]*\s*,\s*)?\{([^{}]*)\}\s*from\s*(?:"([^"\n]+)"|'([^'\n]+)')`)

	// A trailing line comment. Only used inside a braced list, where no string
	// can hold a `//`; jsCommentRE leaves these alone because a url holds one.
	trailingCommentRE = regexp.MustCompile(`//[^\n]*`)

	schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlLinkRE    = regexp.MustCompile(`(?i)<link\b([^>]*)>`)

	// Groups: 1 the name; 2-4 the value as double-quoted, single-quoted, bare.
	htmlAttrRE = regexp.MustCompile(`([^\s"'<>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>=` + "`" + `]+)))?`)

	// Groups: 1 double-quoted, 2 single-quoted.
	requireRE = regexp.MustCompile(`\brequire\(\s*(?:"([^"\n]*)"|'([^'\n]*)')\s*\)`)

	// Groups: 1-3 as double-quoted, single-quoted, bare.
	cssURLRE = regexp.MustCompile(`(?i)\burl\(\s*(?:"([^"\n]*)"|'([^'\n]*)'|([^)\s'"]*))\s*\)`)

	// Groups: 1 double-quoted, 2 single-quoted.
	htmlSrcRE = regexp.MustCompile(`(?i)\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)')`)

	imageExts = map[string]bool{".svg": true, ".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".avif": true}

	// The syntaxes imageSpecs reads in a file, by its lower-cased extension.
	imageSyntaxes = map[string]struct{ script, style, markup bool }{
		".js": {script: true}, ".jsx": {script: true}, ".ts": {script: true}, ".tsx": {script: true},
		".css":  {style: true},
		".html": {true, true, true}, ".vue": {true, true, true}, ".svelte": {true, true, true}, ".astro": {true, true, true},
	}
)

// cssImports returns every @import specifier in css, in order, ignoring
// comments. Trailing layer(), supports() and media lists are not part of it.
func cssImports(css string) []string {
	css = blockCommentRE.ReplaceAllString(css, "")

	var specs []string

	for _, m := range cssImportRE.FindAllStringSubmatch(css, -1) {
		if spec := firstNonEmpty(m[1:]...); spec != "" {
			specs = append(specs, spec)
		}
	}

	return specs
}

// jsCSSImports returns the specifiers of side-effect imports of .css files
// (`import 'x.css'`) in src, in order.
func jsCSSImports(src string) []string {
	var specs []string

	for _, m := range jsCSSImportRE.FindAllStringSubmatch(src, -1) {
		specs = append(specs, firstNonEmpty(m[1:]...))
	}

	return specs
}

// localImports returns the specifiers of the static imports and re-exports in
// src that start with ./, in order and once each, ignoring comments. A
// specifier with a query or a .. segment is left out.
func localImports(src string) []string {
	src = jsCommentRE.ReplaceAllString(src, "")

	var specs []string

	for _, m := range staticImportRE.FindAllStringSubmatch(src, -1) {
		spec := firstNonEmpty(m[1:]...)

		rest, local := strings.CutPrefix(spec, "./")
		if !local || rest == "" || strings.Contains(spec, "?") ||
			slices.Contains(strings.Split(rest, "/"), "..") || slices.Contains(specs, spec) {
			continue
		}

		specs = append(specs, spec)
	}

	return specs
}

// namedImport is one static `import { ... } from 'Spec'` statement and the
// names it imports.
type namedImport struct {
	Spec  string
	Names []string
}

// namedImports returns, in source order, a namedImport for each static import
// in src that lists at least one name, ignoring comments. The name of `a as b`
// is `a`; `default` and `type x` entries and type-only statements are left out,
// and each name is listed once.
func namedImports(src string) []namedImport {
	src = blank(jsCommentRE, src)

	var imports []namedImport

	for _, m := range namedImportRE.FindAllStringSubmatch(src, -1) {
		if m[1] != "" {
			continue
		}

		var names []string

		for _, entry := range strings.Split(trailingCommentRE.ReplaceAllString(m[2], ""), ",") {
			fields := strings.Fields(entry)
			if len(fields) == 0 || fields[0] == "default" ||
				len(fields) > 1 && fields[0] == "type" && fields[1] != "as" ||
				slices.Contains(names, fields[0]) {
				continue
			}

			names = append(names, fields[0])
		}

		if len(names) > 0 {
			imports = append(imports, namedImport{Spec: firstNonEmpty(m[3:]...), Names: names})
		}
	}

	return imports
}

// htmlStylesheets returns the href of each <link> in src whose rel holds the
// token stylesheet and not alternate, in document order, ignoring comments.
func htmlStylesheets(src string) []string {
	src = htmlCommentRE.ReplaceAllString(src, "")

	var hrefs []string

	for _, link := range htmlLinkRE.FindAllStringSubmatch(src, -1) {
		attrs := map[string]string{}

		for _, a := range htmlAttrRE.FindAllStringSubmatch(link[1], -1) {
			// The first of a repeated attribute counts, as in a browser.
			name := strings.ToLower(a[1])
			if _, dup := attrs[name]; !dup {
				attrs[name] = firstNonEmpty(a[2:]...)
			}
		}

		// An alternate stylesheet is not applied until a user picks it.
		rel := strings.Fields(strings.ToLower(attrs["rel"]))
		if attrs["href"] != "" && slices.Contains(rel, "stylesheet") && !slices.Contains(rel, "alternate") {
			hrefs = append(hrefs, attrs["href"])
		}
	}

	return hrefs
}

// splitBare splits a package specifier into the package name (including its
// scope) and the path inside the package. ok is false for anything that is not
// a package: relative and absolute paths, urls, a bare .css file name, which
// is a sibling file, and a scope or name left empty, as in a bundler's "@/"
// alias.
func splitBare(spec string) (name, subpath string, ok bool) {
	spec = strings.TrimPrefix(spec, "~")

	if spec == "" || spec[0] == '.' || spec[0] == '/' || schemeRE.MatchString(spec) {
		return "", "", false
	}

	if !strings.Contains(spec, "/") && strings.HasSuffix(spec, ".css") {
		return "", "", false
	}

	segments := 1
	if strings.HasPrefix(spec, "@") {
		segments = 2
	}

	parts := strings.SplitN(spec, "/", segments+1)
	if len(parts) < segments || parts[segments-1] == "" || parts[0] == "@" {
		return "", "", false
	}

	name = strings.Join(parts[:segments], "/")
	if len(parts) > segments {
		subpath = parts[segments]
	}

	return name, subpath, true
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}

	return ""
}

// imageSpecs returns, in source order and once each, the specifiers in src, a
// file with the lower-cased extension ext, of an image: a script's import or
// require, a style's url() and a markup file's src attribute, each less any
// query or fragment. A remote, data or protocol-relative specifier is left out.
func imageSpecs(src, ext string) []string {
	syntax := imageSyntaxes[strings.ToLower(ext)]

	type hit struct {
		at   int
		spec string
	}

	var hits []hit

	add := func(re *regexp.Regexp, text string) {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			for g := 2; g+1 < len(m); g += 2 {
				if m[g] >= 0 {
					hits = append(hits, hit{m[g], text[m[g]:m[g+1]]})

					break
				}
			}
		}
	}

	if syntax.script {
		text := blank(jsCommentRE, src)

		add(staticImportRE, text)
		add(requireRE, text)
	}

	if syntax.style {
		add(cssURLRE, blank(blockCommentRE, src))
	}

	if syntax.markup {
		add(htmlSrcRE, blank(htmlCommentRE, src))
	}

	slices.SortStableFunc(hits, func(a, b hit) int { return cmp.Compare(a.at, b.at) })

	var specs []string

	for _, h := range hits {
		spec, _, _ := strings.Cut(h.spec, "?")
		spec, _, _ = strings.Cut(spec, "#")

		if spec == "" || strings.HasPrefix(spec, "//") || schemeRE.MatchString(spec) ||
			!imageExts[strings.ToLower(path.Ext(spec))] || slices.Contains(specs, spec) {
			continue
		}

		specs = append(specs, spec)
	}

	return specs
}

// blank replaces each match of re in s with spaces, line breaks kept, so
// offsets in the result are offsets in s.
func blank(re *regexp.Regexp, s string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		out := []byte(m)
		for i, b := range out {
			if b != '\n' {
				out[i] = ' '
			}
		}

		return string(out)
	})
}

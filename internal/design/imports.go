package design

import (
	"regexp"
	"slices"
	"strings"
)

var (
	blockCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

	// A line whose first non-space characters are `//`.
	lineCommentRE = regexp.MustCompile(`(?m)^[ \t]*//.*$`)

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

	schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)

	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlLinkRE    = regexp.MustCompile(`(?i)<link\b([^>]*)>`)

	// Groups: 1 the name; 2-4 the value as double-quoted, single-quoted, bare.
	htmlAttrRE = regexp.MustCompile(`([^\s"'<>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'<>=` + "`" + `]+)))?`)
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
	src = lineCommentRE.ReplaceAllString(blockCommentRE.ReplaceAllString(src, ""), "")

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

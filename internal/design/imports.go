package design

import (
	"regexp"
	"strings"
)

var (
	cssCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

	// Groups: 1-3 url(...) as double-quoted, single-quoted, bare; 4-5 a plain
	// string as double-quoted, single-quoted.
	cssImportRE = regexp.MustCompile(`(?i)@import\s*(?:url\(\s*(?:"([^"]*)"|'([^']*)'|([^)\s'"]*))\s*\)|"([^"]*)"|'([^']*)')`)

	// Groups: 1 double-quoted, 2 single-quoted. Requiring a quote straight
	// after `import` leaves out default, named and dynamic imports.
	jsCSSImportRE = regexp.MustCompile(`\bimport\s*(?:"([^"]+\.css)"|'([^']+\.css)')`)

	schemeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:`)
)

// cssImports returns every @import specifier in css, in order, ignoring
// comments. Trailing layer(), supports() and media lists are not part of it.
func cssImports(css string) []string {
	css = cssCommentRE.ReplaceAllString(css, "")

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

// splitBare splits a package specifier into the package name (including its
// scope) and the path inside the package. ok is false for anything that is not
// a package: relative and absolute paths, urls, and a bare .css file name,
// which is a sibling file.
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
	if len(parts) < segments {
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

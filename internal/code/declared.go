package code

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Names are what a checkout declares of its own: compound identifiers in
// declaration position and the module paths its root manifests name. A
// nested manifest's module (a workspace package, a test fixture) names a
// part of the checkout, not the checkout, so its name is an identifier, and
// the packages the checkout depends on are not identifiers. The leak guard
// keeps them out of web queries.
type Names struct {
	Identifiers []string
	Modules     []string
}

// maxDeclaredFile skips a larger file: generated or vendored data, not
// declarations.
const maxDeclaredFile = 1 << 20

// skippedDirs hold code the project does not declare, or no code.
var skippedDirs = map[string]bool{
	"vendor": true, "node_modules": true, "third_party": true, "dist": true,
	"build": true, "target": true, "venv": true, "__pycache__": true,
}

// declaredExts are the code and configuration files read; documentation is
// not.
var declaredExts = map[string]bool{
	".go": true, ".py": true, ".pyi": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".mjs": true, ".cjs": true, ".rb": true, ".rs": true, ".java": true, ".kt": true, ".kts": true,
	".scala": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".cs": true,
	".swift": true, ".php": true, ".sh": true, ".bash": true, ".zsh": true, ".sql": true, ".proto": true,
	".yaml": true, ".yml": true, ".json": true, ".toml": true, ".ini": true, ".cfg": true, ".conf": true,
	".env": true, ".xml": true, ".tf": true, ".tfvars": true, ".hcl": true, ".gradle": true, ".vue": true,
	".svelte": true, ".ex": true, ".exs": true, ".erl": true, ".hs": true, ".lua": true, ".dart": true,
	".zig": true, ".clj": true, ".groovy": true, ".properties": true, ".graphql": true, ".gql": true,
}

var declaredNames = map[string]bool{"Dockerfile": true, "Makefile": true, "go.mod": true}

// lockFiles name every package a build resolves, the checkout's own and its
// dependencies' alike.
var lockFiles = map[string]bool{
	"package-lock.json": true, "npm-shrinkwrap.json": true, "yarn.lock": true, "pnpm-lock.yaml": true,
	"Cargo.lock": true, "poetry.lock": true, "Pipfile.lock": true, "composer.lock": true, "Gemfile.lock": true,
	"go.sum": true,
}

var (
	keywordDecl = regexp.MustCompile(`\b(?:func|type|var|const|def|class|struct|interface|enum|trait|fn|let|val|object|record|module|namespace|package|function|message|service|alias)\s+(?:\([^)]*\)\s*)?\*?([A-Za-z_]\w*(?:\.[A-Za-z_]\w*)*)`)
	keyDecl     = regexp.MustCompile(`^\s*(?:export\s+)?["']?([A-Za-z_][\w.-]*)["']?\s*[:=]`)
	tagDecl     = regexp.MustCompile(`\b(?:json|yaml|koanf|toml|mapstructure|env|db|xml|form|query)\s*:\s*"([A-Za-z_][\w.-]*)`)
	memberDecl  = regexp.MustCompile(`\b(?:private|public|protected|internal|static|final|readonly|override)\s+(?:[\w<>\[\],.?]+\s+)*([A-Za-z_]\w*)\s*[=;:(]`)
	goField     = regexp.MustCompile(`^\s+([A-Z]\w*)\s+[*\[\]\w]`)
	// compound has two parts at least: a case change or an inner _ . -.
	compound     = regexp.MustCompile(`[a-z0-9][A-Z]|[A-Z]{2}[a-z]|[A-Za-z0-9][_.-][A-Za-z0-9]`)
	goModule     = regexp.MustCompile(`^module\s+(\S+)`)
	manifestName = regexp.MustCompile(`^\s*"?name"?\s*[:=]\s*"([^"]+)"`)
	goMajor      = regexp.MustCompile(`^v[0-9]+$`)
	tomlTable    = regexp.MustCompile(`^\[\[?\s*([A-Za-z_][^\[\]]*?)\s*\]\]?\s*(?:#.*)?$`)
	tomlKey      = regexp.MustCompile(`^["']?([A-Za-z0-9_][\w.-]*)["']?\s*=`)
	poetryTable  = regexp.MustCompile(`^tool\.poetry\.(?:dependencies|dev-dependencies|group\.[^.]+\.dependencies)$`)
)

// Declared reads the names dir declares. It skips hidden directories,
// vendored and built trees, symbolic links, documentation, lockfiles, files
// over maxDeclaredFile and binary files.
func Declared(ctx context.Context, dir string) (Names, error) {
	c := collected{ids: map[string]bool{}, mods: map[string]bool{}, own: map[string]bool{}, deps: map[string]bool{}}
	root := filepath.Clean(dir)

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		name := d.Name()

		switch {
		case d.IsDir() && path != dir && (strings.HasPrefix(name, ".") || skippedDirs[name]):
			return filepath.SkipDir
		case d.IsDir(), !d.Type().IsRegular(): // a symbolic link is not regular
			return nil
		case lockFiles[name]:
			return nil
		case !declaredExts[strings.ToLower(filepath.Ext(name))] && !declaredNames[name]:
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.Size() > maxDeclaredFile {
			return nil
		}

		data, err := os.ReadFile(path) //nolint:gosec // a regular file the walk found under the checkout
		if err != nil {
			return err
		}

		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			return nil
		}

		c.declare(name, data, filepath.Dir(path) == root)

		return nil
	})
	if err != nil {
		return Names{}, fmt.Errorf("code: read the names %s declares: %w", dir, err)
	}

	// A dependency's name is a key in a manifest, not a name the checkout
	// declares. A nested package another one depends on is the checkout's
	// own, so its name is added after.
	for d := range c.deps {
		delete(c.ids, d)
	}

	maps.Copy(c.ids, c.own)

	return Names{Identifiers: slices.Sorted(maps.Keys(c.ids)), Modules: slices.Sorted(maps.Keys(c.mods))}, nil
}

// collected is what Declared has read so far: identifiers, the root
// manifests' modules, the nested manifests' names as identifiers, and the
// names of the packages the checkout depends on.
type collected struct {
	ids, mods, own, deps map[string]bool
}

// declare reads one file; root says it sits at the checkout's root.
func (c collected) declare(name string, data []byte, root bool) {
	isGo := strings.HasSuffix(name, ".go")
	manifest := name == "package.json" || name == "Cargo.toml" || name == "pyproject.toml"

	c.dependencies(name, data)

	for line := range bytes.Lines(data) {
		s := string(line)

		if name == "go.mod" {
			if m := goModule.FindStringSubmatch(s); m != nil {
				c.module(m[1], root)
			}

			continue
		}

		// A manifest's first name is the package's own; later ones are an
		// author's or a dependency's.
		if manifest {
			if m := manifestName.FindStringSubmatch(s); m != nil {
				c.module(m[1], root)

				manifest = false
			}
		}

		for _, re := range []*regexp.Regexp{keywordDecl, tagDecl, memberDecl} {
			for _, m := range re.FindAllStringSubmatch(s, -1) {
				addIdentifier(c.ids, m[1])
			}
		}

		if m := keyDecl.FindStringSubmatch(s); m != nil {
			addIdentifier(c.ids, m[1])
		}

		if isGo {
			if m := goField.FindStringSubmatch(s); m != nil {
				addIdentifier(c.ids, m[1])
			}
		}
	}
}

// module records a manifest's module path or package name: a root
// manifest's as a module, a nested one's own name as an identifier, so a
// compound name (billing-core) is refused and a single word (web, test) is
// not.
func (c collected) module(m string, root bool) {
	if root {
		c.mods[m] = true

		return
	}

	// The last segment that is not a major version: example.com/acme/x/v2
	// gives x, @acme/billing-core gives billing-core.
	segs := strings.Split(m, "/")
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] != "" && !goMajor.MatchString(segs[i]) {
			addIdentifier(c.own, segs[i])

			return
		}
	}
}

// dependencies records the packages a manifest depends on: the keys of
// package.json's dependency objects, the keys of a Cargo.toml table whose
// header ends in dependencies and the crate a [dependencies.<crate>] header
// names, the keys of pyproject.toml's poetry dependency tables, and the
// names in Chart.yaml's dependencies list. A manifest that does not parse
// depends on nothing.
func (c collected) dependencies(name string, data []byte) {
	switch name {
	case "package.json":
		var pkg struct {
			Dependencies         map[string]json.RawMessage `json:"dependencies"`
			DevDependencies      map[string]json.RawMessage `json:"devDependencies"`
			PeerDependencies     map[string]json.RawMessage `json:"peerDependencies"`
			OptionalDependencies map[string]json.RawMessage `json:"optionalDependencies"`
		}

		if json.Unmarshal(data, &pkg) != nil {
			return
		}

		for _, deps := range []map[string]json.RawMessage{pkg.Dependencies, pkg.DevDependencies, pkg.PeerDependencies, pkg.OptionalDependencies} {
			for d := range deps {
				c.deps[d] = true
			}
		}
	case "Cargo.toml":
		c.tomlDependencies(data, func(table string) bool { return strings.HasSuffix(table, "dependencies") }, true)
	case "pyproject.toml":
		c.tomlDependencies(data, poetryTable.MatchString, false)
	case "Chart.yaml":
		var chart struct {
			Dependencies []struct {
				Name string `yaml:"name"`
			} `yaml:"dependencies"`
		}

		if yaml.Unmarshal(data, &chart) != nil {
			return
		}

		for _, d := range chart.Dependencies {
			if d.Name != "" {
				c.deps[d.Name] = true
			}
		}
	}
}

// tomlDependencies records the keys under each table isDeps accepts and,
// when named, the last part of a header whose table before it isDeps
// accepts. A dotted key (serde.workspace) depends on its first part. Any
// line starting with [ ends a dependency table unless it parses as one, so
// a header tomlTable cannot read keeps its keys identifiers.
func (c collected) tomlDependencies(data []byte, isDeps func(table string) bool, named bool) {
	inDeps := false

	for line := range bytes.Lines(data) {
		s := strings.TrimSpace(string(line))

		if strings.HasPrefix(s, "[") {
			inDeps = false

			if m := tomlTable.FindStringSubmatch(s); m != nil {
				table := m[1]
				inDeps = isDeps(table)

				if i := strings.LastIndexByte(table, '.'); named && i > 0 && isDeps(table[:i]) {
					c.deps[strings.Trim(table[i+1:], `"' `)] = true
				}
			}

			continue
		}

		if m := tomlKey.FindStringSubmatch(s); inDeps && m != nil {
			c.deps[m[1]] = true
			first, _, _ := strings.Cut(m[1], ".")
			c.deps[first] = true
		}
	}
}

// addIdentifier keeps a compound name of five characters or more and, for
// a dotted one, each compound part: self.max_retries declares max_retries
// too.
func addIdentifier(ids map[string]bool, s string) {
	keep := func(x string) {
		if len(x) >= 5 && compound.MatchString(x) {
			ids[x] = true
		}
	}

	s = strings.Trim(s, ".-")
	keep(s)

	if strings.Contains(s, ".") {
		for part := range strings.SplitSeq(s, ".") {
			keep(part)
		}
	}
}

package design

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const defaultRegistry = "https://registry.npmjs.org"

// errEnvRegistry means the registry url holds a ${VAR} reference, which the
// architect cannot expand.
var errEnvRegistry = errors.New("design: registry url uses an environment variable")

// registries holds the default registry and the per-scope registries (keyed by
// the scope without "@"). Only registry urls are ever read from the config
// files, which come from a repository the architect does not control.
type registries struct {
	def    string
	scopes map[string]string
}

// readRegistries reads the registry config from rootDir and each parent up to
// treeDir, never above it. Per kind of file the nearest value wins; a scope
// registry comes from .yarnrc.yml, else .npmrc, and the default from
// .yarnrc.yml, else .npmrc, else a classic .yarnrc, else the public registry.
func readRegistries(treeDir, rootDir string) (registries, error) {
	treeDir, rootDir = filepath.Clean(treeDir), filepath.Clean(rootDir)

	rel, err := filepath.Rel(treeDir, rootDir)
	if err != nil || !filepath.IsLocal(rel) {
		return registries{}, fmt.Errorf("design: registry root %s is outside the tree", rootDir)
	}

	var dirs []string

	for dir := rootDir; ; dir = filepath.Dir(dir) {
		dirs = append(dirs, dir)

		if dir == treeDir {
			break
		}
	}

	kinds := []struct {
		file  string
		parse func([]byte) (registries, error)
	}{
		{".yarnrc.yml", parseYarnrcYML},
		{".npmrc", func(b []byte) (registries, error) { return parseNPMRC(b), nil }},
		{".yarnrc", func(b []byte) (registries, error) { return parseYarnrc(b), nil }},
	}

	merged := registries{scopes: map[string]string{}}

	for _, kind := range kinds {
		found := registries{scopes: map[string]string{}}

		for _, dir := range dirs {
			regs, err := readRegistryFile(filepath.Join(dir, kind.file), kind.parse)
			if err != nil {
				return registries{}, err
			}

			found.fill(regs)
		}

		merged.fill(found)
	}

	if merged.def == "" {
		merged.def = defaultRegistry
	}

	return merged, nil
}

// readRegistryFile ignores a missing file and anything that is not a regular
// file, so a symlink cannot lead the read out of the repository tree.
func readRegistryFile(path string, parse func([]byte) (registries, error)) (registries, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return registries{}, nil
	}

	if err != nil {
		return registries{}, fmt.Errorf("design: stat %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return registries{}, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return registries{}, fmt.Errorf("design: read %s: %w", path, err)
	}

	regs, err := parse(data)
	if err != nil {
		// The cause is dropped: a decoder error can quote the offending value,
		// and the file holds credentials.
		return registries{}, fmt.Errorf("design: parse %s: not a valid registry config", path)
	}

	return regs, nil
}

// fill adds the values of o that r does not have yet.
func (r *registries) fill(o registries) {
	if r.def == "" {
		r.def = o.def
	}

	for scope, registry := range o.scopes {
		if _, ok := r.scopes[scope]; !ok {
			r.scopes[scope] = registry
		}
	}
}

func parseYarnrcYML(data []byte) (registries, error) {
	var doc struct {
		Server string `yaml:"npmRegistryServer"`
		Scopes map[string]struct {
			Server string `yaml:"npmRegistryServer"`
		} `yaml:"npmScopes"`
	}

	if err := yaml.Unmarshal(data, &doc); err != nil {
		return registries{}, errors.New("invalid yaml")
	}

	regs := registries{def: strings.TrimSpace(doc.Server), scopes: map[string]string{}}

	for scope, entry := range doc.Scopes {
		if server := strings.TrimSpace(entry.Server); server != "" {
			regs.scopes[scope] = server
		}
	}

	return regs, nil
}

func parseNPMRC(data []byte) registries {
	regs := registries{scopes: map[string]string{}}

	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == ';' || line[0] == '#' {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		value = strings.TrimSpace(value)

		if !ok || value == "" {
			continue
		}

		key = strings.TrimSpace(key)

		if key == "registry" {
			regs.def = value
		} else if scope, ok := strings.CutSuffix(key, ":registry"); ok && len(scope) > 1 && scope[0] == '@' {
			regs.scopes[scope[1:]] = value
		}
	}

	return regs
}

func parseYarnrc(data []byte) registries {
	regs := registries{scopes: map[string]string{}}

	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "registry" {
			continue
		}

		if value := strings.Trim(fields[1], `"`); value != "" {
			regs.def = value
		}
	}

	return regs
}

// forPackage returns the registry url that serves name.
func (r registries) forPackage(name string) (string, error) {
	registry := r.def

	if scope, _, ok := strings.Cut(name, "/"); ok && strings.HasPrefix(scope, "@") {
		if scoped, ok := r.scopes[scope[1:]]; ok {
			registry = scoped
		}
	}

	if strings.Contains(registry, "${") {
		return "", errEnvRegistry
	}

	return registry, nil
}

// tarballURL is the conventional registry location of a package tarball.
func tarballURL(registry, name, version string) string {
	unscoped := name[strings.LastIndex(name, "/")+1:]

	return strings.TrimRight(registry, "/") + "/" + name + "/-/" + unscoped + "-" + version + ".tgz"
}

// pickURL prefers the url the lockfile recorded, but only on the registry's own
// host: a lockfile must not steer the download to another one.
func pickURL(registry string, l locked) string {
	reg, err := url.Parse(registry)
	if err != nil || reg.Host == "" {
		return tarballURL(registry, l.Name, l.Version)
	}

	resolved, _, _ := strings.Cut(l.Resolved, "#")

	if u, err := url.Parse(resolved); err == nil && strings.EqualFold(u.Host, reg.Host) {
		return resolved
	}

	return tarballURL(registry, l.Name, l.Version)
}

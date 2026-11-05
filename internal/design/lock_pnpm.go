package design

import (
	"fmt"
	"os"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// pnpmLock reads lockfile versions 6 to 9. A v6 package lives in packages and
// carries its dependencies; v9 splits them into packages (resolution) and
// snapshots (dependencies, keyed with the peer suffix).
type pnpmLock struct {
	importers map[string]pnpmImporter
	packages  map[string]pnpmPackage
	snapshots map[string]pnpmPackage
}

type pnpmImporter struct {
	Dependencies         map[string]pnpmImporterDep `yaml:"dependencies"`
	DevDependencies      map[string]pnpmImporterDep `yaml:"devDependencies"`
	OptionalDependencies map[string]pnpmImporterDep `yaml:"optionalDependencies"`
}

type pnpmImporterDep struct {
	Version string `yaml:"version"`
}

type pnpmPackage struct {
	Resolution struct {
		Integrity string `yaml:"integrity"`
		Tarball   string `yaml:"tarball"`
		Type      string `yaml:"type"`
		Directory string `yaml:"directory"`
	} `yaml:"resolution"`
	Dependencies map[string]string `yaml:"dependencies"`
}

func readPNPMLock(file string) (lockfile, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("design: read pnpm-lock.yaml: %w", err)
	}

	var doc struct {
		// A v6 lockfile of a single project has no importers; its one importer
		// is the document itself.
		Root      pnpmImporter            `yaml:",inline"`
		Importers map[string]pnpmImporter `yaml:"importers"`
		Packages  map[string]pnpmPackage  `yaml:"packages"`
		Snapshots map[string]pnpmPackage  `yaml:"snapshots"`
	}

	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("design: parse pnpm-lock.yaml: %w", err)
	}

	if _, ok := doc.Importers["."]; !ok {
		if doc.Importers == nil {
			doc.Importers = map[string]pnpmImporter{}
		}

		doc.Importers["."] = doc.Root
	}

	return &pnpmLock{importers: doc.Importers, packages: doc.Packages, snapshots: doc.Snapshots}, nil
}

func (l *pnpmLock) fromRoot(dir, name, _ string) (locked, bool) {
	importer := l.importers[path.Clean(dir)]

	for _, deps := range []map[string]pnpmImporterDep{
		importer.Dependencies, importer.DevDependencies, importer.OptionalDependencies,
	} {
		if dep, ok := deps[name]; ok {
			return l.resolve(name, dep.Version)
		}
	}

	return locked{}, false
}

func (l *pnpmLock) fromPackage(parent locked, name string) (locked, bool) {
	pkg, ok := l.snapshots[parent.pos]
	if !ok {
		pkg = l.packages[parent.pos]
	}

	version, ok := pkg.Dependencies[name]
	if !ok {
		return locked{}, false
	}

	return l.resolve(name, version)
}

func (*pnpmLock) checksummed() bool { return true }

// resolve finds the package a dependency named name records value for: a
// version, or for an alias "<real name>@<version>" (v6 prefixes it with "/").
func (l *pnpmLock) resolve(name, value string) (locked, bool) {
	// full keeps the peer suffix: snapshots are keyed by it.
	full := strings.TrimPrefix(value, "/")
	version, _, _ := strings.Cut(full, "(")

	if strings.HasPrefix(version, "link:") || strings.HasPrefix(version, "file:") {
		return locked{Name: name, Local: true}, true
	}

	if n := yarnDescriptorName(version); n != "" {
		name = n
		full = full[len(n)+1:]
		version = version[len(n)+1:]
	}

	pkg, ok := l.packages[name+"@"+version]
	pos := name + "@" + full

	if !ok {
		pos = "/" + pos

		if pkg, ok = l.packages[pos]; !ok {
			return locked{}, false
		}
	}

	// Only a registry package is told apart from its name and version; git,
	// tarball and directory packages are some other package.
	res := pkg.Resolution

	switch {
	case res.Tarball != "" || res.Type != "" || res.Directory != "":
		return locked{Name: name, Version: version, NotRegistry: true}, true
	case res.Integrity == "":
		return locked{}, false
	}

	return locked{Name: name, Version: version, Integrity: res.Integrity, pos: pos}, true
}

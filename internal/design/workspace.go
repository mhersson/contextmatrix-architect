package design

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// treePackage is a package.json found in a tree: its directory, tree-relative,
// and its manifest.
type treePackage struct {
	dir      string
	manifest map[string]any
}

// treePackages returns the package.json files of the tree at treeDir by
// package name, found once per Gather. A manifest that does not parse is left
// out.
func (s *gathering) treePackages(treeDir string) (map[string][]treePackage, error) {
	if pkgs, ok := s.treePkgs[treeDir]; ok {
		return pkgs, nil
	}

	pkgs := map[string][]treePackage{}

	err := walkFiles(treeDir, "", false, func(rel string, d fs.DirEntry) error {
		if d.Name() != "package.json" {
			return nil
		}

		data, err := os.ReadFile(filepath.Join(treeDir, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}

		var manifest map[string]any
		if json.Unmarshal(data, &manifest) != nil {
			return nil
		}

		if name, _ := manifest["name"].(string); name != "" {
			pkgs[name] = append(pkgs[name], treePackage{dir: parentDir(rel), manifest: manifest})
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("design: gather: find workspace packages: %w", err)
	}

	s.treePkgs[treeDir] = pkgs

	return pkgs, nil
}

// workspace reads the CSS file e imports from name, a package found in the tree
// by its package.json name, and walks it like a relative import, or gaps e.
// Package imports in its files resolve through its own package.json.
func (s *gathering) workspace(ctx context.Context, w *rootWalk, e entry, name string) error {
	treeDir := w.own.treeDir

	pkgs, err := s.treePackages(treeDir)
	if err != nil {
		return err
	}

	// Gap paths are tool-root-relative, as the brief's are.
	at := func(rel string) string { return path.Join(w.tree, rel) }

	found := pkgs[name]
	if len(found) != 1 {
		manifests := make([]string, len(found))
		for i, p := range found {
			manifests[i] = at(path.Join(p.dir, "package.json"))
		}

		reason := "is a package in this repository, but no package.json in it is named " + name

		if len(found) > 1 {
			slices.Sort(manifests)
			reason = "more than one package.json in the repository is named " + name + ": " + strings.Join(manifests, ", ")
		}

		s.gap(w.path, e.Spec, reason)

		return nil
	}

	p := found[0]
	files := &pkgFiles{manifest: p.manifest, onDisk: func(rel string) bool {
		return path.Ext(rel) == ".css" && filepath.IsLocal(rel) && isTreeFile(treeDir, path.Join(p.dir, rel))
	}}

	manifest := at(path.Join(p.dir, "package.json"))

	file, ok := files.resolve(e.Subpath)
	if !ok {
		s.gap(w.path, e.Spec, manifest+" resolves no CSS file for "+cmp.Or(e.Subpath, "."))

		return nil
	}

	rel := path.Join(p.dir, file)
	if err := w.own.load(rel); err != nil {
		s.gap(w.path, e.Spec, at(rel)+" could not be read: "+reasonUnreadable())

		return nil
	}

	deps := manifestDeps(p.manifest)

	return w.walk(rel, func(b entry) error {
		// relDir needs the package under the lockfile's directory.
		if w.lockDir != "" && p.dir != w.lockDir && !strings.HasPrefix(p.dir, w.lockDir+"/") {
			s.gap(w.path, b.Spec, "the lockfile in "+at(w.lockDir)+" does not cover "+manifest)

			return nil
		}

		return s.declaredImport(ctx, w, b, deps, relDir(w.lockDir, p.dir), manifest)
	})
}

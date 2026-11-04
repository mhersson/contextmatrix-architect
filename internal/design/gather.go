package design

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// maxTokenNames bounds the custom property names one root resolves.
const maxTokenNames = 2000

// Tree is a repository checked out at Dir, its top-level directory under the
// tool root.
type Tree struct{ Project, Dir string }

// Request names the trees to gather from. Root is the blueprint's tool root;
// kept packages are written directly under it.
type Request struct {
	Root  string
	Trees []Tree
}

// Material is the design material of the requested trees. Every path in it is
// relative to the tool root and slash-separated.
type Material struct {
	Roots    []Root    // by Project, then Path
	Packages []Package // by Dir
	Gaps     []Gap
}

// Root is one frontend root of a tree.
type Root struct {
	Project, Path string // Path is tool-root-relative, e.g. "portal/frontend"
	Frame         []string
	Brand         []Asset
	Tokens        []Token
	Fonts         []Font
	Packages      []string // Dir of each package this root's CSS reaches
}

// Package is a kept package: the CSS the project imports from it, reflowed,
// under Dir.
type Package struct {
	Name, Version, Dir, Registry string // Registry is the url's host
	Verified                     bool   // the archive matched the lockfile's checksum
	Files                        []string
}

// Gap is design material that could not be had. Root is the Path of the root
// it belongs to, Import the import spec as written.
type Gap struct{ Root, Import, Reason string }

// gathering is the state of one Gather call.
type gathering struct {
	g        *Gatherer
	root     string
	pkgs     map[string]*keptPkg // by dir
	order    []*keptPkg          // in the order first reached
	gaps     []Gap
	seenGaps map[Gap]bool
}

// rootWalk is one frontend root's walk through the packages its CSS imports.
type rootWalk struct {
	path    string // Root.Path
	lock    lockfile
	lockGap string // why lock cannot be used; "" when it can
	regs    registries
	regErr  error
	starts  map[string]bool // "<pkg dir>/<file>" of each start file walked
	seen    map[string]bool // "<pkg dir>/<file>" of each file in decls
	reached map[string]bool // pkg dirs
	decls   []decl
}

// Gather collects the design material of every frontend root in req.Trees and
// writes the package CSS those roots import under req.Root. What cannot be had
// is a Gap; only a local read or write failure or a cancelled ctx is an error.
func (g *Gatherer) Gather(ctx context.Context, req Request) (Material, error) {
	s := &gathering{g: g, root: req.Root, pkgs: map[string]*keptPkg{}, seenGaps: map[Gap]bool{}}

	var m Material

	for _, tree := range req.Trees {
		roots, err := s.tree(ctx, tree)
		if err != nil {
			return Material{}, err
		}

		m.Roots = append(m.Roots, roots...)
	}

	pkgs, err := s.write(ctx)
	if err != nil {
		return Material{}, err
	}

	kept := map[string]bool{}
	for _, p := range pkgs {
		kept[p.Dir] = true
	}

	for i := range m.Roots {
		m.Roots[i].Packages = slices.DeleteFunc(m.Roots[i].Packages, func(dir string) bool { return !kept[dir] })
	}

	slices.SortFunc(m.Roots, func(a, b Root) int {
		return cmp.Or(strings.Compare(a.Project, b.Project), strings.Compare(a.Path, b.Path))
	})
	slices.SortFunc(pkgs, func(a, b Package) int { return strings.Compare(a.Dir, b.Dir) })

	m.Packages = pkgs
	m.Gaps = s.gaps

	return m, nil
}

func (s *gathering) tree(ctx context.Context, tree Tree) ([]Root, error) {
	treeDir := filepath.Join(s.root, filepath.FromSlash(tree.Dir))

	fronts, err := scanTree(treeDir)
	if err != nil {
		return nil, fmt.Errorf("design: gather %s: %w", tree.Dir, err)
	}

	if len(fronts) == 0 {
		return nil, nil
	}

	lock, err := readLockfile(treeDir)

	var lockGap string

	switch {
	case err != nil:
		slog.Warn("design: read lockfile", "tree", tree.Dir, "error", err)

		lockGap = "the lockfile could not be read"
	case lock == nil:
		lockGap = "no lockfile in the repository"
	}

	roots := make([]Root, 0, len(fronts))

	for _, f := range fronts {
		w := &rootWalk{
			path:    path.Join(filepath.ToSlash(tree.Dir), f.Dir),
			lock:    lock,
			lockGap: lockGap,
			starts:  map[string]bool{},
			seen:    map[string]bool{},
			reached: map[string]bool{},
		}

		root, err := s.frontend(ctx, tree, treeDir, w, f)
		if err != nil {
			return nil, err
		}

		roots = append(roots, root)
	}

	return roots, nil
}

func (s *gathering) frontend(ctx context.Context, tree Tree, treeDir string, w *rootWalk, f frontend) (Root, error) {
	treePrefix := filepath.ToSlash(tree.Dir)
	root := Root{Project: tree.Project, Path: w.path}

	for _, rel := range frameFiles(treeDir, f) {
		root.Frame = append(root.Frame, path.Join(treePrefix, rel))
	}

	for _, a := range brandFiles(treeDir, f) {
		a.Path = path.Join(treePrefix, a.Path)
		root.Brand = append(root.Brand, a)
	}

	rootDir := filepath.Join(treeDir, filepath.FromSlash(f.Dir))

	w.regs, w.regErr = readRegistries(treeDir, rootDir)
	if w.regErr != nil {
		slog.Warn("design: read registry config", "root", w.path, "error", w.regErr)
	}

	manifest := path.Join(w.path, "package.json")
	deps := declaredDeps(filepath.Join(rootDir, "package.json"))

	for _, e := range f.Entries {
		rangeSpec, ok := deps[e.Name]
		if !ok {
			s.gap(w.path, e.Spec, "not declared in "+manifest)

			continue
		}

		if w.lockGap != "" {
			s.gap(w.path, e.Spec, w.lockGap)

			continue
		}

		l, ok := w.lock.fromRoot(f.Dir, e.Name, rangeSpec)
		if !ok {
			s.gap(w.path, e.Spec, "the lockfile has no entry for it")

			continue
		}

		if err := s.visit(ctx, w, l, e.Spec, e.Subpath); err != nil {
			return Root{}, err
		}
	}

	if err := ownMaterial(treeDir, w, f, &root); err != nil {
		return Root{}, err
	}

	root.Packages = slices.Sorted(maps.Keys(w.reached))

	return root, nil
}

// ownMaterial adds the root's own CSS declarations after the package ones and
// resolves the tokens the root's own CSS and sources reference.
func ownMaterial(treeDir string, w *rootWalk, f frontend, root *Root) error {
	var (
		ownCSS []string
		names  []string
	)

	for _, rel := range slices.Sorted(slices.Values(f.CSS)) {
		css, err := readTreeFile(treeDir, rel)
		if err != nil {
			return err
		}

		ownCSS = append(ownCSS, css)
		w.decls = append(w.decls, rootDecls(css)...)
		names = append(names, references(css)...)
	}

	for _, rel := range f.Sources {
		src, err := readTreeFile(treeDir, rel)
		if err != nil {
			return err
		}

		names = append(names, references(src)...)
	}

	slices.Sort(names)
	names = slices.Compact(names)

	root.Tokens = resolveTokens(names[:min(len(names), maxTokenNames)], w.decls)
	root.Fonts = fonts(w.decls, ownCSS)

	return nil
}

func readTreeFile(treeDir, rel string) (string, error) {
	data, err := os.ReadFile(filepath.Join(treeDir, filepath.FromSlash(rel)))
	if err != nil {
		return "", fmt.Errorf("design: gather: read %s: %w", rel, err)
	}

	return string(data), nil
}

// declaredDeps returns the range of each dependency, dev dependency and peer
// dependency in the package.json at file; nil when it cannot be read.
func declaredDeps(file string) map[string]string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}

	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}

	deps := map[string]string{}

	for _, field := range []string{"dependencies", "devDependencies", "peerDependencies"} {
		m, _ := manifest[field].(map[string]any)
		for name, v := range m {
			if rangeSpec, ok := v.(string); ok {
				if _, dup := deps[name]; !dup {
					deps[name] = rangeSpec
				}
			}
		}
	}

	return deps
}

func (s *gathering) gap(root, spec, reason string) {
	g := Gap{Root: root, Import: spec, Reason: reason}
	if !s.seenGaps[g] {
		s.seenGaps[g] = true
		s.gaps = append(s.gaps, g)
	}
}

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

// PackageDir is the top-level directory under the tool root a package's kept
// files are written to. "_" starts no project name, so no tree collides.
func PackageDir(name, version string) string {
	return "_npm+" + strings.ReplaceAll(name, "/", "+") + "@" + version
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
	path     string // Root.Path
	lock     lockfile
	lockGap  string // why lock cannot be used; "" when it can
	lockRoot string // the root's directory relative to the lockfile's
	regs     registries
	regErr   error
	deps     map[string]string // the root package.json's declared ranges
	seen     map[string]bool   // tool-root-relative path of each file walked
	reached  map[string]bool   // pkg dirs
	decls    []decl
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

	locks := map[string]dirLock{}
	roots := make([]Root, 0, len(fronts))

	for _, f := range fronts {
		lockDir, lock, lockGap := nearestLock(tree.Dir, treeDir, f.Dir, locks)

		w := &rootWalk{
			path:     path.Join(filepath.ToSlash(tree.Dir), f.Dir),
			lock:     lock,
			lockGap:  lockGap,
			lockRoot: relDir(lockDir, f.Dir),
			seen:     map[string]bool{},
			reached:  map[string]bool{},
		}

		root, err := s.frontend(ctx, tree, treeDir, w, f)
		if err != nil {
			return nil, err
		}

		roots = append(roots, root)
	}

	return roots, nil
}

// dirLock is what readLockfile found in one directory of a tree.
type dirLock struct {
	lock lockfile
	err  error
}

// nearestLock finds the lockfile in the frontend root at dir or the nearest
// directory above it, up to the tree root; locks caches each directory read.
// lockGap says why no lockfile can be used.
func nearestLock(treeName, treeDir, dir string, locks map[string]dirLock) (lockDir string, lock lockfile, lockGap string) {
	for d := dir; ; d = parentDir(d) {
		found, ok := locks[d]
		if !ok {
			found.lock, found.err = readLockfile(filepath.Join(treeDir, filepath.FromSlash(d)))
			if found.err != nil {
				slog.Warn("design: read lockfile", "tree", treeName, "dir", d, "error", found.err)
			}

			locks[d] = found
		}

		switch {
		case found.err != nil:
			return d, nil, "the lockfile could not be read"
		case found.lock != nil:
			return d, found.lock, ""
		case d == "":
			return "", nil, "no lockfile in the repository"
		}
	}
}

// relDir is the tree-relative dir relative to base, an ancestor of it or dir
// itself.
func relDir(base, dir string) string {
	if base == "" {
		return dir
	}

	return strings.TrimPrefix(strings.TrimPrefix(dir, base), "/")
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

	w.deps = declaredDeps(filepath.Join(rootDir, "package.json"))

	own := &pkgFiles{css: make(map[string]string, len(f.CSS))}

	for _, rel := range f.CSS {
		css, err := readTreeFile(treeDir, rel)
		if err != nil {
			return Root{}, err
		}

		own.css[rel] = css
	}

	// The CSS packages a script imports come before the root's own CSS.
	for _, e := range f.Entries {
		if _, isCSS := own.css[e.File]; isCSS {
			continue
		}

		if err := s.rootImport(ctx, w, e); err != nil {
			return Root{}, err
		}
	}

	ownDecls := func(rel string) []decl { return rootDecls(own.css[rel]) }

	for _, rel := range ownStarts(own) {
		err := w.importOrder(own, treePrefix, rel, ownDecls, func(e entry) error {
			if isLocalFile(treeDir, e.File, e.Spec) {
				return nil
			}

			return s.rootImport(ctx, w, e)
		})
		if err != nil {
			return Root{}, err
		}
	}

	if err := ownMaterial(treeDir, w, f, own, &root); err != nil {
		return Root{}, err
	}

	root.Packages = slices.Sorted(maps.Keys(w.reached))

	return root, nil
}

// rootImport resolves a package import of the root's own files through the
// root's package.json and the lockfile, and walks the package.
func (s *gathering) rootImport(ctx context.Context, w *rootWalk, e entry) error {
	rangeSpec, ok := w.deps[e.Name]
	if !ok {
		s.gap(w.path, e.Spec, "not declared in "+path.Join(w.path, "package.json"))

		return nil
	}

	if w.lockGap != "" {
		s.gap(w.path, e.Spec, w.lockGap)

		return nil
	}

	l, ok := w.lock.fromRoot(w.lockRoot, e.Name, rangeSpec)
	if !ok {
		s.gap(w.path, e.Spec, "the lockfile has no entry for it")

		return nil
	}

	return s.visit(ctx, w, l, e.Spec, e.Subpath)
}

// ownStarts lists the root's CSS files in the order their walks start: those
// no other of them imports, then the rest, so files in a cycle are walked too,
// each group in path order.
func ownStarts(own *pkgFiles) []string {
	imported := map[string]bool{}

	for file, css := range own.css {
		for _, spec := range cssImports(css) {
			if rel, ok := own.sibling(file, spec); ok && rel != file {
				imported[rel] = true
			}
		}
	}

	var starts, rest []string

	for _, rel := range slices.Sorted(maps.Keys(own.css)) {
		if imported[rel] {
			rest = append(rest, rel)
		} else {
			starts = append(starts, rel)
		}
	}

	return append(starts, rest...)
}

// ownMaterial resolves the tokens the root's own CSS and sources reference,
// and its fonts.
func ownMaterial(treeDir string, w *rootWalk, f frontend, own *pkgFiles, root *Root) error {
	var (
		ownCSS []string
		names  []string
	)

	for _, rel := range slices.Sorted(maps.Keys(own.css)) {
		ownCSS = append(ownCSS, own.css[rel])
		names = append(names, references(own.css[rel])...)
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

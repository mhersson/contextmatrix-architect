package design

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxTokenNames bounds the custom property names one root resolves.
	maxTokenNames = 2000
	// maxFrameSourceBytes is the largest frame file whose text is read.
	maxFrameSourceBytes = 32 << 10
	// maxFrameImages bounds the images listed for one root.
	maxFrameImages = 20
)

// Tree is a repository checked out at Dir, its top-level directory under the
// tool root.
type Tree struct{ Project, Dir string }

// Request names the trees to gather from. Root is the blueprint's tool root;
// kept packages are written directly under it.
type Request struct {
	Root     string
	Trees    []Tree
	ImageCap int // the largest package image kept, in bytes; 0 keeps none
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
	Frame         []Asset
	Brand         []Asset
	Images        []Asset // the images the frame files with text use, each once
	// PackageImages are the images of kept packages named like the frame files'
	// imports; Path is tool-root-relative under the package's Dir, Size in bytes.
	PackageImages []Asset
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
// it belongs to, Import the import spec as written, or "" for a gap of the
// root as a whole.
type Gap struct{ Root, Import, Reason string }

// gathering is the state of one Gather call.
type gathering struct {
	g        *Gatherer
	fetchBy  time.Time // downloads stop here; Gather's own ctx is not wrapped
	root     string
	imageCap int
	pkgs     map[string]*keptPkg // by dir
	order    []*keptPkg          // in the order first reached
	gaps     []Gap
	seenGaps map[Gap]bool
	treePkgs map[string]map[string][]treePackage // by tree dir, then package name
	wants    []imageWant
}

// imageWant is the names a frame file of the root at root imports from the
// package whose kept files go under dir.
type imageWant struct {
	root, dir string
	names     []string
}

// rootWalk is one frontend root's walk through the packages its CSS imports.
type rootWalk struct {
	path     string // Root.Path
	tree     string // the tree's directory under the tool root
	lock     lockfile
	lockGap  string // why lock cannot be used; "" when it can
	lockDir  string // the lockfile's directory, tree-relative
	lockRoot string // the root's directory relative to the lockfile's
	regs     registries
	regErr   error
	deps     map[string]string // the root package.json's declared ranges
	seen     map[string]bool   // tool-root-relative path of each file walked
	reached  map[string]bool   // pkg dirs
	decls    []decl
	own      *ownTree // the root's own files
	// walk walks a file of own as the root's own CSS, giving each package
	// import it meets to pkg.
	walk func(rel string, pkg func(entry) error) error
}

// Gather collects the design material of every frontend root in req.Trees and
// writes the package CSS those roots import, and the images of those packages
// their frame files name, under req.Root. What cannot be had is a Gap; only a
// local read or write failure or a cancelled ctx is an error.
func (g *Gatherer) Gather(ctx context.Context, req Request) (Material, error) {
	s := &gathering{
		g: g, fetchBy: time.Now().Add(g.fetchFor), root: req.Root, imageCap: req.ImageCap, pkgs: map[string]*keptPkg{}, seenGaps: map[Gap]bool{},
		treePkgs: map[string]map[string][]treePackage{},
	}

	var m Material

	for _, tree := range req.Trees {
		roots, err := s.tree(ctx, tree)
		if err != nil {
			return Material{}, err
		}

		m.Roots = append(m.Roots, roots...)
	}

	images := s.pickPackageImages()
	for i := range m.Roots {
		m.Roots[i].PackageImages = images[m.Roots[i].Path]
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
		m.Roots[i].PackageImages = slices.DeleteFunc(m.Roots[i].PackageImages, func(a Asset) bool {
			dir, _, _ := strings.Cut(a.Path, "/")

			return !kept[dir]
		})
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
			tree:     filepath.ToSlash(tree.Dir),
			lock:     lock,
			lockGap:  lockGap,
			lockDir:  lockDir,
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

	var imageSources []imageSource

	for _, a := range frameFiles(treeDir, f) {
		text, err := readText(treeDir, a, maxFrameSourceBytes)
		if err != nil {
			return Root{}, err
		}

		if text != "" {
			imageSources = append(imageSources, imageSource{file: a.Path, text: text})
		}

		a.Text = text
		a.Path = path.Join(treePrefix, a.Path)
		root.Frame = append(root.Frame, a)
	}

	root.Images = frameImages(treeDir, f.Dir, treePrefix, imageSources)

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

	ot := &ownTree{treeDir: treeDir, files: own, gap: func(spec, reason string) { s.gap(w.path, spec, reason) }}
	ownDecls := func(rel string) []decl { return rootDecls(own.css[rel]) }
	rootPkg := func(e entry) error { return s.rootImport(ctx, w, e) }
	w.own = ot
	w.walk = func(rel string, pkg func(entry) error) error {
		return w.importOrder(own, treePrefix, rel, ownDecls, pkg, ot.local)
	}
	walkOwn := func(rel string) error { return w.walk(rel, rootPkg) }

	for _, e := range f.Entries {
		var err error

		switch {
		case e.Name != "":
			err = s.rootImport(ctx, w, e)
		case e.Local == "":
			s.gap(w.path, e.Spec, "an import alias the architect does not resolve")
		case ot.read(e.Local, e.Spec):
			err = walkOwn(e.Local)
		}

		if err != nil {
			return Root{}, err
		}
	}

	unreached := slices.DeleteFunc(slices.Clone(f.CSS), func(rel string) bool { return w.seen[treePrefix+"/"+rel] })

	switch {
	case len(unreached) > 0 && len(unreached) == len(f.CSS):
		// A single file has no load order in question.
		if len(f.CSS) > 1 {
			s.gap(w.path, "", "no HTML link or script import reaches the root's CSS, so its load order is unknown "+
				"and token values come from every CSS file in path order")
		}

		for _, rel := range ownStarts(own, f.CSS) {
			if err := walkOwn(rel); err != nil {
				return Root{}, err
			}
		}
	case len(unreached) > 0:
		filled, err := w.beneath(ownStarts(own, unreached), walkOwn)
		if err != nil {
			return Root{}, err
		}

		if len(filled) > 0 {
			s.gap(w.path, "", "nothing the architect reads loads "+fileList(treePrefix, filled)+
				", so their values count only for tokens the loaded CSS does not declare")
		}
	}

	s.wantImages(w, imageSources)

	if len(f.Styles) > 0 {
		s.gap(w.path, "", "the root's styles in "+strings.Join(f.Styles, ", ")+
			" are not read, so its tokens and fonts leave them out")
	}

	if err := ownMaterial(treeDir, w, f, own, &root); err != nil {
		return Root{}, err
	}

	root.Packages = slices.Sorted(maps.Keys(w.reached))

	return root, nil
}

// imageSource is a frame file's tree-relative path and text.
type imageSource struct{ file, text string }

// frameImages lists, as tool-root-relative assets, the images the sources use:
// each source's in import order, the sources in order, once each, at most
// maxFrameImages. rootDir is the frontend root's tree-relative directory.
func frameImages(treeDir, rootDir, treePrefix string, sources []imageSource) []Asset {
	var images []Asset

	seen := map[string]bool{}

	for _, src := range sources {
		for _, spec := range imageSpecs(src.text, path.Ext(src.file)) {
			rel, ok := imageTarget(treeDir, rootDir, src.file, spec)
			if !ok || seen[rel] {
				continue
			}

			size, ok := imageSize(treeDir, rel)
			if !ok {
				continue
			}

			seen[rel] = true
			images = append(images, Asset{Path: path.Join(treePrefix, rel), Size: size})

			if len(images) == maxFrameImages {
				return images
			}
		}
	}

	return images
}

// wantImages adds a want for each named import of the script sources from a
// package the root declares and the lockfile resolves. Nothing is fetched for
// it, and an import that does not resolve is passed over.
func (s *gathering) wantImages(w *rootWalk, sources []imageSource) {
	if w.lockGap != "" {
		return
	}

	for _, src := range sources {
		if !imageSyntaxes[strings.ToLower(path.Ext(src.file))].script {
			continue
		}

		for _, imp := range namedImports(src.text) {
			name, _, ok := splitBare(imp.Spec)
			if !ok {
				continue
			}

			rangeSpec, ok := w.deps[name]
			if !ok {
				continue
			}

			l, ok := w.lock.fromRoot(w.lockRoot, name, rangeSpec)
			if !ok || l.Local || !pkgNameRE.MatchString(l.Name) || !pkgVersionRE.MatchString(l.Version) {
				continue
			}

			s.wants = append(s.wants, imageWant{root: w.path, dir: PackageDir(l.Name, l.Version), names: imp.Names})
		}
	}
}

// imageTarget resolves an image spec of the tree-relative file from: ./ and
// ../ against its directory, an @/ or ~/ alias as a style import's, and a
// leading / against the root's public folder. ok is false for a package.
func imageTarget(treeDir, rootDir, from, spec string) (string, bool) {
	switch {
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"):
		return path.Join(path.Dir(from), spec), true
	case strings.HasPrefix(spec, "@/"), strings.HasPrefix(spec, "~/"):
		rel := aliasTarget(treeDir, rootDir, spec[2:])

		return rel, rel != ""
	case strings.HasPrefix(spec, "/"):
		return path.Join(rootDir, "public", spec), true
	}

	return "", false
}

// imageSize returns the size of the tree-relative file rel when a mockup may
// reference it: a regular file reached through no symlink, none of its
// segments .git or node_modules, as show_mockup requires.
func imageSize(treeDir, rel string) (int64, bool) {
	if !filepath.IsLocal(rel) {
		return 0, false
	}

	var (
		p    = treeDir
		info fs.FileInfo
	)

	for part := range strings.SplitSeq(rel, "/") {
		if strings.EqualFold(part, ".git") || strings.EqualFold(part, "node_modules") {
			return 0, false
		}

		p = filepath.Join(p, part)

		var err error
		if info, err = os.Lstat(p); err != nil || info.Mode()&fs.ModeSymlink != 0 {
			return 0, false
		}
	}

	return info.Size(), info.Mode().IsRegular()
}

// rootImport resolves a package import of the root's own files through the
// root's package.json and the lockfile, and walks the package.
func (s *gathering) rootImport(ctx context.Context, w *rootWalk, e entry) error {
	return s.declaredImport(ctx, w, e, w.deps, w.lockRoot, path.Join(w.path, "package.json"))
}

// declaredImport resolves e through deps, the declarations of the package.json
// at manifest, and the lockfile's importer at lockRoot, and walks the package.
func (s *gathering) declaredImport(ctx context.Context, w *rootWalk, e entry, deps map[string]string, lockRoot, manifest string) error {
	rangeSpec, ok := deps[e.Name]
	if !ok {
		s.gap(w.path, e.Spec, "not declared in "+manifest)

		return nil
	}

	if w.lockGap != "" {
		s.gap(w.path, e.Spec, w.lockGap)

		return nil
	}

	l, ok := w.lock.fromRoot(lockRoot, e.Name, rangeSpec)
	if !ok {
		s.gap(w.path, e.Spec, "the lockfile has no entry for it")

		return nil
	}

	if l.Local {
		return s.workspace(ctx, w, e, l.Name)
	}

	return s.visit(ctx, w, l, e.Spec, e.Subpath)
}

// ownStarts lists the root's own CSS files in the order their walks start:
// those no other of them imports, then the rest, so files in a cycle are
// walked too, each group in path order.
func ownStarts(own *pkgFiles, files []string) []string {
	imported := map[string]bool{}

	for _, file := range files {
		for _, spec := range cssImports(own.css[file]) {
			if rel, ok := own.sibling(file, spec); ok && rel != file {
				imported[rel] = true
			}
		}
	}

	var starts, rest []string

	for _, rel := range slices.Sorted(slices.Values(files)) {
		if imported[rel] {
			rest = append(rest, rel)
		} else {
			starts = append(starts, rel)
		}
	}

	return append(starts, rest...)
}

// beneath walks starts, own CSS files no entry reached, beneath the
// declarations so far, which win; it returns the starts that added a name.
func (w *rootWalk) beneath(starts []string, walk func(rel string) error) ([]string, error) {
	loaded := w.decls

	declared := make(map[string]bool, len(loaded))
	for _, d := range loaded {
		declared[d.Name] = true
	}

	w.decls = nil

	var filled []string

	for _, rel := range starts {
		n := len(w.decls)

		if err := walk(rel); err != nil {
			return nil, err
		}

		if slices.ContainsFunc(w.decls[n:], func(d decl) bool { return !declared[d.Name] }) {
			filled = append(filled, rel)
		}
	}

	w.decls = append(w.decls, loaded...)

	return filled, nil
}

// fileList names up to five tree-relative files by their tool-root-relative
// path, and how many more there are.
func fileList(treePrefix string, files []string) string {
	const show = 5

	names := make([]string, 0, min(len(files), show))
	for _, rel := range files[:min(len(files), show)] {
		names = append(names, path.Join(treePrefix, rel))
	}

	list := strings.Join(names, ", ")
	if len(files) > show {
		list += fmt.Sprintf(" and %d more", len(files)-show)
	}

	return list
}

// ownMaterial resolves the tokens the root's own CSS and sources reference,
// and its fonts. A file own gained from elsewhere in the tree adds only
// declarations, like a package's.
func ownMaterial(treeDir string, w *rootWalk, f frontend, own *pkgFiles, root *Root) error {
	var (
		ownCSS []string
		names  []string
	)

	for _, rel := range slices.Sorted(slices.Values(f.CSS)) {
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

// readText returns the text of the tree-relative file a, or "" when it is over
// limit bytes, holds a NUL byte or is not UTF-8.
func readText(treeDir string, a Asset, limit int64) (string, error) {
	if a.Size > limit {
		return "", nil
	}

	text, err := readTreeFile(treeDir, a.Path)
	if err != nil {
		return "", err
	}

	// The file may have grown since it was listed.
	if int64(len(text)) > limit || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return "", nil
	}

	return text, nil
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

	return manifestDeps(manifest)
}

// manifestDeps returns the range of each dependency, dev dependency and peer
// dependency a package.json declares.
func manifestDeps(manifest map[string]any) map[string]string {
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

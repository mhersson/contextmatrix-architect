package design

import (
	"cmp"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const (
	maxFrameFiles = 20
	maxBrandFiles = 10
)

var (
	sourceExts = map[string]bool{
		".js": true, ".jsx": true, ".ts": true, ".tsx": true, ".vue": true, ".svelte": true,
	}

	// The extensions tried, in order, for an import written without one.
	childExts = []string{".tsx", ".ts", ".jsx", ".js", ".vue", ".svelte"}

	frameExts = map[string]bool{
		".tsx": true, ".jsx": true, ".ts": true, ".js": true,
		".vue": true, ".svelte": true, ".astro": true, ".html": true, ".css": true,
	}

	// Frame extensions that hold markup or styles, never plain script.
	uiExts = map[string]bool{
		".tsx": true, ".jsx": true, ".vue": true, ".svelte": true, ".astro": true, ".html": true, ".css": true,
	}

	// Style files the walk finds but never reads, by the language they name.
	styleLangExts = map[string]string{
		".scss": "SCSS", ".sass": "SCSS", ".less": "Less", ".styl": "Stylus",
	}

	// The lang of a Vue or Svelte style block, by the language it names.
	blockLangs = map[string]string{
		"scss": "SCSS", "sass": "SCSS", "less": "Less", "stylus": "Stylus", "styl": "Stylus",
	}

	styleBlockRE = regexp.MustCompile(`(?is)<style\b([^>]*)>`)
	langAttrRE   = regexp.MustCompile(`(?i)\blang\s*=\s*["']?([a-z]+)`)

	// CSS-in-JS dependencies, exact or by name prefix.
	cssInJSDeps     = map[string]bool{"styled-components": true, "goober": true, "jss": true, "react-jss": true, "styled-jsx": true}
	cssInJSPrefixes = []string{
		"@emotion/", "@mui/", "@vanilla-extract/", "@chakra-ui/", "@mantine/",
		"@linaria/", "@stitches/", "@pandacss/", "@stylexjs/",
	}

	// Frame words by rank class: the lower the class, the earlier the file.
	frameWords = map[string]int{
		"layout": 0, "shell": 0,
		"header": 1, "masthead": 1, "topbar": 1, "appbar": 1, "navigation": 1, "navbar": 1, "nav": 1,
		"sidenav": 1, "topnav": 1,
		"footer":  2,
		"sidebar": 3, "menu": 3,
		"hero": 4,
	}

	// Directories whose files are logic, data, tests, demos, icons, providers or
	// email templates, never the page frame.
	nonFrameDirs = map[string]bool{
		"api": true, "constants": true, "utils": true, "hooks": true, "pages": true,
		"services": true, "store": true, "types": true, "test": true, "tests": true,
		"__tests__": true, "__mocks__": true, "email": true, "emails": true, "mail": true, "mails": true,
		"stories": true, "icon": true, "icons": true, "context": true, "contexts": true,
		"provider": true, "providers": true,
	}

	componentDirWords = map[string]bool{
		"component": true, "components": true, "layout": true, "layouts": true,
	}

	layoutDirWords = map[string]bool{"layout": true, "layouts": true}

	brandWords = map[string]bool{"logo": true, "brand": true}

	// The file names an app most often starts from.
	entryNames = map[string]bool{"main": true, "index": true, "_app": true, "layout": true}

	// Marks of a server or build template in an href, which names no file as
	// written.
	templateMarks = []string{"{{", "{%", "<%", "${"}

	// Marker segments of files that are not part of the running frontend.
	nonFrameMarkers = []string{".test.", ".spec.", ".stories.", ".cy."}
)

// frontend is one frontend root: a directory with a package.json that owns CSS,
// imports CSS from a package, or styles its sources in a language the walk does
// not read. Dir is relative to the scanned tree ("" for the tree itself); CSS
// and Sources are tree-relative paths of the files that belong to this root and
// to no root nested below it. Entries are the style imports of its HTML and
// script files in load order. Styles names the unread style languages of its
// own files, sorted.
type frontend struct {
	Dir     string
	CSS     []string
	Entries []entry
	Sources []string
	Styles  []string
}

// entry is a style import of an HTML or script file. File is the
// tree-relative importing file, Spec the specifier as written. A package
// import has Name and Subpath; any other has Local, the tree-relative file it
// names, set whether or not that exists, except an alias that did not resolve.
type entry struct {
	File, Spec, Name, Subpath string
	Local                     string
}

// Asset is a file found under a frontend root and its size in bytes. Text is
// the file's text when the brief may carry it, else "".
type Asset struct {
	Path string
	Size int64
	Text string
}

// scanTree finds the frontend roots under treeDir in a single walk. A root is
// the nearest directory holding a package.json above a .css file or above a
// source file that imports a package's CSS, or one above a source file when
// its files use SCSS, Less, Stylus or Vue or Svelte style blocks, or its
// package.json a CSS-in-JS library.
func scanTree(treeDir string) ([]frontend, error) {
	type candidate struct{ rel, ext string }

	var (
		pkgDirs    = map[string]bool{}
		candidates []candidate
	)

	err := walkFiles(treeDir, "", false, func(rel string, d fs.DirEntry) error {
		name := d.Name()
		if name == "package.json" {
			pkgDirs[parentDir(rel)] = true

			return nil
		}

		if ext := strings.ToLower(path.Ext(name)); ext == ".css" || ext == ".html" || sourceExts[ext] || styleLangExts[ext] != "" {
			candidates = append(candidates, candidate{rel: rel, ext: ext})
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("design: scan tree: %w", err)
	}

	var (
		files   []scanned
		roots   = map[string]bool{}
		sources = map[string]bool{} // package dirs holding a source file
		styled  = map[string]bool{} // package dirs with a style language
	)

	for _, c := range candidates {
		pkg, ok := nearestDir(pkgDirs, parentDir(c.rel))
		if !ok {
			continue
		}

		f := scanned{rel: c.rel, ext: c.ext}

		if lang := styleLangExts[c.ext]; lang != "" {
			f.styles = []string{lang}
			styled[pkg] = true
		}

		if sourceExts[c.ext] {
			sources[pkg] = true
		}

		if c.ext != ".css" && styleLangExts[c.ext] == "" {
			src, err := os.ReadFile(filepath.Join(treeDir, filepath.FromSlash(c.rel)))
			if err != nil {
				return nil, fmt.Errorf("design: scan tree: read %s: %w", c.rel, err)
			}

			if c.ext == ".vue" || c.ext == ".svelte" {
				f.styles = blockStyles(c.ext, string(src))
				styled[pkg] = styled[pkg] || len(f.styles) > 0
			}

			if c.ext == ".html" {
				f.entries = htmlEntries(treeDir, pkg, c.rel, string(src))
			} else {
				f.entries = scriptEntries(treeDir, pkg, c.rel, jsCSSImports(string(src)))
			}
		}

		if c.ext == ".css" || slices.ContainsFunc(f.entries, func(e entry) bool { return e.Name != "" }) {
			roots[pkg] = true
		}

		files = append(files, f)
	}

	for pkg := range sources {
		if styled[pkg] || len(cssInJS(treeDir, pkg)) > 0 {
			roots[pkg] = true
		}
	}

	var (
		byDir   = map[string]*frontend{}
		loaders = map[string][]scanned{}
	)

	for _, f := range files {
		dir, ok := nearestDir(roots, parentDir(f.rel))
		if !ok {
			continue
		}

		fe := byDir[dir]
		if fe == nil {
			fe = &frontend{Dir: dir}
			byDir[dir] = fe
		}

		switch {
		case f.ext == ".css":
			fe.CSS = append(fe.CSS, f.rel)
		case sourceExts[f.ext]:
			fe.Sources = append(fe.Sources, f.rel)
		}

		fe.Styles = append(fe.Styles, f.styles...)

		if len(f.entries) > 0 {
			loaders[dir] = append(loaders[dir], f)
		}
	}

	out := make([]frontend, 0, len(byDir))

	for dir, fe := range byDir {
		ls := loaders[dir]
		slices.SortFunc(ls, func(a, b scanned) int { return loadOrder(dir, a, b) })

		for _, f := range ls {
			fe.Entries = append(fe.Entries, f.entries...)
		}

		fe.Styles = append(fe.Styles, cssInJS(treeDir, dir)...)
		slices.Sort(fe.Styles)
		fe.Styles = slices.Compact(fe.Styles)

		out = append(out, *fe)
	}

	slices.SortFunc(out, func(a, b frontend) int { return strings.Compare(a.Dir, b.Dir) })

	return out, nil
}

// scanned is a file of the tree that may belong to a frontend root; ext is
// lower-case.
type scanned struct {
	rel, ext string
	entries  []entry
	styles   []string // unread style languages of the file
}

// blockStyles names the style languages of the <style> blocks in a Vue or
// Svelte file; a block without a known lang is "<Vue|Svelte> <style> blocks".
func blockStyles(ext, src string) []string {
	var out []string

	for _, m := range styleBlockRE.FindAllStringSubmatch(src, -1) {
		if lm := langAttrRE.FindStringSubmatch(m[1]); lm != nil {
			if lang := blockLangs[strings.ToLower(lm[1])]; lang != "" {
				out = append(out, lang)

				continue
			}
		}

		out = append(out, map[string]string{".vue": "Vue", ".svelte": "Svelte"}[ext]+" <style> blocks")
	}

	return out
}

// cssInJS lists the CSS-in-JS dependencies of the package.json in dir as
// "CSS-in-JS (<dep>)".
func cssInJS(treeDir, dir string) []string {
	var out []string

	for dep := range declaredDeps(filepath.Join(treeDir, filepath.FromSlash(dir), "package.json")) {
		if cssInJSDeps[dep] || slices.ContainsFunc(cssInJSPrefixes, func(p string) bool { return strings.HasPrefix(dep, p) }) {
			out = append(out, "CSS-in-JS ("+dep+")")
		}
	}

	return out
}

// loadOrder approximates the order a root's files load their CSS in without a
// script import graph: HTML files, shortest path first, then script files,
// shallowest first, an entry name first at one depth, then by path.
func loadOrder(rootDir string, a, b scanned) int {
	switch aHTML, bHTML := a.ext == ".html", b.ext == ".html"; {
	case aHTML && bHTML:
		return comparePaths(a.rel, b.rel)
	case aHTML:
		return -1
	case bHTML:
		return 1
	}

	rank := func(rel string) int {
		name := path.Base(rel)
		if entryNames[strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))] {
			return 0
		}

		return 1
	}

	depth := func(rel string) int { return strings.Count(relDir(rootDir, rel), "/") }

	return cmp.Or(
		cmp.Compare(depth(a.rel), depth(b.rel)),
		cmp.Compare(rank(a.rel), rank(b.rel)),
		strings.Compare(a.rel, b.rel),
	)
}

// scriptEntries turns a script's CSS imports into entries. A relative import,
// a bare-looking one naming a file beside the script and an @/ or ~/ alias,
// against pkgDir, the script's nearest package.json directory, are local.
func scriptEntries(treeDir, pkgDir, file string, specs []string) []entry {
	var entries []entry

	for _, spec := range specs {
		e := entry{File: file, Spec: spec}

		switch {
		case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
			e.Local = path.Join(path.Dir(file), spec)
		case strings.HasPrefix(spec, "@/") || strings.HasPrefix(spec, "~/"):
			e.Local = aliasTarget(treeDir, pkgDir, spec[2:])
		case isLocalFile(treeDir, file, spec):
			e.Local = path.Join(path.Dir(file), spec)
		default:
			name, subpath, ok := splitBare(spec)
			if !ok {
				continue
			}

			e.Name, e.Subpath = name, subpath
		}

		entries = append(entries, e)
	}

	return entries
}

// aliasTarget is the file an @/ or ~/ alias names, as most bundler setups map
// it: pkgDir/src/<rest>, else pkgDir/<rest>; "" when neither is a file.
func aliasTarget(treeDir, pkgDir, rest string) string {
	for _, rel := range []string{path.Join(pkgDir, "src", rest), path.Join(pkgDir, rest)} {
		if isTreeFile(treeDir, rel) {
			return rel
		}
	}

	return ""
}

// htmlEntries turns the stylesheet links of an HTML file into entries. An href
// less its query and fragment is read against the file's directory, a leading
// / and %PUBLIC_URL% included, and a leading / then against pkgDir and
// pkgDir/public, where dev servers serve static files from. A link the tree
// does not hold stays an entry, to gather as a gap; a remote or templated href
// is left out.
func htmlEntries(treeDir, pkgDir, file, src string) []entry {
	var entries []entry

	for _, href := range htmlStylesheets(src) {
		if slices.ContainsFunc(templateMarks, func(m string) bool { return strings.Contains(href, m) }) {
			continue
		}

		target := strings.TrimPrefix(href, "%PUBLIC_URL%")
		if i := strings.IndexAny(target, "?#"); i >= 0 {
			target = target[:i]
		}

		if target == "" || strings.HasPrefix(target, "//") || schemeRE.MatchString(target) {
			continue
		}

		rest, rooted := strings.CutPrefix(target, "/")

		local := path.Join(path.Dir(file), rest)
		if rooted && !isTreeFile(treeDir, local) {
			for _, alt := range []string{path.Join(pkgDir, rest), path.Join(pkgDir, "public", rest)} {
				if isTreeFile(treeDir, alt) {
					local = alt

					break
				}
			}
		}

		entries = append(entries, entry{File: file, Spec: href, Local: local})
	}

	return entries
}

func isLocalFile(treeDir, file, spec string) bool {
	return filepath.IsLocal(spec) && isTreeFile(treeDir, path.Join(path.Dir(file), spec))
}

// isTreeFile reports whether the slash path rel inside treeDir is a regular
// file.
func isTreeFile(treeDir, rel string) bool {
	return filepath.IsLocal(rel) && isRegularFile(filepath.Join(treeDir, filepath.FromSlash(rel)))
}

func isRegularFile(p string) bool {
	info, err := os.Stat(p)

	return err == nil && info.Mode().IsRegular()
}

// frameHit is a frame file found by name.
type frameHit struct {
	asset        Asset
	unit         string
	class        int
	depth        int
	ownFolder    bool
	isStylesheet bool
}

// frameFiles lists the files of f that most likely hold the page frame (layout,
// shell, navigation, header, footer), best first. A frame component that is its
// own folder is followed by the files it imports with ./, one level.
func frameFiles(treeDir string, f frontend) []Asset {
	var (
		hits    []frameHit
		visited = map[string]fs.DirEntry{}
	)

	err := walkFiles(treeDir, f.Dir, true, func(rel string, d fs.DirEntry) error {
		visited[rel] = d

		unit, class, ownFolder, ok := frameUnit(relDir(f.Dir, rel))
		if !ok {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		hits = append(hits, frameHit{
			asset:        Asset{Path: rel, Size: info.Size()},
			unit:         unit,
			class:        class,
			depth:        strings.Count(unit, "/"),
			ownFolder:    ownFolder,
			isStylesheet: strings.EqualFold(path.Ext(rel), ".css"),
		})

		return nil
	})
	if err != nil {
		slog.Warn("design: walk frame files", "dir", f.Dir, "error", err)
	}

	// A unit's files stay together; its stylesheet follows its other files.
	slices.SortFunc(hits, func(a, b frameHit) int {
		return cmp.Or(
			cmp.Compare(a.depth, b.depth),
			cmp.Compare(a.class, b.class),
			comparePaths(a.unit, b.unit),
			cmp.Compare(boolInt(a.isStylesheet), boolInt(b.isStylesheet)),
			comparePaths(a.asset.Path, b.asset.Path),
		)
	})

	var (
		assets  = make([]Asset, 0, len(hits))
		emitted = map[string]bool{}
		// Files brought in as a child, which are never parents: one level.
		viaParent = map[string]bool{}
	)

	emit := func(a Asset) bool {
		if emitted[a.Path] {
			return false
		}

		emitted[a.Path] = true
		assets = append(assets, a)

		return true
	}

	for start := 0; start < len(hits); {
		end := start + 1
		for end < len(hits) && hits[end].unit == hits[start].unit {
			end++
		}

		for _, h := range hits[start:end] {
			emit(h.asset)
		}

		// A parent ranked past the cap puts its children past it too: not read.
		for i := start; i < min(end, maxFrameFiles); i++ {
			if viaParent[hits[i].asset.Path] {
				continue
			}

			for _, c := range frameChildren(treeDir, f.Dir, hits[i], visited) {
				if emit(c) {
					viaParent[c.Path] = true
				}
			}
		}

		start = end
	}

	return assets[:min(len(assets), maxFrameFiles)]
}

// frameChildren lists the files the frame file h imports with ./ that the frame
// walk visited and the frame rules allow, in import order. Only a frame file
// that is its own folder and not a stylesheet has children; one whose text
// cannot be read, as for the frame source, has none.
func frameChildren(treeDir, rootDir string, h frameHit, visited map[string]fs.DirEntry) []Asset {
	if !h.ownFolder || h.isStylesheet {
		return nil
	}

	text, err := readText(treeDir, h.asset, maxFrameSourceBytes)
	if err != nil {
		slog.Warn("design: read frame file", "path", h.asset.Path, "error", err)

		return nil
	}

	var children []Asset

	for _, spec := range localImports(text) {
		rel, ok := resolveChild(path.Dir(h.asset.Path), spec, visited)
		if !ok || !isChildFile(splitFrameName(relDir(rootDir, rel))) {
			continue
		}

		info, err := visited[rel].Info()
		if err != nil {
			slog.Warn("design: stat frame child", "path", rel, "error", err)

			continue
		}

		children = append(children, Asset{Path: rel, Size: info.Size()})
	}

	return children
}

// resolveChild finds the visited file a ./ specifier names from dir: the
// specifier itself when it has a source extension, else with each extension,
// else as a folder's index.
func resolveChild(dir, spec string, visited map[string]fs.DirEntry) (string, bool) {
	target := path.Join(dir, spec)

	isVisited := func(rel string) bool {
		_, ok := visited[rel]

		return ok
	}

	if sourceExts[strings.ToLower(path.Ext(target))] {
		return target, isVisited(target)
	}

	for _, ext := range childExts {
		if rel := target + ext; isVisited(rel) {
			return rel, true
		}
	}

	for _, ext := range childExts {
		if rel := path.Join(target, "index"+ext); isVisited(rel) {
			return rel, true
		}
	}

	return "", false
}

// isChildFile applies the frame rules that are not about frame words.
func isChildFile(n frameName) bool {
	return sourceExts[n.ext] && !n.isTestOrDeclaration() && !n.isLogic()
}

func boolInt(b bool) int {
	if b {
		return 1
	}

	return 0
}

// brandFiles lists the logo and brand images of f: files named just logo or
// brand first, then shortest path first.
func brandFiles(treeDir string, f frontend) []Asset {
	var assets []Asset

	err := walkFiles(treeDir, f.Dir, true, func(rel string, d fs.DirEntry) error {
		name := d.Name()
		if ext := strings.ToLower(path.Ext(name)); ext != ".svg" && ext != ".png" {
			return nil
		}

		if !hasWord(nameWords(strings.TrimSuffix(name, path.Ext(name))), brandWords) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		assets = append(assets, Asset{Path: rel, Size: info.Size()})

		return nil
	})
	if err != nil {
		slog.Warn("design: walk brand files", "dir", f.Dir, "error", err)
	}

	slices.SortFunc(assets, func(a, b Asset) int {
		return cmp.Or(
			cmp.Compare(boolInt(!namedJustBrand(a.Path)), boolInt(!namedJustBrand(b.Path))),
			comparePaths(a.Path, b.Path),
		)
	})

	return assets[:min(len(assets), maxBrandFiles)]
}

// namedJustBrand reports whether the file's stem is the single word logo or
// brand.
func namedJustBrand(p string) bool {
	words := nameWords(strings.TrimSuffix(path.Base(p), path.Ext(p)))

	return len(words) == 1 && brandWords[words[0]]
}

// frameName is what the frame rules read of a path relative to a frontend root.
// ext is lower-case. unit is the directory of an index file or of a file named
// after its directory (ownFolder), else the directory and the file's stem; words
// are the name words of the unit's own name.
type frameName struct {
	dir, name, ext, stem string
	dirName              string
	segments             []string
	stemWords, words     []string
	ownFolder            bool
	unit                 string
}

func splitFrameName(rootRel string) frameName {
	dir, name := path.Split(rootRel)
	dir = strings.TrimSuffix(dir, "/")

	n := frameName{dir: dir, name: name, ext: strings.ToLower(path.Ext(name))}

	if dir != "" {
		n.segments = strings.Split(dir, "/")
		n.dirName = path.Base(dir)
	}

	n.stem, _, _ = strings.Cut(name, ".")
	n.stemWords = nameWords(n.stem)
	n.ownFolder = strings.EqualFold(n.stem, "index") || strings.EqualFold(n.stem, n.dirName)

	if n.ownFolder {
		n.unit, n.words = dir, nameWords(n.dirName)
	} else {
		n.unit, n.words = path.Join(dir, n.stem), n.stemWords
	}

	return n
}

// isTestOrDeclaration reports a type declaration file and the marker names of
// files that are not part of the running frontend.
func (n frameName) isTestOrDeclaration() bool {
	lower := strings.ToLower(n.name)

	return strings.HasSuffix(lower, ".d.ts") ||
		slices.ContainsFunc(nonFrameMarkers, func(m string) bool { return strings.Contains(lower, m) })
}

func (n frameName) isLayout() bool {
	return hasWord(nameWords(n.dirName), layoutDirWords) || slices.Contains(n.words, "layout")
}

// isLogic reports a file in a folder of logic, data, tests, demos, icons,
// providers or email templates, a hook, and a file such as types.ts or
// headerTypes.ts beside a component; only the last word counts, so StoreHeader
// stays. A logo kept in an icon folder and a layout kept in a pages folder are
// still the frame.
func (n frameName) isLogic() bool {
	isLogo := len(n.words) > 0 && n.words[len(n.words)-1] == "logo"
	isLayout := n.isLayout()

	inLogicDir := slices.ContainsFunc(n.segments, func(s string) bool {
		switch s = strings.ToLower(s); s {
		case "icon", "icons":
			return !isLogo
		case "pages":
			return !isLayout
		}

		return nonFrameDirs[s]
	})

	isHook := len(n.words) > 0 && n.words[0] == "use"
	endsInLogicWord := len(n.stemWords) > 0 && nonFrameDirs[n.stemWords[len(n.stemWords)-1]]

	return inLogicDir || isHook || endsInLogicWord
}

// frameUnit decides whether rootRel, a file path relative to the frontend root,
// is a real frame file. ownFolder says its unit is its own folder; class is the
// rank class of its best frame word, 0 for a layout or the app shell.
func frameUnit(rootRel string) (unit string, class int, ownFolder, ok bool) {
	n := splitFrameName(rootRel)

	if !frameExts[n.ext] || n.isTestOrDeclaration() {
		return "", 0, false, false
	}

	// Next's pages/_app is the shell though pages holds routes.
	if n.stem == "_app" && (n.dir == "pages" || n.dir == "src/pages") && (n.ext == ".tsx" || n.ext == ".jsx" || n.ext == ".js") {
		return path.Join(n.dir, n.stem), 0, false, true
	}

	if n.isLogic() {
		return "", 0, false, false
	}

	class, hasFrameWord := frameClass(n.words)
	inComponentDir := slices.ContainsFunc(n.segments, func(s string) bool {
		return hasWord(nameWords(s), componentDirWords)
	})

	switch {
	case n.isLayout():
		return n.unit, 0, n.ownFolder, true
	// Only App.js among script names: a lower-case app.js is usually a Node server.
	case strings.Count(n.unit, "/") <= 1 && (uiExts[n.ext] || n.name == "App.js") &&
		(slices.Equal(n.words, []string{"app"}) || slices.Equal(n.words, []string{"root"})):
		return n.unit, 0, n.ownFolder, true
	case hasFrameWord && (inComponentDir || n.ownFolder || uiExts[n.ext]):
		return n.unit, class, n.ownFolder, true
	}

	return "", 0, false, false
}

// frameClass is the lowest class among the frame words of words, each taken
// alone or joined with the word after it. A last word logo ranks as a header.
func frameClass(words []string) (class int, ok bool) {
	if len(words) > 0 && words[len(words)-1] == "logo" {
		class, ok = frameWords["header"], true
	}

	for i, w := range words {
		candidates := []string{w}
		if i+1 < len(words) {
			candidates = append(candidates, w+words[i+1])
		}

		for _, c := range candidates {
			if k, found := frameWords[c]; found && (!ok || k < class) {
				class, ok = k, true
			}
		}
	}

	return class, ok
}

func hasWord(words []string, set map[string]bool) bool {
	return slices.ContainsFunc(words, func(w string) bool { return set[w] })
}

// nameWords lower-cases s and splits it at camelCase boundaries and at every
// character that is not a letter or digit.
func nameWords(s string) []string {
	var (
		words []string
		cur   []rune
		rs    = []rune(s)
	)

	flush := func() {
		if len(cur) > 0 {
			words = append(words, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}

	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()

			continue
		}

		if unicode.IsUpper(r) && len(cur) > 0 {
			prev := rs[i-1]
			endsAcronym := unicode.IsUpper(prev) && i+1 < len(rs) && unicode.IsLower(rs[i+1])

			if unicode.IsLower(prev) || endsAcronym {
				flush()
			}
		}

		cur = append(cur, r)
	}

	flush()

	return words
}

func comparePaths(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// walkFiles visits every regular file under treeDir/dir. Directories named
// node_modules, dist, build, out, coverage or vendor and hidden directories are
// skipped, and with ownOnly so is every subdirectory holding its own
// package.json. rel is the slash-separated path relative to treeDir.
func walkFiles(treeDir, dir string, ownOnly bool, visit func(rel string, d fs.DirEntry) error) error {
	start := filepath.Join(treeDir, filepath.FromSlash(dir))

	return filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if p != start && (skipDir(d.Name()) || ownOnly && hasPackageJSON(p)) {
				return filepath.SkipDir
			}

			return nil
		}

		if !d.Type().IsRegular() {
			return nil
		}

		rel, err := filepath.Rel(treeDir, p)
		if err != nil {
			return err
		}

		return visit(filepath.ToSlash(rel), d)
	})
}

func hasPackageJSON(dir string) bool {
	return isRegularFile(filepath.Join(dir, "package.json"))
}

func skipDir(name string) bool {
	switch name {
	case "node_modules", "dist", "build", "out", "coverage", "vendor":
		return true
	}

	return strings.HasPrefix(name, ".")
}

func parentDir(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}

	return ""
}

// nearestDir returns dir or its closest ancestor that is in set. The tree root
// is "".
func nearestDir(set map[string]bool, dir string) (string, bool) {
	for {
		if set[dir] {
			return dir, true
		}

		if dir == "" {
			return "", false
		}

		dir = parentDir(dir)
	}
}

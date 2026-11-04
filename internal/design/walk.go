package design

import (
	"cmp"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
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

	frameExts = map[string]bool{
		".tsx": true, ".jsx": true, ".ts": true, ".js": true,
		".vue": true, ".svelte": true, ".html": true, ".css": true,
	}

	frameWords = map[string]bool{
		"layout": true, "shell": true, "app": true, "header": true, "navigation": true,
		"navbar": true, "nav": true, "menu": true, "sidebar": true, "hero": true, "footer": true,
	}

	brandWords = map[string]bool{"logo": true, "brand": true}

	// Marker segments of files that are not part of the running frontend.
	nonFrameMarkers = []string{".test.", ".spec.", ".stories."}
)

// frontend is one frontend root: a directory with a package.json that owns CSS
// or imports CSS from a package. Dir is relative to the scanned tree ("" for
// the tree itself); CSS and Sources are tree-relative paths of the files that
// belong to this root and to no root nested below it.
type frontend struct {
	Dir     string
	CSS     []string
	Entries []entry
	Sources []string
}

// entry is a style import of a package from a project file. File is the
// tree-relative importing file, Spec the specifier as written.
type entry struct {
	File, Spec, Name, Subpath string
}

// Asset is a file found under a frontend root and its size in bytes.
type Asset struct {
	Path string
	Size int64
}

// scanTree finds the frontend roots under treeDir in a single walk. A root is
// the nearest directory holding a package.json above a .css file, or above a
// source file that imports a package's CSS.
func scanTree(treeDir string) ([]frontend, error) {
	type candidate struct {
		rel string
		css bool
	}

	var (
		pkgDirs    = map[string]bool{}
		candidates []candidate
	)

	err := walkFiles(treeDir, "", func(rel string, d fs.DirEntry) error {
		name := d.Name()
		if name == "package.json" {
			pkgDirs[parentDir(rel)] = true

			return nil
		}

		switch ext := strings.ToLower(path.Ext(name)); {
		case ext == ".css":
			candidates = append(candidates, candidate{rel: rel, css: true})
		case sourceExts[ext]:
			candidates = append(candidates, candidate{rel: rel})
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("design: scan tree: %w", err)
	}

	type scanned struct {
		rel     string
		css     bool
		entries []entry
	}

	var (
		files []scanned
		roots = map[string]bool{}
	)

	for _, c := range candidates {
		pkg, ok := nearestDir(pkgDirs, parentDir(c.rel))
		if !ok {
			continue
		}

		src, err := os.ReadFile(filepath.Join(treeDir, filepath.FromSlash(c.rel)))
		if err != nil {
			return nil, fmt.Errorf("design: scan tree: read %s: %w", c.rel, err)
		}

		specs := jsCSSImports(string(src))
		if c.css {
			specs = cssImports(string(src))
		}

		entries := packageEntries(treeDir, c.rel, specs)
		if c.css || len(entries) > 0 {
			roots[pkg] = true
		}

		files = append(files, scanned{rel: c.rel, css: c.css, entries: entries})
	}

	byDir := map[string]*frontend{}

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

		if f.css {
			fe.CSS = append(fe.CSS, f.rel)
		} else {
			fe.Sources = append(fe.Sources, f.rel)
		}

		fe.Entries = append(fe.Entries, f.entries...)
	}

	out := make([]frontend, 0, len(byDir))
	for _, fe := range byDir {
		out = append(out, *fe)
	}

	slices.SortFunc(out, func(a, b frontend) int { return strings.Compare(a.Dir, b.Dir) })

	return out, nil
}

// packageEntries keeps the specs that name a package. A spec that resolves to
// an existing file next to the importing file is a local import even when it
// looks like a package path.
func packageEntries(treeDir, file string, specs []string) []entry {
	var entries []entry

	for _, spec := range specs {
		name, subpath, ok := splitBare(spec)
		if !ok || isLocalFile(treeDir, file, spec) {
			continue
		}

		entries = append(entries, entry{File: file, Spec: spec, Name: name, Subpath: subpath})
	}

	return entries
}

func isLocalFile(treeDir, file, spec string) bool {
	if !filepath.IsLocal(spec) {
		return false
	}

	dir := filepath.Dir(filepath.FromSlash(file))

	info, err := os.Stat(filepath.Join(treeDir, dir, filepath.FromSlash(spec)))

	return err == nil && info.Mode().IsRegular()
}

// frameFiles lists the files of f that most likely hold the page frame (layout,
// shell, navigation, header, footer), shortest path first.
func frameFiles(treeDir string, f frontend) []string {
	var files []string

	err := walkFiles(treeDir, f.Dir, func(rel string, _ fs.DirEntry) error {
		if isFrameFile(rel) {
			files = append(files, rel)
		}

		return nil
	})
	if err != nil {
		slog.Warn("design: walk frame files", "dir", f.Dir, "error", err)
	}

	slices.SortFunc(files, comparePaths)

	return files[:min(len(files), maxFrameFiles)]
}

// brandFiles lists the logo and brand images of f, shortest path first.
func brandFiles(treeDir string, f frontend) []Asset {
	var assets []Asset

	err := walkFiles(treeDir, f.Dir, func(rel string, d fs.DirEntry) error {
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

	slices.SortFunc(assets, func(a, b Asset) int { return comparePaths(a.Path, b.Path) })

	return assets[:min(len(assets), maxBrandFiles)]
}

func isFrameFile(rel string) bool {
	name := path.Base(rel)
	lower := strings.ToLower(name)

	if !frameExts[path.Ext(lower)] {
		return false
	}

	for _, marker := range nonFrameMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}

	if strings.HasPrefix(lower, "index.") {
		return hasWord(nameWords(path.Base(path.Dir(rel))), frameWords)
	}

	return hasWord(nameWords(strings.TrimSuffix(name, path.Ext(name))), frameWords)
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
// skipped. rel is the slash-separated path relative to treeDir.
func walkFiles(treeDir, dir string, visit func(rel string, d fs.DirEntry) error) error {
	start := filepath.Join(treeDir, filepath.FromSlash(dir))

	return filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			if p != start && skipDir(d.Name()) {
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

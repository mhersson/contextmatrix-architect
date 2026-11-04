package design

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const (
	// maxKeptFileBytes is the largest package.json or CSS file readPackage keeps.
	maxKeptFileBytes = 8 << 20
	// maxArchiveBytes bounds the decompressed bytes read from one archive, so a
	// small gzip cannot expand without limit.
	maxArchiveBytes = 1 << 30
	// maxPackageCSSBytes bounds the CSS one package holds in memory.
	maxPackageCSSBytes = 128 << 20
)

// exportConditions are the export conditions searched, in order.
var exportConditions = []string{"style", "default", "import", "browser", "require"}

// pkgFiles is the part of a package that design gathering reads: its manifest
// and its CSS files, keyed by slash path relative to the package root.
type pkgFiles struct {
	manifest map[string]any
	css      map[string]string
}

type readLimits struct {
	file  int64 // largest package.json or CSS file kept
	total int64 // decompressed bytes read from the archive, kept or not
	css   int64 // CSS held in memory
}

// readPackage reads the package.json and the CSS files from the npm tarball at
// tgzPath. The archive is untrusted: nothing is written to disk, and entries
// that are not regular files or whose path is absolute or leaves the package
// are skipped.
func readPackage(ctx context.Context, tgzPath string) (*pkgFiles, error) {
	return readPackageLimited(ctx, tgzPath, readLimits{
		file:  maxKeptFileBytes,
		total: maxArchiveBytes,
		css:   maxPackageCSSBytes,
	})
}

func readPackageLimited(ctx context.Context, tgzPath string, lim readLimits) (*pkgFiles, error) {
	f, err := os.Open(tgzPath)
	if err != nil {
		return nil, fmt.Errorf("design: read package: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("design: read package: %w", err)
	}
	defer gz.Close()

	var (
		src      = &boundedReader{r: gz, left: lim.total}
		tr       = tar.NewReader(src)
		p        = &pkgFiles{css: map[string]string{}}
		cssBytes int64
	)

	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("design: read package: %w", err)
		}

		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return p, nil
		}

		// The header is valid and the next call moves on past the entry.
		if errors.Is(err, tar.ErrInsecurePath) {
			continue
		}

		if err != nil {
			return nil, src.wrap(lim, err)
		}

		if hdr.Typeflag != tar.TypeReg || hdr.Size > lim.file {
			continue
		}

		rel, ok := packagePath(hdr.Name)
		if !ok {
			continue
		}

		isCSS := path.Ext(rel) == ".css"
		if rel != "package.json" && !isCSS {
			continue
		}

		if isCSS && cssBytes+hdr.Size > lim.css {
			return nil, fmt.Errorf("design: read package: more than %d bytes of CSS", lim.css)
		}

		body := make([]byte, hdr.Size)
		if _, err := io.ReadFull(tr, body); err != nil {
			return nil, src.wrap(lim, err)
		}

		if !isCSS {
			if err := json.Unmarshal(body, &p.manifest); err != nil {
				return nil, fmt.Errorf("design: read package: package.json: %w", err)
			}

			continue
		}

		cssBytes += hdr.Size
		p.css[rel] = string(body)
	}
}

// boundedReader fails once more than left bytes have been read from r.
type boundedReader struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func (b *boundedReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)

	b.left -= int64(n)
	if b.left < 0 {
		b.exceeded = true

		return n, errors.New("archive is over the size limit")
	}

	return n, err
}

func (b *boundedReader) wrap(lim readLimits, err error) error {
	if b.exceeded {
		return fmt.Errorf("design: read package: more than %d bytes uncompressed", lim.total)
	}

	return fmt.Errorf("design: read package: %w", err)
}

// packagePath drops the archive's top directory from name and returns the
// cleaned slash path inside the package. ok is false for absolute names and
// for paths that are empty or climb out of the package.
func packagePath(name string) (string, bool) {
	if strings.HasPrefix(name, "/") {
		return "", false
	}

	_, rel, found := strings.Cut(name, "/")
	if !found {
		return "", false
	}

	rel = path.Clean(rel)
	if rel == "." || !filepath.IsLocal(rel) {
		return "", false
	}

	return rel, true
}

// resolve maps the part of a package specifier after the package name to a CSS
// file of the package. A file at that literal path wins, then the manifest's
// exports. ok is false for a path that is not exported or whose target is not
// a CSS file the package holds.
func (p *pkgFiles) resolve(subpath string) (string, bool) {
	if _, ok := p.css[subpath]; ok {
		return subpath, true
	}

	exports := exportsMap(p.manifest["exports"])

	request := "."
	if subpath != "" {
		request = "./" + subpath
	}

	if target, ok := exports[request]; ok {
		return p.target(target, func(s string) string { return s })
	}

	var (
		bestKey, bestBase, bestSuffix string
		found                         bool
	)

	for key := range exports {
		base, suffix, ok := strings.Cut(key, "*")
		if !ok || strings.Contains(suffix, "*") {
			continue
		}

		if len(request) < len(key) || !strings.HasPrefix(request, base) || !strings.HasSuffix(request, suffix) {
			continue
		}

		// The longest base wins, then the longest key.
		if !found || len(base) > len(bestBase) || len(base) == len(bestBase) && len(key) > len(bestKey) {
			bestKey, bestBase, bestSuffix, found = key, base, suffix, true
		}
	}

	if !found {
		return "", false
	}

	match := request[len(bestBase) : len(request)-len(bestSuffix)]

	return p.target(exports[bestKey], func(s string) string { return strings.ReplaceAll(s, "*", match) })
}

// exportsMap returns the subpath map of an exports value. Any other value is
// the target of ".".
func exportsMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		for key := range m {
			if strings.HasPrefix(key, ".") {
				return m
			}
		}
	}

	return map[string]any{".": v}
}

// target resolves one exports value to a CSS file of the package. A null value
// is not exported and, in a condition object, stops the search.
func (p *pkgFiles) target(v any, sub func(string) string) (string, bool) {
	switch v := v.(type) {
	case string:
		rel, ok := strings.CutPrefix(sub(v), "./")
		if !ok {
			return "", false
		}

		rel = path.Clean(rel)
		if !filepath.IsLocal(rel) {
			return "", false
		}

		if _, ok := p.css[rel]; !ok {
			return "", false
		}

		return rel, true
	case []any:
		for _, item := range v {
			if rel, ok := p.target(item, sub); ok {
				return rel, true
			}
		}
	case map[string]any:
		for _, cond := range exportConditions {
			item, ok := v[cond]
			if !ok {
				continue
			}

			if item == nil {
				return "", false
			}

			if rel, ok := p.target(item, sub); ok {
				return rel, true
			}
		}
	}

	return "", false
}

// declares returns the version range the package declares for the dependency
// name in dependencies, peerDependencies or optionalDependencies.
func (p *pkgFiles) declares(name string) (string, bool) {
	for _, field := range []string{"dependencies", "peerDependencies", "optionalDependencies"} {
		deps, _ := p.manifest[field].(map[string]any)
		if rng, ok := deps[name].(string); ok {
			return rng, true
		}
	}

	return "", false
}

// closure follows the relative @imports of the start files inside the
// package. It returns the start files that exist and every CSS file reached
// from them, sorted.
func (p *pkgFiles) closure(start []string) []string {
	var files []string

	seen := map[string]bool{}

	for _, f := range start {
		if _, ok := p.css[f]; ok && !seen[f] {
			seen[f] = true
			files = append(files, f)
		}
	}

	for i := 0; i < len(files); i++ {
		file := files[i]

		for _, spec := range cssImports(p.css[file]) {
			if rel, ok := p.sibling(file, spec); ok && !seen[rel] {
				seen[rel] = true
				files = append(files, rel)
			}
		}
	}

	slices.Sort(files)

	return files
}

// sibling resolves spec against the directory of file. A CSS file found there
// is a relative import even when the spec reads like a package path.
func (p *pkgFiles) sibling(file, spec string) (string, bool) {
	if spec == "" || spec[0] == '/' || spec[0] == '~' || schemeRE.MatchString(spec) {
		return "", false
	}

	rel := path.Join(path.Dir(file), spec)
	if !filepath.IsLocal(rel) {
		return "", false
	}

	if _, ok := p.css[rel]; !ok {
		return "", false
	}

	return rel, true
}

package design

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
)

// maxKeptCSSBytes bounds the reflowed CSS one Gather writes.
const maxKeptCSSBytes = 8 << 20

// Names and versions come from untrusted lockfiles and end up in a directory
// name, so only npm's own character sets pass.
var (
	pkgNameRE    = regexp.MustCompile(`^(@[a-z0-9-~][a-z0-9-._~]*/)?[a-z0-9-~][a-z0-9-._~]*$`)
	pkgVersionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+-]*$`)
)

// keptPkg is a package fetched once per Gather, whichever roots reach it.
type keptPkg struct {
	name, version, dir, registry string
	verified                     bool
	files                        *pkgFiles
	failed                       string            // gap reason when it could not be fetched or read
	keep                         map[string]bool   // files reached by any root
	images                       []string          // picked image files, written beside the kept CSS
	written                      map[string]bool   // the picked images that were written
	decls                        map[string][]decl // rootDecls by file
	firstRoot, firstImport       string
}

func (p *keptPkg) rootDecls(file string) []decl {
	d, ok := p.decls[file]
	if !ok {
		d = rootDecls(p.files.css[file])
		p.decls[file] = d
	}

	return d
}

// visit walks the package l from the file subpath names; spec is the import
// that reached it.
func (s *gathering) visit(ctx context.Context, w *rootWalk, l locked, spec, subpath string) error {
	p, reason, err := s.pkg(ctx, w, l, spec)
	if err != nil {
		return err
	}

	if reason != "" {
		s.gap(w.path, spec, reason)

		return nil
	}

	start, ok := p.files.resolve(subpath)
	if !ok {
		s.gap(w.path, spec, fmt.Sprintf("%s is not a file in %s@%s", cmp.Or(subpath, "."), p.name, p.version))

		return nil
	}

	return s.walk(ctx, w, p, l, start)
}

// walk keeps the files start reaches in p and adds their declarations to the
// root's in import order.
func (s *gathering) walk(ctx context.Context, w *rootWalk, p *keptPkg, l locked, start string) error {
	if w.seen[p.dir+"/"+start] {
		return nil
	}

	w.reached[p.dir] = true

	for _, file := range p.files.closure([]string{start}) {
		p.keep[file] = true
	}

	return w.importOrder(p.files, p.dir, start, p.rootDecls, func(b entry) error {
		return s.bare(ctx, w, p, l, b)
	}, nil)
}

// importOrder adds the declarations of file and of everything it imports to
// w.decls in import order: depth first in source order, a file after its
// imports, each once. files holds file and its relative imports, and dir, its
// tool-root-relative directory, keys them in w.seen; pkg gets each package
// import where it is imported. local, when not nil, resolves an import that is
// no file of files: unhandled, it goes on to pkg; handled with rel "", it was
// a gap.
func (w *rootWalk) importOrder(files *pkgFiles, dir, file string, declsOf func(string) []decl, pkg func(entry) error,
	local func(file, spec string) (rel string, handled bool),
) error {
	if w.seen[dir+"/"+file] {
		return nil
	}

	w.seen[dir+"/"+file] = true

	for _, spec := range cssImports(files.css[file]) {
		rel, ok := files.sibling(file, spec)
		if !ok && local != nil {
			if rel, ok = local(file, spec); ok && rel == "" {
				continue
			}
		}

		if ok {
			if err := w.importOrder(files, dir, rel, declsOf, pkg, local); err != nil {
				return err
			}

			continue
		}

		if name, subpath, ok := splitBare(spec); ok {
			if err := pkg(entry{File: file, Spec: spec, Name: name, Subpath: subpath}); err != nil {
				return err
			}
		}
	}

	w.decls = append(w.decls, declsOf(file)...)

	return nil
}

// bare resolves a package import found in parent's CSS through parent's
// declarations and the lockfile.
func (s *gathering) bare(ctx context.Context, w *rootWalk, parent *keptPkg, pl locked, b entry) error {
	if _, ok := parent.files.declares(b.Name); !ok {
		s.gap(w.path, b.Spec, "not declared in "+parent.dir+"/package.json")

		return nil
	}

	l, ok := w.lock.fromPackage(pl, b.Name)
	if !ok {
		s.gap(w.path, b.Spec, "the lockfile has no entry for it")

		return nil
	}

	return s.visit(ctx, w, l, b.Spec, b.Subpath)
}

// pkg returns the package l, fetched and read once per Gather, or the reason
// it cannot be had.
func (s *gathering) pkg(ctx context.Context, w *rootWalk, l locked, spec string) (*keptPkg, string, error) {
	switch {
	case l.Local:
		return nil, "is a package in this repository; read it in the tree", nil
	case l.NotRegistry, l.Resolved != "" && !isHTTPURL(l.Resolved):
		return nil, "is not installed from a registry", nil
	case !pkgNameRE.MatchString(l.Name) || !pkgVersionRE.MatchString(l.Version):
		return nil, "has a name or version the architect will not use", nil
	case w.regErr != nil:
		return nil, "the registry config could not be read", nil
	}

	registry, err := w.regs.forPackage(l.Name)
	if err != nil {
		return nil, "the registry url uses an environment variable", nil
	}

	integrity := ""
	if w.lock.checksummed() {
		integrity = l.Integrity
	}

	rawURL, ok := pickURL(registry, l, parseIntegrity(integrity) != nil)
	if !ok {
		return nil, "is not installed from a registry", nil
	}

	dir := PackageDir(l.Name, l.Version)
	if p, ok := s.pkgs[dir]; ok {
		return p, p.failed, nil
	}

	if err := ctx.Err(); err != nil {
		return nil, "", fmt.Errorf("design: gather: %w", err)
	}

	p := &keptPkg{
		name: l.Name, version: l.Version, dir: dir,
		keep: map[string]bool{}, decls: map[string][]decl{},
		firstRoot: w.path, firstImport: spec,
	}
	s.pkgs[dir] = p
	s.order = append(s.order, p)

	if u, err := url.Parse(rawURL); err == nil {
		p.registry = u.Host
	}

	fetchCtx, cancel := context.WithDeadline(ctx, s.fetchBy)
	tgz, verified, err := s.g.tarball(fetchCtx, rawURL, integrity)

	outOfTime := fetchCtx.Err() != nil

	cancel()

	if err != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("design: gather: %w", ctx.Err())
		}

		var gap gapError

		switch {
		case errors.As(err, &gap):
			p.failed = gap.reason
		case outOfTime:
			p.failed = "the downloads ran out of time"
		default:
			slog.Warn("design: fetch package", "package", dir, "error", err)

			p.failed = "the registry could not be reached"
		}

		return p, p.failed, nil
	}

	p.verified = verified

	p.files, err = readPackage(ctx, tgz, int64(s.imageCap))
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("design: gather: %w", ctx.Err())
		}

		slog.Warn("design: read package", "package", dir, "error", err)

		p.failed = "the package archive could not be read"

		return p, p.failed, nil
	}

	return p, "", nil
}

func isHTTPURL(s string) bool {
	u, err := url.Parse(s)

	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

// write keeps the reached files and the picked images of each package, in the
// order the packages were first reached, while the kept CSS stays within
// maxKeptCSSBytes; images do not count toward it.
func (s *gathering) write(ctx context.Context) ([]Package, error) {
	var (
		out   []Package
		total int
	)

	for _, p := range s.order {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("design: gather: %w", err)
		}

		if p.files == nil || len(p.keep) == 0 {
			continue
		}

		files := slices.Sorted(maps.Keys(p.keep))
		contents := make([]string, 0, len(files))
		size := 0

		for _, f := range files {
			if total+size > maxKeptCSSBytes {
				break
			}

			css := reflow(p.files.css[f])
			size += len(css)
			contents = append(contents, css)
		}

		if total+size > maxKeptCSSBytes {
			s.gap(p.firstRoot, p.firstImport, "the kept CSS would pass 8 MiB")

			continue
		}

		total += size

		if err := s.writePkg(p, files, contents); err != nil {
			return nil, err
		}

		kept := make([]string, len(files))
		for i, f := range files {
			kept[i] = path.Join(p.dir, f)
		}

		out = append(out, Package{
			Name: p.name, Version: p.version, Dir: p.dir, Registry: p.registry,
			Verified: p.verified, Files: kept,
		})
	}

	return out, nil
}

// writePkg replaces the package's directory under the tool root with files and
// the package's picked images. An image the file system refuses, as a package
// archive can name a path it cannot hold, is skipped and left out of p.written.
func (s *gathering) writePkg(p *keptPkg, files, contents []string) error {
	dst := filepath.Join(s.root, p.dir)
	if filepath.Dir(dst) != filepath.Clean(s.root) || filepath.Base(dst) != p.dir {
		return fmt.Errorf("design: gather: package dir %q is not directly under the tool root", p.dir)
	}

	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("design: gather: replace %s: %w", p.dir, err)
	}

	put := func(rel string, data []byte) error {
		file := filepath.Join(dst, filepath.FromSlash(rel))

		if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
			return err
		}

		return os.WriteFile(file, data, 0o600)
	}

	for i, f := range files {
		if err := put(f, []byte(contents[i])); err != nil {
			return fmt.Errorf("design: gather: write %s: %w", p.dir, err)
		}
	}

	p.written = map[string]bool{}

	for _, rel := range p.images {
		if err := put(rel, p.files.images[rel]); err != nil {
			slog.Warn("design: skip package image", "package", p.dir, "image", rel, "error", err)

			continue
		}

		p.written[rel] = true
	}

	return nil
}

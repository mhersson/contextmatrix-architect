package design

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// reasonNoFile is the gap reason for an import of a file the tree does not
// hold.
const reasonNoFile = "no such file in the repository"

// reasonUnreadable is the gap reason for a tree file readInTree refuses.
func reasonUnreadable() string {
	return fmt.Sprintf("a symbolic link, not a regular file, over %d MiB or unreadable", maxKeptFileBytes>>20)
}

// readInTree reads the file at rel, slash-separated and relative to treeDir.
// The tree is untrusted: a path that leaves it, a symlink anywhere on the
// path, a file that is not regular and one over maxKeptFileBytes are refused.
func readInTree(treeDir, rel string) (string, error) {
	local := filepath.FromSlash(rel)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("design: read %s: the path leaves the tree", rel)
	}

	var (
		p    = treeDir
		info fs.FileInfo
	)

	for _, part := range strings.Split(filepath.Clean(local), string(filepath.Separator)) {
		p = filepath.Join(p, part)

		var err error
		if info, err = os.Lstat(p); err != nil {
			return "", fmt.Errorf("design: read %s: %w", rel, err)
		}

		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("design: read %s: %s is a symbolic link", rel, part)
		}
	}

	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("design: read %s: not a regular file", rel)
	}

	f, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("design: read %s: %w", rel, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxKeptFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("design: read %s: %w", rel, err)
	}

	if len(data) > maxKeptFileBytes {
		return "", fmt.Errorf("design: read %s: more than %d bytes", rel, maxKeptFileBytes)
	}

	return string(data), nil
}

// ownTree reads, from the rest of the tree, a CSS file the root's own files
// import that is not one of its own CSS files, such as a workspace package's
// or one under node_modules. files is the root's own; each file read is added
// to it.
type ownTree struct {
	treeDir string
	files   *pkgFiles
	gap     func(spec, reason string)
}

// local resolves an import in the root's file that sibling did not find.
// handled is false for a spec left to the package walk; rel is "" when the
// import was a gap.
func (o *ownTree) local(file, spec string) (rel string, handled bool) {
	if spec == "" || spec[0] == '/' || spec[0] == '~' || schemeRE.MatchString(spec) {
		return "", false
	}

	if spec[0] != '.' && !isLocalFile(o.treeDir, file, spec) {
		return "", false
	}

	target := path.Join(path.Dir(file), spec)
	if !o.read(target, spec) {
		return "", true
	}

	return target, true
}

// read adds the tree file rel to o.files unless it is there, or gaps spec when
// it cannot be read; it reports whether rel is in o.files.
func (o *ownTree) read(rel, spec string) bool {
	if _, ok := o.files.css[rel]; ok {
		return true
	}

	if !filepath.IsLocal(rel) {
		o.gap(spec, "the path leaves the repository")

		return false
	}

	if err := o.load(rel); err != nil {
		reason := reasonUnreadable()
		if errors.Is(err, fs.ErrNotExist) {
			reason = reasonNoFile
		}

		o.gap(spec, reason)

		return false
	}

	return true
}

// load adds the tree file rel to o.files unless it is there.
func (o *ownTree) load(rel string) error {
	if _, ok := o.files.css[rel]; ok {
		return nil
	}

	css, err := readInTree(o.treeDir, rel)
	if err != nil {
		return err
	}

	o.files.css[rel] = css

	return nil
}

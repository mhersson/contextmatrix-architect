package design

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// locked is a package as the lockfile records it; pos is opaque and only the
// lockfile that produced it reads it.
type locked struct {
	Name, Version, Resolved, Integrity string
	Local                              bool // a workspace or link package inside the repository
	NotRegistry                        bool // a git, tarball, file, directory or patched package
	pos                                string
}

type lockfile interface {
	// fromRoot resolves a dependency declared by the frontend root at dir
	// (relative to the lockfile's directory) with rangeSpec, the value in its
	// package.json.
	fromRoot(dir, name, rangeSpec string) (locked, bool)
	// fromPackage resolves a dependency of an already resolved package.
	fromPackage(parent locked, name string) (locked, bool)
	// checksummed reports whether Integrity is the npm tarball integrity.
	checksummed() bool
}

// readLockfile reads the lockfile in dir: package-lock.json, yarn.lock or
// pnpm-lock.yaml, first found in that order; nil when none.
func readLockfile(dir string) (lockfile, error) {
	for _, name := range []string{"package-lock.json", "yarn.lock", "pnpm-lock.yaml"} {
		path := filepath.Join(dir, name)

		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, fmt.Errorf("design: stat %s: %w", name, err)
		}

		switch name {
		case "package-lock.json":
			return readNPMLock(path)
		case "yarn.lock":
			return readYarnLock(path)
		case "pnpm-lock.yaml":
			return readPNPMLock(path)
		}
	}

	return nil, nil
}

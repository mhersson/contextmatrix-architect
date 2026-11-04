package design

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
)

type npmLock struct {
	packages map[string]npmLockEntry
}

type npmLockEntry struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Resolved  string `json:"resolved"`
	Integrity string `json:"integrity"`
	Link      bool   `json:"link"`
}

func readNPMLock(file string) (lockfile, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("design: read package-lock.json: %w", err)
	}

	var doc struct {
		Packages map[string]npmLockEntry `json:"packages"`
	}

	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("design: parse package-lock.json: %w", err)
	}

	return &npmLock{packages: doc.Packages}, nil
}

func (l *npmLock) fromRoot(dir, name, _ string) (locked, bool) {
	return l.resolve(dir, name)
}

func (l *npmLock) fromPackage(parent locked, name string) (locked, bool) {
	return l.resolve(parent.pos, name)
}

func (*npmLock) checksummed() bool { return true }

// resolve finds name the way node does: in base/node_modules, then in each
// parent directory's node_modules up to the tree root's.
func (l *npmLock) resolve(base, name string) (locked, bool) {
	for dir := path.Clean(base); ; dir = path.Dir(dir) {
		if dir == "." {
			dir = ""
		}

		key := path.Join(dir, "node_modules", name)
		if entry, ok := l.packages[key]; ok {
			// A name that climbs out of node_modules (../frontend) lands on a
			// key that is not a package of that name.
			if keyTail(key) != name {
				return locked{}, false
			}

			// An alias is keyed by the alias; the entry carries the real name.
			realName := name
			if entry.Name != "" {
				realName = entry.Name
			}

			return locked{
				Name:      realName,
				Version:   entry.Version,
				Resolved:  entry.Resolved,
				Integrity: entry.Integrity,
				Local:     entry.Link,
				pos:       key,
			}, true
		}

		if dir == "" {
			return locked{}, false
		}
	}
}

// keyTail is the package name a packages key ends in: the text after its last
// node_modules/, or the whole key when it has none.
func keyTail(key string) string {
	if i := strings.LastIndex(key, "node_modules/"); i >= 0 {
		return key[i+len("node_modules/"):]
	}

	return key
}

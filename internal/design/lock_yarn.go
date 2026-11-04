package design

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// yarnLock serves both yarn.lock formats; entries is keyed by descriptor, so
// the descriptors of one header share one entry.
type yarnLock struct {
	berry   bool
	entries map[string]*yarnEntry
}

type yarnEntry struct {
	Version      string            `yaml:"version"`
	Resolution   string            `yaml:"resolution"`
	Dependencies map[string]string `yaml:"dependencies"`
	resolved     string
	integrity    string
}

func readYarnLock(file string) (lockfile, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("design: read yarn.lock: %w", err)
	}

	// Berry is YAML; classic is not, so the key is found by text.
	if bytes.HasPrefix(data, []byte("__metadata:")) || bytes.Contains(data, []byte("\n__metadata:")) {
		var doc map[string]*yarnEntry
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("design: parse yarn.lock: %w", err)
		}

		entries := make(map[string]*yarnEntry, len(doc))

		for key, entry := range doc {
			if key == "__metadata" || entry == nil {
				continue
			}

			for _, descriptor := range strings.Split(key, ",") {
				entries[strings.TrimSpace(descriptor)] = entry
			}
		}

		return &yarnLock{berry: true, entries: entries}, nil
	}

	return &yarnLock{entries: parseYarnClassic(string(data))}, nil
}

func (l *yarnLock) fromRoot(_, name, rangeSpec string) (locked, bool) {
	return l.resolve(name, rangeSpec)
}

func (l *yarnLock) fromPackage(parent locked, name string) (locked, bool) {
	entry := l.entries[parent.pos]
	if entry == nil {
		return locked{}, false
	}

	rangeSpec, ok := entry.Dependencies[name]
	if !ok {
		return locked{}, false
	}

	return l.resolve(name, rangeSpec)
}

// checksummed is false for Berry: its checksum covers yarn's own archive, not
// the npm tarball.
func (l *yarnLock) checksummed() bool { return !l.berry }

func (l *yarnLock) resolve(name, rangeSpec string) (locked, bool) {
	descriptor := name + "@" + rangeSpec
	if l.berry && !strings.Contains(rangeSpec, ":") {
		descriptor = name + "@npm:" + rangeSpec
	}

	entry := l.entries[descriptor]
	if entry == nil {
		return locked{}, false
	}

	// An alias is recorded under the alias; the real name is in the
	// resolution (Berry) or the descriptor's npm: range (classic).
	realName := name
	local := false

	if l.berry {
		n := yarnDescriptorName(entry.Resolution)
		if n == "" {
			return locked{}, false
		}

		// Only a registry package or a package inside the repository can be
		// told apart from its name and version; git, tarball, patch: and
		// file: resolutions are some other package.
		protocol := strings.TrimPrefix(entry.Resolution, n+"@")
		registry := "npm:" + entry.Version

		switch {
		case protocol == registry || strings.HasPrefix(protocol, registry+"::"):
		case strings.HasPrefix(protocol, "workspace:"),
			strings.HasPrefix(protocol, "link:"),
			strings.HasPrefix(protocol, "portal:"):
			local = true
		case strings.HasPrefix(protocol, "npm:"):
			// A registry resolution of another version is not this entry's package.
			return locked{}, false
		default:
			return locked{Name: n, Version: entry.Version, NotRegistry: true}, true
		}

		realName = n
	} else if target, ok := strings.CutPrefix(rangeSpec, "npm:"); ok && target != "" {
		// npm:<name> without a range names the package whole.
		realName = target
		if n := yarnDescriptorName(target); n != "" {
			realName = n
		}
	}

	return locked{
		Name:      realName,
		Version:   entry.Version,
		Resolved:  entry.resolved,
		Integrity: entry.integrity,
		Local:     local,
		pos:       descriptor,
	}, true
}

// yarnDescriptorName is the package name of a "name@range" string, or "" when
// it has no range.
func yarnDescriptorName(s string) string {
	// A scope's leading @ belongs to the name.
	start := 0
	if strings.HasPrefix(s, "@") {
		start = 1
	}

	i := strings.Index(s[start:], "@")
	if i < 0 {
		return ""
	}

	return s[:start+i]
}

func parseYarnClassic(data string) map[string]*yarnEntry {
	entries := map[string]*yarnEntry{}

	var (
		cur    *yarnEntry
		inDeps bool
	)

	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")

		trimmed := strings.TrimLeft(line, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		switch len(line) - len(trimmed) {
		case 0:
			cur, inDeps = nil, false

			header, ok := strings.CutSuffix(trimmed, ":")
			if !ok {
				continue
			}

			cur = &yarnEntry{}

			for s := strings.TrimSpace(header); s != ""; {
				var descriptor string

				descriptor, s = yarnToken(s)
				if descriptor != "" {
					entries[descriptor] = cur
				}

				s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), ","))
			}
		case 2:
			if cur == nil {
				continue
			}

			key, value := yarnKeyValue(trimmed)
			inDeps = key == "dependencies:"

			switch key {
			case "version":
				cur.Version = value
			case "resolved":
				cur.resolved = value
			case "integrity":
				cur.integrity = value
			}
		case 4:
			if cur == nil || !inDeps {
				continue
			}

			if cur.Dependencies == nil {
				cur.Dependencies = map[string]string{}
			}

			name, rangeSpec := yarnKeyValue(trimmed)
			cur.Dependencies[name] = rangeSpec
		}
	}

	return entries
}

func yarnKeyValue(s string) (key, value string) {
	key, rest := yarnToken(s)
	value, _ = yarnToken(strings.TrimSpace(rest))

	return key, value
}

// yarnToken reads one token: a double-quoted string, or text up to the next
// space or comma.
func yarnToken(s string) (token, rest string) {
	if strings.HasPrefix(s, `"`) {
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				i++
			case '"':
				if unquoted, err := strconv.Unquote(s[:i+1]); err == nil {
					return unquoted, s[i+1:]
				}

				return s[1:i], s[i+1:]
			}
		}
	}

	if end := strings.IndexAny(s, " ,"); end >= 0 {
		return s[:end], s[end:]
	}

	return s, ""
}

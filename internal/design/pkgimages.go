package design

import (
	"cmp"
	"maps"
	"path"
	"slices"
	"strings"
)

// maxPackageImages bounds the images one Gather keeps of a package.
const maxPackageImages = 24

// imageIndex holds image paths and, for each, the words of its file name less
// the extension.
type imageIndex struct {
	paths []string
	words [][]string
}

func newImageIndex(paths []string) imageIndex {
	x := imageIndex{paths: paths, words: make([][]string, len(paths))}

	for i, p := range paths {
		base := path.Base(p)
		x.words[i] = nameWords(strings.TrimSuffix(base, path.Ext(base)))
	}

	return x
}

// matches returns the paths whose file name holds the words of name as a
// contiguous run, fewest words first so that an exact name leads, then by
// comparePaths. It returns nil when name has no words.
func (x imageIndex) matches(name string) []string {
	want := nameWords(name)
	if len(want) == 0 {
		return nil
	}

	var hits []int

	for i, words := range x.words {
		if hasRun(words, want) {
			hits = append(hits, i)
		}
	}

	slices.SortFunc(hits, func(a, b int) int {
		return cmp.Or(cmp.Compare(len(x.words[a]), len(x.words[b])), comparePaths(x.paths[a], x.paths[b]))
	})

	var out []string

	for _, i := range hits {
		out = append(out, x.paths[i])
	}

	return out
}

// hasRun reports whether run is a contiguous part of words.
func hasRun(words, run []string) bool {
	for i := 0; i+len(run) <= len(words); i++ {
		if slices.Equal(words[i:i+len(run)], run) {
			return true
		}
	}

	return false
}

// pickImages takes up to limit paths from ranked in rounds: round k takes the
// k-th path of each list in order, skipping one already taken, so that every
// list is served before any is served twice.
func pickImages(ranked [][]string, limit int) []string {
	var out []string

	for k := 0; len(out) < limit; k++ {
		more := false

		for _, list := range ranked {
			if k >= len(list) {
				continue
			}

			more = true

			if slices.Contains(out, list[k]) {
				continue
			}

			out = append(out, list[k])

			if len(out) == limit {
				return out
			}
		}

		if !more {
			break
		}
	}

	return out
}

// pickPackageImages sets the images each wanted package keeps, from every name
// the wants give it, and returns each root's images by Root.Path: the matches
// of its wants' names a package keeps, in want order, once each. Only a package
// the CSS walk fetched and reached gives images; none is fetched for them.
func (s *gathering) pickPackageImages() map[string][]Asset {
	var dirs []string

	names := map[string][]string{} // by dir

	for _, w := range s.wants {
		list, seen := names[w.dir]
		if !seen {
			dirs = append(dirs, w.dir)
		}

		for _, n := range w.names {
			if !slices.Contains(list, n) {
				list = append(list, n)
			}
		}

		names[w.dir] = list
	}

	ranked := map[string]map[string][]string{} // by dir, then name

	for _, dir := range dirs {
		p := s.pkgs[dir]
		if p == nil || p.files == nil || len(p.keep) == 0 || len(p.files.images) == 0 {
			continue
		}

		x := newImageIndex(slices.Sorted(maps.Keys(p.files.images)))
		byName := make(map[string][]string, len(names[dir]))
		lists := make([][]string, len(names[dir]))

		for i, n := range names[dir] {
			lists[i] = x.matches(n)
			byName[n] = lists[i]
		}

		p.images = pickImages(lists, maxPackageImages)
		ranked[dir] = byName
	}

	type rootImage struct{ root, path string }

	out := map[string][]Asset{}
	listed := map[rootImage]bool{}

	for _, w := range s.wants {
		p := s.pkgs[w.dir]

		for _, n := range w.names {
			// ranked holds w.dir only when p was picked from, so p is set here.
			for _, rel := range ranked[w.dir][n] {
				key := rootImage{w.root, path.Join(w.dir, rel)}

				if !slices.Contains(p.images, rel) || listed[key] {
					continue
				}

				listed[key] = true
				out[w.root] = append(out[w.root], Asset{Path: key.path, Size: int64(len(p.files.images[rel]))})
			}
		}
	}

	return out
}

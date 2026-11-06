package design

import (
	"cmp"
	"maps"
	"path"
	"slices"
	"strings"
)

// maxPackageImages bounds the images one frontend root keeps of a package.
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

// foldCaseDuplicates drops from sorted every path equal to an earlier one when
// lower-cased: on a case-insensitive file system they are one file.
func foldCaseDuplicates(sorted []string) []string {
	seen := make(map[string]bool, len(sorted))
	out := make([]string, 0, len(sorted))

	for _, p := range sorted {
		folded := strings.ToLower(p)
		if seen[folded] {
			continue
		}

		seen[folded] = true

		out = append(out, p)
	}

	return out
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

// pickPackageImages sets the images each wanted package keeps and returns each
// root's images by Root.Path. A root picks up to maxPackageImages of a package
// from the names its wants give it, so one root's names never take another's
// share; the package keeps the union of its roots' picks, and a root lists its
// own, in name order, once each. Only a package the CSS walk fetched and reached
// gives images; none is fetched for them.
func (s *gathering) pickPackageImages() map[string][]Asset {
	type rootPkg struct{ root, dir string }

	var order []rootPkg

	names := map[rootPkg][]string{}

	for _, w := range s.wants {
		k := rootPkg{w.root, w.dir}
		list, seen := names[k]

		if !seen {
			order = append(order, k)
		}

		for _, n := range w.names {
			if !slices.Contains(list, n) {
				list = append(list, n)
			}
		}

		names[k] = list
	}

	indexes := map[string]imageIndex{} // by dir
	out := map[string][]Asset{}

	for _, k := range order {
		p := s.pkgs[k.dir]
		if p == nil || p.files == nil || len(p.keep) == 0 || len(p.files.images) == 0 {
			continue
		}

		x, ok := indexes[k.dir]
		if !ok {
			x = newImageIndex(foldCaseDuplicates(slices.Sorted(maps.Keys(p.files.images))))
			indexes[k.dir] = x
		}

		lists := make([][]string, len(names[k]))
		for i, n := range names[k] {
			lists[i] = x.matches(n)
		}

		picked := pickImages(lists, maxPackageImages)

		for _, rel := range picked {
			if !slices.Contains(p.images, rel) {
				p.images = append(p.images, rel)
			}
		}

		listed := map[string]bool{}

		for _, list := range lists {
			for _, rel := range list {
				if !slices.Contains(picked, rel) || listed[rel] {
					continue
				}

				listed[rel] = true
				out[k.root] = append(out[k.root], Asset{Path: path.Join(k.dir, rel), Size: int64(len(p.files.images[rel]))})
			}
		}
	}

	return out
}

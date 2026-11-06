package design

import (
	"cmp"
	"path"
	"slices"
	"strings"
)

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

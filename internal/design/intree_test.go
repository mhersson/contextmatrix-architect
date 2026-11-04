package design

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadInTree(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, "tree")
	outside := t.TempDir()

	writeTree(t, base, map[string]string{"x.css": "a{}"})
	writeTree(t, outside, map[string]string{"evil.css": "a{}"})
	writeTree(t, tree, map[string]string{
		"src/a.css":     ":root{--a:#111111}",
		"real/b.css":    "b{}",
		"src/dir.css/x": "",
	})
	require.NoError(t, os.Symlink(filepath.Join(outside, "evil.css"), filepath.Join(tree, "src", "link.css")))
	require.NoError(t, os.Symlink(filepath.Join(tree, "real"), filepath.Join(tree, "linked")))

	sized := func(name string, size int64) {
		file := filepath.Join(tree, "src", name)
		require.NoError(t, os.WriteFile(file, nil, 0o600))
		require.NoError(t, os.Truncate(file, size))
	}
	sized("cap.css", maxKeptFileBytes)
	sized("big.css", maxKeptFileBytes+1)

	tests := []struct {
		name    string
		rel     string
		wantLen int
		wantErr bool
	}{
		{name: "a regular file", rel: "src/a.css", wantLen: len(":root{--a:#111111}")},
		{name: "a file at the cap", rel: "src/cap.css", wantLen: maxKeptFileBytes},
		{name: "a path that leaves the tree", rel: "../x.css", wantErr: true},
		{name: "a missing file", rel: "src/missing.css", wantErr: true},
		{name: "a directory", rel: "src/dir.css", wantErr: true},
		{name: "a symlinked file", rel: "src/link.css", wantErr: true},
		{name: "a file under a symlinked directory", rel: "linked/b.css", wantErr: true},
		{name: "a file over the cap", rel: "src/big.css", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			css, err := readInTree(tree, tc.rel)
			if tc.wantErr {
				assert.Error(t, err)

				return
			}

			require.NoError(t, err)
			assert.Len(t, css, tc.wantLen)
		})
	}
}

func TestOwnTree_Local(t *testing.T) {
	const file = "app/src/index.css"

	tree := t.TempDir()
	writeTree(t, tree, map[string]string{
		file:                  "",
		"app/src/sub/x.css":   ":root{--x:#111111}",
		"app/other/sub/y.css": "",
		"app/vendor/v.css":    ":root{--v:#222222}",
	})

	tests := []struct {
		name    string
		spec    string
		want    string // the file read; "" for none
		handled bool
		gap     bool
	}{
		{name: "a relative import is read", spec: "../vendor/v.css", want: "app/vendor/v.css", handled: true},
		{name: "a bare-looking import of a file beside it is read", spec: "sub/x.css", want: "app/src/sub/x.css", handled: true},
		{name: "a bare-looking import of no such file is a package's", spec: "sub/y.css"},
		{name: "a relative import of no such file is a gap", spec: "./missing.css", handled: true, gap: true},
		{name: "a relative import that leaves the tree is a gap", spec: "../../../x.css", handled: true, gap: true},
		{name: "a webpack package import", spec: "~bootstrap/dist/css/bootstrap.css"},
		{name: "an absolute path", spec: "/x.css"},
		{name: "a url", spec: "https://cdn.example.test/x.css"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			own := &pkgFiles{css: map[string]string{file: ""}}

			var gaps []string

			o := &ownTree{treeDir: tree, files: own, gap: func(spec, reason string) {
				assert.NotEmpty(t, reason)

				gaps = append(gaps, spec)
			}}

			rel, handled := o.local(file, tc.spec)
			assert.Equal(t, tc.want, rel)
			assert.Equal(t, tc.handled, handled)

			if tc.gap {
				assert.Equal(t, []string{tc.spec}, gaps)
			} else {
				assert.Empty(t, gaps)
			}

			if tc.want != "" {
				assert.Contains(t, own.css, tc.want)
			}
		})
	}
}

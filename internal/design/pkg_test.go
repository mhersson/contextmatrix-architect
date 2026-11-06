package design

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	link     string
}

// tgzEntries builds a gzipped tarball from entries in order, including entry
// kinds tgz cannot write.
func tgzEntries(t *testing.T, entries []tarEntry) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, e := range entries {
		typeflag := e.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}

		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: typeflag, Linkname: e.link}
		if typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}

		require.NoError(t, tw.WriteHeader(hdr))

		if typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.body))
			require.NoError(t, err)
		}
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	return buf.Bytes()
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o750))

	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, data, 0o600))

	return p
}

func TestReadPackage(t *testing.T) {
	root := t.TempDir()
	in := filepath.Join(root, "in")

	archive := tgzEntries(t, []tarEntry{
		{name: "package/package.json", body: `{"name":"@acme/ui","dependencies":{"@acme/tokens":"^2.0.0"}}`},
		{name: "package/style/core.css", body: `a{b:c}`},
		{name: "package/style/x.js", body: `export {}`},
		{name: "package/package.json.d/package.json", body: `{"name":"nested"}`},
		{name: "package/../../evil.css", body: `a{}`},
		{name: "package/a/../../evil2.css", body: `a{}`},
		{name: "/package/abs.css", body: `a{}`},
		{name: "package/style/link.css", typeflag: tar.TypeSymlink, link: "../../../etc/passwd"},
		{name: "package/style/hard.css", typeflag: tar.TypeLink, link: "package/style/core.css"},
		{name: "package/style/dir.css/", typeflag: tar.TypeDir},
	})
	tarball := writeFile(t, in, "pkg.tgz", archive)

	p, err := readPackage(t.Context(), tarball, 0)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"style/core.css": `a{b:c}`}, p.css)
	assert.Equal(t, "@acme/ui", p.manifest["name"])

	rng, ok := p.declares("@acme/tokens")
	assert.True(t, ok)
	assert.Equal(t, "^2.0.0", rng)

	var found []string

	require.NoError(t, filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		if rel != "." {
			found = append(found, filepath.ToSlash(rel))
		}

		return nil
	}))
	assert.Equal(t, []string{"in", "in/pkg.tgz"}, found, "reading must not write anything")
}

func TestReadPackage_Errors(t *testing.T) {
	tests := []struct {
		name    string
		archive []byte
		raw     []byte
	}{
		{
			name:    "malformed manifest",
			archive: tgz(t, map[string]string{"package/package.json": `{not json`}),
		},
		{
			name: "not gzip",
			raw:  []byte("plain text"),
		},
		{
			name: "truncated archive",
			raw:  tgz(t, map[string]string{"package/a.css": strings.Repeat("x", 4000)})[:40],
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.archive
			if data == nil {
				data = tt.raw
			}

			p, err := readPackage(t.Context(), writeFile(t, t.TempDir(), "pkg.tgz", data), 0)
			require.Error(t, err)
			assert.Nil(t, p)
		})
	}

	t.Run("missing file", func(t *testing.T) {
		_, err := readPackage(t.Context(), filepath.Join(t.TempDir(), "absent.tgz"), 0)
		require.Error(t, err)
	})
}

func TestReadPackage_NoManifest(t *testing.T) {
	p, err := readPackage(t.Context(), writeFile(t, t.TempDir(), "pkg.tgz", tgz(t, map[string]string{"package/a.css": `a{}`})), 0)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"a.css": `a{}`}, p.css)

	_, ok := p.declares("@acme/tokens")
	assert.False(t, ok)
}

func TestReadPackage_CancelledContext(t *testing.T) {
	path := writeFile(t, t.TempDir(), "pkg.tgz", tgz(t, map[string]string{"package/a.css": `a{}`}))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	p, err := readPackage(ctx, path, 0)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, p)
}

func TestReadPackageLimits(t *testing.T) {
	limits := readLimits{file: 16, total: 8 << 10, css: 24, imageFile: 8, images: 12}

	tests := []struct {
		name        string
		files       map[string]string
		noImageFile bool // runs the row with imageFile 0
		wantCSS     map[string]string
		wantImages  map[string]string
		wantErr     bool
	}{
		{
			name:    "within every bound",
			files:   map[string]string{"package/a.css": strings.Repeat("a", 12), "package/b.css": strings.Repeat("b", 10)},
			wantCSS: map[string]string{"a.css": strings.Repeat("a", 12), "b.css": strings.Repeat("b", 10)},
		},
		{
			name:    "css file over the per-file bound is skipped",
			files:   map[string]string{"package/big.css": strings.Repeat("x", 17), "package/ok.css": "a{}"},
			wantCSS: map[string]string{"ok.css": "a{}"},
		},
		{
			name:    "css file at the per-file bound is kept",
			files:   map[string]string{"package/at.css": strings.Repeat("x", 16)},
			wantCSS: map[string]string{"at.css": strings.Repeat("x", 16)},
		},
		{
			name:    "css total over the bound",
			files:   map[string]string{"package/a.css": strings.Repeat("a", 16), "package/b.css": strings.Repeat("b", 9)},
			wantErr: true,
		},
		{
			name:    "uncompressed total over the bound counts skipped files",
			files:   map[string]string{"package/a.css": "a{}", "package/blob.bin": strings.Repeat("z", 16<<10)},
			wantErr: true,
		},
		{
			name:    "oversized css counts toward the uncompressed total",
			files:   map[string]string{"package/blob.css": strings.Repeat("z", 16<<10)},
			wantErr: true,
		},
		{
			name:       "image kept beside css",
			files:      map[string]string{"package/icons/heart.svg": "<svg/>", "package/a.css": "a{}"},
			wantCSS:    map[string]string{"a.css": "a{}"},
			wantImages: map[string]string{"icons/heart.svg": "<svg/>"},
		},
		{
			name:       "image extension is matched without case and the path keeps its case",
			files:      map[string]string{"package/icons/Heart.SVG": "<svg/>"},
			wantCSS:    map[string]string{},
			wantImages: map[string]string{"icons/Heart.SVG": "<svg/>"},
		},
		{
			name:       "image over the per-image bound is skipped",
			files:      map[string]string{"package/big.png": strings.Repeat("x", 9)},
			wantCSS:    map[string]string{},
			wantImages: map[string]string{},
		},
		{
			name:       "image over the held total is skipped and the read goes on",
			files:      map[string]string{"package/a.png": strings.Repeat("a", 8), "package/b.png": strings.Repeat("b", 8), "package/c.svg": "<s/>"},
			wantCSS:    map[string]string{},
			wantImages: map[string]string{"a.png": strings.Repeat("a", 8), "c.svg": "<s/>"},
		},
		{
			name: "image under node_modules or .git is skipped",
			files: map[string]string{
				"package/node_modules/x/heart.svg": "<svg/>", "package/.git/heart.svg": "<svg/>", "package/Node_Modules/y.svg": "<svg/>",
			},
			wantCSS:    map[string]string{},
			wantImages: map[string]string{},
		},
		{
			name:       "empty image is not held",
			files:      map[string]string{"package/empty.svg": "", "package/heart.svg": "<svg/>"},
			wantCSS:    map[string]string{},
			wantImages: map[string]string{"heart.svg": "<svg/>"},
		},
		{
			name:       "file that is not an image is not held",
			files:      map[string]string{"package/notes.txt": "hello"},
			wantCSS:    map[string]string{},
			wantImages: map[string]string{},
		},
		{
			name:        "no image is held without a per-image bound",
			files:       map[string]string{"package/heart.svg": "<svg/>"},
			noImageFile: true,
			wantCSS:     map[string]string{},
			wantImages:  map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, t.TempDir(), "pkg.tgz", tgz(t, tt.files))

			lim := limits
			if tt.noImageFile {
				lim.imageFile = 0
			}

			p, err := readPackageLimited(t.Context(), path, lim)
			if tt.wantErr {
				require.Error(t, err)
				assert.Nil(t, p)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantCSS, p.css)

			got := map[string]string{}
			for rel, body := range p.images {
				got[rel] = string(body)
			}

			assert.Equal(t, valueOrEmpty(tt.wantImages), got)
		})
	}
}

func valueOrEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}

	return m
}

func TestReadPackage_ImageCap(t *testing.T) {
	path := writeFile(t, t.TempDir(), "pkg.tgz", tgz(t, map[string]string{"package/a.svg": "1234", "package/b.svg": "12345"}))

	p, err := readPackage(t.Context(), path, 4)
	require.NoError(t, err)

	assert.Equal(t, map[string][]byte{"a.svg": []byte("1234")}, p.images)
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		css      []string
		manifest map[string]any
		subpath  string
		want     string
		wantOK   bool
	}{
		{
			name:    "literal file",
			css:     []string{"style/core.css"},
			subpath: "style/core.css",
			want:    "style/core.css",
			wantOK:  true,
		},
		{
			name:     "literal file wins over exports",
			css:      []string{"style/core.css", "dist/style/core.css"},
			manifest: map[string]any{"exports": map[string]any{"./style/*": "./dist/style/*"}},
			subpath:  "style/core.css",
			want:     "style/core.css",
			wantOK:   true,
		},
		{
			name:     "pattern key",
			css:      []string{"dist/style/a.css"},
			manifest: map[string]any{"exports": map[string]any{"./style/*": "./dist/style/*"}},
			subpath:  "style/a.css",
			want:     "dist/style/a.css",
			wantOK:   true,
		},
		{
			name:     "exact key",
			css:      []string{"dist/theme.css"},
			manifest: map[string]any{"exports": map[string]any{"./theme.css": "./dist/theme.css"}},
			subpath:  "theme.css",
			want:     "dist/theme.css",
			wantOK:   true,
		},
		{
			name: "dot export with style condition",
			css:  []string{"index.css"},
			manifest: map[string]any{"exports": map[string]any{
				".": map[string]any{"style": "./index.css", "default": "./index.js"},
			}},
			subpath: "",
			want:    "index.css",
			wantOK:  true,
		},
		{
			name: "style wins over default",
			css:  []string{"s.css", "d.css"},
			manifest: map[string]any{"exports": map[string]any{
				".": map[string]any{"default": "./d.css", "style": "./s.css"},
			}},
			subpath: "",
			want:    "s.css",
			wantOK:  true,
		},
		{
			name: "condition order style default import browser require",
			css:  []string{"d.css", "i.css", "b.css", "r.css"},
			manifest: map[string]any{"exports": map[string]any{
				".": map[string]any{"require": "./r.css", "browser": "./b.css", "import": "./i.css", "default": "./d.css"},
			}},
			subpath: "",
			want:    "d.css",
			wantOK:  true,
		},
		{
			name: "nested conditions",
			css:  []string{"dist/n.css"},
			manifest: map[string]any{"exports": map[string]any{
				"./n.css": map[string]any{"import": map[string]any{"types": "./n.d.ts", "default": "./dist/n.css"}},
			}},
			subpath: "n.css",
			want:    "dist/n.css",
			wantOK:  true,
		},
		{
			name: "condition without a css target is skipped",
			css:  []string{"theme.css"},
			manifest: map[string]any{"exports": map[string]any{
				".": map[string]any{"default": "./index.js", "import": "./theme.css"},
			}},
			subpath: "",
			want:    "theme.css",
			wantOK:  true,
		},
		{
			name:     "array fallback",
			css:      []string{"b.css"},
			manifest: map[string]any{"exports": map[string]any{"./x.css": []any{"./a.css", "./b.css"}}},
			subpath:  "x.css",
			want:     "b.css",
			wantOK:   true,
		},
		{
			name:     "string exports is the dot export",
			css:      []string{"index.css"},
			manifest: map[string]any{"exports": "./index.css"},
			subpath:  "",
			want:     "index.css",
			wantOK:   true,
		},
		{
			name:     "condition exports is the dot export",
			css:      []string{"index.css"},
			manifest: map[string]any{"exports": map[string]any{"style": "./index.css"}},
			subpath:  "",
			want:     "index.css",
			wantOK:   true,
		},
		{
			name: "null condition stops the search",
			css:  []string{"d.css"},
			manifest: map[string]any{"exports": map[string]any{
				".": map[string]any{"style": nil, "default": "./d.css"},
			}},
			subpath: "",
			wantOK:  false,
		},
		{
			name:     "null pattern is not exported",
			css:      []string{"dist/private/a.css"},
			manifest: map[string]any{"exports": map[string]any{"./private/*": nil}},
			subpath:  "private/a.css",
			wantOK:   false,
		},
		{
			name: "longest pattern wins and null blocks",
			css:  []string{"dist/private/a.css"},
			manifest: map[string]any{"exports": map[string]any{
				"./*":         "./dist/*",
				"./private/*": nil,
			}},
			subpath: "private/a.css",
			wantOK:  false,
		},
		{
			name: "pattern with a suffix",
			css:  []string{"dist/a.css"},
			manifest: map[string]any{"exports": map[string]any{
				"./*.css": "./dist/*.css",
			}},
			subpath: "a.css",
			want:    "dist/a.css",
			wantOK:  true,
		},
		{
			name: "pattern key with two stars is ignored",
			css:  []string{"dist/a/b.css"},
			manifest: map[string]any{"exports": map[string]any{
				"./*/*": "./dist/*/*",
			}},
			subpath: "a/b.css",
			wantOK:  false,
		},
		{
			name:     "target that is not a css file",
			css:      []string{"index.css"},
			manifest: map[string]any{"exports": map[string]any{".": "./index.js"}},
			subpath:  "",
			wantOK:   false,
		},
		{
			name:     "target missing from the package",
			manifest: map[string]any{"exports": map[string]any{"./a.css": "./gone.css"}},
			subpath:  "a.css",
			wantOK:   false,
		},
		{
			name:     "target outside the package",
			css:      []string{"a.css"},
			manifest: map[string]any{"exports": map[string]any{"./b.css": "./../a.css"}},
			subpath:  "b.css",
			wantOK:   false,
		},
		{
			name:    "no exports and no literal file",
			css:     []string{"a.css"},
			subpath: "b.css",
			wantOK:  false,
		},
		{
			name:    "empty subpath without exports",
			css:     []string{"a.css"},
			subpath: "",
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &pkgFiles{manifest: tt.manifest, css: map[string]string{}}
			for _, f := range tt.css {
				p.css[f] = ""
			}

			got, ok := p.resolve(tt.subpath)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDeclares(t *testing.T) {
	p := &pkgFiles{manifest: map[string]any{
		"dependencies":         map[string]any{"a": "^1.0.0", "both": "1.1.1"},
		"peerDependencies":     map[string]any{"b": ">=2", "both": "2.2.2"},
		"optionalDependencies": map[string]any{"c": "3.x"},
		"devDependencies":      map[string]any{"d": "4.0.0"},
	}}

	tests := []struct {
		name   string
		dep    string
		want   string
		wantOK bool
	}{
		{name: "dependency", dep: "a", want: "^1.0.0", wantOK: true},
		{name: "peer dependency", dep: "b", want: ">=2", wantOK: true},
		{name: "optional dependency", dep: "c", want: "3.x", wantOK: true},
		{name: "dependencies come first", dep: "both", want: "1.1.1", wantOK: true},
		{name: "dev dependency is not declared", dep: "d"},
		{name: "unknown", dep: "zzz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := p.declares(tt.dep)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClosure(t *testing.T) {
	tests := []struct {
		name      string
		css       map[string]string
		start     []string
		wantFiles []string
	}{
		{
			name: "relative imports followed and package imports not",
			css: map[string]string{
				"a.css":     `@import "./b.css"; @import "@acme/tokens/vars.css";`,
				"b.css":     `@import "./sub/c.css"; @import "theme.css";`,
				"theme.css": `a{b:c}`,
			},
			start:     []string{"a.css"},
			wantFiles: []string{"a.css", "b.css", "theme.css"},
		},
		{
			name: "an existing relative file wins over a package reading",
			css: map[string]string{
				"a.css":     `@import "sub/c.css";`,
				"sub/c.css": `a{}`,
			},
			start:     []string{"a.css"},
			wantFiles: []string{"a.css", "sub/c.css"},
		},
		{
			name: "missing path-like import is not followed",
			css: map[string]string{
				"a.css": `@import "sub/c.css";`,
			},
			start:     []string{"a.css"},
			wantFiles: []string{"a.css"},
		},
		{
			name: "parent directory and nested directory",
			css: map[string]string{
				"dist/x/a.css":      `@import "../b.css"; @import url(./deep/d.css);`,
				"dist/b.css":        `a{}`,
				"dist/x/deep/d.css": `@import "../../b.css";`,
			},
			start:     []string{"dist/x/a.css"},
			wantFiles: []string{"dist/b.css", "dist/x/a.css", "dist/x/deep/d.css"},
		},
		{
			name: "cycles end",
			css: map[string]string{
				"a.css": `@import "./b.css";`,
				"b.css": `@import "./a.css";`,
			},
			start:     []string{"a.css"},
			wantFiles: []string{"a.css", "b.css"},
		},
		{
			name: "imports that leave the package are dropped",
			css: map[string]string{
				"a.css": `@import "../outside.css"; @import "./../../x.css"; @import "/abs.css";`,
			},
			start:     []string{"a.css"},
			wantFiles: []string{"a.css"},
		},
		{
			name: "urls are not followed",
			css: map[string]string{
				"a.css": `@import "https://example.test/a.css"; @import url(data:text/css,a{});`,
			},
			start:     []string{"a.css"},
			wantFiles: []string{"a.css"},
		},
		{
			name: "a tilde prefix is a package, not a file",
			css: map[string]string{
				"a.css":        `@import "~@acme/x.css";`,
				"~@acme/x.css": `a{}`,
			},
			start:     []string{"a.css"},
			wantFiles: []string{"a.css"},
		},
		{
			name: "several starts and an unknown start",
			css: map[string]string{
				"a.css": `@import "./c.css";`,
				"b.css": `@import "./c.css";`,
				"c.css": ``,
			},
			start:     []string{"b.css", "a.css", "gone.css", "a.css"},
			wantFiles: []string{"a.css", "b.css", "c.css"},
		},
		{
			name:      "no start",
			css:       map[string]string{"a.css": `@import "./b.css";`},
			wantFiles: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &pkgFiles{css: tt.css}

			assert.Equal(t, tt.wantFiles, p.closure(tt.start))
		})
	}
}

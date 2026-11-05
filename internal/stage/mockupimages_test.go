package stage

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const logoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 8 8"><path d="M0 0h8v8H0z" fill="#0a7f5a"/></svg>`

// avifBytes is a minimal ftyp box naming brand.
func avifBytes(brand string) []byte {
	return append([]byte{0, 0, 0, 24}, []byte("ftyp"+brand+"\x00\x00\x00\x00mif1avif")...)
}

func pngBytes(t *testing.T) []byte {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewGray(image.Rect(0, 0, 1, 1))))

	return b.Bytes()
}

// pngOfSize is a PNG signature padded to n bytes.
func pngOfSize(t *testing.T, n int) []byte {
	t.Helper()

	data := pngBytes(t)

	return append(data, make([]byte, n-len(data))...)
}

// imageRoot is a tool root with the trees shop and shop@api, a kept package
// dir beside them and a directory that is no tree, holding valid images
// where only the rule a case breaks keeps a file out.
func imageRoot(t *testing.T) (string, map[string][]byte) {
	t.Helper()

	root := t.TempDir()
	files := map[string][]byte{
		"shop/web/src/assets/logo.svg":           []byte(logoSVG),
		"shop/web/public/hero.png":               pngBytes(t),
		"shop/web/public/LOUD.PNG":               pngBytes(t),
		"shop@api/static/badge.avif":             avifBytes("avif"),
		"shop@api/static/sequence.avif":          avifBytes("avis"),
		"shop/web/exact.png":                     pngOfSize(t, MaxImageBytes),
		"shop/web/big.png":                       pngOfSize(t, MaxImageBytes+1),
		"shop/web/drawn.png":                     []byte(logoSVG),
		"shop/web/still.avif":                    pngBytes(t),
		"shop/web/heic.avif":                     avifBytes("heic"),
		"shop/web/latin1.svg":                    []byte("<svg>\xe6\xf8\xe5</svg>"),
		"shop/web/page.svg":                      []byte("<html><body>no drawing</body></html>"),
		"shop/web/src/assets/logo.txt":           []byte(logoSVG),
		"shop/.git/logo.svg":                     []byte(logoSVG),
		"shop/web/node_modules/pkg/logo.svg":     []byte(logoSVG),
		"shop/web/NODE_MODULES/pkg/logo.svg":     []byte(logoSVG),
		"_npm+x@1/logo.svg":                      []byte(logoSVG),
		"elsewhere/logo.svg":                     []byte(logoSVG),
		"shop/web/src/dir.svg/inside/marker.svg": []byte(logoSVG),
	}

	for rel, data := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}

	require.NoError(t, os.Symlink(filepath.Join(root, "shop/web/src/assets"), filepath.Join(root, "shop/web/linked")))
	require.NoError(t, os.Symlink(filepath.Join(root, "shop/web/src/assets/logo.svg"), filepath.Join(root, "shop/web/src/assets/alias.svg")))

	return root, files
}

func TestImageFiles_DataURI(t *testing.T) {
	root, files := imageRoot(t)
	img := ImageFiles{Root: root, Trees: []string{"shop", "shop@api"}}

	taken := []struct{ path, mime string }{
		{"shop/web/src/assets/logo.svg", "image/svg+xml"},
		{"shop/web/public/hero.png", "image/png"},
		{"shop/web/public/LOUD.PNG", "image/png"},
		{"shop@api/static/badge.avif", "image/avif"},
		{"shop@api/static/sequence.avif", "image/avif"},
		{"shop/web/exact.png", "image/png"},
	}

	for _, tt := range taken {
		t.Run(tt.path, func(t *testing.T) {
			uri, err := img.DataURI(context.Background(), tt.path)
			require.NoError(t, err)

			prefix := "data:" + tt.mime + ";base64,"
			require.True(t, strings.HasPrefix(uri, prefix), uri)

			data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(uri, prefix))
			require.NoError(t, err)
			assert.Equal(t, files[tt.path], data)
		})
	}

	refused := []struct{ name, path string }{
		{"an empty path", ""},
		{"an absolute path", "/shop/web/src/assets/logo.svg"},
		{"a backslash", `shop\web\src\assets\logo.svg`},
		{"an empty segment", "shop/web//src/assets/logo.svg"},
		{"a trailing slash", "shop/web/src/assets/logo.svg/"},
		{"a dot segment", "shop/./web/src/assets/logo.svg"},
		{"a dot-dot segment", "shop/web/../web/src/assets/logo.svg"},
		{"a dot-dot out of the trees", "shop/../elsewhere/logo.svg"},
		{"a directory that is no tree", "elsewhere/logo.svg"},
		{"a kept package", "_npm+x@1/logo.svg"},
		{"a tree named in another case", "Shop/web/src/assets/logo.svg"},
		{"a git directory", "shop/.git/logo.svg"},
		{"a node_modules directory", "shop/web/node_modules/pkg/logo.svg"},
		{"a node_modules directory in capitals", "shop/web/NODE_MODULES/pkg/logo.svg"},
		{"no image extension", "shop/web/src/assets/logo.txt"},
		{"a symlinked directory inside the tree", "shop/web/linked/logo.svg"},
		{"a symlinked file inside the tree", "shop/web/src/assets/alias.svg"},
		{"a directory", "shop/web/src/dir.svg"},
		{"a missing file", "shop/web/src/assets/missing.svg"},
		{"a file taken for a directory", "shop/web/src/assets/logo.svg/x.svg"},
		{"one byte over the cap", "shop/web/big.png"},
		{"a png that is no png", "shop/web/drawn.png"},
		{"an avif that is no avif", "shop/web/still.avif"},
		{"an ftyp box of another brand", "shop/web/heic.avif"},
		{"an svg that is not UTF-8", "shop/web/latin1.svg"},
		{"an svg with no drawing", "shop/web/page.svg"},
	}

	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			uri, err := img.DataURI(context.Background(), tt.path)
			require.Error(t, err)
			assert.Empty(t, uri)
			assert.Contains(t, err.Error(), tt.path, "the refusal names the path")
		})
	}
}

func TestImageFiles_DataURIStopsWithTheContext(t *testing.T) {
	root, _ := imageRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ImageFiles{Root: root, Trees: []string{"shop"}}.DataURI(ctx, "shop/web/src/assets/logo.svg")
	require.ErrorIs(t, err, context.Canceled)
}

// fakeImages resolves the paths it holds to a fixed URI each, counting the
// calls by path; any other path is refused.
type fakeImages struct {
	uris  map[string]string
	calls map[string]int
}

func (f *fakeImages) DataURI(_ context.Context, path string) (string, error) {
	if f.calls == nil {
		f.calls = map[string]int{}
	}

	f.calls[path]++

	uri, ok := f.uris[path]
	if !ok {
		return "", errors.New("there is no file at " + path)
	}

	return uri, nil
}

// testURI is the URI fakeImages gives path.
func testURI(path string) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(path))
}

func knownImages(paths ...string) *fakeImages {
	f := &fakeImages{uris: map[string]string{}}
	for _, p := range paths {
		f.uris[p] = testURI(p)
	}

	return f
}

func TestInlineImages_ReplacesReferences(t *testing.T) {
	x, y := testURI("shop/x.svg"), testURI("shop/y.svg")

	tests := []struct{ name, src, want string }{
		{"an img src", `<img src="asset:shop/x.svg" alt="">`, `<img src="` + x + `" alt="">`},
		{"an unquoted value", `<img src=asset:shop/x.svg alt="">`, `<img src=` + x + ` alt="">`},
		{"a single-quoted value", `<img alt='' src = 'asset:shop/x.svg'>`, `<img alt='' src = '` + x + `'>`},
		{"an svg image href", `<svg><image href="asset:shop/x.svg"/></svg>`, `<svg><image href="` + x + `"/></svg>`},
		{"a srcset", `<img srcset="asset:shop/x.svg 1x, asset:shop/y.svg 2x">`, `<img srcset="` + x + ` 1x, ` + y + ` 2x">`},
		{"a style attribute", `<div style="background:url(asset:shop/x.svg)"></div>`, `<div style="background:url(` + x + `)"></div>`},
		{
			"a style element in each url form",
			`<style>.a{background:url(asset:shop/x.svg)}.b{background:url("asset:shop/x.svg")}.c{background:url('asset:shop/y.svg')}</style>`,
			`<style>.a{background:url(` + x + `)}.b{background:url("` + x + `")}.c{background:url('` + y + `')}</style>`,
		},
		{
			"a script string and a template literal",
			"<script>var a = '<img src=\"asset:shop/x.svg\">'; var b = `<img src=\"asset:shop/y.svg\">`;</script>",
			"<script>var a = '<img src=\"" + x + "\">'; var b = `<img src=\"" + y + "\">`;</script>",
		},
		{"an event attribute", `<button onclick="show('asset:shop/x.svg')">Go</button>`, `<button onclick="show('` + x + `')">Go</button>`},
		{"a fragment kept after the URI", `<svg><use href="asset:shop/x.svg#home"/></svg>`, `<svg><use href="` + x + `#home"/></svg>`},
		{"a query kept after the URI", `<img src="asset:shop/x.svg?v=1">`, `<img src="` + x + `?v=1">`},
		{"a path of letters beyond ascii", `<img src="asset:shop/løve.svg">`, `<img src="` + testURI("shop/løve.svg") + `">`},
		{
			"a document around them, byte for byte",
			"<!doctype html>\r\n<html><head><title>asset:shop/x.svg</title></head>\n<body><!-- asset:shop/x.svg --><p class=\"a\">Logo:</p>\n<IMG\n  src=\"asset:shop/x.svg\"\n  ALT=\"Logo\"\n></body></html>",
			"<!doctype html>\r\n<html><head><title>asset:shop/x.svg</title></head>\n<body><!-- asset:shop/x.svg --><p class=\"a\">Logo:</p>\n<IMG\n  src=\"" + x + "\"\n  ALT=\"Logo\"\n></body></html>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inlineImages(context.Background(), tt.src, knownImages("shop/x.svg", "shop/y.svg", "shop/løve.svg"))
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// What is no reference is never resolved, so a run without images takes it.
func TestInlineImages_LeavesWhatIsNoReference(t *testing.T) {
	tests := []struct{ name, src string }{
		{"markup text", `<p>Images are written asset:shop/x.svg</p>`},
		{"a comment", `<!-- asset:shop/x.svg --><p>Hi</p>`},
		{"a textarea", `<textarea>asset:shop/x.svg</textarea>`},
		{"a title", `<title>asset:shop/x.svg</title>`},
		{"a prefix of letters", `<img alt="myasset:shop/x.svg"><script>var u = "myasset:shop/x.svg"</script>`},
		{"a prefix with a dash, underscore or dollar", `<img alt="data-asset:shop/x.svg" title="my_asset:x $asset:x 1asset:x">`},
		{"no path after the prefix", `<img alt="asset:" title="asset: shop"><style>a{content:"asset:"}</style>`},
		{"an attribute named like a reference", `<div asset:shop="1"></div>`},
		{"no reference at all", terminalSource},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inlineImages(context.Background(), tt.src, nil)
			require.NoError(t, err)
			assert.Equal(t, tt.src, got)
		})
	}
}

func TestInlineImages_ResolvesEachPathOnce(t *testing.T) {
	img := knownImages("shop/x.svg")

	got, err := inlineImages(context.Background(), `<img src="asset:shop/x.svg"><style>a{background:url(asset:shop/x.svg)}</style>`, img)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(got, testURI("shop/x.svg")), "each reference inserts the whole file")
	assert.Equal(t, map[string]int{"shop/x.svg": 1}, img.calls)
}

func TestInlineImages_RefusesAReferenceItCannotInsert(t *testing.T) {
	tests := []struct {
		name, src string
		img       Images
	}{
		{"an unknown file", `<img src="asset:shop/x.svg"><img src="asset:shop/missing.svg">`, knownImages("shop/x.svg")},
		{"a run without trees", `<p>Logo</p><img src="asset:shop/missing.svg">`, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inlineImages(context.Background(), tt.src, tt.img)
			require.Error(t, err)
			assert.Empty(t, got)
			assert.Contains(t, err.Error(), "asset:shop/missing.svg", "the refusal names the reference")
		})
	}
}

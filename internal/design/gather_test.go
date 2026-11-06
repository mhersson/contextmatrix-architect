package design

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	uiPath     = "/@acme/ui/-/ui-1.2.3.tgz"
	tokensPath = "/@acme/tokens/-/tokens-2.0.1.tgz"
	uiDir      = "_npm+@acme+ui@1.2.3"
	tokensDir  = "_npm+@acme+tokens@2.0.1"
	coreImport = "@acme/ui/style/core.css"
)

// acmeTarballs returns @acme/ui 1.2.3, whose style/core.css imports its own
// theme and @acme/tokens, and @acme/tokens 2.0.1, keyed by registry path.
func acmeTarballs(t *testing.T, coreCSS, varsCSS string) map[string][]byte {
	t.Helper()

	return acmeStyleTarballs(t, map[string]string{"core.css": coreCSS}, varsCSS)
}

// acmeStyleTarballs is acmeTarballs with the files of @acme/ui's style
// directory given by name, beside its theme.css.
func acmeStyleTarballs(t *testing.T, style map[string]string, varsCSS string) map[string][]byte {
	t.Helper()

	return acmeTarballsWith(t, style, varsCSS, nil)
}

// acmeTarballsWith is acmeStyleTarballs with the files of extra, keyed by
// registry path and then archive path, added to the archives.
func acmeTarballsWith(t *testing.T, style map[string]string, varsCSS string, extra map[string]map[string]string) map[string][]byte {
	t.Helper()

	ui := map[string]string{
		"package/package.json": `{"name":"@acme/ui","version":"1.2.3",` +
			`"dependencies":{"@acme/tokens":"^2.0.0"},` +
			`"exports":{"./style/*":"./style/*","./theme.css":"./dist/theme.css"}}`,
		"package/style/theme.css": ":root{--color-border:var(--acme-grey-8)}",
	}
	for name, css := range style {
		ui["package/style/"+name] = css
	}

	tokens := map[string]string{
		"package/package.json": `{"name":"@acme/tokens","version":"2.0.1"}`,
		"package/vars.css":     varsCSS,
	}

	maps.Copy(ui, extra[uiPath])
	maps.Copy(tokens, extra[tokensPath])

	return map[string][]byte{uiPath: tgz(t, ui), tokensPath: tgz(t, tokens)}
}

const (
	defaultCoreCSS = "@import './theme.css';\n@import '@acme/tokens/vars.css';\n.btn{border:0}"
	defaultVarsCSS = ":root{--acme-grey-8:#ebebeb}"
)

func defaultTarballs(t *testing.T) map[string][]byte {
	t.Helper()

	return acmeTarballs(t, defaultCoreCSS, defaultVarsCSS)
}

// acmeUIImages are image files of @acme/ui's archive, keyed by archive path:
// icons named heart, heart_medium and play, a logo, a heart under
// node_modules and a heart_large over testImageCap.
func acmeUIImages() map[string]string {
	return map[string]string{
		"package/assets/icons/heart.svg":        `<svg id="heart"/>`,
		"package/assets/icons/heart_medium.svg": `<svg id="heart-medium"/>`,
		"package/assets/icons/heart_large.svg":  "<svg>" + strings.Repeat(" ", testImageCap) + "</svg>",
		"package/assets/icons/play.svg":         `<svg id="play"/>`,
		"package/assets/images/acme-logo.png":   "\x89PNG\r\n\x1a\nlogo",
		"package/node_modules/x/heart.svg":      `<svg id="nested"/>`,
	}
}

// acmeImageTarballs is defaultTarballs with acmeUIImages in @acme/ui.
func acmeImageTarballs(t *testing.T) map[string][]byte {
	t.Helper()

	return acmeTarballsWith(t, map[string]string{"core.css": defaultCoreCSS}, defaultVarsCSS,
		map[string]map[string]string{uiPath: acmeUIImages()})
}

// registryServer serves tarballs by path, or answers status to everything
// when status is not 0.
func registryServer(t *testing.T, tarballs map[string][]byte, status int) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := tarballs[r.URL.Path]

		switch {
		case status != 0:
			w.WriteHeader(status)
		case !ok:
			http.NotFound(w, r)
		default:
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

func npmLockJSON(t *testing.T, entries map[string]map[string]any) string {
	t.Helper()

	data, err := json.Marshal(map[string]any{"lockfileVersion": 3, "packages": entries})
	require.NoError(t, err)

	return string(data)
}

// portalTree is a tree whose frontend loads src/index.css from src/main.tsx,
// and index.css imports @acme/ui/style/core.css, with @acme/ui and
// @acme/tokens locked on the registry at srvURL. Only @acme/ui carries an
// integrity.
func portalTree(t *testing.T, srvURL string, tarballs map[string][]byte) map[string]string {
	t.Helper()

	return map[string]string{
		".npmrc":                 "registry=" + srvURL + "/\n",
		"frontend/package.json":  `{"name":"frontend","dependencies":{"@acme/ui":"^1.2.0"}}`,
		"frontend/src/main.tsx":  "import './index.css'\n",
		"frontend/src/index.css": "@import '" + coreImport + "';\n.tile{border:1px solid var(--color-border)}\n",
		"package-lock.json": npmLockJSON(t, map[string]map[string]any{
			"node_modules/@acme/ui": {
				"version": "1.2.3", "resolved": srvURL + uiPath, "integrity": integrity(tarballs[uiPath]),
			},
			"node_modules/@acme/tokens": {"version": "2.0.1", "resolved": srvURL + tokensPath},
		}),
	}
}

func portalRequest(root string) Request {
	return Request{Root: root, Trees: []Tree{{Project: "portal", Dir: "portal"}}}
}

// gatherPortal writes files as the tree "portal" under root and gathers it.
func gatherPortal(t *testing.T, srv *httptest.Server, root string, files map[string]string) (Material, error) {
	t.Helper()

	return gatherPortalImages(t, srv, root, files, 0)
}

// gatherPortalImages is gatherPortal keeping package images of up to imageCap
// bytes.
func gatherPortalImages(t *testing.T, srv *httptest.Server, root string, files map[string]string, imageCap int) (Material, error) {
	t.Helper()

	writeTree(t, filepath.Join(root, "portal"), files)

	req := portalRequest(root)
	req.ImageCap = imageCap

	return NewGatherer(t.TempDir(), srv.Client().Transport).Gather(t.Context(), req)
}

func tokenValue(t *testing.T, r Root, name string) string {
	t.Helper()

	for _, tok := range r.Tokens {
		if tok.Name == name {
			assert.False(t, tok.Unresolved, name)

			return tok.Value
		}
	}

	require.Failf(t, "token not found", "%s in %s", name, r.Path)

	return ""
}

func TestGather_EndToEnd(t *testing.T) {
	tarballs := defaultTarballs(t)
	srv := registryServer(t, tarballs, 0)

	files := portalTree(t, srv.URL, tarballs)
	files["frontend/src/components/Footer/index.tsx"] = "export const Footer = () => null\n"
	files["frontend/src/logo.svg"] = "<svg/>"

	root := t.TempDir()
	writeTree(t, filepath.Join(root, uiDir), map[string]string{"stale.css": "a{}"})

	m, err := gatherPortal(t, srv, root, files)
	require.NoError(t, err)

	assert.Empty(t, m.Gaps)

	host, err := url.Parse(srv.URL)
	require.NoError(t, err)

	require.Len(t, m.Packages, 2)
	assert.Equal(t, Package{
		Name: "@acme/tokens", Version: "2.0.1", Dir: tokensDir, Registry: host.Host,
		Files: []string{tokensDir + "/vars.css"},
	}, m.Packages[0])
	assert.Equal(t, Package{
		Name: "@acme/ui", Version: "1.2.3", Dir: uiDir, Registry: host.Host, Verified: true,
		Files: []string{uiDir + "/style/core.css", uiDir + "/style/theme.css"},
	}, m.Packages[1])

	core, err := os.ReadFile(filepath.Join(root, uiDir, "style", "core.css"))
	require.NoError(t, err)
	assert.Equal(t, reflow("@import './theme.css';\n@import '@acme/tokens/vars.css';\n.btn{border:0}"), string(core))
	assert.NoFileExists(t, filepath.Join(root, uiDir, "stale.css"))

	require.Len(t, m.Roots, 1)

	r := m.Roots[0]
	assert.Equal(t, "portal", r.Project)
	assert.Equal(t, "portal/frontend", r.Path)
	assert.Equal(t, "#ebebeb", tokenValue(t, r, "--color-border"))
	assert.Contains(t, assetPaths(r.Frame), "portal/frontend/src/components/Footer/index.tsx")
	require.Len(t, r.Brand, 1)
	assert.Equal(t, "portal/frontend/src/logo.svg", r.Brand[0].Path)
	assert.Equal(t, []string{tokensDir, uiDir}, r.Packages)
}

func TestGather_FrameText(t *testing.T) {
	const footer = "export const Footer = () => null\n"

	tarballs := defaultTarballs(t)
	srv := registryServer(t, tarballs, 0)

	files := portalTree(t, srv.URL, tarballs)
	files["frontend/src/components/Footer/index.tsx"] = footer
	files["frontend/src/components/Header/index.tsx"] = strings.Repeat("a", 33<<10)
	files["frontend/src/components/Menu/index.tsx"] = "a\x00b"
	files["frontend/src/components/Nav/index.tsx"] = "\xff\xfe"
	files["frontend/src/logo.svg"] = `<svg id="a"/>`
	files["frontend/src/assets/brand/logo.svg"] = "<svg/>"
	files["frontend/public/logo.png"] = "png"

	m, err := gatherPortal(t, srv, t.TempDir(), files)
	require.NoError(t, err)
	require.Len(t, m.Roots, 1)

	frame := map[string]Asset{}
	for _, a := range m.Roots[0].Frame {
		frame[a.Path] = a
	}

	const dir = "portal/frontend/src/components/"

	require.Len(t, frame, 4)
	assert.Equal(t, Asset{Path: dir + "Footer/index.tsx", Size: int64(len(footer)), Text: footer}, frame[dir+"Footer/index.tsx"])
	assert.Equal(t, Asset{Path: dir + "Header/index.tsx", Size: 33 << 10}, frame[dir+"Header/index.tsx"], "over the limit")
	assert.Equal(t, Asset{Path: dir + "Menu/index.tsx", Size: 3}, frame[dir+"Menu/index.tsx"], "a NUL byte")
	assert.Equal(t, Asset{Path: dir + "Nav/index.tsx", Size: 2}, frame[dir+"Nav/index.tsx"], "invalid UTF-8")

	brand := m.Roots[0].Brand
	assert.Equal(t, []string{
		"portal/frontend/src/logo.svg", "portal/frontend/public/logo.png", "portal/frontend/src/assets/brand/logo.svg",
	}, assetPaths(brand))

	for _, a := range brand {
		assert.Empty(t, a.Text, a.Path)
	}
}

func TestGather_LogoIsTheFileNamedLogo(t *testing.T) {
	tarballs := defaultTarballs(t)
	srv := registryServer(t, tarballs, 0)

	files := portalTree(t, srv.URL, tarballs)
	files["frontend/public/google-logo.svg"] = `<svg id="g"/>`
	files["frontend/src/assets/images/logo.svg"] = `<svg id="l"/>`

	m, err := gatherPortal(t, srv, t.TempDir(), files)
	require.NoError(t, err)
	require.Len(t, m.Roots, 1)

	brand := m.Roots[0].Brand
	require.Len(t, brand, 2)
	assert.Equal(t, "portal/frontend/src/assets/images/logo.svg", brand[0].Path)
	assert.Equal(t, int64(len(`<svg id="l"/>`)), brand[0].Size)
	assert.Empty(t, brand[0].Text)
	assert.Empty(t, brand[1].Text)
}

// imageTree is a frontend whose frame files import images, with the files
// they name and a few they do not resolve to.
func imageTree() map[string]string {
	return map[string]string{
		"frontend/package.json":  `{"name":"frontend"}`,
		"frontend/src/main.tsx":  "import './index.css'\n",
		"frontend/src/index.css": ":root{--brand:#123456}\n",

		"frontend/src/components/SiteHeader/index.tsx": "import logo from './logo.svg'\n" +
			"import gone from './gone.png'\n" +
			"import icon from 'some-package/icon.svg'\n" +
			"import out from '../../../../../outside.png'\n" +
			"export const SiteHeader = () => null\n",
		"frontend/src/components/SiteHeader/logo.svg": `<svg id="logo"/>`,

		"frontend/src/components/HeroBanner/index.tsx": "import hero from '@/img/hero.svg?url'\nexport const HeroBanner = () => null\n",
		"frontend/src/components/HeroBanner/index.module.css": ".hero{background:url(../../img/bg.png)}\n" +
			".wave{background:url(/bg/wave.svg)}\n",

		"frontend/src/img/hero.svg":   `<svg id="hero"/>`,
		"frontend/src/img/bg.png":     "png-bytes",
		"frontend/public/bg/wave.svg": `<svg id="wave"/>`,
	}
}

func TestGather_FrameImages(t *testing.T) {
	srv := registryServer(t, nil, 0)

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "outside.png"), []byte("png"), 0o600))

	m, err := gatherPortal(t, srv, root, imageTree())
	require.NoError(t, err)
	require.Len(t, m.Roots, 1)

	const dir = "portal/frontend/"

	// In frame rank order, then import order; each with its size.
	assert.Equal(t, []Asset{
		{Path: dir + "src/components/SiteHeader/logo.svg", Size: int64(len(`<svg id="logo"/>`))},
		{Path: dir + "src/img/hero.svg", Size: int64(len(`<svg id="hero"/>`))},
		{Path: dir + "src/img/bg.png", Size: int64(len("png-bytes"))},
		{Path: dir + "public/bg/wave.svg", Size: int64(len(`<svg id="wave"/>`))},
	}, m.Roots[0].Images)
}

func TestGather_FrameImagesAreFilesOfTheTreeAMockupMayTake(t *testing.T) {
	srv := registryServer(t, nil, 0)

	files := imageTree()
	files["frontend/src/components/SiteHeader/index.tsx"] = "import logo from './logo.svg'\n" +
		"import link from './link.svg'\n" +
		"import via from '../../linked/hero.svg'\n" +
		"import pkg from '../../../node_modules/pkg/a.svg'\n" +
		"import dir from './folder.svg'\n"
	files["frontend/node_modules/pkg/a.svg"] = "<svg/>"

	root := t.TempDir()
	writeTree(t, filepath.Join(root, "portal"), files)
	require.NoError(t, os.Symlink("logo.svg", filepath.Join(root, "portal/frontend/src/components/SiteHeader/link.svg")))
	require.NoError(t, os.Symlink("img", filepath.Join(root, "portal/frontend/src/linked")))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "portal/frontend/src/components/SiteHeader/folder.svg"), 0o750))

	m, err := NewGatherer(t.TempDir(), srv.Client().Transport).Gather(t.Context(), portalRequest(root))
	require.NoError(t, err)
	require.Len(t, m.Roots, 1)

	assert.Equal(t, []string{
		"portal/frontend/src/components/SiteHeader/logo.svg",
		"portal/frontend/src/img/hero.svg",
		"portal/frontend/src/img/bg.png",
		"portal/frontend/public/bg/wave.svg",
	}, assetPaths(m.Roots[0].Images), "a symlink, a file under node_modules and a directory are not listed")
}

func TestGather_FrameImagesAreListedOnceAndCapped(t *testing.T) {
	srv := registryServer(t, nil, 0)

	files := map[string]string{
		"frontend/package.json":                     `{"name":"frontend"}`,
		"frontend/src/index.css":                    ":root{--brand:#123456}\n",
		"frontend/src/components/Footer/index.tsx":  "import a from '../Header/shared.png'\nimport b from './own.png'\n",
		"frontend/src/components/Footer/own.png":    "png",
		"frontend/src/components/Header/shared.png": "png",
	}

	var header strings.Builder

	header.WriteString("import shared from './shared.png'\n")

	for i := range maxFrameImages + 5 {
		name := fmt.Sprintf("img%02d.png", i)
		header.WriteString("import i" + name[3:5] + " from './" + name + "'\n")

		files["frontend/src/components/Header/"+name] = "png"
	}

	files["frontend/src/components/Header/index.tsx"] = header.String()

	m, err := gatherPortal(t, srv, t.TempDir(), files)
	require.NoError(t, err)
	require.Len(t, m.Roots, 1)

	images := assetPaths(m.Roots[0].Images)
	require.Len(t, images, maxFrameImages)
	assert.Equal(t, "portal/frontend/src/components/Header/shared.png", images[0])
	assert.Equal(t, "portal/frontend/src/components/Header/img18.png", images[maxFrameImages-1])
	assert.NotContains(t, images, "portal/frontend/src/components/Footer/own.png")

	// Without the crowd of the header's, the shared image is listed once.
	files["frontend/src/components/Header/index.tsx"] = "import shared from './shared.png'\n"

	m, err = gatherPortal(t, srv, t.TempDir(), files)
	require.NoError(t, err)
	require.Len(t, m.Roots, 1)
	assert.Equal(t, []string{
		"portal/frontend/src/components/Header/shared.png",
		"portal/frontend/src/components/Footer/own.png",
	}, assetPaths(m.Roots[0].Images))
}

func TestGather_ImagesOfAFrameFileWithoutText(t *testing.T) {
	srv := registryServer(t, nil, 0)

	files := imageTree()
	files["frontend/src/components/SiteHeader/index.tsx"] = "import logo from './logo.svg'\n" + strings.Repeat("a", 33<<10)

	m, err := gatherPortal(t, srv, t.TempDir(), files)
	require.NoError(t, err)
	require.Len(t, m.Roots, 1)

	assert.NotContains(t, assetPaths(m.Roots[0].Images), "portal/frontend/src/components/SiteHeader/logo.svg", "its text was not read")
	assert.Contains(t, assetPaths(m.Roots[0].Images), "portal/frontend/src/img/hero.svg")
}

// portalRoots is portalTree with its frontend root copied to each of dirs,
// each holding the files of frame, keyed by path under the root.
func portalRoots(t *testing.T, srvURL string, tarballs map[string][]byte, dirs []string, frame map[string]string) map[string]string {
	t.Helper()

	files := map[string]string{}

	for name, content := range portalTree(t, srvURL, tarballs) {
		rel, ok := strings.CutPrefix(name, "frontend/")
		if !ok {
			files[name] = content

			continue
		}

		for _, dir := range dirs {
			files[dir+"/"+rel] = strings.Replace(content, `"name":"frontend"`, `"name":"`+dir+`"`, 1)
		}
	}

	for _, dir := range dirs {
		for rel, content := range frame {
			files[dir+"/"+rel] = content
		}
	}

	return files
}

// imageFilesUnder lists the files under dir with an image's extension, as
// slash paths relative to dir.
func imageFilesUnder(t *testing.T, dir string) []string {
	t.Helper()

	var out []string

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !imageExts[strings.ToLower(filepath.Ext(p))] {
			return err
		}

		rel, err := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))

		return err
	})
	require.NoError(t, err)

	return out
}

func TestGather_PackageImagesTheFrameImports(t *testing.T) {
	tarballs := acmeImageTarballs(t)
	srv := registryServer(t, tarballs, 0)
	root := t.TempDir()

	files := portalRoots(t, srv.URL, tarballs, []string{"frontend"}, map[string]string{
		"src/components/Header/index.tsx": "import { heart, play as Play } from '@acme/ui/icons'\n",
		"src/components/Footer/index.tsx": "import { Logo } from '@acme/ui'\n",
	})

	m, err := gatherPortalImages(t, srv, root, files, testImageCap)
	require.NoError(t, err)
	assert.Empty(t, m.Gaps)
	require.Len(t, m.Roots, 1)

	// The Header ranks before the Footer; each name's exact match leads.
	rels := []string{
		"assets/icons/heart.svg", "assets/icons/heart_medium.svg", "assets/icons/play.svg", "assets/images/acme-logo.png",
	}
	images := acmeUIImages()

	want := make([]Asset, len(rels))
	for i, rel := range rels {
		want[i] = Asset{Path: uiDir + "/" + rel, Size: int64(len(images["package/"+rel]))}
	}

	assert.Equal(t, want, m.Roots[0].PackageImages)

	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(root, uiDir, filepath.FromSlash(rel)))
		require.NoError(t, err)
		assert.Equal(t, images["package/"+rel], string(data), rel)
	}

	assert.ElementsMatch(t, rels, imageFilesUnder(t, filepath.Join(root, uiDir)),
		"heart_large is over the cap and the node_modules copy is not kept")
}

func TestGather_PackageImagesNeedAKeptPackage(t *testing.T) {
	const iconsPath = "/@acme/icons/-/icons-1.0.0.tgz"

	tarballs := acmeImageTarballs(t)
	tarballs[iconsPath] = tgz(t, map[string]string{
		"package/package.json": `{"name":"@acme/icons","version":"1.0.0"}`,
		"package/star.svg":     `<svg id="star"/>`,
	})

	tests := []struct {
		name     string
		imageCap int
		deps     string // frontend/package.json's dependencies
		header   string
	}{
		{
			name:   "no image cap keeps none",
			deps:   `{"@acme/ui":"^1.2.0"}`,
			header: "import { heart } from '@acme/ui/icons'\n",
		},
		{
			name:     "a package no CSS imports is not fetched",
			imageCap: testImageCap,
			deps:     `{"@acme/ui":"^1.2.0","@acme/icons":"^1.0.0"}`,
			header:   "import { star } from '@acme/icons'\n",
		},
		{
			name:     "an undeclared package is passed over",
			imageCap: testImageCap,
			deps:     `{"@acme/ui":"^1.2.0"}`,
			header:   "import { heart } from '@acme/other'\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var iconRequests atomic.Int32

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == iconsPath {
					iconRequests.Add(1)
				}

				body, ok := tarballs[r.URL.Path]
				if !ok {
					http.NotFound(w, r)

					return
				}

				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)

			files := portalTree(t, srv.URL, tarballs)
			files["frontend/package.json"] = `{"name":"frontend","dependencies":` + tc.deps + `}`
			files["frontend/src/components/Header/index.tsx"] = tc.header
			files["package-lock.json"] = npmLockJSON(t, map[string]map[string]any{
				"node_modules/@acme/ui": {
					"version": "1.2.3", "resolved": srv.URL + uiPath, "integrity": integrity(tarballs[uiPath]),
				},
				"node_modules/@acme/tokens": {"version": "2.0.1", "resolved": srv.URL + tokensPath},
				"node_modules/@acme/icons":  {"version": "1.0.0", "resolved": srv.URL + iconsPath},
			})

			root := t.TempDir()

			m, err := gatherPortalImages(t, srv, root, files, tc.imageCap)
			require.NoError(t, err)

			assert.Empty(t, m.Gaps)
			assert.Len(t, m.Packages, 2, "the CSS's packages are still kept")
			require.Len(t, m.Roots, 1)
			assert.Empty(t, m.Roots[0].PackageImages)
			assert.Empty(t, imageFilesUnder(t, filepath.Join(root, uiDir)))
			assert.Zero(t, iconRequests.Load())
		})
	}
}

func TestGather_PackageImagesCap(t *testing.T) {
	const logo = uiDir + "/assets/images/acme-logo.png"

	images := map[string]string{"package/assets/images/acme-logo.png": "\x89PNG\r\n\x1a\nlogo"}
	for i := 1; i <= 30; i++ {
		images[fmt.Sprintf("package/assets/icons/arrow_%d.svg", i)] = "<svg/>"
	}

	tarballs := acmeTarballsWith(t, map[string]string{"core.css": defaultCoreCSS}, defaultVarsCSS,
		map[string]map[string]string{uiPath: images})

	tests := []struct {
		name string
		dirs []string
	}{
		{name: "one root", dirs: []string{"frontend"}},
		{name: "two roots naming the same images count them once", dirs: []string{"frontend", "admin"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := registryServer(t, tarballs, 0)
			root := t.TempDir()

			files := portalRoots(t, srv.URL, tarballs, tc.dirs, map[string]string{
				"src/components/Header/index.tsx": "import { arrow } from '@acme/ui/icons'\n",
				"src/components/Footer/index.tsx": "import { Logo } from '@acme/ui'\n",
			})

			m, err := gatherPortalImages(t, srv, root, files, testImageCap)
			require.NoError(t, err)
			require.Len(t, m.Roots, len(tc.dirs))

			for _, r := range m.Roots {
				listed := assetPaths(r.PackageImages)
				assert.Len(t, listed, maxPackageImages, r.Path)
				assert.Contains(t, listed, logo, r.Path)
			}

			assert.Len(t, imageFilesUnder(t, filepath.Join(root, uiDir)), maxPackageImages)
		})
	}
}

func TestGather_PackageImagesOfTwoRoots(t *testing.T) {
	const heart = "assets/icons/heart.svg"

	tarballs := acmeImageTarballs(t)
	srv := registryServer(t, tarballs, 0)
	root := t.TempDir()

	files := portalRoots(t, srv.URL, tarballs, []string{"frontend", "admin"}, map[string]string{
		"src/components/Header/index.tsx": "import { heart } from '@acme/ui'\n",
	})

	m, err := gatherPortalImages(t, srv, root, files, testImageCap)
	require.NoError(t, err)
	assert.Empty(t, m.Gaps)
	require.Len(t, m.Roots, 2)

	for _, r := range m.Roots {
		assert.Contains(t, assetPaths(r.PackageImages), uiDir+"/"+heart, r.Path)
	}

	require.Len(t, m.Packages, 2)
	assert.Equal(t, uiDir, m.Packages[1].Dir)

	data, err := os.ReadFile(filepath.Join(root, uiDir, filepath.FromSlash(heart)))
	require.NoError(t, err)
	assert.Equal(t, acmeUIImages()["package/"+heart], string(data))
	assert.ElementsMatch(t, []string{heart, "assets/icons/heart_medium.svg"}, imageFilesUnder(t, filepath.Join(root, uiDir)))
}

func TestGather_PackageImagesOfAPackageOverTheCSSCap(t *testing.T) {
	// As in TestGather_KeptCSSCap, @acme/tokens would take the kept CSS past
	// the cap.
	filler := strings.Repeat("a;", 7<<18)
	heart := map[string]string{"package/assets/heart.svg": `<svg id="heart"/>`}
	tarballs := acmeTarballsWith(t,
		map[string]string{"core.css": "@import '@acme/tokens/vars.css';\n" + filler},
		":root{--acme-grey-8:#ebebeb}\n"+filler,
		map[string]map[string]string{uiPath: heart, tokensPath: heart})
	srv := registryServer(t, tarballs, 0)
	root := t.TempDir()

	files := portalTree(t, srv.URL, tarballs)
	files["frontend/package.json"] = `{"name":"frontend","dependencies":{"@acme/ui":"^1.2.0","@acme/tokens":"^2.0.0"}}`
	files["frontend/src/components/Header/index.tsx"] = "import { heart } from '@acme/ui'\n" +
		"import { heart as tokenHeart } from '@acme/tokens'\n"

	m, err := gatherPortalImages(t, srv, root, files, testImageCap)
	require.NoError(t, err)

	require.Len(t, m.Packages, 1)
	assert.Equal(t, uiDir, m.Packages[0].Dir)
	assert.NoDirExists(t, filepath.Join(root, tokensDir))

	require.Len(t, m.Roots, 1)
	assert.Equal(t, []string{uiDir + "/assets/heart.svg"}, assetPaths(m.Roots[0].PackageImages))
}

func TestGather_TokensInImportOrder(t *testing.T) {
	const grey = ":root{--acme-grey-8:#ebebeb}"

	tests := []struct {
		name  string
		style map[string]string // @acme/ui's style files
		vars  string
		own   map[string]string // frontend files
		want  string
	}{
		{
			name:  "a file's own declarations come after its imports",
			style: map[string]string{"core.css": "@import './theme.css';\n:root{--color-border:#ff0000}"},
			vars:  grey,
			want:  "#ff0000",
		},
		{
			name: "the later of two relative imports wins",
			style: map[string]string{
				"core.css":    "@import './z-base.css';\n@import './a-brand.css';",
				"z-base.css":  ":root{--color-border:#111111}",
				"a-brand.css": ":root{--color-border:#222222}",
			},
			vars: grey,
			want: "#222222",
		},
		{
			name:  "a package imported after a relative import wins",
			style: map[string]string{"core.css": "@import './theme.css';\n@import '@acme/tokens/vars.css';"},
			vars:  ":root{--color-border:#555555}",
			want:  "#555555",
		},
		{
			name:  "the root's own CSS overrides a package token",
			style: map[string]string{"core.css": "@import './theme.css';\n@import '@acme/tokens/vars.css';"},
			vars:  grey,
			own: map[string]string{
				"frontend/src/index.css": "@import '" + coreImport + "';\n:root{--acme-grey-8:#000000}\n" +
					".tile{border:1px solid var(--color-border)}\n",
			},
			want: "#000000",
		},
		{
			name:  "a package the root imports after its own file wins",
			style: map[string]string{"core.css": "@import './theme.css';\n@import '@acme/tokens/vars.css';"},
			vars:  grey,
			own: map[string]string{
				"frontend/src/brand.css": ":root{--color-border:#333333}",
				"frontend/src/index.css": "@import './brand.css';\n@import '" + coreImport + "';\n" +
					".tile{border:1px solid var(--color-border)}\n",
			},
			want: "#ebebeb",
		},
		{
			name:  "own files that import each other are read",
			style: map[string]string{"core.css": "@import './theme.css';\n@import '@acme/tokens/vars.css';"},
			vars:  grey,
			own: map[string]string{
				"frontend/src/main.tsx": "import './index.css'\nimport './a.css'\n",
				"frontend/src/a.css":    "@import './b.css';\n:root{--color-border:#666666}",
				"frontend/src/b.css":    "@import './a.css';",
			},
			want: "#666666",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tarballs := acmeStyleTarballs(t, tc.style, tc.vars)
			srv := registryServer(t, tarballs, 0)

			files := portalTree(t, srv.URL, tarballs)
			for name, content := range tc.own {
				files[name] = content
			}

			m, err := gatherPortal(t, srv, t.TempDir(), files)
			require.NoError(t, err)

			assert.Empty(t, m.Gaps)
			require.Len(t, m.Roots, 1)
			assert.Equal(t, tc.want, tokenValue(t, m.Roots[0], "--color-border"))
		})
	}
}

func TestGather_NearestLockfile(t *testing.T) {
	tarballs := defaultTarballs(t)

	pnpmLock := "lockfileVersion: '9.0'\n\nimporters:\n  .:\n    dependencies:\n" +
		"      '@acme/ui':\n        specifier: ^1.2.0\n        version: 1.2.3\n\n" +
		"packages:\n  '@acme/tokens@2.0.1':\n    resolution: {integrity: " + integrity(tarballs[tokensPath]) + "}\n" +
		"  '@acme/ui@1.2.3':\n    resolution: {integrity: " + integrity(tarballs[uiPath]) + "}\n\n" +
		"snapshots:\n  '@acme/tokens@2.0.1': {}\n  '@acme/ui@1.2.3':\n    dependencies:\n      '@acme/tokens': 2.0.1\n"

	tests := []struct {
		name string
		edit func(t *testing.T, files map[string]string, srvURL string)
	}{
		{
			name: "a package-lock.json beside the frontend",
			edit: func(*testing.T, map[string]string, string) {},
		},
		{
			name: "a pnpm-lock.yaml beside the frontend keys its importer from there",
			edit: func(_ *testing.T, files map[string]string, _ string) {
				delete(files, "web/package-lock.json")
				files["web/pnpm-lock.yaml"] = pnpmLock
			},
		},
		{
			name: "the nearest lockfile wins over the tree root's",
			edit: func(t *testing.T, files map[string]string, srvURL string) {
				files["package-lock.json"] = npmLockJSON(t, map[string]map[string]any{
					"node_modules/@acme/other": {"version": "1.0.0", "resolved": srvURL + "/@acme/other/-/other-1.0.0.tgz"},
				})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := registryServer(t, tarballs, 0)

			files := map[string]string{
				"go.mod":  "module example.test/portal\n",
				"main.go": "package main\n",
			}

			for name, content := range portalTree(t, srv.URL, tarballs) {
				if web, ok := strings.CutPrefix(name, "frontend/"); ok {
					name = "web/" + web
				}

				if name == "package-lock.json" {
					name = "web/package-lock.json"
				}

				files[name] = content
			}

			tc.edit(t, files, srv.URL)

			m, err := gatherPortal(t, srv, t.TempDir(), files)
			require.NoError(t, err)

			assert.Empty(t, m.Gaps)
			require.Len(t, m.Roots, 1)
			assert.Equal(t, "portal/web", m.Roots[0].Path)
			assert.Equal(t, []string{tokensDir, uiDir}, m.Roots[0].Packages)
		})
	}
}

func TestGather_Gaps(t *testing.T) {
	tarballs := defaultTarballs(t)

	closed := httptest.NewTLSServer(http.NotFoundHandler())
	closed.Close()

	tests := []struct {
		name   string
		status int
		spec   string
		edit   func(t *testing.T, files map[string]string, srvURL string)
		word   string
	}{
		{
			name: "no lockfile",
			edit: func(_ *testing.T, files map[string]string, _ string) { delete(files, "package-lock.json") },
			word: "repository",
		},
		{
			name: "unreadable lockfile",
			edit: func(_ *testing.T, files map[string]string, _ string) { files["package-lock.json"] = "{" },
			word: "read",
		},
		{
			name: "undeclared",
			edit: func(_ *testing.T, files map[string]string, _ string) {
				files["frontend/package.json"] = `{"name":"frontend","dependencies":{"@acme/other":"1.0.0"}}`
			},
			word: "declared",
		},
		{
			name: "no lock entry",
			edit: func(t *testing.T, files map[string]string, srvURL string) {
				files["package-lock.json"] = npmLockJSON(t, map[string]map[string]any{
					"node_modules/@acme/other": {"version": "1.0.0", "resolved": srvURL + "/@acme/other/-/other-1.0.0.tgz"},
				})
			},
			word: "entry",
		},
		{
			name: "workspace package not in the tree",
			edit: func(t *testing.T, files map[string]string, _ string) {
				files["package-lock.json"] = npmLockJSON(t, map[string]map[string]any{
					"node_modules/@acme/ui": {"resolved": "packages/ui", "link": true},
				})
			},
			word: "@acme/ui",
		},
		{
			name: "not from a registry",
			edit: func(t *testing.T, files map[string]string, _ string) {
				files["package-lock.json"] = npmLockJSON(t, map[string]map[string]any{
					"node_modules/@acme/ui": {"version": "1.2.3", "resolved": "git+ssh://git@example.test/acme/ui.git#abc123"},
				})
			},
			word: "installed",
		},
		{
			name: "a patched package",
			edit: func(_ *testing.T, files map[string]string, _ string) {
				const patch = "patch:@acme/ui@npm%3A1.2.3#./patches/ui.patch"

				delete(files, "package-lock.json")
				files["frontend/package.json"] = `{"name":"frontend","dependencies":{"@acme/ui":"` + patch + `"}}`
				files["yarn.lock"] = "__metadata:\n  version: 8\n\n\"@acme/ui@" + patch + "\":\n" +
					"  version: 1.2.3\n  resolution: \"@acme/ui@" + patch + "::version=1.2.3&hash=abc\"\n"
			},
			word: "installed",
		},
		{
			name: "a tarball package",
			edit: func(_ *testing.T, files map[string]string, _ string) {
				delete(files, "package-lock.json")
				files["pnpm-lock.yaml"] = "lockfileVersion: '9.0'\n\nimporters:\n  frontend:\n    dependencies:\n" +
					"      '@acme/ui':\n        specifier: ^1.2.0\n        version: 1.2.3\n\n" +
					"packages:\n  '@acme/ui@1.2.3':\n" +
					"    resolution: {integrity: sha512-ui123, tarball: 'https://example.test/ui.tgz'}\n"
			},
			word: "installed",
		},
		{
			name: "unsafe version",
			edit: func(t *testing.T, files map[string]string, srvURL string) {
				files["package-lock.json"] = npmLockJSON(t, map[string]map[string]any{
					"node_modules/@acme/ui": {"version": "../1.2.3", "resolved": srvURL + uiPath},
				})
			},
			word: "version",
		},
		{
			name: "unreadable registry config",
			edit: func(_ *testing.T, files map[string]string, _ string) {
				files[".yarnrc.yml"] = "npmRegistryServer: [\n"
			},
			word: "config",
		},
		{
			name: "registry from an environment variable",
			edit: func(_ *testing.T, files map[string]string, _ string) {
				files[".npmrc"] = "registry=${ACME_REGISTRY}\n"
			},
			word: "environment",
		},
		{
			name:   "registry needs a login",
			status: http.StatusUnauthorized,
			word:   "login",
		},
		{
			name: "unreachable registry",
			edit: func(_ *testing.T, files map[string]string, _ string) {
				files[".npmrc"] = "registry=" + closed.URL + "/\n"
			},
			word: "reached",
		},
		{
			name: "missing exports subpath",
			spec: "@acme/ui/theme.css",
			edit: func(_ *testing.T, files map[string]string, _ string) {
				files["frontend/src/index.css"] = "@import '@acme/ui/theme.css';\n"
			},
			word: "theme.css",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := registryServer(t, tarballs, tc.status)

			files := portalTree(t, srv.URL, tarballs)
			if tc.edit != nil {
				tc.edit(t, files, srv.URL)
			}

			m, err := gatherPortal(t, srv, t.TempDir(), files)
			require.NoError(t, err)

			spec := tc.spec
			if spec == "" {
				spec = coreImport
			}

			require.Len(t, m.Gaps, 1, "%+v", m.Gaps)
			assert.Equal(t, "portal/frontend", m.Gaps[0].Root)
			assert.Equal(t, spec, m.Gaps[0].Import)
			assert.Contains(t, m.Gaps[0].Reason, tc.word)
			assert.Empty(t, m.Packages)
			require.Len(t, m.Roots, 1)
		})
	}
}

func TestGather_LockedURL(t *testing.T) {
	tarballs := acmeTarballs(t, "@import './theme.css';", ":root{--acme-grey-8:#ebebeb}")
	ui := integrity(tarballs[uiPath])

	tests := []struct {
		name      string
		rangeSpec string
		lock      func(t *testing.T, srvURL string) (file, content string)
		gap       string // a word of the gap reason; "" when the package is kept
	}{
		{
			name:      "an unchecked url on another host is not fetched",
			rangeSpec: "github:acme/ui#v1",
			lock: func(*testing.T, string) (string, string) {
				return "yarn.lock", "\"@acme/ui@github:acme/ui#v1\":\n  version \"1.2.3\"\n" +
					"  resolved \"https://codeload.example.test/acme/ui/tar.gz/v1\"\n"
			},
			gap: "installed",
		},
		{
			name:      "a checked url on another host is fetched from the registry",
			rangeSpec: "^1.2.0",
			lock: func(*testing.T, string) (string, string) {
				return "yarn.lock", "\"@acme/ui@^1.2.0\":\n  version \"1.2.3\"\n" +
					"  resolved \"https://mirror.example.test/acme-ui-1.2.3.tgz#abc\"\n  integrity " + ui + "\n"
			},
		},
		{
			name:      "an http url on the registry host is fetched over https",
			rangeSpec: "^1.2.0",
			lock: func(t *testing.T, srvURL string) (string, string) {
				return "package-lock.json", npmLockJSON(t, map[string]map[string]any{
					"node_modules/@acme/ui": {
						"version": "1.2.3", "resolved": strings.Replace(srvURL, "https:", "http:", 1) + uiPath, "integrity": ui,
					},
				})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)

				body, ok := tarballs[r.URL.Path]
				if !ok {
					http.NotFound(w, r)

					return
				}

				_, _ = w.Write(body)
			}))
			t.Cleanup(srv.Close)

			files := portalTree(t, srv.URL, tarballs)
			files["frontend/package.json"] = `{"name":"frontend","dependencies":{"@acme/ui":"` + tc.rangeSpec + `"}}`
			delete(files, "package-lock.json")

			file, content := tc.lock(t, srv.URL)
			files[file] = content

			m, err := gatherPortal(t, srv, t.TempDir(), files)
			require.NoError(t, err)

			if tc.gap != "" {
				require.Len(t, m.Gaps, 1, "%+v", m.Gaps)
				assert.Contains(t, m.Gaps[0].Reason, tc.gap)
				assert.Empty(t, m.Packages)
				assert.Zero(t, requests.Load())

				return
			}

			assert.Empty(t, m.Gaps)
			require.Len(t, m.Packages, 1)
			assert.Equal(t, uiDir, m.Packages[0].Dir)
			assert.True(t, m.Packages[0].Verified)
		})
	}
}

func TestGather_KeptCSSCap(t *testing.T) {
	// "a;" reflows to three bytes, so each package keeps about 5 MiB.
	filler := strings.Repeat("a;", 7<<18)
	tarballs := acmeTarballs(t,
		"@import '@acme/tokens/vars.css';\n"+filler,
		":root{--acme-grey-8:#ebebeb}\n"+filler)
	srv := registryServer(t, tarballs, 0)

	root := t.TempDir()

	m, err := gatherPortal(t, srv, root, portalTree(t, srv.URL, tarballs))
	require.NoError(t, err)

	require.Len(t, m.Packages, 1)
	assert.Equal(t, uiDir, m.Packages[0].Dir)
	assert.NoDirExists(t, filepath.Join(root, tokensDir))

	require.Len(t, m.Gaps, 1)
	assert.Equal(t, "@acme/tokens/vars.css", m.Gaps[0].Import)
	assert.Contains(t, m.Gaps[0].Reason, "MiB")

	require.Len(t, m.Roots, 1)
	assert.Equal(t, []string{uiDir}, m.Roots[0].Packages)
}

func TestGather_CancelledContext(t *testing.T) {
	tarballs := defaultTarballs(t)
	srv := registryServer(t, tarballs, 0)

	root := t.TempDir()
	writeTree(t, filepath.Join(root, "portal"), portalTree(t, srv.URL, tarballs))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := NewGatherer(t.TempDir(), srv.Client().Transport).Gather(ctx, portalRequest(root))
	require.ErrorIs(t, err, context.Canceled)
}

func TestGather_TwoRootsKeepTokensApart(t *testing.T) {
	srv := registryServer(t, nil, 0)

	m, err := gatherPortal(t, srv, t.TempDir(), map[string]string{
		"frontend/package.json":  `{"name":"frontend"}`,
		"frontend/src/main.tsx":  "import './index.css'\n",
		"frontend/src/index.css": ":root{--brand:#111111}\n.a{color:var(--brand)}\n",
		"admin/package.json":     `{"name":"admin"}`,
		"admin/src/main.tsx":     "import './index.css'\n",
		"admin/src/index.css":    ":root{--brand:#222222}\n.b{color:var(--brand)}\n",
	})
	require.NoError(t, err)

	assert.Empty(t, m.Gaps)
	require.Len(t, m.Roots, 2)
	assert.Equal(t, "portal/admin", m.Roots[0].Path)
	assert.Equal(t, "#222222", tokenValue(t, m.Roots[0], "--brand"))
	assert.Equal(t, "portal/frontend", m.Roots[1].Path)
	assert.Equal(t, "#111111", tokenValue(t, m.Roots[1], "--brand"))
}

func TestGather_NoFrontend(t *testing.T) {
	srv := registryServer(t, nil, 0)

	m, err := gatherPortal(t, srv, t.TempDir(), map[string]string{
		"go.mod":  "module example.test/portal\n",
		"main.go": "package main\n",
	})
	require.NoError(t, err)
	assert.Equal(t, Material{}, m)
}

func TestGather_ClassicFileEntryIsNotFetched(t *testing.T) {
	tarballs := acmeTarballs(t, "@import './theme.css';", ":root{--acme-grey-8:#ebebeb}")

	var requests atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)

		body, ok := tarballs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	files := portalTree(t, srv.URL, tarballs)
	delete(files, "package-lock.json")
	files["frontend/package.json"] = `{"name":"frontend","dependencies":{"@acme/ui":"file:./vendor/ui"}}`
	files["yarn.lock"] = "# yarn lockfile v1\n\n\"@acme/ui@file:./vendor/ui\":\n  version \"1.2.3\"\n"

	m, err := gatherPortal(t, srv, t.TempDir(), files)
	require.NoError(t, err)

	assert.Zero(t, requests.Load())
	assert.Empty(t, m.Packages)
	require.Len(t, m.Gaps, 1, "%+v", m.Gaps)
	assert.Equal(t, "portal/frontend", m.Gaps[0].Root)
	assert.Equal(t, coreImport, m.Gaps[0].Import)
	assert.NotEmpty(t, m.Gaps[0].Reason)
}

// hangingPortal writes, under a new tool root it returns, a tree whose frontend
// imports @acme/a and @acme/b, both locked on a registry that never answers.
func hangingPortal(t *testing.T) (*httptest.Server, string) {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(srv.Close)

	return srv, hangingRoot(t, srv.URL)
}

// hangingRoot is a new tool root holding hangingPortal's tree for the registry at srvURL.
func hangingRoot(t *testing.T, srvURL string) string {
	t.Helper()

	root := t.TempDir()
	writeTree(t, filepath.Join(root, "portal"), map[string]string{
		".npmrc":                 "registry=" + srvURL + "/\n",
		"frontend/package.json":  `{"name":"frontend","dependencies":{"@acme/a":"1.0.0","@acme/b":"1.0.0"}}`,
		"frontend/src/index.css": "@import '@acme/a/x.css';\n@import '@acme/b/x.css';\n",
		"package-lock.json": npmLockJSON(t, map[string]map[string]any{
			"node_modules/@acme/a": {"version": "1.0.0", "resolved": srvURL + "/a.tgz"},
			"node_modules/@acme/b": {"version": "1.0.0", "resolved": srvURL + "/b.tgz"},
		}),
	})

	return root
}

func TestGather_AnUnreachableHostIsSkippedByTheNextGather(t *testing.T) {
	srv, root := hangingPortal(t)

	ct := &countingTransport{rt: srv.Client().Transport}
	g := NewGatherer(t.TempDir(), ct)
	g.client.Timeout = 100 * time.Millisecond

	for i := range 2 {
		m, err := g.Gather(t.Context(), portalRequest(root))
		require.NoError(t, err)

		// The first gather's one request marks the host; the second sends none.
		assert.EqualValues(t, 1, ct.n.Load(), "requests after gather %d", i+1)

		imports := map[string]bool{}
		for _, gap := range m.Gaps {
			imports[gap.Import] = true
		}

		assert.True(t, imports["@acme/a/x.css"], "gather %d", i+1)
		assert.True(t, imports["@acme/b/x.css"], "gather %d", i+1)
	}
}

func TestGather_ConcurrentGathersShareOneGatherer(t *testing.T) {
	srv, first := hangingPortal(t)

	g := NewGatherer(t.TempDir(), srv.Client().Transport)
	g.client.Timeout = 100 * time.Millisecond

	roots := []string{first}
	for range 7 {
		roots = append(roots, hangingRoot(t, srv.URL))
	}

	mats := make([]Material, len(roots))
	errs := make([]error, len(roots))

	var wg sync.WaitGroup

	for i, root := range roots {
		wg.Add(1)

		go func() {
			defer wg.Done()

			mats[i], errs[i] = g.Gather(t.Context(), portalRequest(root))
		}()
	}

	wg.Wait()

	for i := range roots {
		require.NoError(t, errs[i])

		imports := map[string]bool{}
		for _, gap := range mats[i].Gaps {
			imports[gap.Import] = true
		}

		assert.True(t, imports["@acme/a/x.css"], "gather %d", i)
		assert.True(t, imports["@acme/b/x.css"], "gather %d", i)
	}
}

func TestGather_FetchingIsBounded(t *testing.T) {
	srv, root := hangingPortal(t)

	g := NewGatherer(t.TempDir(), srv.Client().Transport)
	g.fetchFor = 200 * time.Millisecond

	start := time.Now()
	m, err := g.Gather(t.Context(), portalRequest(root))

	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)

	imports := map[string]bool{}

	for _, gap := range m.Gaps {
		assert.Equal(t, "portal/frontend", gap.Root)
		assert.NotEmpty(t, gap.Reason)

		imports[gap.Import] = true
	}

	assert.True(t, imports["@acme/a/x.css"])
	assert.True(t, imports["@acme/b/x.css"])
}

func TestGather_AnUnreachedOwnFileStillReachesItsPackage(t *testing.T) {
	tarballs := defaultTarballs(t)
	srv := registryServer(t, tarballs, 0)

	// main.tsx loads app.css; index.css, which imports @acme/ui, is loaded by
	// nothing the scan reads.
	files := portalTree(t, srv.URL, tarballs)
	files["frontend/src/main.tsx"] = "import './app.css'\n"
	files["frontend/src/app.css"] = ".app{margin:0}\n"

	m, err := gatherPortal(t, srv, t.TempDir(), files)
	require.NoError(t, err)

	require.Len(t, m.Roots, 1)
	assert.Equal(t, "#ebebeb", tokenValue(t, m.Roots[0], "--color-border"))
	assert.Equal(t, []string{tokensDir, uiDir}, m.Roots[0].Packages)

	require.Len(t, m.Gaps, 1, "%+v", m.Gaps)
	assert.Equal(t, Gap{Root: "portal/frontend"}, Gap{Root: m.Gaps[0].Root, Import: m.Gaps[0].Import})
	assert.NotEmpty(t, m.Gaps[0].Reason)
}

// rootAt returns the root of m at path.
func rootAt(t *testing.T, m Material, path string) Root {
	t.Helper()

	for _, r := range m.Roots {
		if r.Path == path {
			return r
		}
	}

	require.Failf(t, "root not found", "%s in %+v", path, m.Roots)

	return Root{}
}

// gapsOf returns the gaps of m that belong to the root at path.
func gapsOf(m Material, path string) []Gap {
	var gaps []Gap

	for _, g := range m.Gaps {
		if g.Root == path {
			gaps = append(gaps, g)
		}
	}

	return gaps
}

func TestGather_RelativeImportLeavingTheRootIsRead(t *testing.T) {
	tests := []struct {
		name  string
		spec  string // imported by frontend/src/index.css
		files map[string]string
		token string
		want  string
	}{
		{
			name: "a workspace package's file",
			spec: "../../packages/ui/tokens.css",
			files: map[string]string{
				"packages/ui/package.json": `{"name":"@acme/ui"}`,
				"packages/ui/tokens.css":   ":root{--brand:#123456}",
			},
			token: "--brand",
			want:  "#123456",
		},
		{
			name: "an installed package's file",
			spec: "../node_modules/bootstrap/dist/css/bootstrap.css",
			files: map[string]string{
				"frontend/node_modules/bootstrap/dist/css/bootstrap.css": ":root{--bs-primary:#0d6efd}",
			},
			token: "--bs-primary",
			want:  "#0d6efd",
		},
		{
			name: "a bare-looking import of a vendored file",
			spec: "vendor/theme.css",
			files: map[string]string{
				"frontend/src/vendor/theme.css": ":root{--brand:#654321}",
			},
			token: "--brand",
			want:  "#654321",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{
				"frontend/package.json":  `{"name":"frontend"}`,
				"frontend/src/main.tsx":  "import './index.css'\n",
				"frontend/src/index.css": "@import '" + tc.spec + "';\n.a{color:var(" + tc.token + ")}\n",
			}
			for name, content := range tc.files {
				files[name] = content
			}

			m, err := gatherPortal(t, registryServer(t, nil, 0), t.TempDir(), files)
			require.NoError(t, err)

			assert.Empty(t, gapsOf(m, "portal/frontend"))
			assert.Equal(t, tc.want, tokenValue(t, rootAt(t, m, "portal/frontend"), tc.token))
		})
	}
}

// workspaceThenApp is a tree whose frontend's main.tsx is mainTSX, beside an
// app.css declaring --brand #654321, with @acme/tokens a workspace package
// declaring it #123456 and locked as an npm link.
func workspaceThenApp(t *testing.T, mainTSX string) map[string]string {
	t.Helper()

	return map[string]string{
		"package-lock.json": npmLockJSON(t, map[string]map[string]any{
			"node_modules/@acme/tokens": {"resolved": "packages/tokens", "link": true},
		}),
		"frontend/package.json":        `{"name":"frontend","dependencies":{"@acme/tokens":"^1.0.0"}}`,
		"frontend/src/main.tsx":        mainTSX,
		"frontend/src/app.css":         ":root{--brand:#654321}\n.a{color:var(--brand)}\n",
		"packages/tokens/package.json": `{"name":"@acme/tokens","version":"1.0.0"}`,
		"packages/tokens/tokens.css":   ":root{--brand:#123456}",
	}
}

func TestGather_WorkspacePackageImportedByName(t *testing.T) {
	npmLink := func(t *testing.T) string {
		t.Helper()

		return npmLockJSON(t, map[string]map[string]any{
			"node_modules/@acme/tokens": {"resolved": "packages/tokens", "link": true},
			"packages/tokens":           {"name": "@acme/tokens", "version": "1.0.0"},
		})
	}

	npmApp := func(t *testing.T, spec string) map[string]string {
		t.Helper()

		return map[string]string{
			"package-lock.json":          npmLink(t),
			"frontend/package.json":      `{"name":"frontend","dependencies":{"@acme/tokens":"^1.0.0"}}`,
			"frontend/src/main.tsx":      "import './index.css'\n",
			"frontend/src/index.css":     "@import '" + spec + "';\n.a{color:var(--brand)}\n",
			"packages/tokens/tokens.css": ":root{--brand:#123456}",
		}
	}

	tests := []struct {
		name  string
		files func(t *testing.T) map[string]string
		root  string // Root.Path of the app
		spec  string // the app's import, for a gap
		want  string // --brand's value; "" when the import is a gap
		data  string // a tool-root-relative path or a name the gap's reason holds
	}{
		{
			name: "an npm link entry",
			files: func(t *testing.T) map[string]string {
				t.Helper()

				files := npmApp(t, "@acme/tokens/tokens.css")
				files["packages/tokens/package.json"] = `{"name":"@acme/tokens","version":"1.0.0"}`

				return files
			},
			root: "portal/frontend",
			want: "#123456",
		},
		{
			name: "a pnpm link importer dependency",
			files: func(*testing.T) map[string]string {
				return map[string]string{
					"pnpm-lock.yaml": "lockfileVersion: '9.0'\n\nimporters:\n  apps/web:\n    dependencies:\n" +
						"      '@repo/ui':\n        specifier: workspace:*\n        version: link:../../packages/ui\n" +
						"  packages/ui: {}\n",
					"apps/web/package.json":       `{"name":"web","dependencies":{"@repo/ui":"workspace:*"}}`,
					"apps/web/src/main.tsx":       "import './index.css'\n",
					"apps/web/src/index.css":      "@import '@repo/ui/styles.css';\n.a{color:var(--brand)}\n",
					"packages/ui/package.json":    `{"name":"@repo/ui","exports":{"./styles.css":"./dist/index.css"}}`,
					"packages/ui/dist/index.css":  "@import './tokens.css';",
					"packages/ui/dist/tokens.css": ":root{--brand:#123456}",
				}
			},
			root: "portal/apps/web",
			want: "#123456",
		},
		{
			name: "a Yarn Berry workspace entry",
			files: func(*testing.T) map[string]string {
				return map[string]string{
					"yarn.lock": "__metadata:\n  version: 8\n  cacheKey: 10c0\n\n" +
						"\"@workspace/ui@workspace:*, @workspace/ui@workspace:packages/ui\":\n" +
						"  version: 0.0.0-use.local\n  resolution: \"@workspace/ui@workspace:packages/ui\"\n" +
						"  languageName: unknown\n  linkType: soft\n",
					"apps/web/package.json":   `{"name":"web","dependencies":{"@workspace/ui":"workspace:*"}}`,
					"apps/web/app/layout.tsx": "import '@workspace/ui/globals.css'\n",
					"apps/web/app/page.css":   ".a{color:var(--brand)}\n",
					"packages/ui/package.json": `{"name":"@workspace/ui","exports":` +
						`{"./globals.css":"./src/styles/globals.css","./components/*":"./src/components/*.tsx"}}`,
					"packages/ui/src/styles/globals.css": ":root{--brand:#123456}",
				}
			},
			root: "portal/apps/web",
			want: "#123456",
		},
		{
			name: "a later import overrides the workspace file",
			files: func(t *testing.T) map[string]string {
				t.Helper()

				return workspaceThenApp(t, "import '@acme/tokens/tokens.css'\nimport './app.css'\n")
			},
			root: "portal/frontend",
			want: "#654321",
		},
		{
			name: "the workspace file overrides an earlier import",
			files: func(t *testing.T) map[string]string {
				t.Helper()

				return workspaceThenApp(t, "import './app.css'\nimport '@acme/tokens/tokens.css'\n")
			},
			root: "portal/frontend",
			want: "#123456",
		},
		{
			name: "a subpath that names no file",
			files: func(t *testing.T) map[string]string {
				t.Helper()

				files := npmApp(t, "@acme/tokens/missing.css")
				files["packages/tokens/package.json"] = `{"name":"@acme/tokens","version":"1.0.0"}`

				return files
			},
			root: "portal/frontend",
			spec: "@acme/tokens/missing.css",
			data: "portal/packages/tokens/package.json",
		},
		{
			name: "a workspace file over the size cap",
			files: func(t *testing.T) map[string]string {
				t.Helper()

				files := npmApp(t, "@acme/tokens/big.css")
				files["packages/tokens/package.json"] = `{"name":"@acme/tokens","version":"1.0.0"}`
				files["packages/tokens/big.css"] = strings.Repeat(" ", maxKeptFileBytes+1)

				return files
			},
			root: "portal/frontend",
			spec: "@acme/tokens/big.css",
			data: "portal/packages/tokens/big.css",
		},
		{
			name: "no package.json of that name",
			files: func(t *testing.T) map[string]string {
				t.Helper()

				return npmApp(t, "@acme/tokens/tokens.css")
			},
			root: "portal/frontend",
			spec: "@acme/tokens/tokens.css",
			data: "@acme/tokens",
		},
		{
			name: "two package.json files of that name",
			files: func(t *testing.T) map[string]string {
				t.Helper()

				files := npmApp(t, "@acme/tokens/tokens.css")
				files["packages/tokens/package.json"] = `{"name":"@acme/tokens","version":"1.0.0"}`
				files["legacy/tokens/package.json"] = `{"name":"@acme/tokens","version":"0.9.0"}`

				return files
			},
			root: "portal/frontend",
			spec: "@acme/tokens/tokens.css",
			data: "portal/legacy/tokens/package.json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32

			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.NotFound(w, r)
			}))
			t.Cleanup(srv.Close)

			files := tc.files(t)
			files[".npmrc"] = "registry=" + srv.URL + "/\n"

			m, err := gatherPortal(t, srv, t.TempDir(), files)
			require.NoError(t, err)

			assert.Zero(t, requests.Load())

			gaps := gapsOf(m, tc.root)

			if tc.want != "" {
				assert.Empty(t, gaps)
				assert.Equal(t, tc.want, tokenValue(t, rootAt(t, m, tc.root), "--brand"))

				return
			}

			require.Len(t, gaps, 1, "%+v", m.Gaps)
			assert.Equal(t, tc.spec, gaps[0].Import)
			assert.Contains(t, gaps[0].Reason, tc.data)
		})
	}
}

func TestGather_AWorkspaceFileImportsItsOwnPackage(t *testing.T) {
	const basePath = "/@acme/base/-/base-1.0.0.tgz"

	tarballs := map[string][]byte{basePath: tgz(t, map[string]string{
		"package/package.json": `{"name":"@acme/base","version":"1.0.0"}`,
		"package/base.css":     ":root{--base:#0000aa}",
	})}
	srv := registryServer(t, tarballs, 0)

	// Only packages/ui declares @acme/base, under its own pnpm importer.
	m, err := gatherPortal(t, srv, t.TempDir(), map[string]string{
		".npmrc": "registry=" + srv.URL + "/\n",
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\n\nimporters:\n" +
			"  apps/web:\n    dependencies:\n      '@workspace/ui':\n" +
			"        specifier: workspace:*\n        version: link:../../packages/ui\n" +
			"  packages/ui:\n    dependencies:\n      '@acme/base':\n        specifier: ^1.0.0\n        version: 1.0.0\n\n" +
			"packages:\n  '@acme/base@1.0.0':\n    resolution: {integrity: " + integrity(tarballs[basePath]) + "}\n\n" +
			"snapshots:\n  '@acme/base@1.0.0': {}\n",
		"apps/web/package.json":  `{"name":"web","dependencies":{"@workspace/ui":"workspace:*"}}`,
		"apps/web/src/main.tsx":  "import './index.css'\n",
		"apps/web/src/index.css": "@import '@workspace/ui/globals.css';\n.a{color:var(--base)}\n",
		"packages/ui/package.json": `{"name":"@workspace/ui","dependencies":{"@acme/base":"^1.0.0"},` +
			`"exports":{"./globals.css":"./src/globals.css"}}`,
		"packages/ui/src/globals.css": "@import '@acme/base/base.css';\n:root{--ui:#123456}",
	})
	require.NoError(t, err)

	assert.Empty(t, gapsOf(m, "portal/apps/web"))

	web := rootAt(t, m, "portal/apps/web")
	assert.Equal(t, "#0000aa", tokenValue(t, web, "--base"))
	assert.Equal(t, []string{"_npm+@acme+base@1.0.0"}, web.Packages)
}

func TestGather_AWorkspaceFilesPackageImportIsAGap(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		data  string // a tool-root-relative path the gap's reason holds
	}{
		{
			// The app's own lockfile cannot name the workspace's importer.
			name: "the workspace is outside the lockfile's directory",
			files: map[string]string{
				"apps/web/package-lock.json": npmLockJSON(t, map[string]map[string]any{
					"node_modules/@acme/tokens": {"resolved": "../../packages/tokens", "link": true},
				}),
				"apps/web/package.json":        `{"name":"web","dependencies":{"@acme/tokens":"^1.0.0"}}`,
				"apps/web/src/main.tsx":        "import '@acme/tokens/tokens.css'\n",
				"packages/tokens/package.json": `{"name":"@acme/tokens","dependencies":{"@acme/base":"^1.0.0"}}`,
				"packages/tokens/tokens.css":   "@import '@acme/base/base.css';\n:root{--brand:#123456}",
			},
			data: "portal/packages/tokens/package.json",
		},
		{
			// Only the app declares @acme/base; the workspace's own package.json counts.
			name: "the workspace does not declare it",
			files: map[string]string{
				"package-lock.json": npmLockJSON(t, map[string]map[string]any{
					"node_modules/@workspace/ui": {"resolved": "packages/ui", "link": true},
					"node_modules/@acme/base":    {"version": "1.0.0", "resolved": "https://registry.example.test/base.tgz"},
				}),
				"apps/web/package.json":    `{"name":"web","dependencies":{"@workspace/ui":"*","@acme/base":"^1.0.0"}}`,
				"apps/web/src/main.tsx":    "import '@workspace/ui/globals.css'\n",
				"packages/ui/package.json": `{"name":"@workspace/ui"}`,
				"packages/ui/globals.css":  "@import '@acme/base/base.css';\n:root{--brand:#123456}",
			},
			data: "portal/packages/ui/package.json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := gatherPortal(t, registryServer(t, nil, 0), t.TempDir(), tc.files)
			require.NoError(t, err)

			gaps := gapsOf(m, "portal/apps/web")
			require.Len(t, gaps, 1, "%+v", m.Gaps)
			assert.Equal(t, "@acme/base/base.css", gaps[0].Import)
			assert.Contains(t, gaps[0].Reason, tc.data)
		})
	}
}

func TestGather_RelativeImportThatCannotBeReadIsAGap(t *testing.T) {
	tests := []struct {
		name   string
		spec   string // imported by frontend/src/index.css
		link   bool   // frontend/src/linked.css is a symlink to a file outside the tree
		noFile bool   // the gap's reason is reasonNoFile
	}{
		{name: "a missing file", spec: "./missing.css", noFile: true},
		{name: "a file outside the tree", spec: "../../../outside.css"},
		{name: "a symlink", spec: "./linked.css", link: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tree := filepath.Join(root, "portal")

			writeTree(t, root, map[string]string{"outside.css": ":root{--brand:#ff0000}"})
			writeTree(t, tree, map[string]string{
				"frontend/package.json":  `{"name":"frontend"}`,
				"frontend/src/main.tsx":  "import './index.css'\n",
				"frontend/src/index.css": "@import '" + tc.spec + "';\n.a{color:var(--brand)}\n",
			})

			if tc.link {
				require.NoError(t, os.Symlink(filepath.Join(root, "outside.css"), filepath.Join(tree, "frontend", "src", "linked.css")))
			}

			srv := registryServer(t, nil, 0)

			m, err := NewGatherer(t.TempDir(), srv.Client().Transport).Gather(t.Context(), portalRequest(root))
			require.NoError(t, err)

			require.Len(t, m.Gaps, 1, "%+v", m.Gaps)
			assert.Equal(t, "portal/frontend", m.Gaps[0].Root)
			assert.Equal(t, tc.spec, m.Gaps[0].Import)
			assert.NotEmpty(t, m.Gaps[0].Reason)

			// Which reason applies is pinned, not its wording.
			if tc.noFile {
				assert.Equal(t, reasonNoFile, m.Gaps[0].Reason)
			} else {
				assert.NotEqual(t, reasonNoFile, m.Gaps[0].Reason)
			}
		})
	}
}

func TestGather_TokensFollowTheLoadedCSS(t *testing.T) {
	primary := func(v string) string { return ":root{--primary:" + v + "}\n.a{color:var(--primary)}\n" }

	tests := []struct {
		name  string
		files map[string]string // under frontend/
		want  string
		gaps  []Gap // Root and Import of each
	}{
		{
			name: "a theme nothing imports",
			files: map[string]string{
				"src/main.tsx":        "import './index.css'\n",
				"src/index.css":       primary("#0055ff"),
				"src/themes/zeta.css": primary("#ff0000"),
			},
			want: "#0055ff",
		},
		{
			name: "a vendored copy",
			files: map[string]string{
				"src/main.tsx":                 "import './app.css'\n",
				"src/app.css":                  primary("#0055ff"),
				"static/lib/bootstrap.min.css": ":root{--primary:#ff0000}",
			},
			want: "#0055ff",
		},
		{
			name: "a script's later import wins",
			files: map[string]string{
				"src/main.tsx":   "import './tokens.css'\nimport './index.css'\n",
				"src/index.css":  primary("#0055ff"),
				"src/tokens.css": primary("#ff0000"),
			},
			want: "#0055ff",
		},
		{
			name: "a script's later import wins, swapped",
			files: map[string]string{
				"src/main.tsx":   "import './index.css'\nimport './tokens.css'\n",
				"src/index.css":  primary("#0055ff"),
				"src/tokens.css": primary("#ff0000"),
			},
			want: "#ff0000",
		},
		{
			name: "a shallow entry loads before a component",
			files: map[string]string{
				"src/main.tsx":  "import './index.css'\n",
				"src/index.css": primary("#ff0000"),
				"src/App.tsx":   "import './App.css'\n",
				"src/App.css":   primary("#0055ff"),
			},
			want: "#0055ff",
		},
		{
			name: "an HTML entry",
			files: map[string]string{
				"index.html":     `<link rel="stylesheet" href="/css/site.css">`,
				"css/site.css":   primary("#0055ff"),
				"css/zz-old.css": primary("#ff0000"),
			},
			want: "#0055ff",
		},
		{
			name: "a nested page's / link to a file of the root",
			files: map[string]string{
				"about/index.html": `<link rel="stylesheet" href="/css/site.css">`,
				"css/site.css":     primary("#0055ff"),
				"css/zz-old.css":   primary("#ff0000"),
			},
			want: "#0055ff",
		},
		{
			name: "own CSS loaded by a file the scan skips fills a name",
			files: map[string]string{
				"src/layouts/Layout.astro":   "---\nimport '../styles/global.css'\n---\n<slot />\n",
				"src/styles/global.css":      ":root{--primary:#0055ff}\n",
				"src/components/Counter.tsx": "import './Counter.css'\n",
				"src/components/Counter.css": ".counter{color:var(--primary)}\n",
			},
			want: "#0055ff",
			gaps: []Gap{{Root: "portal/frontend"}},
		},
		{
			name: "an HTML entry's / link served from public",
			files: map[string]string{
				"index.html":       `<link rel="stylesheet" href="/theme.css">`,
				"public/theme.css": primary("#0055ff"),
				"src/legacy.css":   primary("#ff0000"),
			},
			want: "#0055ff",
		},
		{
			name: "an HTML entry's link with a query",
			files: map[string]string{
				"index.html":     `<link rel="stylesheet" href="css/site.css?v=3">`,
				"css/site.css":   primary("#0055ff"),
				"css/zz-old.css": primary("#ff0000"),
			},
			want: "#0055ff",
		},
		{
			name: "an HTML entry's link to a missing file",
			files: map[string]string{
				"index.html":     `<link rel="stylesheet" href="css/site.css"><link rel="stylesheet" href="css/gone.css">`,
				"css/site.css":   primary("#0055ff"),
				"css/zz-old.css": primary("#ff0000"),
			},
			want: "#0055ff",
			gaps: []Gap{{Root: "portal/frontend", Import: "css/gone.css"}},
		},
		{
			name: "an import alias",
			files: map[string]string{
				"src/pages/_app.tsx":     "import '@/styles/globals.css'\n",
				"src/styles/globals.css": primary("#0055ff"),
				"src/styles/legacy.css":  primary("#ff0000"),
			},
			want: "#0055ff",
		},
		{
			name: "an import alias that does not resolve",
			files: map[string]string{
				"src/main.tsx":  "import './index.css'\nimport '@/styles/gone.css'\n",
				"src/index.css": primary("#0055ff"),
			},
			want: "#0055ff",
			gaps: []Gap{{Root: "portal/frontend", Import: "@/styles/gone.css"}},
		},
		{
			name: "no entry reaches the root's CSS",
			files: map[string]string{
				"src/index.css": primary("#0055ff"),
				"src/zeta.css":  primary("#ff0000"),
			},
			want: "#ff0000",
			gaps: []Gap{{Root: "portal/frontend"}},
		},
		{
			name: "no entry reaches own files that import each other",
			files: map[string]string{
				"src/a.css": "@import './b.css';\n" + primary("#0055ff"),
				"src/b.css": "@import './a.css';\n" + primary("#ff0000"),
			},
			want: "#0055ff",
			gaps: []Gap{{Root: "portal/frontend"}},
		},
		{
			name: "no entry reaches a single CSS file",
			files: map[string]string{
				"src/index.css": primary("#0055ff"),
			},
			want: "#0055ff",
		},
		{
			name: "a script imports a missing file",
			files: map[string]string{
				"src/main.tsx":  "import './index.css'\nimport './gone.css'\n",
				"src/index.css": primary("#0055ff"),
			},
			want: "#0055ff",
			gaps: []Gap{{Root: "portal/frontend", Import: "./gone.css"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"frontend/package.json": `{"name":"frontend"}`}
			for name, content := range tc.files {
				files["frontend/"+name] = content
			}

			m, err := gatherPortal(t, registryServer(t, nil, 0), t.TempDir(), files)
			require.NoError(t, err)

			require.Len(t, m.Roots, 1)
			assert.Equal(t, tc.want, tokenValue(t, m.Roots[0], "--primary"))

			require.Len(t, m.Gaps, len(tc.gaps), "%+v", m.Gaps)

			for i, want := range tc.gaps {
				assert.Equal(t, want.Root, m.Gaps[i].Root)
				assert.Equal(t, want.Import, m.Gaps[i].Import)
				assert.NotEmpty(t, m.Gaps[i].Reason)
			}
		})
	}
}

func TestGather_StyleLanguageRoots(t *testing.T) {
	tests := []struct {
		name     string
		files    map[string]string
		root     string // "" when no root is expected
		language string // named by the root gap; "" when none is expected
		frame    []string
		brand    []string
	}{
		{
			name: "scss",
			files: map[string]string{
				"app/package.json":              `{"name":"app"}`,
				"app/src/main.ts":               "import './styles.scss'\n",
				"app/src/styles.scss":           ":root{--brand:#123456}",
				"app/src/components/Header.tsx": "export const Header = () => null\n",
				"app/src/logo.svg":              "<svg/>",
			},
			root:     "portal/app",
			language: "SCSS",
			frame:    []string{"portal/app/src/components/Header.tsx"},
			brand:    []string{"portal/app/src/logo.svg"},
		},
		{
			name: "vue style block",
			files: map[string]string{
				"web/package.json": `{"name":"web"}`,
				"web/src/main.ts":  "import './App.vue'\n",
				"web/src/App.vue":  "<template/><style>.a{color:red}</style>",
			},
			root:     "portal/web",
			language: "Vue",
			frame:    []string{"portal/web/src/App.vue"},
		},
		{
			name: "svelte style block with a lang",
			files: map[string]string{
				"web/package.json":   `{"name":"web"}`,
				"web/src/main.ts":    "import './App.svelte'\n",
				"web/src/App.svelte": `<style lang="less">.a{color:red}</style>`,
			},
			root:     "portal/web",
			language: "Less",
			frame:    []string{"portal/web/src/App.svelte"},
		},
		{
			name: "css-in-js",
			files: map[string]string{
				"ui/package.json": `{"dependencies":{"styled-components":"^6"}}`,
				"ui/src/App.tsx":  "export const App = () => null\n",
			},
			root:     "portal/ui",
			language: "styled-components",
			frame:    []string{"portal/ui/src/App.tsx"},
		},
		{
			name: "not a frontend",
			files: map[string]string{
				"api/package.json":  `{"dependencies":{"express":"^4"}}`,
				"api/src/server.ts": "export {}\n",
			},
		},
		{
			name: "own css and an scss file",
			files: map[string]string{
				"app/package.json":   `{"name":"app"}`,
				"app/src/main.ts":    "import './app.css'\n",
				"app/src/app.css":    ":root{--brand:#123456}",
				"app/src/extra.scss": ".a{color:red}",
			},
			root:     "portal/app",
			language: "SCSS",
			frame:    []string{"portal/app/src/app.css"},
		},
		{
			name: "template css and styled-components",
			files: map[string]string{
				"app/package.json":   `{"dependencies":{"styled-components":"^6"}}`,
				"app/src/main.tsx":   "import './index.css'\n",
				"app/src/index.css":  ":root{font-family:system-ui}",
				"app/src/Button.tsx": "export {}\n",
			},
			root:     "portal/app",
			language: "styled-components",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := registryServer(t, nil, 0)

			m, err := gatherPortal(t, srv, t.TempDir(), tt.files)
			require.NoError(t, err)

			if tt.root == "" {
				assert.Empty(t, m.Roots)

				return
			}

			require.Len(t, m.Roots, 1)

			r := rootAt(t, m, tt.root)
			assert.Equal(t, tt.frame, assetPaths(r.Frame))

			var brand []string
			for _, a := range r.Brand {
				brand = append(brand, a.Path)
			}

			assert.Equal(t, tt.brand, brand)

			var named []Gap

			for _, g := range gapsOf(m, tt.root) {
				if g.Import == "" && tt.language != "" && strings.Contains(g.Reason, tt.language) {
					named = append(named, g)
				}
			}

			if tt.language != "" {
				assert.Len(t, named, 1)
			}
		})
	}
}

package design

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()

	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}

func frontendDirs(fs []frontend) []string {
	dirs := make([]string, 0, len(fs))
	for _, f := range fs {
		dirs = append(dirs, f.Dir)
	}

	return dirs
}

func TestScanTree_Roots(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":               `{}`,
		"frontend/package.json":      `{}`,
		"frontend/src/index.css":     `body { margin: 0 }`,
		"admin/package.json":         `{}`,
		"admin/src/main.tsx":         `import '@acme/ui/theme.css'; import { x } from './x';`,
		"server/package.json":        `{}`,
		"server/src/main.ts":         `import './local.js'`,
		"server/src/other.ts":        `export const a = 1`,
		"frontend/src/components.ts": `export const b = 2`,
	})

	got, err := scanTree(root)
	require.NoError(t, err)

	require.Equal(t, []string{"admin", "frontend"}, frontendDirs(got))

	admin := got[0]
	assert.Empty(t, admin.CSS)
	assert.Equal(t, []string{"admin/src/main.tsx"}, admin.Sources)
	assert.Equal(t, []entry{{
		File:    "admin/src/main.tsx",
		Spec:    "@acme/ui/theme.css",
		Name:    "@acme/ui",
		Subpath: "theme.css",
	}}, admin.Entries)

	frontendRoot := got[1]
	assert.Equal(t, []string{"frontend/src/index.css"}, frontendRoot.CSS)
	assert.Equal(t, []string{"frontend/src/components.ts"}, frontendRoot.Sources)
	assert.Empty(t, frontendRoot.Entries)
}

func TestScanTree_RootAtTreeRoot(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":  `{}`,
		"src/style.css": `@import '@acme/ui/core.css';`,
	})

	got, err := scanTree(root)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Empty(t, got[0].Dir)
	assert.Equal(t, []string{"src/style.css"}, got[0].CSS)
	assert.Equal(t, []entry{{
		File:    "src/style.css",
		Spec:    "@acme/ui/core.css",
		Name:    "@acme/ui",
		Subpath: "core.css",
	}}, got[0].Entries)
}

func TestScanTree_NestedRootOwnsItsFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"package.json":           `{}`,
		"style.css":              `a {}`,
		"app.ts":                 `export {}`,
		"admin/package.json":     `{}`,
		"admin/admin.css":        `b {}`,
		"admin/src/admin.ts":     `export {}`,
		"admin/src/theme.tsx":    `import 'pkg-x/theme.css'`,
		"admin/lib/package.json": `{}`,
		"admin/lib/util.ts":      `export {}`,
	})

	got, err := scanTree(root)
	require.NoError(t, err)

	require.Equal(t, []string{"", "admin"}, frontendDirs(got))
	assert.Equal(t, []string{"style.css"}, got[0].CSS)
	assert.Equal(t, []string{"app.ts"}, got[0].Sources)
	assert.Equal(t, []string{"admin/admin.css"}, got[1].CSS)
	assert.Equal(t, []string{"admin/lib/util.ts", "admin/src/admin.ts", "admin/src/theme.tsx"}, got[1].Sources)
}

func TestScanTree_LocalFileWinsOverBareSpecifier(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []entry
	}{
		{
			name: "css import resolving to a sibling subdirectory file is local",
			files: map[string]string{
				"app/src/index.css": `@import 'sub/x.css';`,
				"app/src/sub/x.css": `a {}`,
			},
		},
		{
			name: "css import with no such file is a package",
			files: map[string]string{
				"app/src/index.css": `@import 'sub/x.css';`,
			},
			want: []entry{{File: "app/src/index.css", Spec: "sub/x.css", Name: "sub", Subpath: "x.css"}},
		},
		{
			name: "css import resolves relative to the importing file",
			files: map[string]string{
				"app/src/index.css":   `@import 'sub/x.css';`,
				"app/other/sub/x.css": `a {}`,
			},
			want: []entry{{File: "app/src/index.css", Spec: "sub/x.css", Name: "sub", Subpath: "x.css"}},
		},
		{
			name: "js side-effect import resolving to a local file is local",
			files: map[string]string{
				"app/src/main.ts":   `import 'sub/y.css'`,
				"app/src/sub/y.css": `a {}`,
			},
		},
		{
			name: "js side-effect import with no such file is a package",
			files: map[string]string{
				"app/src/main.ts": `import 'sub/y.css'`,
			},
			want: []entry{{File: "app/src/main.ts", Spec: "sub/y.css", Name: "sub", Subpath: "y.css"}},
		},
		{
			name: "a directory of that name is not a file",
			files: map[string]string{
				"app/src/main.ts":     `import 'sub/y.css'`,
				"app/src/sub/y.css/a": `a {}`,
			},
			want: []entry{{File: "app/src/main.ts", Spec: "sub/y.css", Name: "sub", Subpath: "y.css"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"app/package.json": `{}`,
				// Guarantees a root even when the case only has a JS import.
				"app/base.css": `a {}`,
			}

			for k, v := range tt.files {
				files[k] = v
			}

			writeTree(t, root, files)

			got, err := scanTree(root)
			require.NoError(t, err)
			require.Equal(t, []string{"app"}, frontendDirs(got))
			assert.Equal(t, tt.want, got[0].Entries)
		})
	}
}

func TestScanTree_BareJSImportMakesRootWithoutCSSFile(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"top/package.json": `{}`,
		"top/main.ts":      `import 'sub/z.css'`,
	})

	got, err := scanTree(root)
	require.NoError(t, err)

	require.Equal(t, []string{"top"}, frontendDirs(got))
	assert.Empty(t, got[0].CSS)
	require.Len(t, got[0].Entries, 1)
	assert.Equal(t, "sub", got[0].Entries[0].Name)
}

func TestScanTree_SkipsVendoredDirs(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"frontend/package.json": `{}`,
		"frontend/src/app.ts":   `export {}`,
		"frontend/src/real.css": `a {}`,
		"other/package.json":    `{}`,
		"other/src/main.ts":     `export {}`,
	}

	skipped := []string{"node_modules/x", "dist", ".cache", "build", "out", "coverage", "vendor"}
	for _, dir := range skipped {
		files["frontend/"+dir+"/skipped.css"] = `a {}`
		files["other/"+dir+"/package.json"] = `{}`
		files["other/"+dir+"/skipped.css"] = `a {}`
		files["other/"+dir+"/main.ts"] = `import '@acme/ui/x.css'`
	}

	writeTree(t, root, files)

	got, err := scanTree(root)
	require.NoError(t, err)

	require.Equal(t, []string{"frontend"}, frontendDirs(got))
	assert.Equal(t, []string{"frontend/src/real.css"}, got[0].CSS)
	assert.Equal(t, []string{"frontend/src/app.ts"}, got[0].Sources)
}

func TestScanTree_IgnoresSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"evil.css": `@import '@acme/ui/x.css';`})
	writeTree(t, root, map[string]string{"app/package.json": `{}`})
	require.NoError(t, os.Symlink(filepath.Join(outside, "evil.css"), filepath.Join(root, "app", "link.css")))

	got, err := scanTree(root)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestScanTree_Empty(t *testing.T) {
	got, err := scanTree(t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestScanTree_MissingDir(t *testing.T) {
	_, err := scanTree(filepath.Join(t.TempDir(), "absent"))
	assert.Error(t, err)
}

func TestFrameFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/components/BaseLayout/index.tsx":           ``,
		"src/components/NavigationBar/index.module.css": ``,
		"src/App.tsx":                                ``,
		"src/components/Footer/Footer.tsx":           ``,
		"src/pages/profile/Teams.tsx":                ``,
		"src/components/Header/Header.test.tsx":      ``,
		"src/components/Header/Header.stories.tsx":   ``,
		"src/components/Header/Header.spec.ts":       ``,
		"src/components/Application/Application.tsx": ``,
		"src/main-menu.vue":                          ``,
		"src/theme/nav_item.svelte":                  ``,
		"src/theme/HTMLHero.html":                    ``,
		"src/notes.md":                               ``,
		"node_modules/pkg/Layout.tsx":                ``,
		"dist/App.js":                                ``,
	})

	got := frameFiles(root, frontend{Dir: ""})

	assert.Equal(t, []string{
		"src/App.tsx",
		"src/main-menu.vue",
		"src/theme/HTMLHero.html",
		"src/theme/nav_item.svelte",
		"src/components/Footer/Footer.tsx",
		"src/components/BaseLayout/index.tsx",
		"src/components/NavigationBar/index.module.css",
	}, got)
}

func TestFrameFiles_UnderRootDir(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"admin/src/Layout.tsx": ``,
		"other/src/Layout.tsx": ``,
	})

	got := frameFiles(root, frontend{Dir: "admin"})

	assert.Equal(t, []string{"admin/src/Layout.tsx"}, got)
}

func TestFrameFiles_CapAndOrder(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}

	for i := range 25 {
		files[fmt.Sprintf("src/layout%02d/Layout.tsx", i)] = ``
	}

	files["src/App.tsx"] = ``

	writeTree(t, root, files)

	got := frameFiles(root, frontend{Dir: ""})

	require.Len(t, got, 20)
	assert.Equal(t, "src/App.tsx", got[0])

	rest := got[1:]
	assert.IsIncreasing(t, rest)
	assert.Equal(t, "src/layout00/Layout.tsx", rest[0])
	assert.Equal(t, "src/layout18/Layout.tsx", rest[len(rest)-1])
}

func TestBrandFiles(t *testing.T) {
	root := t.TempDir()
	logo := `<svg xmlns="http://www.w3.org/2000/svg"/>`
	writeTree(t, root, map[string]string{
		"src/components/DeveloperLogo/logo.svg": logo,
		"src/icons/close.svg":                   `<svg/>`,
		"public/brand-mark.png":                 "png",
		"public/companyLogo.PNG":                "PNG",
		"public/logo.jpg":                       "jpg",
		"public/logo.svg.map":                   "map",
		"public/catalog.svg":                    `<svg/>`,
		"node_modules/pkg/logo.svg":             `<svg/>`,
	})

	got := brandFiles(root, frontend{Dir: ""})

	assert.Equal(t, []Asset{
		{Path: "public/brand-mark.png", Size: 3},
		{Path: "public/companyLogo.PNG", Size: 3},
		{Path: "src/components/DeveloperLogo/logo.svg", Size: int64(len(logo))},
	}, got)
}

func TestBrandFiles_CapAndRootDir(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"other/logo.svg": `<svg/>`}

	for i := range 12 {
		files[fmt.Sprintf("web/assets/logo-%02d.svg", i)] = `<svg/>`
	}

	writeTree(t, root, files)

	got := brandFiles(root, frontend{Dir: "web"})

	require.Len(t, got, 10)

	for _, a := range got {
		assert.Contains(t, a.Path, "web/")
	}
}

func writeNestedPackageTree(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app/package.json":                   `{}`,
		"app/src/Footer.tsx":                 ``,
		"app/packages/server/package.json":   `{}`,
		"app/packages/server/src/Header.tsx": ``,
		"app/packages/server/logo.svg":       `<svg/>`,
	})

	return root
}

func TestFrameFiles_SkipNestedPackages(t *testing.T) {
	root := writeNestedPackageTree(t)

	assert.Equal(t, []string{"app/src/Footer.tsx"}, frameFiles(root, frontend{Dir: "app"}))
	assert.Equal(t,
		[]string{"app/packages/server/src/Header.tsx"},
		frameFiles(root, frontend{Dir: "app/packages/server"}))
}

func TestBrandFiles_SkipNestedPackages(t *testing.T) {
	root := writeNestedPackageTree(t)

	assert.Empty(t, brandFiles(root, frontend{Dir: "app"}))
	assert.Equal(t,
		[]Asset{{Path: "app/packages/server/logo.svg", Size: int64(len(`<svg/>`))}},
		brandFiles(root, frontend{Dir: "app/packages/server"}))
}

package design

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
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
		"src/main.ts":   `import '@acme/ui/core.css'`,
	})

	got, err := scanTree(root)
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Empty(t, got[0].Dir)
	assert.Equal(t, []string{"src/style.css"}, got[0].CSS)
	assert.Equal(t, []entry{{
		File:    "src/main.ts",
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
			// The own walk resolves these; TestOwnTree_Local covers them.
			name: "a css file's imports are not entries",
			files: map[string]string{
				"app/src/index.css": `@import 'sub/x.css'; @import '@acme/ui/x.css';`,
				"app/src/sub/x.css": `a {}`,
			},
		},
		{
			name: "js side-effect import resolving to a local file is local",
			files: map[string]string{
				"app/src/main.ts":   `import 'sub/y.css'`,
				"app/src/sub/y.css": `a {}`,
			},
			want: []entry{{File: "app/src/main.ts", Spec: "sub/y.css", Local: "app/src/sub/y.css"}},
		},
		{
			name: "js side-effect import with no such file is a package",
			files: map[string]string{
				"app/src/main.ts": `import 'sub/y.css'`,
			},
			want: []entry{{File: "app/src/main.ts", Spec: "sub/y.css", Name: "sub", Subpath: "y.css"}},
		},
		{
			name: "js side-effect import resolves relative to the importing file",
			files: map[string]string{
				"app/src/main.ts":     `import 'sub/y.css'`,
				"app/other/sub/y.css": `a {}`,
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

func TestScanTree_EntriesInLoadOrder(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app/package.json": `{}`,
		"app/index.html": `<link rel="icon" href="/favicon.ico">` +
			`<link rel="stylesheet" href="https://cdn.example.test/x.css">` +
			`<link rel="stylesheet" href="/b.css"><link rel="stylesheet" href="./a.css?v=2#top">` +
			`<link rel="stylesheet" href="/gone.css"><link rel="stylesheet" href="/theme.css">` +
			`<link rel="stylesheet" href="{{ asset('x.css') }}"><link rel="stylesheet" href="${base}/y.css">`,
		"app/a.css":                     `a {}`,
		"app/b.css":                     `b {}`,
		"app/public/theme.css":          `t {}`,
		"app/src/routes/layout.tsx":     `import './layout.css'`,
		"app/public/index.html":         `<link rel="stylesheet" href="%PUBLIC_URL%/p.css">`,
		"app/public/p.css":              `p {}`,
		"app/top.ts":                    `import './top.css'`,
		"app/src/main.tsx":              `import './z.css'; import '@acme/ui/x.css'`,
		"app/src/index.ts":              `import 'sub/y.css'`,
		"app/src/sub/y.css":             `y {}`,
		"app/src/App.tsx":               `import './App.css'`,
		"app/src/pages/_app.tsx":        `import '@/styles/g.css'; import '~/missing.css'`,
		"app/src/styles/g.css":          `g {}`,
		"app/src/components/Button.tsx": `import '../../shared.css'`,
	})

	got, err := scanTree(root)
	require.NoError(t, err)

	require.Equal(t, []string{"app"}, frontendDirs(got))
	assert.Equal(t, []entry{
		{File: "app/index.html", Spec: "/b.css", Local: "app/b.css"},
		{File: "app/index.html", Spec: "./a.css?v=2#top", Local: "app/a.css"},
		{File: "app/index.html", Spec: "/gone.css", Local: "app/gone.css"},
		{File: "app/index.html", Spec: "/theme.css", Local: "app/public/theme.css"},
		{File: "app/public/index.html", Spec: "%PUBLIC_URL%/p.css", Local: "app/public/p.css"},
		{File: "app/top.ts", Spec: "./top.css", Local: "app/top.css"},
		{File: "app/src/index.ts", Spec: "sub/y.css", Local: "app/src/sub/y.css"},
		{File: "app/src/main.tsx", Spec: "./z.css", Local: "app/src/z.css"},
		{File: "app/src/main.tsx", Spec: "@acme/ui/x.css", Name: "@acme/ui", Subpath: "x.css"},
		{File: "app/src/App.tsx", Spec: "./App.css", Local: "app/src/App.css"},
		{File: "app/src/pages/_app.tsx", Spec: "@/styles/g.css", Local: "app/src/styles/g.css"},
		{File: "app/src/pages/_app.tsx", Spec: "~/missing.css"},
		{File: "app/src/routes/layout.tsx", Spec: "./layout.css", Local: "app/src/routes/layout.css"},
		{File: "app/src/components/Button.tsx", Spec: "../../shared.css", Local: "app/shared.css"},
	}, got[0].Entries)
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

func assetPaths(as []Asset) []string {
	var paths []string

	for _, a := range as {
		paths = append(paths, a.Path)
	}

	return paths
}

func TestFrameFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}

	for _, rel := range []string{
		"app/layout.tsx",
		"src/routes/+layout.svelte",
		"src/layouts/default.vue",
		"src/components/BaseLayout/index.tsx",
		"src/components/BaseLayout/index.module.css",
		"src/components/TopBar/TopBar.tsx",
		"src/components/NavigationBar/index.tsx",
		"src/components/NavigationBar/index.module.css",
		"src/components/NavigationBar/NavigationButton/index.tsx",
		"src/components/NavigationBar/RecentDropdown.tsx",
		"src/components/Footer/Footer.tsx",
		"src/components/Footer/Footer.module.css",
		"src/components/SideBar.tsx",
		"src/components/HeroPage/index.tsx",
		"src/App.tsx",
		"src/App.module.css",
		"src/api/app.ts",
		"src/constants/navHistory.ts",
		"src/components/hooks/navState.ts",
		"src/components/Header/useHeader.ts",
		"src/components/Header/Header.d.ts",
		"src/components/Header/Header.test.tsx",
		"src/components/Header/Header.stories.tsx",
		"src/components/__tests__/Footer.tsx",
		"src/pages/teams/components/TeamHeader.tsx",
		"src/utils/menu.ts",
		"src/theme/nav_item.svelte",
		"node_modules/pkg/Layout.tsx",
		"dist/Layout.js",
	} {
		files[rel] = "x"
	}

	writeTree(t, root, files)

	assert.Equal(t, []string{
		"src/App.tsx",
		"src/App.module.css",
		"app/layout.tsx",
		"src/routes/+layout.svelte",
		"src/layouts/default.vue",
		"src/components/BaseLayout/index.tsx",
		"src/components/BaseLayout/index.module.css",
		"src/theme/nav_item.svelte",
		"src/components/TopBar/TopBar.tsx",
		"src/components/NavigationBar/index.tsx",
		"src/components/NavigationBar/index.module.css",
		"src/components/Footer/Footer.tsx",
		"src/components/Footer/Footer.module.css",
		"src/components/SideBar.tsx",
		"src/components/HeroPage/index.tsx",
		"src/components/NavigationBar/NavigationButton/index.tsx",
	}, assetPaths(frameFiles(root, frontend{Dir: ""})))
}

func TestFrameFiles_Layouts(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  []string
	}{
		{
			name: "angular",
			files: []string{
				"src/app/header/header.component.ts",
				"src/app/header/header.component.html",
				"src/app/app.component.html",
				"src/app/app.component.ts",
				"src/main.ts",
			},
			want: []string{
				"src/app/app.component.html",
				"src/app/header/header.component.ts",
				"src/app/header/header.component.html",
			},
		},
		{
			name:  "create-vue",
			files: []string{"src/App.vue", "src/main.ts", "src/components/HelloWorld.vue"},
			want:  []string{"src/App.vue"},
		},
		{
			name:  "nuxt",
			files: []string{"app.vue", "nuxt.config.ts", "pages/index.vue"},
			want:  []string{"app.vue"},
		},
		{
			name:  "flat react",
			files: []string{"src/App.tsx", "src/Header.tsx", "src/Footer.tsx", "src/Nav.tsx", "src/main.tsx"},
			want:  []string{"src/App.tsx", "src/Nav.tsx", "src/Header.tsx", "src/Footer.tsx"},
		},
		{
			name:  "feature folder",
			files: []string{"src/shared/ui/Navbar/index.tsx"},
			want:  []string{"src/shared/ui/Navbar/index.tsx"},
		},
		{
			name:  "email templates",
			files: []string{"src/emails/components/Header.tsx", "src/mail/layouts/Base.tsx"},
		},
		{
			name: "files beside a layout",
			files: []string{
				"src/components/BaseLayout/index.tsx",
				"src/components/BaseLayout/types.ts",
				"src/components/BaseLayout/constants.ts",
				"src/components/BaseLayout/utils.ts",
			},
			want: []string{"src/components/BaseLayout/index.tsx"},
		},
		{
			name:  "a hook folder",
			files: []string{"src/components/useNav/index.ts"},
		},
		{
			name:  "a frame word in a script file needs a component or own folder",
			files: []string{"src/nav.ts", "src/shared/header.js"},
		},
		{
			name: "next app router colocates frame files",
			files: []string{
				"app/(marketing)/site-header.tsx",
				"app/dashboard/sidebar.tsx",
				"app/dashboard/page.tsx",
				"app/ui/dashboard/nav-links.tsx",
			},
			want: []string{
				"app/(marketing)/site-header.tsx",
				"app/dashboard/sidebar.tsx",
				"app/ui/dashboard/nav-links.tsx",
			},
		},
		{
			name: "shared ui folders",
			files: []string{
				"src/shared/Header.tsx",
				"src/common/Header.tsx",
				"src/containers/Header.tsx",
				"src/widgets/Header.tsx",
				"src/ui/Header.tsx",
			},
			want: []string{
				"src/ui/Header.tsx",
				"src/common/Header.tsx",
				"src/shared/Header.tsx",
				"src/widgets/Header.tsx",
				"src/containers/Header.tsx",
			},
		},
		{
			name:  "feature-sliced widget",
			files: []string{"widgets/header/ui/Header.tsx"},
			want:  []string{"widgets/header/ui/Header.tsx"},
		},
		{
			name:  "sveltekit lib and routes",
			files: []string{"src/lib/Footer.svelte", "src/routes/Header.svelte", "src/routes/+page.svelte"},
			want:  []string{"src/routes/Header.svelte", "src/lib/Footer.svelte"},
		},
		{
			name:  "django partials",
			files: []string{"templates/partials/footer.html"},
			want:  []string{"templates/partials/footer.html"},
		},
		{
			name: "next pages router",
			files: []string{
				"pages/_app.tsx",
				"pages/_document.tsx",
				"pages/index.tsx",
				"pages/api/hello.ts",
			},
			want: []string{"pages/_app.tsx"},
		},
		{
			name:  "next pages router under src",
			files: []string{"src/pages/_app.jsx"},
			want:  []string{"src/pages/_app.jsx"},
		},
		{
			name:  "next pages router app in plain script",
			files: []string{"src/pages/_app.js"},
			want:  []string{"src/pages/_app.js"},
		},
		{
			name:  "remix root",
			files: []string{"app/root.tsx", "app/entry.client.tsx", "app/routes/_index.tsx"},
			want:  []string{"app/root.tsx"},
		},
		{
			name: "astro",
			files: []string{
				"src/layouts/Layout.astro",
				"src/components/Header.astro",
				"src/components/Footer.astro",
				"src/pages/index.astro",
				"src/ui/Header.astro",
			},
			want: []string{
				"src/layouts/Layout.astro",
				"src/ui/Header.astro",
				"src/components/Header.astro",
				"src/components/Footer.astro",
			},
		},
		{
			name:  "create react app",
			files: []string{"src/App.js", "src/App.css", "src/index.js"},
			want:  []string{"src/App.js", "src/App.css"},
		},
		{
			name:  "a lower-case app.js is a server",
			files: []string{"app.js", "src/app.js"},
		},
		{
			name: "a folder word that is not the last name word",
			files: []string{
				"src/components/StoreHeader.tsx",
				"src/components/MailLayout.tsx",
				"src/components/ServicesNav.tsx",
				"src/components/EmailFooter.tsx",
			},
			want: []string{
				"src/components/MailLayout.tsx",
				"src/components/ServicesNav.tsx",
				"src/components/StoreHeader.tsx",
				"src/components/EmailFooter.tsx",
			},
		},
		{
			name: "a folder word as the last name word",
			files: []string{
				"src/components/Header/headerTypes.ts",
				"src/components/navUtils.ts",
				"src/components/sidebarStore.ts",
				"src/layouts/layoutUtils.ts",
			},
		},
		{
			name:  "cypress tests",
			files: []string{"src/components/Header.cy.tsx"},
		},
		{
			name:  "side and top navigation",
			files: []string{"src/app/sidenav/sidenav.component.html", "src/components/topnav.tsx"},
			want:  []string{"src/app/sidenav/sidenav.component.html", "src/components/topnav.tsx"},
		},
		{
			name: "a name ending in logo",
			files: []string{
				"src/components/Logo.tsx",
				"src/components/SiteLogo.tsx",
				"src/components/acme-logo.tsx",
				"src/components/LogoCloud.tsx",
				"components/icons/logo.tsx",
				"src/components/icons/Logo.tsx",
			},
			want: []string{
				"src/components/Logo.tsx",
				"components/icons/logo.tsx",
				"src/components/SiteLogo.tsx",
				"src/components/acme-logo.tsx",
				"src/components/icons/Logo.tsx",
			},
		},
		{
			name: "react router layout under pages",
			files: []string{
				"src/pages/Layout.jsx",
				"src/pages/Layout.js",
				"src/pages/RootLayout.jsx",
				"src/pages/Layout/index.jsx",
				"src/pages/Home.jsx",
				"pages/api/hello.js",
				"pages/api/layout.js",
			},
			want: []string{
				"src/pages/Layout.js",
				"src/pages/Layout.jsx",
				"src/pages/Layout/index.jsx",
				"src/pages/RootLayout.jsx",
			},
		},
		{
			name: "demo, icon, context and provider files",
			files: []string{
				"stories/Header.tsx",
				"stories/header.css",
				"src/components/MenuIcon.tsx",
				"src/components/icons/Menu.tsx",
				"src/components/SidebarContext.tsx",
				"src/contexts/Nav.tsx",
				"src/providers/Header.tsx",
				"src/components/NavProvider.tsx",
			},
		},
		{
			name:  "the app shell sits at most one level down",
			files: []string{"src/lib/deep/app.tsx", "src/lib/deep/App.js", "src/lib/deep/root.tsx"},
		},
		{
			name:  "a unit's stylesheet follows its other files",
			files: []string{"src/components/Header/Header.css", "src/components/Header/Header.tsx"},
			want:  []string{"src/components/Header/Header.tsx", "src/components/Header/Header.css"},
		},
		{
			// Path order alone puts the Nav unit first; footer alone ranks after it.
			name:  "a file of a layout folder ranks as a layout",
			files: []string{"src/components/Nav.tsx", "src/layouts/Footer.vue"},
			want:  []string{"src/layouts/Footer.vue", "src/components/Nav.tsx"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{}

			for _, rel := range tt.files {
				files[rel] = "x"
			}

			writeTree(t, root, files)

			assert.Equal(t, tt.want, assetPaths(frameFiles(root, frontend{Dir: ""})))
		})
	}
}

func TestFrameFiles_RulesReadFromTheRoot(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pages/site/package.json":              `{}`,
		"pages/site/src/components/Footer.tsx": ``,
		"pages/site/src/api/Header.tsx":        ``,
	})

	got := frameFiles(root, frontend{Dir: "pages/site"})

	assert.Equal(t, []string{"pages/site/src/components/Footer.tsx"}, assetPaths(got))
}

func TestFrameFiles_Size(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"src/components/Footer.tsx": "0123456789"})

	assert.Equal(t,
		[]Asset{{Path: "src/components/Footer.tsx", Size: 10}},
		frameFiles(root, frontend{Dir: ""}))
}

func TestFrameFiles_UnderRootDir(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"admin/src/Layout.tsx": ``,
		"other/src/Layout.tsx": ``,
	})

	got := frameFiles(root, frontend{Dir: "admin"})

	assert.Equal(t, []string{"admin/src/Layout.tsx"}, assetPaths(got))
}

func TestFrameFiles_CapAndOrder(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}

	// The hyphen keeps "Nav" a word of its own; nameWords does not split at digits.
	for i := range 25 {
		files[fmt.Sprintf("src/components/Nav-%02d/index.tsx", i)] = ``
	}

	files["app/layout.tsx"] = ``

	writeTree(t, root, files)

	got := assetPaths(frameFiles(root, frontend{Dir: ""}))

	require.Len(t, got, 20)
	assert.Equal(t, "app/layout.tsx", got[0])
	assert.Equal(t, "src/components/Nav-00/index.tsx", got[1])
	assert.Equal(t, "src/components/Nav-18/index.tsx", got[19])
}

// importing is the text of a component that imports each of specs.
func importing(specs ...string) string {
	var b strings.Builder

	for i, spec := range specs {
		fmt.Fprintf(&b, "import M%d from '%s'\n", i, spec)
	}

	return b.String() + "export default function Component() { return null }\n"
}

func TestFrameFiles_ChildrenFollowTheirParent(t *testing.T) {
	root := t.TempDir()
	parent := importing("./Menu", "./Brand/Brand", "./index.module.css", "../Footer")
	writeTree(t, root, map[string]string{
		"src/components/NavBar/index.tsx":        parent,
		"src/components/NavBar/index.module.css": `.bar { color: red }`,
		"src/components/NavBar/Menu.tsx":         "export default () => null\n",
		"src/components/NavBar/Brand/Brand.tsx":  "export default () => null\n",
		"src/components/Footer/index.tsx":        `x`,
		"src/components/HeroBanner/index.tsx":    `x`,
	})

	got := frameFiles(root, frontend{Dir: ""})

	assert.Equal(t, []string{
		"src/components/NavBar/index.tsx",
		"src/components/NavBar/index.module.css",
		"src/components/NavBar/Menu.tsx",
		"src/components/NavBar/Brand/Brand.tsx",
		"src/components/Footer/index.tsx",
		"src/components/HeroBanner/index.tsx",
	}, assetPaths(got))
	assert.Equal(t, int64(len("export default () => null\n")), got[2].Size)
}

func TestFrameFiles_ChildResolution(t *testing.T) {
	tests := []struct {
		name  string
		spec  string
		files []string
		want  string
	}{
		{
			name:  "a file before a folder",
			spec:  "./Items",
			files: []string{"Items.tsx", "Items/index.tsx"},
			want:  "Items.tsx",
		},
		{name: "a folder index", spec: "./Items", files: []string{"Items/index.tsx"}, want: "Items/index.tsx"},
		{
			name:  "a folder index in extension order",
			spec:  "./Items",
			files: []string{"Items/index.js", "Items/index.ts"},
			want:  "Items/index.ts",
		},
		{name: "tsx before ts", spec: "./Items", files: []string{"Items.ts", "Items.tsx"}, want: "Items.tsx"},
		{name: "ts before jsx", spec: "./Items", files: []string{"Items.jsx", "Items.ts"}, want: "Items.ts"},
		{name: "jsx before js", spec: "./Items", files: []string{"Items.js", "Items.jsx"}, want: "Items.jsx"},
		{name: "js before vue", spec: "./Items", files: []string{"Items.vue", "Items.js"}, want: "Items.js"},
		{name: "vue before svelte", spec: "./Items", files: []string{"Items.svelte", "Items.vue"}, want: "Items.vue"},
		{name: "svelte", spec: "./Items", files: []string{"Items.svelte"}, want: "Items.svelte"},
		{name: "a spec with its extension", spec: "./Items.vue", files: []string{"Items.vue"}, want: "Items.vue"},
		{
			name:  "a spec with its extension is not extended",
			spec:  "./Items.js",
			files: []string{"Items.js", "Items.js.tsx"},
			want:  "Items.js",
		},
		{
			name:  "a spec with a dotted name",
			spec:  "./Items.component",
			files: []string{"Items.component.ts"},
			want:  "Items.component.ts",
		},
		{name: "a nested path", spec: "./Items/List", files: []string{"Items/List.tsx"}, want: "Items/List.tsx"},
		{name: "a trailing slash", spec: "./Items/", files: []string{"Items/index.tsx"}, want: "Items/index.tsx"},
		{name: "nothing found", spec: "./Items", files: []string{"Other.tsx"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{"src/components/NavBar/index.tsx": importing(tt.spec)}

			for _, f := range tt.files {
				files["src/components/NavBar/"+f] = "x"
			}

			writeTree(t, root, files)

			want := []string{"src/components/NavBar/index.tsx"}
			if tt.want != "" {
				want = append(want, "src/components/NavBar/"+tt.want)
			}

			assert.Equal(t, want, assetPaths(frameFiles(root, frontend{Dir: ""})))
		})
	}
}

func TestFrameFiles_ChildThatIsAFrameFile(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/components/NavBar/index.tsx":                importing("./NavMenu"),
		"src/components/NavBar/NavMenu/index.tsx":        `x`,
		"src/components/NavBar/NavMenu/index.module.css": `x`,
		"src/components/Footer/index.tsx":                `x`,
	})

	assert.Equal(t, []string{
		"src/components/NavBar/index.tsx",
		"src/components/NavBar/NavMenu/index.tsx",
		"src/components/Footer/index.tsx",
		"src/components/NavBar/NavMenu/index.module.css",
	}, assetPaths(frameFiles(root, frontend{Dir: ""})))
}

func TestFrameFiles_ChildrenNotFollowed(t *testing.T) {
	tests := []struct {
		name  string
		spec  string
		files []string
	}{
		{name: "a hook", spec: "./useNav", files: []string{"useNav.ts"}},
		{name: "a hook folder", spec: "./useNav", files: []string{"useNav/index.ts"}},
		{name: "types", spec: "./types", files: []string{"types.ts"}},
		{name: "constants", spec: "./constants", files: []string{"constants.ts"}},
		{name: "a last name word that is a folder word", spec: "./navApi", files: []string{"navApi.ts"}},
		{name: "a declaration", spec: "./Items.d", files: []string{"Items.d.ts"}},
		{name: "a test", spec: "./Items.test", files: []string{"Items.test.tsx"}},
		{name: "a spec file", spec: "./Items.spec", files: []string{"Items.spec.tsx"}},
		{name: "a story", spec: "./Items.stories", files: []string{"Items.stories.tsx"}},
		{name: "a hooks folder", spec: "./hooks/items", files: []string{"hooks/items.ts"}},
		{name: "a utils folder", spec: "./utils/items", files: []string{"utils/items.ts"}},
		{name: "an icons folder", spec: "./icons/Arrow", files: []string{"icons/Arrow.tsx"}},
		{name: "a pages folder", spec: "./pages/Items", files: []string{"pages/Items.tsx"}},
		{name: "a stylesheet", spec: "./Items.css", files: []string{"Items.css"}},
		{name: "a data file", spec: "./items.json", files: []string{"items.json"}},
		{name: "the parent directory", spec: "../Shared", files: []string{"../Shared.tsx"}},
		{name: "an alias", spec: "@/components/Shared", files: []string{"../Shared.tsx"}},
		{name: "a package", spec: "shared", files: []string{"shared.tsx"}},
		{name: "a query", spec: "./Items?raw", files: []string{"Items.tsx"}},
		{name: "a dotted segment", spec: "./Items/../Shared", files: []string{"Shared.tsx"}},
		{
			name:  "a nested package",
			spec:  "./widget",
			files: []string{"widget/package.json", "widget/index.tsx"},
		},
		{name: "a dependency folder", spec: "./node_modules/pkg/index", files: []string{"node_modules/pkg/index.tsx"}},
		{name: "a build folder", spec: "./dist/Items", files: []string{"dist/Items.js"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{
				"src/components/NavBar/index.tsx":   importing(tt.spec, "./Control"),
				"src/components/NavBar/Control.tsx": `x`,
			}

			for _, f := range tt.files {
				files[path.Clean("src/components/NavBar/"+f)] = "x"
			}

			writeTree(t, root, files)

			assert.Equal(t, []string{
				"src/components/NavBar/index.tsx",
				"src/components/NavBar/Control.tsx",
			}, assetPaths(frameFiles(root, frontend{Dir: ""})))
		})
	}
}

func TestFrameFiles_ChildrenFollowOnlyOneLevelAndOnlyAFolder(t *testing.T) {
	t.Run("a child's own imports", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{
			"src/components/NavBar/index.tsx": importing("./Links"),
			"src/components/NavBar/Links.tsx": importing("./Item"),
			"src/components/NavBar/Item.tsx":  `x`,
		})

		assert.Equal(t, []string{
			"src/components/NavBar/index.tsx",
			"src/components/NavBar/Links.tsx",
		}, assetPaths(frameFiles(root, frontend{Dir: ""})))
	})

	t.Run("a loose file", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{
			"src/App.tsx":                importing("./Routes", "./Providers/Theme"),
			"src/Routes.tsx":             `x`,
			"src/Providers/Theme.tsx":    `x`,
			"src/components/Footer.tsx":  importing("./Columns"),
			"src/components/Columns.tsx": `x`,
		})

		assert.Equal(t, []string{
			"src/App.tsx",
			"src/components/Footer.tsx",
		}, assetPaths(frameFiles(root, frontend{Dir: ""})))
	})

	t.Run("a stylesheet", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{
			"src/components/NavBar/index.module.css": "@import './Links';\n",
			"src/components/NavBar/Links.tsx":        `x`,
		})

		assert.Equal(t, []string{"src/components/NavBar/index.module.css"},
			assetPaths(frameFiles(root, frontend{Dir: ""})))
	})
}

func TestFrameFiles_ChildThatIsAFrameFileIsNotAParent(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/components/NavBar/index.tsx":                importing("./NavMenu"),
		"src/components/NavBar/NavMenu/index.tsx":        importing("./Items"),
		"src/components/NavBar/NavMenu/Items.tsx":        `x`,
		"src/components/NavBar/NavMenu/index.module.css": `x`,
		"src/components/Footer/index.tsx":                `x`,
	})

	assert.Equal(t, []string{
		"src/components/NavBar/index.tsx",
		"src/components/NavBar/NavMenu/index.tsx",
		"src/components/Footer/index.tsx",
		"src/components/NavBar/NavMenu/index.module.css",
	}, assetPaths(frameFiles(root, frontend{Dir: ""})))
}

func TestFrameFiles_ReExportingIndexLeavesTheComponentAParent(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/components/NavBar/index.tsx":  "export { default } from './NavBar'\n",
		"src/components/NavBar/NavBar.tsx": importing("./Links"),
		"src/components/NavBar/Links.tsx":  `x`,
	})

	assert.Equal(t, []string{
		"src/components/NavBar/index.tsx",
		"src/components/NavBar/NavBar.tsx",
		"src/components/NavBar/Links.tsx",
	}, assetPaths(frameFiles(root, frontend{Dir: ""})))
}

func TestFrameFiles_ChildLogoInAnIconFolder(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/components/NavBar/index.tsx":       importing("./icons/Arrow", "./icons/Logo"),
		"src/components/NavBar/icons/Arrow.tsx": `x`,
		"src/components/NavBar/icons/Logo.tsx":  `x`,
		"src/components/Footer/index.tsx":       `x`,
	})

	assert.Equal(t, []string{
		"src/components/NavBar/index.tsx",
		"src/components/NavBar/icons/Logo.tsx",
		"src/components/Footer/index.tsx",
	}, assetPaths(frameFiles(root, frontend{Dir: ""})))
}

func TestFrameFiles_ChildrenReadRulesFromTheRoot(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"pages/site/package.json":                    `{}`,
		"pages/site/src/components/NavBar/index.tsx": importing("./Links"),
		"pages/site/src/components/NavBar/Links.tsx": `x`,
		"pages/site/admin/package.json":              `{}`,
		"pages/site/admin/Layout/index.tsx":          importing("./Links"),
		"pages/site/admin/Layout/Links.tsx":          `x`,
	})

	assert.Equal(t, []string{
		"pages/site/src/components/NavBar/index.tsx",
		"pages/site/src/components/NavBar/Links.tsx",
	}, assetPaths(frameFiles(root, frontend{Dir: "pages/site"})))
}

func TestFrameFiles_SharedChildFollowsTheFirstParent(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/components/NavBar/index.tsx":      importing("./Menu/Links"),
		"src/components/NavBar/Menu/index.tsx": importing("./Links"),
		"src/components/NavBar/Menu/Links.tsx": `x`,
		"src/components/Footer/index.tsx":      `x`,
	})

	assert.Equal(t, []string{
		"src/components/NavBar/index.tsx",
		"src/components/NavBar/Menu/Links.tsx",
		"src/components/Footer/index.tsx",
		"src/components/NavBar/Menu/index.tsx",
	}, assetPaths(frameFiles(root, frontend{Dir: ""})))
}

func TestFrameFiles_ChildrenKeepImportOrder(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/components/NavBar/index.tsx": importing("./Zeta", "./Alpha", "./Mid"),
		"src/components/NavBar/Alpha.tsx": `x`,
		"src/components/NavBar/Mid.tsx":   `x`,
		"src/components/NavBar/Zeta.tsx":  `x`,
	})

	assert.Equal(t, []string{
		"src/components/NavBar/index.tsx",
		"src/components/NavBar/Zeta.tsx",
		"src/components/NavBar/Alpha.tsx",
		"src/components/NavBar/Mid.tsx",
	}, assetPaths(frameFiles(root, frontend{Dir: ""})))
}

func TestFrameFiles_ChildrenAreCapped(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	specs := make([]string, 0, 25)

	for i := range 25 {
		specs = append(specs, fmt.Sprintf("./Item%02d", i))
		files[fmt.Sprintf("src/components/NavBar/Item%02d.tsx", i)] = `x`
	}

	files["src/components/NavBar/index.tsx"] = importing(specs...)
	writeTree(t, root, files)

	got := assetPaths(frameFiles(root, frontend{Dir: ""}))

	require.Len(t, got, 20)
	assert.Equal(t, "src/components/NavBar/index.tsx", got[0])
	assert.Equal(t, "src/components/NavBar/Item00.tsx", got[1])
	assert.Equal(t, "src/components/NavBar/Item18.tsx", got[19])
}

func TestFrameFiles_ParentWithoutText(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "binary", content: importing("./Links") + "\x00"},
		{name: "not UTF-8", content: importing("./Links") + "\xff\xfe"},
		{name: "over the frame source limit", content: importing("./Links") + "// " + strings.Repeat("x", maxFrameSourceBytes)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{
				"src/components/NavBar/index.tsx": tt.content,
				"src/components/NavBar/Links.tsx": `x`,
			})

			assert.Equal(t, []string{"src/components/NavBar/index.tsx"},
				assetPaths(frameFiles(root, frontend{Dir: ""})))
		})
	}
}

func TestFrameFiles_SymlinkedChildNotFollowed(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"src/components/NavBar/index.tsx": importing("./Links"),
		"src/Shared.tsx":                  `x`,
	})
	require.NoError(t, os.Symlink(
		filepath.Join(root, "src", "Shared.tsx"),
		filepath.Join(root, "src", "components", "NavBar", "Links.tsx")))

	assert.Equal(t, []string{"src/components/NavBar/index.tsx"},
		assetPaths(frameFiles(root, frontend{Dir: ""})))
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
		{Path: "src/components/DeveloperLogo/logo.svg", Size: int64(len(logo))},
		{Path: "public/brand-mark.png", Size: 3},
		{Path: "public/companyLogo.PNG", Size: 3},
	}, got)
}

func TestBrandFiles_NamedJustLogoFirst(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"public/google-logo.svg":     `<svg/>`,
		"src/assets/images/logo.svg": `<svg/>`,
		"public/brand.svg":           `<svg/>`,
	})

	assert.Equal(t,
		[]string{"public/brand.svg", "src/assets/images/logo.svg", "public/google-logo.svg"},
		assetPaths(brandFiles(root, frontend{Dir: ""})))
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
		"app/package.json":                              `{}`,
		"app/src/components/Footer.tsx":                 ``,
		"app/packages/server/package.json":              `{}`,
		"app/packages/server/src/components/Header.tsx": ``,
		"app/packages/server/logo.svg":                  `<svg/>`,
	})

	return root
}

func TestFrameFiles_SkipNestedPackages(t *testing.T) {
	root := writeNestedPackageTree(t)

	assert.Equal(t,
		[]string{"app/src/components/Footer.tsx"},
		assetPaths(frameFiles(root, frontend{Dir: "app"})))
	assert.Equal(t,
		[]string{"app/packages/server/src/components/Header.tsx"},
		assetPaths(frameFiles(root, frontend{Dir: "app/packages/server"})))
}

func TestBrandFiles_SkipNestedPackages(t *testing.T) {
	root := writeNestedPackageTree(t)

	assert.Empty(t, brandFiles(root, frontend{Dir: "app"}))
	assert.Equal(t,
		[]Asset{{Path: "app/packages/server/logo.svg", Size: int64(len(`<svg/>`))}},
		brandFiles(root, frontend{Dir: "app/packages/server"}))
}

func TestScanTree_Styles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a/package.json":   `{"dependencies":{"@emotion/react":"1","react":"1"}}`,
		"a/src/main.ts":    "export {}",
		"a/src/s.scss":     "a{}",
		"a/src/t.sass":     "a",
		"a/src/V.vue":      "<style lang='stylus'></style>",
		"a/src/W.vue":      "<style scoped></style>",
		"b/package.json":   `{}`,
		"b/lib/x.less":     "a{}",
		"c/package.json":   `{}`,
		"c/src/App.svelte": "<style lang=\"scss\"></style>",
		"d/package.json":   `{}`,
		"d/src/App.svelte": "<div/>",
		"f/package.json":   `{}`,
		"f/src/App.svelte": "<style></style>",
		"f/src/Zed.svelte": "<div/>",
		"e/package.json":   `{}`,
		"e/src/only.styl":  "a",
	})

	got, err := scanTree(root)
	require.NoError(t, err)

	styles := map[string][]string{}
	for _, f := range got {
		styles[f.Dir] = f.Styles
	}

	assert.Equal(t, map[string][]string{
		"a": {"CSS-in-JS (@emotion/react)", "SCSS", "Stylus", "Vue <style> blocks"},
		"c": {"SCSS"},
		"f": {"Svelte <style> blocks"},
	}, styles)
}

package design

import (
	"context"
	"encoding/json"
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

	ui := map[string]string{
		"package/package.json": `{"name":"@acme/ui","version":"1.2.3",` +
			`"dependencies":{"@acme/tokens":"^2.0.0"},` +
			`"exports":{"./style/*":"./style/*","./theme.css":"./dist/theme.css"}}`,
		"package/style/theme.css": ":root{--color-border:var(--acme-grey-8)}",
	}
	for name, css := range style {
		ui["package/style/"+name] = css
	}

	return map[string][]byte{
		uiPath: tgz(t, ui),
		tokensPath: tgz(t, map[string]string{
			"package/package.json": `{"name":"@acme/tokens","version":"2.0.1"}`,
			"package/vars.css":     varsCSS,
		}),
	}
}

func defaultTarballs(t *testing.T) map[string][]byte {
	t.Helper()

	return acmeTarballs(t,
		"@import './theme.css';\n@import '@acme/tokens/vars.css';\n.btn{border:0}",
		":root{--acme-grey-8:#ebebeb}")
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

	writeTree(t, filepath.Join(root, "portal"), files)

	return NewGatherer(t.TempDir(), srv.Client().Transport).Gather(t.Context(), portalRequest(root))
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
	assert.Contains(t, r.Frame, "portal/frontend/src/components/Footer/index.tsx")
	require.Len(t, r.Brand, 1)
	assert.Equal(t, "portal/frontend/src/logo.svg", r.Brand[0].Path)
	assert.Equal(t, []string{tokensDir, uiDir}, r.Packages)
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
			name: "workspace package",
			edit: func(t *testing.T, files map[string]string, _ string) {
				files["package-lock.json"] = npmLockJSON(t, map[string]map[string]any{
					"node_modules/@acme/ui": {"resolved": "packages/ui", "link": true},
				})
			},
			word: "tree",
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

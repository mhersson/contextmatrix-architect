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
	"sync/atomic"
	"testing"

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

// portalTree is a tree whose frontend imports @acme/ui/style/core.css, with
// @acme/ui and @acme/tokens locked on the registry at srvURL. Only @acme/ui
// carries an integrity.
func portalTree(t *testing.T, srvURL string, tarballs map[string][]byte) map[string]string {
	t.Helper()

	return map[string]string{
		".npmrc":                 "registry=" + srvURL + "/\n",
		"frontend/package.json":  `{"name":"frontend","dependencies":{"@acme/ui":"^1.2.0"}}`,
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
				"frontend/src/a.css": "@import './b.css';\n:root{--color-border:#666666}",
				"frontend/src/b.css": "@import './a.css';",
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
		"frontend/src/index.css": ":root{--brand:#111111}\n.a{color:var(--brand)}\n",
		"admin/package.json":     `{"name":"admin"}`,
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

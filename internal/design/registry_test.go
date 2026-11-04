package design

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadRegistries(t *testing.T) {
	const npmjs = "https://registry.npmjs.org"

	tests := []struct {
		name  string
		files map[string]string
		root  string // tree-relative
		want  map[string]string
	}{
		{
			name: "no config uses the public registry",
			root: "frontend",
			want: map[string]string{"@acme/ui": npmjs, "left-pad": npmjs},
		},
		{
			name: "yarnrc.yml scope registry beats the default",
			files: map[string]string{".yarnrc.yml": "npmRegistryServer: 'https://npm.example.test/group'\n" +
				"npmScopes:\n  acme:\n    npmRegistryServer: 'https://npm.example.test/private'\n"},
			root: "frontend",
			want: map[string]string{
				"@acme/ui": "https://npm.example.test/private",
				"left-pad": "https://npm.example.test/group",
			},
		},
		{
			name: "npmrc registry and scope with an auth line",
			files: map[string]string{".npmrc": "; registry=https://comment.example.test/\n" +
				"# @acme:registry=https://comment.example.test/\n" +
				"registry=https://npm.example.test/\r\n" +
				"//npm.example.test/:_authToken=secret\n" +
				"@acme:registry = https://acme.example.test/\n"},
			root: "",
			want: map[string]string{
				"@acme/ui": "https://acme.example.test/",
				"left-pad": "https://npm.example.test/",
			},
		},
		{
			name:  "classic yarnrc registry",
			files: map[string]string{".yarnrc": "# comment\nregistry \"https://classic.example.test/\"\n"},
			root:  "frontend",
			want:  map[string]string{"@acme/ui": "https://classic.example.test/"},
		},
		{
			name: "the nearest file wins",
			files: map[string]string{
				".npmrc":          "registry=https://root.example.test/\n",
				"frontend/.npmrc": "registry=https://frontend.example.test/\n",
			},
			root: "frontend",
			want: map[string]string{"left-pad": "https://frontend.example.test/"},
		},
		{
			name: "a parent supplies the keys the nearer file lacks",
			files: map[string]string{
				".npmrc":          "registry=https://root.example.test/\n@acme:registry=https://acme.example.test/\n",
				"frontend/.npmrc": "registry=https://frontend.example.test/\n",
			},
			root: "frontend",
			want: map[string]string{
				"@acme/ui": "https://acme.example.test/",
				"left-pad": "https://frontend.example.test/",
			},
		},
		{
			name: "yarnrc.yml beats npmrc, npmrc beats classic yarnrc",
			files: map[string]string{
				".yarnrc.yml": "npmRegistryServer: 'https://yml.example.test/'\n",
				".npmrc":      "registry=https://npmrc.example.test/\n@acme:registry=https://acme.example.test/\n",
				".yarnrc":     "registry \"https://classic.example.test/\"\n",
			},
			root: "",
			want: map[string]string{
				"@acme/ui": "https://acme.example.test/",
				"left-pad": "https://yml.example.test/",
			},
		},
		{
			name: "a file outside the tree is not read",
			files: map[string]string{
				"../.npmrc": "registry=https://outside.example.test/\n",
			},
			root: "frontend",
			want: map[string]string{"left-pad": npmjs},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			treeDir := filepath.Join(base, "tree")
			require.NoError(t, os.MkdirAll(filepath.Join(treeDir, "frontend"), 0o750))

			for rel, content := range tt.files {
				path := filepath.Join(treeDir, filepath.FromSlash(rel))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}

			regs, err := readRegistries(treeDir, filepath.Join(treeDir, filepath.FromSlash(tt.root)))
			require.NoError(t, err)

			for pkg, want := range tt.want {
				got, err := regs.forPackage(pkg)
				require.NoError(t, err, pkg)
				assert.Equal(t, want, got, pkg)
			}

			assert.NotContains(t, fmt.Sprintf("%+v", regs), "secret")
		})
	}
}

func TestReadRegistries_EnvInterpolation(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		pkg   string
	}{
		{
			name:  "yarnrc.yml default",
			files: map[string]string{".yarnrc.yml": "npmRegistryServer: '${REGISTRY}'\n"},
			pkg:   "left-pad",
		},
		{
			name:  "npmrc default",
			files: map[string]string{".npmrc": "registry=https://${HOST}/npm/\n"},
			pkg:   "left-pad",
		},
		{
			name:  "npmrc scope",
			files: map[string]string{".npmrc": "@acme:registry=${ACME_REGISTRY}\n"},
			pkg:   "@acme/ui",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, tt.files)

			regs, err := readRegistries(root, root)
			require.NoError(t, err)

			_, err = regs.forPackage(tt.pkg)
			assert.ErrorIs(t, err, errEnvRegistry)
		})
	}

	t.Run("a scope that is not interpolated still resolves", func(t *testing.T) {
		root := t.TempDir()
		writeTree(t, root, map[string]string{
			".npmrc": "registry=${REGISTRY}\n@acme:registry=https://acme.example.test/\n",
		})

		regs, err := readRegistries(root, root)
		require.NoError(t, err)

		got, err := regs.forPackage("@acme/ui")
		require.NoError(t, err)
		assert.Equal(t, "https://acme.example.test/", got)
	})
}

func TestReadRegistries_UnparseableYarnrc(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"syntax error", "npmAuthToken: s3cr3t-token\nnpmScopes: [unclosed\n"},
		{"wrong shape", "npmAuthToken: s3cr3t-token\nnpmScopes: s3cr3t-token\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{".yarnrc.yml": tt.content})

			_, err := readRegistries(root, root)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "design:")
			assert.NotContains(t, err.Error(), "s3cr3t-token")
		})
	}
}

func TestReadRegistries_IgnoresSymlinkedConfig(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"npmrc": "registry=https://outside.example.test/\n"})
	require.NoError(t, os.Symlink(filepath.Join(outside, "npmrc"), filepath.Join(root, ".npmrc")))

	regs, err := readRegistries(root, root)
	require.NoError(t, err)

	got, err := regs.forPackage("left-pad")
	require.NoError(t, err)
	assert.Equal(t, "https://registry.npmjs.org", got)
}

func TestReadRegistries_RootOutsideTree(t *testing.T) {
	_, err := readRegistries(t.TempDir(), t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "design:")
}

func TestTarballURL(t *testing.T) {
	tests := []struct {
		name, registry, pkg, version, want string
	}{
		{"scoped with trailing slash", "https://npm.example.test/group/", "@acme/ui", "1.2.3", "https://npm.example.test/group/@acme/ui/-/ui-1.2.3.tgz"},
		{"unscoped", "https://npm.example.test", "left-pad", "1.0.2", "https://npm.example.test/left-pad/-/left-pad-1.0.2.tgz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tarballURL(tt.registry, tt.pkg, tt.version))
		})
	}
}

func TestPickURL(t *testing.T) {
	const registry = "https://npm.example.test/group/"

	tests := []struct {
		name    string
		l       locked
		checked bool
		want    string // "" when no url may be used
	}{
		{
			name: "resolved on the registry host drops its fragment",
			l:    locked{Name: "@acme/ui", Version: "1.2.3", Resolved: "https://npm.example.test/other/@acme/ui/-/ui-1.2.3.tgz#abc123"},
			want: "https://npm.example.test/other/@acme/ui/-/ui-1.2.3.tgz",
		},
		{
			name: "http resolved on the registry host falls back to the registry",
			l:    locked{Name: "@acme/ui", Version: "1.2.3", Resolved: "http://npm.example.test/other/@acme/ui/-/ui-1.2.3.tgz"},
			want: "https://npm.example.test/group/@acme/ui/-/ui-1.2.3.tgz",
		},
		{
			name: "registry-shaped resolved on another host falls back to the registry",
			l:    locked{Name: "@acme/ui", Version: "1.2.3", Resolved: "https://elsewhere.example.test/npm/@acme/ui/-/ui-1.2.3.tgz"},
			want: "https://npm.example.test/group/@acme/ui/-/ui-1.2.3.tgz",
		},
		{
			name:    "checked resolved on another host falls back to the registry",
			l:       locked{Name: "@acme/ui", Version: "1.2.3", Resolved: "https://codeload.example.test/acme/ui/tar.gz/v1"},
			checked: true,
			want:    "https://npm.example.test/group/@acme/ui/-/ui-1.2.3.tgz",
		},
		{
			name: "unchecked resolved on another host is not used",
			l:    locked{Name: "@acme/ui", Version: "1.2.3", Resolved: "https://codeload.example.test/acme/ui/tar.gz/v1"},
		},
		{
			name: "resolved of another version on another host is not used",
			l:    locked{Name: "@acme/ui", Version: "1.2.3", Resolved: "https://elsewhere.example.test/@acme/ui/-/ui-1.2.4.tgz"},
		},
		{
			name: "no resolved falls back to the registry",
			l:    locked{Name: "left-pad", Version: "1.0.2"},
			want: "https://npm.example.test/group/left-pad/-/left-pad-1.0.2.tgz",
		},
		{
			name: "unchecked resolved that is not a url is not used",
			l:    locked{Name: "left-pad", Version: "1.0.2", Resolved: "packages/left-pad"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pickURL(registry, tt.l, tt.checked)
			assert.Equal(t, tt.want != "", ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

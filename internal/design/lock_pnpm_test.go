package design

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pnpmV9Fixture = `lockfileVersion: '9.0'

settings:
  autoInstallPeers: true
  excludeLinksFromLockfile: false

importers:

  .:
    dependencies:
      '@acme/shared':
        specifier: workspace:*
        version: link:packages/shared

  frontend:
    dependencies:
      '@acme/charts':
        specifier: ^3.1.0
        version: 3.1.0(react@19.0.0)
      '@acme/ui':
        specifier: ^1.2.0
        version: 1.2.3
      ui-charts:
        specifier: npm:@acme/charts@^3.1.0
        version: '@acme/charts@3.1.0(react@19.0.0)'
      ui-cjs:
        specifier: npm:@acme/ui@^1.2.0
        version: '@acme/ui@1.2.3'
    devDependencies:
      left-pad:
        specifier: ^1.0.0
        version: 1.0.2
    optionalDependencies:
      '@acme/optional':
        specifier: ^0.5.0
        version: 0.5.0

packages:

  '@acme/charts@3.1.0':
    resolution: {integrity: sha512-charts310}

  '@acme/optional@0.5.0':
    resolution: {integrity: sha512-opt050}

  '@acme/tokens@2.0.1':
    resolution: {integrity: sha512-tok201}

  '@acme/ui@1.2.3':
    resolution: {integrity: sha512-ui123}

  left-pad@1.0.2:
    resolution: {integrity: sha512-lp102}

snapshots:

  '@acme/charts@3.1.0(react@19.0.0)':
    dependencies:
      '@acme/tokens': 2.0.1(react@19.0.0)

  '@acme/optional@0.5.0': {}

  '@acme/tokens@2.0.1(react@19.0.0)':
    dependencies:
      left-pad: 1.0.2
      react: 19.0.0

  '@acme/ui@1.2.3':
    dependencies:
      '@acme/tokens': 2.0.1(react@19.0.0)

  left-pad@1.0.2: {}
`

const pnpmV6Fixture = `lockfileVersion: '6.0'

settings:
  autoInstallPeers: true
  excludeLinksFromLockfile: false

importers:

  .:
    dependencies:
      '@acme/shared':
        specifier: workspace:*
        version: link:packages/shared

  frontend:
    dependencies:
      '@acme/charts':
        specifier: ^3.1.0
        version: 3.1.0(react@19.0.0)
      '@acme/ui':
        specifier: ^1.2.0
        version: 1.2.3
      ui-charts:
        specifier: npm:@acme/charts@^3.1.0
        version: /@acme/charts@3.1.0(react@19.0.0)
      ui-cjs:
        specifier: npm:@acme/ui@^1.2.0
        version: /@acme/ui@1.2.3
    devDependencies:
      left-pad:
        specifier: ^1.0.0
        version: 1.0.2
    optionalDependencies:
      '@acme/optional':
        specifier: ^0.5.0
        version: 0.5.0

packages:

  /@acme/charts@3.1.0(react@19.0.0):
    resolution: {integrity: sha512-charts310}
    peerDependencies:
      react: '*'
    dependencies:
      '@acme/tokens': 2.0.1(react@19.0.0)
      react: 19.0.0
    dev: false

  /@acme/optional@0.5.0:
    resolution: {integrity: sha512-opt050}
    dev: false
    optional: true

  /@acme/tokens@2.0.1(react@19.0.0):
    resolution: {integrity: sha512-tok201}
    peerDependencies:
      react: '*'
    dependencies:
      left-pad: 1.0.2
      react: 19.0.0
    dev: false

  /@acme/ui@1.2.3:
    resolution: {integrity: sha512-ui123}
    dependencies:
      '@acme/tokens': 2.0.1(react@19.0.0)
    dev: false

  /left-pad@1.0.2:
    resolution: {integrity: sha512-lp102}
    dev: true
`

func TestPNPMLock(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
	}{
		{name: "lockfile v9", fixture: pnpmV9Fixture},
		{name: "lockfile v6", fixture: pnpmV6Fixture},
	}

	for _, fixture := range tests {
		t.Run(fixture.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{"pnpm-lock.yaml": fixture.fixture})

			lock, err := readLockfile(root)
			require.NoError(t, err)
			require.NotNil(t, lock)
			assert.True(t, lock.checksummed())

			ui, ok := lock.fromRoot("frontend", "@acme/ui", "^1.2.0")
			require.True(t, ok)

			tokens, ok := lock.fromPackage(ui, "@acme/tokens")
			require.True(t, ok)

			charts, ok := lock.fromRoot("frontend", "@acme/charts", "^3.1.0")
			require.True(t, ok)

			tests := []struct {
				name      string
				resolve   func() (locked, bool)
				wantOK    bool
				wantName  string
				wantVer   string
				integrity string
				local     bool
			}{
				{
					name:    "an importer dependency resolves with its integrity",
					resolve: func() (locked, bool) { return lock.fromRoot("frontend", "@acme/ui", "^1.2.0") },
					wantOK:  true, wantName: "@acme/ui", wantVer: "1.2.3", integrity: "sha512-ui123",
				},
				{
					name:    "a dev dependency resolves",
					resolve: func() (locked, bool) { return lock.fromRoot("frontend", "left-pad", "^1.0.0") },
					wantOK:  true, wantName: "left-pad", wantVer: "1.0.2", integrity: "sha512-lp102",
				},
				{
					name:    "an optional dependency resolves",
					resolve: func() (locked, bool) { return lock.fromRoot("frontend", "@acme/optional", "^0.5.0") },
					wantOK:  true, wantName: "@acme/optional", wantVer: "0.5.0", integrity: "sha512-opt050",
				},
				{
					name:    "a peer suffix is not part of the version",
					resolve: func() (locked, bool) { return lock.fromRoot("frontend", "@acme/charts", "^3.1.0") },
					wantOK:  true, wantName: "@acme/charts", wantVer: "3.1.0", integrity: "sha512-charts310",
				},
				{
					name:    "a dependency resolves through the parent's dependencies",
					resolve: func() (locked, bool) { return lock.fromPackage(ui, "@acme/tokens") },
					wantOK:  true, wantName: "@acme/tokens", wantVer: "2.0.1", integrity: "sha512-tok201",
				},
				{
					name:    "a dependency of a package resolved with a peer suffix is found",
					resolve: func() (locked, bool) { return lock.fromPackage(tokens, "left-pad") },
					wantOK:  true, wantName: "left-pad", wantVer: "1.0.2", integrity: "sha512-lp102",
				},
				{
					name:    "a peer-suffixed package resolves its dependencies",
					resolve: func() (locked, bool) { return lock.fromPackage(charts, "@acme/tokens") },
					wantOK:  true, wantName: "@acme/tokens", wantVer: "2.0.1", integrity: "sha512-tok201",
				},
				{
					name:    "an alias resolves to the real package name",
					resolve: func() (locked, bool) { return lock.fromRoot("frontend", "ui-cjs", "npm:@acme/ui@^1.2.0") },
					wantOK:  true, wantName: "@acme/ui", wantVer: "1.2.3", integrity: "sha512-ui123",
				},
				{
					name:    "a peer-suffixed alias resolves to the real package name",
					resolve: func() (locked, bool) { return lock.fromRoot("frontend", "ui-charts", "npm:@acme/charts@^3.1.0") },
					wantOK:  true, wantName: "@acme/charts", wantVer: "3.1.0", integrity: "sha512-charts310",
				},
				{
					name: "a peer-suffixed alias resolves its dependencies",
					resolve: func() (locked, bool) {
						alias, ok := lock.fromRoot("frontend", "ui-charts", "npm:@acme/charts@^3.1.0")
						if !ok {
							return locked{}, false
						}

						return lock.fromPackage(alias, "@acme/tokens")
					},
					wantOK: true, wantName: "@acme/tokens", wantVer: "2.0.1", integrity: "sha512-tok201",
				},
				{
					name:    "a link is local",
					resolve: func() (locked, bool) { return lock.fromRoot("", "@acme/shared", "workspace:*") },
					wantOK:  true, wantName: "@acme/shared", local: true,
				},
				{
					name:    "a dependency of another importer is not found",
					resolve: func() (locked, bool) { return lock.fromRoot("", "@acme/ui", "^1.2.0") },
				},
				{
					name:    "an unknown importer is not found",
					resolve: func() (locked, bool) { return lock.fromRoot("backend", "@acme/ui", "^1.2.0") },
				},
				{
					name:    "an unknown package is not found",
					resolve: func() (locked, bool) { return lock.fromRoot("frontend", "@acme/missing", "*") },
				},
				{
					name:    "a name that is only a path prefix is not found",
					resolve: func() (locked, bool) { return lock.fromRoot("frontend", "../frontend", "*") },
				},
				{
					name:    "a dependency the parent does not declare is not found",
					resolve: func() (locked, bool) { return lock.fromPackage(ui, "@acme/missing") },
				},
			}

			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					got, ok := tt.resolve()
					require.Equal(t, tt.wantOK, ok)

					if !tt.wantOK {
						return
					}

					assert.Equal(t, tt.wantName, got.Name)
					assert.Equal(t, tt.wantVer, got.Version)
					assert.Equal(t, tt.integrity, got.Integrity)
					assert.Equal(t, tt.local, got.Local)
				})
			}

			leftPad, ok := lock.fromRoot("frontend", "left-pad", "^1.0.0")
			require.True(t, ok)

			_, ok = lock.fromPackage(leftPad, "react")
			assert.False(t, ok, "a package with no dependencies has none to find")
		})
	}
}

const pnpmV6SingleProjectFixture = `lockfileVersion: '6.0'

settings:
  autoInstallPeers: true

dependencies:
  '@acme/ui':
    specifier: ^1.2.0
    version: 1.2.3

packages:

  /@acme/ui@1.2.3:
    resolution: {integrity: sha512-ui123}
    dev: false
`

func TestPNPMLock_SingleProject(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"pnpm-lock.yaml": pnpmV6SingleProjectFixture})

	lock, err := readLockfile(root)
	require.NoError(t, err)
	require.NotNil(t, lock)

	got, ok := lock.fromRoot("", "@acme/ui", "^1.2.0")
	require.True(t, ok)
	assert.Equal(t, "1.2.3", got.Version)
	assert.Equal(t, "sha512-ui123", got.Integrity)

	_, ok = lock.fromRoot("frontend", "@acme/ui", "^1.2.0")
	assert.False(t, ok)
}

const pnpmResolutionsFixture = `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      '@acme/directory':
        specifier: ^1.0.0
        version: 1.0.0
      '@acme/git':
        specifier: github:acme/git
        version: 1.0.0
      '@acme/registry':
        specifier: ^1.0.0
        version: 1.0.0
      '@acme/tarball':
        specifier: ^1.0.0
        version: 1.0.0
      '@acme/unchecked':
        specifier: ^1.0.0
        version: 1.0.0
      '@acme/vendored':
        specifier: file:vendor/vendored
        version: file:vendor/vendored(react@19.0.0)

packages:

  '@acme/directory@1.0.0':
    resolution: {directory: vendor/directory, type: directory, integrity: sha512-dir100}

  '@acme/git@1.0.0':
    resolution: {commit: abc123, repo: 'https://example.test/acme/git.git', type: git, integrity: sha512-git100}

  '@acme/registry@1.0.0':
    resolution: {integrity: sha512-reg100}

  '@acme/tarball@1.0.0':
    resolution: {integrity: sha512-tar100, tarball: 'https://example.test/tarball.tgz'}

  '@acme/unchecked@1.0.0':
    resolution: {}

  '@acme/vendored@file:vendor/vendored':
    resolution: {directory: vendor/vendored, type: directory}
`

func TestPNPMLock_Resolutions(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"pnpm-lock.yaml": pnpmResolutionsFixture})

	lock, err := readLockfile(root)
	require.NoError(t, err)
	require.NotNil(t, lock)

	tests := []struct {
		name   string
		pkg    string
		wantOK bool
		local  bool
	}{
		{name: "a registry resolution is the package", pkg: "@acme/registry", wantOK: true},
		{name: "a tarball resolution is not the registry package", pkg: "@acme/tarball"},
		{name: "a git resolution is not the registry package", pkg: "@acme/git"},
		{name: "a directory resolution is not the registry package", pkg: "@acme/directory"},
		{name: "a resolution without integrity cannot be checked", pkg: "@acme/unchecked"},
		{name: "a file version is local whatever its resolution", pkg: "@acme/vendored", wantOK: true, local: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := lock.fromRoot("", tt.pkg, "*")
			require.Equal(t, tt.wantOK, ok)

			if !tt.wantOK {
				return
			}

			assert.Equal(t, tt.pkg, got.Name)
			assert.Equal(t, tt.local, got.Local)
		})
	}
}

func TestPNPMLock_Malformed(t *testing.T) {
	tests := []struct {
		name string
		lock string
	}{
		{name: "invalid yaml", lock: "lockfileVersion: '9.0'\nimporters: {\n"},
		{name: "the pre-v6 importer format", lock: "lockfileVersion: 5.4\nimporters:\n  .:\n    specifiers:\n      react: ^18.2.0\n    dependencies:\n      react: 18.2.0\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, map[string]string{"pnpm-lock.yaml": tt.lock})

			lock, err := readLockfile(root)
			require.Error(t, err)
			assert.Equal(t, lockfile(nil), lock)
		})
	}
}

package design

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const npmLockFixture = `{
  "name": "acme-app",
  "lockfileVersion": 3,
  "packages": {
    "": {"name": "acme-app", "workspaces": ["frontend"]},
    "frontend": {"name": "frontend", "version": "0.0.0"},
    "node_modules/@acme/ui": {
      "version": "1.2.3",
      "resolved": "https://registry.example.test/@acme/ui/-/ui-1.2.3.tgz",
      "integrity": "sha512-ui123"
    },
    "frontend/node_modules/@acme/ui": {
      "version": "1.3.0",
      "resolved": "https://registry.example.test/@acme/ui/-/ui-1.3.0.tgz",
      "integrity": "sha512-ui130"
    },
    "node_modules/@acme/ui/node_modules/@acme/tokens": {
      "version": "2.0.1",
      "resolved": "https://registry.example.test/@acme/tokens/-/tokens-2.0.1.tgz",
      "integrity": "sha512-tok201"
    },
    "node_modules/@acme/tokens": {
      "version": "1.0.0",
      "resolved": "https://registry.example.test/@acme/tokens/-/tokens-1.0.0.tgz",
      "integrity": "sha512-tok100"
    },
    "node_modules/@acme/shared": {"resolved": "packages/shared", "link": true}
  }
}`

func TestNPMLock(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"package-lock.json": npmLockFixture})

	lock, err := readLockfile(root)
	require.NoError(t, err)
	require.NotNil(t, lock)
	assert.True(t, lock.checksummed())

	ui123, ok := lock.fromRoot("", "@acme/ui", "^1.2.0")
	require.True(t, ok)

	tests := []struct {
		name     string
		resolve  func() (locked, bool)
		wantOK   bool
		wantVer  string
		wantFull bool
		local    bool
	}{
		{
			name:    "nearest node_modules wins",
			resolve: func() (locked, bool) { return lock.fromRoot("frontend", "@acme/ui", "^1.3.0") },
			wantOK:  true, wantVer: "1.3.0",
		},
		{
			name:    "root resolves the hoisted package with its tarball",
			resolve: func() (locked, bool) { return lock.fromRoot("", "@acme/ui", "^1.2.0") },
			wantOK:  true, wantVer: "1.2.3", wantFull: true,
		},
		{
			name:    "a dependency prefers the copy nested under its parent",
			resolve: func() (locked, bool) { return lock.fromPackage(ui123, "@acme/tokens") },
			wantOK:  true, wantVer: "2.0.1",
		},
		{
			name:    "a nested root falls back to the hoisted package",
			resolve: func() (locked, bool) { return lock.fromRoot("frontend", "@acme/tokens", "*") },
			wantOK:  true, wantVer: "1.0.0",
		},
		{
			name:    "a link is local",
			resolve: func() (locked, bool) { return lock.fromRoot("", "@acme/shared", "*") },
			wantOK:  true, local: true,
		},
		{
			name:    "an unknown package is not found",
			resolve: func() (locked, bool) { return lock.fromRoot("", "@acme/missing", "*") },
		},
		{
			name:    "an unknown dependency of a package is not found",
			resolve: func() (locked, bool) { return lock.fromPackage(ui123, "@acme/missing") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.resolve()
			require.Equal(t, tt.wantOK, ok)

			if !tt.wantOK {
				return
			}

			assert.Equal(t, tt.local, got.Local)
			assert.Equal(t, tt.wantVer, got.Version)

			if tt.wantFull {
				assert.Equal(t, "@acme/ui", got.Name)
				assert.Equal(t, "https://registry.example.test/@acme/ui/-/ui-1.2.3.tgz", got.Resolved)
				assert.Equal(t, "sha512-ui123", got.Integrity)
			}
		})
	}
}

func TestNPMLock_Malformed(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"package-lock.json": `{"packages": `})

	lock, err := readLockfile(root)
	require.Error(t, err)
	assert.Nil(t, lock)
}

func TestReadLockfile_None(t *testing.T) {
	lock, err := readLockfile(t.TempDir())
	require.NoError(t, err)
	assert.Nil(t, lock)
}

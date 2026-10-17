package code

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	}
}

func TestDeclared(t *testing.T) {
	dir := t.TempDir()

	writeTree(t, dir, map[string]string{
		"go.mod": "module github.com/acme/payments\n\ngo 1.27\n",
		"internal/server/health.go": "package server\n\n// healthzHandler answers the probe; see docs_only_name.\n" +
			"func healthzHandler() {}\n\nfunc (s *Server) startListener() {}\n\n" +
			"type ServerConfig struct {\n\tMaxRunCostUSD float64 `koanf:\"max_run_cost_usd\"`\n}\n",
		"config.yaml":               "max_turns: 80\n# commented_key: 1\nport: 8080\n",
		"web/package.json":          "{\n  \"name\": \"@acme/payments-web\",\n  \"version\": \"1.0.0\"\n}\n",
		"app/main.py":               "class PaymentReconciler:\n    def reconcile_batch(self):\n        self.max_retries = 3\n",
		"src/Billing.java":          "public class Billing {\n    private final int retryLimit = 3;\n}\n",
		"README.md":                 "Runs PostgreSQL through CloudNativePG.\nreadme_only_key: 1\n",
		"vendor/lib/lib.go":         "package lib\n\nfunc vendoredThing() {}\n",
		"node_modules/dep/index.js": "function moduleThing() {}\n",
		".github/workflows/ci.yml":  "hidden_key: 1\n",
		"data.json":                 "{\"binary_key\": 1}\x00\n",
	})

	// A link out of the tree is never followed.
	outside := filepath.Join(t.TempDir(), "outside.go")
	require.NoError(t, os.WriteFile(outside, []byte("package x\n\nfunc outsideThing() {}\n"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "linked.go")))

	names, err := Declared(context.Background(), dir)
	require.NoError(t, err)

	// web/package.json is nested: its package name is an identifier, not a
	// module.
	assert.Equal(t, []string{
		"MaxRunCostUSD", "PaymentReconciler", "ServerConfig", "healthzHandler", "max_retries",
		"max_run_cost_usd", "max_turns", "payments-web", "reconcile_batch", "retryLimit", "self.max_retries", "startListener",
	}, names.Identifiers)
	assert.Equal(t, []string{"github.com/acme/payments"}, names.Modules)
}

func TestDeclared_StopsWithTheContext(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.go": "package a\n\nfunc doThing() {}\n"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Declared(ctx, dir)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDeclared_AnEmptyTreeDeclaresNothing(t *testing.T) {
	names, err := Declared(context.Background(), t.TempDir())
	require.NoError(t, err)
	assert.Empty(t, names.Identifiers)
	assert.Empty(t, names.Modules)
}

func TestDeclared_Sources(t *testing.T) {
	oversized := "package big\n\nfunc bigThingName() {}\n// " + strings.Repeat("x", maxDeclaredFile)

	tests := []struct {
		name            string
		files           map[string]string
		wantIdentifiers []string
		wantModules     []string
	}{
		{
			name:            "a file over the size cap is skipped",
			files:           map[string]string{"big.go": oversized, "small.go": "package s\n\nfunc smallThingName() {}\n"},
			wantIdentifiers: []string{"smallThingName"},
		},
		{
			name:        "cargo reads the package name",
			files:       map[string]string{"Cargo.toml": "[package]\nname = \"payments-core\"\nversion = \"0.1.0\"\n"},
			wantModules: []string{"payments-core"},
		},
		{
			name:        "pyproject reads the project name",
			files:       map[string]string{"pyproject.toml": "[project]\nname = \"payments-worker\"\n"},
			wantModules: []string{"payments-worker"},
		},
		{
			name: "a later name in a manifest is not a module",
			files: map[string]string{
				"Cargo.toml": "[package]\nname = \"payments-core\"\n\n[[bin]]\nname = \"payments-cli\"\n",
			},
			wantModules: []string{"payments-core"},
		},
		{
			name:        "a root go.mod is a module",
			files:       map[string]string{"go.mod": "module github.com/acme/payments\n\ngo 1.27\n"},
			wantModules: []string{"github.com/acme/payments"},
		},
		{
			name: "a nested test fixture's module adds no single word",
			files: map[string]string{
				"go.mod":                     "module github.com/acme/payments\n",
				"internal/x/testdata/go.mod": "module example.com/test\n",
			},
			wantModules: []string{"github.com/acme/payments"},
		},
		{
			name:            "a nested package's compound name is an identifier",
			files:           map[string]string{"packages/billing/package.json": "{\n  \"name\": \"@acme/billing-core\"\n}\n"},
			wantIdentifiers: []string{"billing-core"},
		},
		{
			name:  "a nested package named by one word adds nothing",
			files: map[string]string{"apps/web/package.json": "{\n  \"name\": \"web\"\n}\n"},
		},
		{
			name: "package.json dependencies are not identifiers",
			files: map[string]string{
				// devDependencies and build-assets are keys the file declares;
				// the packages it depends on are not.
				"package.json": "{\n  \"name\": \"payments-web\",\n  \"scripts\": {\n    \"build-assets\": \"vite build\"\n  },\n" +
					"  \"dependencies\": {\n    \"react-dom\": \"^18.3.1\",\n    \"socket.io\": \"^4.7.5\"\n  },\n" +
					"  \"devDependencies\": {\n    \"core-js\": \"^3.37.0\"\n  }\n}\n",
			},
			wantIdentifiers: []string{"build-assets", "devDependencies"},
			wantModules:     []string{"payments-web"},
		},
		{
			name:            "a package.json that does not parse depends on nothing",
			files:           map[string]string{"package.json": "{\n  \"dependencies\": {\n    \"react-dom\": \"^18.3.1\",\n  }\n}\n"},
			wantIdentifiers: []string{"react-dom"},
		},
		{
			name: "a nested package another one depends on is still an identifier",
			files: map[string]string{
				"package.json":                  "{\n  \"name\": \"acme\",\n  \"dependencies\": {\n    \"billing-core\": \"workspace:*\"\n  }\n}\n",
				"packages/billing/package.json": "{\n  \"name\": \"billing-core\"\n}\n",
			},
			wantIdentifiers: []string{"billing-core"},
			wantModules:     []string{"acme"},
		},
		{
			name: "cargo dependency tables are not identifiers",
			files: map[string]string{
				"Cargo.toml": "[package]\nname = \"payments-core\"\n\n" +
					"[dependencies] # from crates.io\ntokio-postgres = \"0.7\"\nserde_json = { version = \"1\" }\n\n" +
					"[dependencies.serde_with]\nversion = \"3\"\n\n" +
					"[target.'cfg(unix)'.dev-dependencies]\npretty_assertions = \"1\"\n\n" +
					"[workspace.dependencies]\ntracing-subscriber = \"0.3\"\n\n" +
					"[patch.crates-io]\nserde_with = { path = \"../serde_with\" }\n\n" +
					"[features]\nwith_metrics = []\n",
			},
			wantIdentifiers: []string{"with_metrics"},
			wantModules:     []string{"payments-core"},
		},
		{
			name:            "any table header ends a dependency table",
			files:           map[string]string{"Cargo.toml": "[dependencies]\nserde_json = \"1\"\n\n[\"my-section\"]\nretry_limit = 3\n"},
			wantIdentifiers: []string{"retry_limit"},
		},
		{
			name: "poetry dependency tables are not identifiers",
			files: map[string]string{
				"pyproject.toml": "[tool.poetry]\nname = \"payments-worker\"\n\n" +
					"[tool.poetry.dependencies]\npython = \"^3.12\"\npydantic-settings = \"^2\"\n\n" +
					"[tool.poetry.group.dev.dependencies]\npytest-asyncio = \"^0.23\"\n\n" +
					"[tool.payments]\nretry_limit = 3\n",
			},
			wantIdentifiers: []string{"retry_limit"},
			wantModules:     []string{"payments-worker"},
		},
		{
			name: "a chart's dependency is no identifier in its values",
			files: map[string]string{
				"Chart.yaml": "name: payments\ndependencies:\n  - name: cloudnative-pg\n    version: 0.20.0\n" +
					"    repository: https://cloudnative-pg.github.io/charts\n",
				"values.yaml": "cloudnative-pg:\n  enabled: true\nreplica_count: 2\n",
			},
			wantIdentifiers: []string{"replica_count"},
		},
		{
			name: "a lockfile contributes nothing",
			files: map[string]string{
				"package-lock.json": "{\n  \"name\": \"payments-web\",\n  \"lockfileVersion\": 3,\n  \"packages\": {\n" +
					"    \"node_modules/react-dom\": {\n      \"version\": \"18.3.1\"\n    }\n  }\n}\n",
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\nimporters:\n  .:\n    dependencies:\n      left-pad:\n        specifier: ^1.3.0\n",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTree(t, dir, tc.files)

			names, err := Declared(context.Background(), dir)
			require.NoError(t, err)

			assert.Equal(t, tc.wantIdentifiers, names.Identifiers)
			assert.Equal(t, tc.wantModules, names.Modules)
		})
	}
}

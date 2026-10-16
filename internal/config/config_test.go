package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))

	return p
}

func TestLoad_DefaultsFileEnvPrecedence(t *testing.T) {
	p := writeFile(t, "contextmatrix_url: http://cm:8080\napi_key: "+strings.Repeat("k", 32)+"\nport: 9100\nmodels:\n  reviewer: openai/gpt-5.5\n")
	t.Setenv("CONTEXTMATRIX_ARCHITECT_PORT", "9200")
	t.Setenv("CONTEXTMATRIX_ARCHITECT_MODELS__PRODUCT_OWNER", "anthropic/claude-sonnet-4")

	cfg, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, "http://cm:8080", cfg.ContextMatrixURL)
	assert.Equal(t, 9200, cfg.Port, "env wins over file")
	assert.Equal(t, "info", cfg.LogLevel, "default survives")
	assert.Equal(t, 330*time.Second, cfg.ReplaySkew)
	assert.Equal(t, time.Minute, cfg.WatchInterval)
	assert.Equal(t, "openai/gpt-5.5", cfg.Models["reviewer"])
	assert.Equal(t, "anthropic/claude-sonnet-4", cfg.Models["product_owner"])
}

func TestLoad_UnknownKeyFails(t *testing.T) {
	p := writeFile(t, "contextmatrix_url: http://cm:8080\napi_key: "+strings.Repeat("k", 32)+"\nprot: 1\n")

	_, err := Load(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prot")
}

func TestLoad_MissingFileIsDefaultsPlusEnv(t *testing.T) {
	t.Setenv("CONTEXTMATRIX_ARCHITECT_CONTEXTMATRIX_URL", "http://cm:8080")
	t.Setenv("CONTEXTMATRIX_ARCHITECT_API_KEY", strings.Repeat("k", 32))

	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	require.NoError(t, err)
	assert.Equal(t, 9095, cfg.Port)
	assert.InDelta(t, 5, cfg.MaxRunCostUSD, 1e-9)
	assert.Equal(t, 80, cfg.MaxTurns)
	require.NoError(t, cfg.Validate())
}

func TestValidate(t *testing.T) {
	valid := func() *Config {
		return &Config{
			ContextMatrixURL: "http://cm:8080", APIKey: strings.Repeat("k", 32), CallerIdentity: "human:architect", Port: 9095,
			LogLevel: "info", StateDir: "/tmp/x", MaxRunCostUSD: 5, MaxTurns: 80, WorkspaceMaxAgeDays: 7, WatchInterval: time.Minute,
		}
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"ok", func(*Config) {}, ""},
		{"zero cost cap disables the per-run cap", func(c *Config) { c.MaxRunCostUSD = 0 }, ""},
		{"no url", func(c *Config) { c.ContextMatrixURL = "" }, "contextmatrix_url"},
		{"short key", func(c *Config) { c.APIKey = "short" }, "api_key"},
		{"no state dir", func(c *Config) { c.StateDir = "" }, "state_dir"},
		{"negative cost cap", func(c *Config) { c.MaxRunCostUSD = -1 }, "max_run_cost_usd"},
		{"zero turns", func(c *Config) { c.MaxTurns = 0 }, "max_turns"},
		{"zero workspace age", func(c *Config) { c.WorkspaceMaxAgeDays = 0 }, "workspace_max_age_days"},
		{"zero watch interval", func(c *Config) { c.WatchInterval = 0 }, "watch_interval_seconds"},
		{"bad level", func(c *Config) { c.LogLevel = "loud" }, "log_level"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.mutate(c)

			err := c.Validate()
			if tc.want == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestDefaultsYAMLRoundTrips(t *testing.T) {
	out, err := DefaultsYAML()
	require.NoError(t, err)

	p := writeFile(t, string(out))

	cfg, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, 9095, cfg.Port)
	assert.Contains(t, string(out), "contextmatrix_url:")
	assert.Contains(t, string(out), "max_run_cost_usd:")
}

func TestCallerIdentity(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "human:architect", cfg.CallerIdentity)

	cfg.ContextMatrixURL = "http://cm"
	cfg.APIKey = strings.Repeat("k", 32)

	for _, bad := range []string{"", "architect", "human:", "agent:architect"} {
		cfg.CallerIdentity = bad
		err := cfg.Validate()
		require.Error(t, err, bad)
		assert.Contains(t, err.Error(), "caller_identity")
	}

	cfg.CallerIdentity = "human:alice"
	require.NoError(t, cfg.Validate())
}

// contextmatrix-setup drops a user's value for any key the defaults omit, and
// keeps a map printed as {} verbatim.
func TestDefaultsYAMLListsEveryKey(t *testing.T) {
	out, err := DefaultsYAML()
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, yaml.Unmarshal(out, &got))

	want := map[string]bool{}
	rt := reflect.TypeFor[raw]()

	for i := range rt.NumField() {
		want[rt.Field(i).Tag.Get("koanf")] = true
	}

	assert.ElementsMatch(t, slices.Collect(maps.Keys(want)), slices.Collect(maps.Keys(got)))
	assert.IsType(t, map[string]any{}, got["models"])
	assert.Empty(t, got["models"])
}

func TestLoad_StateDirResolution(t *testing.T) {
	base := "contextmatrix_url: http://cm:8080\napi_key: " + strings.Repeat("k", 32) + "\n"
	xdg := t.TempDir()
	home := t.TempDir()

	tests := []struct {
		name    string
		file    string
		envDir  string
		xdg     string
		home    string
		want    string
		wantErr string
	}{
		{name: "xdg state home", xdg: xdg, home: home, want: filepath.Join(xdg, "contextmatrix-architect")},
		{name: "home fallback", home: home, want: filepath.Join(home, ".local", "state", "contextmatrix-architect")},
		{name: "neither is empty and invalid", wantErr: "state_dir"},
		{name: "file wins over xdg", file: "state_dir: /srv/from-file\n", xdg: xdg, home: home, want: "/srv/from-file"},
		{name: "env wins over xdg", envDir: "/srv/from-env", xdg: xdg, home: home, want: "/srv/from-env"},
		{name: "env wins over file", file: "state_dir: /srv/from-file\n", envDir: "/srv/from-env", xdg: xdg, home: home, want: "/srv/from-env"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", tt.xdg)
			t.Setenv("HOME", tt.home)
			// An empty env value would override the file, so unset it.
			t.Setenv("CONTEXTMATRIX_ARCHITECT_STATE_DIR", "")

			if tt.envDir != "" {
				t.Setenv("CONTEXTMATRIX_ARCHITECT_STATE_DIR", tt.envDir)
			} else {
				require.NoError(t, os.Unsetenv("CONTEXTMATRIX_ARCHITECT_STATE_DIR"))
			}

			cfg, err := Load(writeFile(t, base+tt.file))
			require.NoError(t, err)

			if tt.wantErr != "" {
				assert.Empty(t, cfg.StateDir)

				err := cfg.Validate()
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			assert.Equal(t, tt.want, cfg.StateDir)
			require.NoError(t, cfg.Validate())
		})
	}
}

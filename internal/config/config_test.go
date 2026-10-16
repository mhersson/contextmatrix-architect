package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			LogLevel: "info", StateDir: "/tmp/x", MaxRunCostUSD: 5, MaxTurns: 80, WorkspaceMaxAgeDays: 7,
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

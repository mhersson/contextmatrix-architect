package cli

import (
	"bytes"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/mhersson/contextmatrix-architect/internal/pack"
)

// validConfig passes Validate: the three keys without a usable default.
// validRequired is it without state_dir, for a case that sets its own.
const (
	validRequired = "contextmatrix_url: http://localhost:18080\n" +
		"api_key: 0123456789abcdef0123456789abcdef\n"
	validConfig = validRequired + "state_dir: /tmp/architect-state\n"
)

// runCLI runs the command tree with args and returns stdout and the error
// main would print.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out bytes.Buffer

	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(&out)
	root.SetErr(&out)

	err := root.Execute()

	return out.String(), err
}

func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()

	p := filepath.Join(dir, "architect.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))

	return p
}

func TestConfigValidate_AcceptsTheNamedFile(t *testing.T) {
	p := writeConfig(t, t.TempDir(), validConfig)

	_, err := runCLI(t, "config", "validate", p)
	require.NoError(t, err)
}

func TestConfigValidate_JudgesOnlyTheNamedFile(t *testing.T) {
	// A valid config.yaml in the working directory must not stand in for the
	// file contextmatrix-setup names.
	cwd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "config.yaml"), []byte(validConfig), 0o600))
	t.Chdir(cwd)

	p := writeConfig(t, t.TempDir(), validConfig+"log_level: loud\n")

	_, err := runCLI(t, "config", "validate", p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log_level")
}

func TestConfigValidate_MissingFileFailsWhateverTheEnv(t *testing.T) {
	t.Setenv("CONTEXTMATRIX_ARCHITECT_CONTEXTMATRIX_URL", "http://localhost:18080")
	t.Setenv("CONTEXTMATRIX_ARCHITECT_API_KEY", strings.Repeat("k", 32))

	_, err := runCLI(t, "config", "validate", filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err)
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestConfigValidate_NamesTheFailingKey(t *testing.T) {
	cases := map[string]struct{ body, key string }{
		"invalid value":      {"contextmatrix_url: http://localhost:18080\napi_key: short\n", "api_key"},
		"unknown key":        {validConfig + "prot: 1\n", "prot"},
		"relative state_dir": {validRequired + "state_dir: ./state\n", "state_dir"},
		"missing pack":       {validConfig + "pack: /typo\n", "/typo"},
		"unknown persona":    {validConfig + "models:\n  designr: x/y\n", "designr"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeConfig(t, t.TempDir(), c.body)

			_, err := runCLI(t, "config", "validate", p)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.key)
		})
	}
}

func TestConfigValidate_UnknownPersonaListsThePacksPersonas(t *testing.T) {
	p := writeConfig(t, t.TempDir(), validConfig+"models:\n  designr: x/y\n  testr: x/z\n  reviewer: x/w\n")

	_, err := runCLI(t, "config", "validate", p)
	require.Error(t, err)

	pk, err2 := pack.LoadEmbedded()
	require.NoError(t, err2)

	for _, name := range append([]string{"designr", "testr"}, pk.Personas()...) {
		assert.Contains(t, err.Error(), name)
	}
}

// serve refuses what validate refuses, before it listens: the port is taken,
// so a regression that listened would fail on it instead.
func TestServe_RefusesWhatValidateRefuses(t *testing.T) {
	taken, err := net.Listen("tcp", ":0") //nolint:gosec // serve binds every interface; only the same bind is sure to collide on every OS
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })

	base := validRequired + "state_dir: " + t.TempDir() + "\n" +
		"port: " + strconv.Itoa(taken.Addr().(*net.TCPAddr).Port) + "\n"

	cases := map[string]struct{ body, key string }{
		"missing pack":    {base + "pack: /typo\n", "/typo"},
		"unknown persona": {base + "models:\n  designr: x/y\n", "designr"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeConfig(t, t.TempDir(), c.body)

			_, err := runCLI(t, "serve", "--config", p)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.key)
		})
	}
}

func TestConfigValidate_TakesExactlyOneFile(t *testing.T) {
	p := writeConfig(t, t.TempDir(), validConfig)

	cases := map[string][]string{
		"none": {"config", "validate"},
		"two":  {"config", "validate", p, p},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runCLI(t, args...)
			require.Error(t, err)
		})
	}
}

// An unknown key fails the load before anything listens, and the error names
// it, so the error proves which file serve read.
const unloadable = validConfig + "prot: 1\n"

func TestServe_ReadsTheUserConfigFileByDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	dir, err := os.UserConfigDir()
	require.NoError(t, err)

	p := filepath.Join(dir, "contextmatrix-architect", "config.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(unloadable), 0o600))

	_, err = runCLI(t, "serve")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prot")
}

func TestServe_FallsBackToConfigYAMLWithoutAUserConfigDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	cwd := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cwd, "config.yaml"), []byte(unloadable), 0o600))
	t.Chdir(cwd)

	_, err := runCLI(t, "serve")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "prot")
}

func TestConfigDefaults_TakesNoArgs(t *testing.T) {
	_, err := runCLI(t, "config", "defaults", "extra")
	require.Error(t, err)
}

// contextmatrix-setup writes the defaults with its own values filled in and
// then validates the file; the two commands must agree.
func TestConfigDefaults_FilledInValidates(t *testing.T) {
	out, err := runCLI(t, "config", "defaults")
	require.NoError(t, err)

	var tree map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(out), &tree))

	tree["api_key"] = strings.Repeat("k", 32)
	tree["contextmatrix_url"] = "http://localhost:18080"
	tree["state_dir"] = t.TempDir()

	body, err := yaml.Marshal(tree)
	require.NoError(t, err)

	_, err = runCLI(t, "config", "validate", writeConfig(t, t.TempDir(), string(body)))
	require.NoError(t, err)
}

// The default state_dir is empty and resolves at load time, so a file that
// fills only the two remaining required keys validates.
func TestConfigDefaults_EmptyStateDirValidates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	out, err := runCLI(t, "config", "defaults")
	require.NoError(t, err)

	var tree map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(out), &tree))
	require.Empty(t, tree["state_dir"])

	tree["api_key"] = strings.Repeat("k", 32)
	tree["contextmatrix_url"] = "http://localhost:18080"

	body, err := yaml.Marshal(tree)
	require.NoError(t, err)

	_, err = runCLI(t, "config", "validate", writeConfig(t, t.TempDir(), string(body)))
	require.NoError(t, err)
}

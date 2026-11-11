package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readPin(t *testing.T, path string) map[string]string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	pin := map[string]string{}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		require.True(t, ok, "malformed pin line %q", line)

		pin[k] = v
	}

	return pin
}

func TestClaudeCodePinMatchesVendorkit(t *testing.T) {
	pin := readPin(t, "../../claude-code.pin")

	assert.Len(t, pin, 3)
	assert.Equal(t, claudecode.Version, pin["CLAUDE_CODE_VERSION"])

	for _, k := range []string{"CLAUDE_CODE_SHA512_AMD64", "CLAUDE_CODE_SHA512_ARM64"} {
		assert.Regexp(t, `^[0-9a-f]{128}$`, pin[k], k)
	}
}

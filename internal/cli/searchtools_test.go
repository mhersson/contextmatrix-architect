package cli

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os/exec"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lookPathOf(present ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		if slices.Contains(present, name) {
			return "/usr/bin/" + name, nil
		}

		return "", exec.ErrNotFound
	}
}

type warnRecord struct {
	Level         string `json:"level"`
	GlobAvailable bool   `json:"glob_available"`
}

func searchWarnings(t *testing.T, lookPath func(string) (string, error)) []warnRecord {
	t.Helper()

	var buf bytes.Buffer

	warnMissingSearchTools(slog.New(slog.NewJSONHandler(&buf, nil)), lookPath)

	var out []warnRecord

	dec := json.NewDecoder(&buf)
	for dec.More() {
		var r warnRecord
		require.NoError(t, dec.Decode(&r))
		out = append(out, r)
	}

	return out
}

func TestWarnMissingSearchTools(t *testing.T) {
	assert.Empty(t, searchWarnings(t, lookPathOf("rg")))
	assert.Empty(t, searchWarnings(t, lookPathOf("rg", "fd")))

	cases := map[string]struct {
		present []string
		glob    bool
	}{
		"fd covers glob":     {[]string{"fd"}, true},
		"fdfind covers glob": {[]string{"fdfind"}, true},
		"nothing":            {nil, false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			recs := searchWarnings(t, lookPathOf(c.present...))
			require.Len(t, recs, 1)
			assert.Equal(t, "WARN", recs[0].Level)
			assert.Equal(t, c.glob, recs[0].GlobAvailable)
		})
	}
}

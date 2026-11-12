package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/mhersson/contextmatrix-vendorkit/claudecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckClaudeCode(t *testing.T) {
	tests := []struct {
		name    string
		version string // empty: no binary
		wantErr bool
		want    []string
		notWant []string
	}{
		{
			name: "no runnable binary", wantErr: true,
			want:    []string{"claude_code sessions are refused"},
			notWant: []string{"claude_code available"},
		},
		{
			name: "tested version", version: claudecode.Version,
			want:    []string{"claude_code available"},
			notWant: []string{"differs", "refused"},
		},
		{
			name: "other version", version: "0.0.1",
			want:    []string{"claude_code available", "claude_code version differs from the tested version"},
			notWant: []string{"refused"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "missing-claude")

			if tc.version != "" {
				bin = filepath.Join(t.TempDir(), "claude")
				script := "#!/bin/sh\necho '" + tc.version + " (Claude Code)'\n"
				require.NoError(t, os.WriteFile(bin, []byte(script), 0o600))
				require.NoError(t, os.Chmod(bin, 0o700)) //nolint:gosec // a test stand-in must be executable
			}

			var buf bytes.Buffer

			logger := slog.New(slog.NewTextHandler(&buf, nil))
			err := checkClaudeCode(context.Background(), bin, logger)

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			for _, w := range tc.want {
				assert.Contains(t, buf.String(), w)
			}

			for _, w := range tc.notWant {
				assert.NotContains(t, buf.String(), w)
			}
		})
	}
}

func TestSweepClaudeHomesLogsTheCount(t *testing.T) {
	tests := []struct {
		name  string
		homes int
		want  string
	}{
		{"nothing left behind", 0, ""},
		{"two left behind", 2, "removed=2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			for i := range tc.homes {
				require.NoError(t, os.Mkdir(filepath.Join(dir, "architect-claude-"+strconv.Itoa(i)), 0o700))
			}

			var buf bytes.Buffer

			sweepClaudeHomes(dir, slog.New(slog.NewTextHandler(&buf, nil)))

			if tc.want == "" {
				assert.Empty(t, buf.String())

				return
			}

			assert.Contains(t, buf.String(), "level=INFO")
			assert.Contains(t, buf.String(), tc.want)
		})
	}
}

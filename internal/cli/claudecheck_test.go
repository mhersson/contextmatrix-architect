package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
		// wantAttrs holds, per expected record in order, its level and the
		// attrs it must carry.
		wantAttrs []wantRecord
	}{
		{
			name: "no runnable binary", wantErr: true,
			wantAttrs: []wantRecord{{level: "WARN", present: []string{"error"}}},
		},
		{
			name: "tested version", version: claudecode.Version,
			wantAttrs: []wantRecord{{level: "INFO", attrs: map[string]any{"version": claudecode.Version}}},
		},
		{
			name: "other version", version: "0.0.1",
			wantAttrs: []wantRecord{
				{level: "INFO", attrs: map[string]any{"version": "0.0.1"}},
				{level: "WARN", attrs: map[string]any{"version": "0.0.1", "tested": claudecode.Version}},
			},
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

			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			err := checkClaudeCode(context.Background(), bin, logger)

			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			records := decodeRecords(t, &buf)
			require.Len(t, records, len(tc.wantAttrs))

			for i, want := range tc.wantAttrs {
				assert.Equal(t, want.level, records[i]["level"])

				for _, key := range want.present {
					assert.Contains(t, records[i], key)
				}

				for key, val := range want.attrs {
					assert.Equal(t, val, records[i][key])
				}
			}

			if tc.version == "" {
				for _, rec := range records {
					assert.NotContains(t, rec, "version")
				}
			}
		})
	}
}

type wantRecord struct {
	level   string
	present []string
	attrs   map[string]any
}

func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var records []map[string]any

	dec := json.NewDecoder(buf)
	for dec.More() {
		var rec map[string]any

		require.NoError(t, dec.Decode(&rec))

		records = append(records, rec)
	}

	return records
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

package export

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/contextmatrix-architect/internal/store"
)

func TestWrite_RendersEveryPart(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	files, err := Write(dir, Bundle{
		Blueprint: store.Blueprint{ID: "BP1", Projects: []string{"alpha"}, Mode: "default", State: "idle", CostCapUSD: 3, CreatedAt: now, UpdatedAt: now},
		Runs:      []store.Run{{ID: 7, Stage: "intake", Attempt: 1, Round: 1, Status: store.RunDone, Model: "m", StartedAt: now, Usage: store.RunUsage{CostUSD: 0.2}}},
		Artifacts: []store.Artifact{{Stage: "need", Version: 1, Body: "the need"}, {Stage: "intake", Version: 1, Body: "# Need", Data: json.RawMessage(`{"slices":1}`)}},
		Events: []store.Event{
			{Time: now, Kind: "question", Payload: json.RawMessage(`{"text":"Which cluster?","options":["dev","prod"]}`)},
			{Time: now, Kind: "answer", Payload: json.RawMessage(`{"text":"dev"}`)},
		},
		Sources:     []store.Source{{Number: 1, Kind: "repo", Identity: "https://example.com/alpha.git", Version: "abc", FetchedAt: now}},
		Transcripts: map[int64]json.RawMessage{7: json.RawMessage(`[{"role":"user","content":"x"}]`)},
	})
	require.NoError(t, err)

	for _, want := range []string{"blueprint.md", "artifacts/need-v1.md", "artifacts/intake-v1.md", "questions.md", "sources.md", "events.jsonl", "transcripts/7.json"} {
		assert.FileExists(t, filepath.Join(dir, want), want)
		assert.Contains(t, files, want)
	}

	questions, err := os.ReadFile(filepath.Join(dir, "questions.md"))
	require.NoError(t, err)
	assert.Contains(t, string(questions), "Which cluster?")
	assert.Contains(t, string(questions), "dev")

	intake, err := os.ReadFile(filepath.Join(dir, "artifacts/intake-v1.md"))
	require.NoError(t, err)
	assert.Contains(t, string(intake), "# Need")
	assert.Contains(t, string(intake), `"slices"`)

	info, err := os.Stat(filepath.Join(dir, "blueprint.md"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

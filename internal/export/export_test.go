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
		Runs:      []store.Run{{ID: 7, Stage: "intake", Kind: store.RunKindResponder, Attempt: 1, Round: 1, Status: store.RunDone, Model: "m", StartedAt: now, Usage: store.RunUsage{CostUSD: 0.2}}},
		Artifacts: []store.Artifact{{Stage: "need", Version: 1, Body: "the need"}, {Stage: "intake", Version: 1, Body: "# Need", Data: json.RawMessage(`{"slices":1}`)}},
		Events:    []store.Event{},
		Questions: []store.Question{{
			BlueprintID: "BP1", Stage: "intake", Number: 1, Text: "Which cluster?", Options: []string{"dev", "prod"},
			Answer: "dev", Responder: store.ResponderHuman, AnsweredAt: now,
		}},
		Gates: []store.Gate{
			{Stage: "signoff", Version: 1, Decision: store.GateReject, Feedback: "split it\nup | now", Responder: store.ResponderHuman, DecidedAt: now},
			{Stage: "signoff", Version: 2, Decision: store.GateApprove, Responder: store.ResponderHuman, DecidedAt: now},
			{Stage: "signoff", Version: 3, Decision: store.GateReject, Feedback: `keep a\|b`, Responder: store.ResponderHuman, DecidedAt: now},
		},
		Emissions: []store.Emission{
			{Kind: store.EmitCard, DraftID: "D1", Project: "alpha", CMID: "ALPHA-007", CreatedAt: now},
			{Kind: store.EmitPlaybook, CMID: "ship-it", CreatedAt: now},
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
	assert.Contains(t, string(questions), "Q1")

	intake, err := os.ReadFile(filepath.Join(dir, "artifacts/intake-v1.md"))
	require.NoError(t, err)
	assert.Contains(t, string(intake), "# Need")
	assert.Contains(t, string(intake), `"slices"`)

	summary, err := os.ReadFile(filepath.Join(dir, "blueprint.md"))
	require.NoError(t, err)
	assert.Contains(t, string(summary), `split it up \| now`, "newlines flattened and the pipe escaped")
	assert.Contains(t, string(summary), `keep a\\\|b`, "a backslash cannot unescape the pipe after it")
	assert.Contains(t, string(summary), "ALPHA-007")
	assert.Contains(t, string(summary), "ship-it")
	assert.Contains(t, string(summary), "| 7 | Intake | "+store.RunKindResponder+" |", "the runs table shows the kind")

	info, err := os.Stat(filepath.Join(dir, "blueprint.md"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
}

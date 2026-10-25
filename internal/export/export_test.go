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

func TestWrite_NamesAStepsSliceOnlyWithTwoOrMoreSlices(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sliced bool
		key    string
		want   string
		absent string
	}{
		{name: "two or more slices", sliced: true, key: "solution_design@SL2", want: "Solution design (SL2)"},
		{name: "one slice", key: "solution_design@SL1", want: "Solution design", absent: "SL1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			_, err := Write(dir, Bundle{
				Blueprint: store.Blueprint{ID: "BP1"},
				Runs:      []store.Run{{ID: 1, Stage: tc.key, Kind: store.RunKindStage, Status: store.RunDone}},
				Sliced:    tc.sliced,
			})
			require.NoError(t, err)

			raw, err := os.ReadFile(filepath.Join(dir, "blueprint.md"))
			require.NoError(t, err)
			assert.Contains(t, string(raw), tc.want)

			if tc.absent != "" {
				assert.NotContains(t, string(raw), tc.absent)
			}
		})
	}
}

func TestWrite_NamesUnattended(t *testing.T) {
	for _, on := range []bool{true, false} {
		dir := t.TempDir()

		_, err := Write(dir, Bundle{Blueprint: store.Blueprint{ID: "BP1", Unattended: on}})
		require.NoError(t, err)

		raw, err := os.ReadFile(filepath.Join(dir, "blueprint.md"))
		require.NoError(t, err)

		if on {
			assert.Contains(t, string(raw), "- Unattended: on\n")
		} else {
			assert.NotContains(t, string(raw), "Unattended")
		}
	}
}

func TestWrite_NamesPlainLanguage(t *testing.T) {
	for _, plain := range []bool{true, false} {
		dir := t.TempDir()

		_, err := Write(dir, Bundle{Blueprint: store.Blueprint{ID: "BP1", PlainLanguage: plain}})
		require.NoError(t, err)

		raw, err := os.ReadFile(filepath.Join(dir, "blueprint.md"))
		require.NoError(t, err)

		if plain {
			assert.Contains(t, string(raw), "- Plain language: on\n")
		} else {
			assert.NotContains(t, string(raw), "Plain language")
		}
	}
}

func TestWrite_ListsTheLessonSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lessons []store.Lesson
		want    []string
	}{
		{
			name: "with lessons",
			lessons: []store.Lesson{
				{ID: 3, State: store.LessonActive, Text: "Name where the key comes from"},
				{ID: 7, State: store.LessonRetired, Text: "Say which | file\nholds it"},
			},
			want: []string{"## Lessons\n", "| L3 | Name where the key comes from |\n", `| L7 | Say which \| file holds it |` + "\n"},
		},
		{name: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			_, err := Write(dir, Bundle{Blueprint: store.Blueprint{ID: "BP1"}, Lessons: tc.lessons})
			require.NoError(t, err)

			raw, err := os.ReadFile(filepath.Join(dir, "blueprint.md"))
			require.NoError(t, err)

			for _, w := range tc.want {
				assert.Contains(t, string(raw), w)
			}

			if len(tc.lessons) == 0 {
				assert.NotContains(t, string(raw), "## Lessons")
			}
		})
	}
}

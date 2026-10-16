package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openTest(t *testing.T) *Store {
	t.Helper()

	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "architect.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func TestBlueprintRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	bp := Blueprint{
		ID: "BP1", Title: "k8s", Projects: []string{"alpha", "beta"}, Mode: "yolo",
		CostCapUSD: 3, Model: "m", State: "new", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.CreateBlueprint(ctx, bp))

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, bp, got)

	require.NoError(t, s.SetBlueprintState(ctx, "BP1", "idle"))
	got, err = s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, "idle", got.State)
	assert.False(t, got.UpdatedAt.Before(now))

	_, err = s.GetBlueprint(ctx, "missing")
	require.ErrorIs(t, err, ErrNotFound)

	require.Error(t, s.CreateBlueprint(ctx, bp), "duplicate id must fail")
}

func TestRunsAndCost(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP1", State: "new", CreatedAt: now, UpdatedAt: now}))

	id1, err := s.StartRun(ctx, Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: RunRunning, Model: "m", StartedAt: now})
	require.NoError(t, err)
	id2, err := s.StartRun(ctx, Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 2, Status: RunRunning, Model: "m", StartedAt: now})
	require.NoError(t, err)
	assert.NotEqual(t, id1, id2)

	ended := now.Add(time.Minute)
	require.NoError(t, s.EndRun(ctx, id1, RunDone, "done", 0.25, ended))
	require.NoError(t, s.EndRun(ctx, id2, RunFailed, "max_cost", 1.5, ended))

	runs, err := s.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, RunDone, runs[0].Status)
	require.NotNil(t, runs[0].EndedAt)
	assert.Equal(t, ended, *runs[0].EndedAt)
	assert.Equal(t, "max_cost", runs[1].Reason)

	cost, err := s.BlueprintCost(ctx, "BP1")
	require.NoError(t, err)
	assert.InDelta(t, 1.75, cost, 1e-9)
}

func TestEventsAppendOnly(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "BP1", Kind: "a", Payload: json.RawMessage(`{"x":1}`)}))
	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "BP1", RunID: 7, Kind: "b", Payload: json.RawMessage(`null`)}))
	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "OTHER", Kind: "c", Payload: json.RawMessage(`{}`)}))

	got, err := s.ListEvents(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Kind)
	assert.JSONEq(t, `{"x":1}`, string(got[0].Payload))
	assert.Equal(t, int64(7), got[1].RunID)
}

func TestTranscriptsAppendOnly(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	require.NoError(t, s.PutTranscript(ctx, 1, json.RawMessage(`[{"role":"user","content":"hi"}]`)))

	got, err := s.GetTranscript(ctx, 1)
	require.NoError(t, err)
	assert.JSONEq(t, `[{"role":"user","content":"hi"}]`, string(got))

	require.Error(t, s.PutTranscript(ctx, 1, json.RawMessage(`[]`)), "one transcript per run")

	_, err = s.GetTranscript(ctx, 99)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestArtifactsVersioned(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	v1, err := s.PutArtifact(ctx, Artifact{BlueprintID: "BP1", Stage: "intake", Body: "# Need v1", Data: json.RawMessage(`{"shape":"one slice"}`)})
	require.NoError(t, err)
	v2, err := s.PutArtifact(ctx, Artifact{BlueprintID: "BP1", Stage: "intake", Body: "# Need v2"})
	require.NoError(t, err)
	assert.Equal(t, 1, v1)
	assert.Equal(t, 2, v2)

	latest, err := s.LatestArtifact(ctx, "BP1", "intake")
	require.NoError(t, err)
	assert.Equal(t, 2, latest.Version)
	assert.Equal(t, "# Need v2", latest.Body)
	assert.False(t, latest.CreatedAt.IsZero())

	_, err = s.LatestArtifact(ctx, "BP1", "slicing")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestOpen_RefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "architect.db")
	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	_, err = s.db.Exec(`PRAGMA user_version = 99`)
	require.NoError(t, err)
	require.NoError(t, s.Close())

	_, err = Open(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer")
}

func TestAppendOnlyTablesRefuseRawWrites(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "BP1", Kind: "a", Payload: json.RawMessage(`{}`)}))
	require.NoError(t, s.PutTranscript(ctx, 1, json.RawMessage(`[]`)))
	_, err := s.PutArtifact(ctx, Artifact{BlueprintID: "BP1", Stage: "intake", Body: "b"})
	require.NoError(t, err)

	tests := []struct {
		name  string
		table string
		stmt  string
	}{
		{"events update", "events", `UPDATE events SET kind = 'z'`},
		{"events delete", "events", `DELETE FROM events`},
		{"transcripts update", "transcripts", `UPDATE transcripts SET messages = '[]'`},
		{"transcripts delete", "transcripts", `DELETE FROM transcripts`},
		{"artifacts update", "artifacts", `UPDATE artifacts SET body = 'x'`},
		{"artifacts delete", "artifacts", `DELETE FROM artifacts`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.db.ExecContext(ctx, tt.stmt)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "append-only")

			var n int
			require.NoError(t, s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tt.table).Scan(&n))
			assert.Equal(t, 1, n)
		})
	}
}

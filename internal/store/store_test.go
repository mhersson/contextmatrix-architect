package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
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

func TestSetBlueprintCostCap(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	bp := Blueprint{ID: "BP1", Mode: "default", CostCapUSD: 2, State: "idle", CreatedAt: created, UpdatedAt: created}
	require.NoError(t, s.CreateBlueprint(ctx, bp))

	require.NoError(t, s.SetBlueprintCostCap(ctx, "BP1", 4.5))

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.InDelta(t, 4.5, got.CostCapUSD, 1e-9)
	assert.True(t, got.UpdatedAt.After(created), "the change bumps updated_at")

	got.CostCapUSD, got.UpdatedAt = bp.CostCapUSD, bp.UpdatedAt
	assert.Equal(t, bp, got, "nothing else changes")

	require.ErrorIs(t, s.SetBlueprintCostCap(ctx, "missing", 1), ErrNotFound)
}

func TestSetBlueprintMode(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	created := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	bp := Blueprint{ID: "BP1", Mode: "default", CostCapUSD: 2, State: "idle", CreatedAt: created, UpdatedAt: created}
	require.NoError(t, s.CreateBlueprint(ctx, bp))

	require.NoError(t, s.SetBlueprintMode(ctx, "BP1", "auto+emit"))

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, "auto+emit", got.Mode)
	assert.True(t, got.UpdatedAt.After(created), "the change bumps updated_at")

	got.Mode, got.UpdatedAt = bp.Mode, bp.UpdatedAt
	assert.Equal(t, bp, got, "nothing else changes")

	require.ErrorIs(t, s.SetBlueprintMode(ctx, "missing", "auto"), ErrNotFound)
}

func TestBlueprintPriceRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	price := &protocol.ModelPrice{PromptUSDPerMTok: 3, CompletionUSDPerMTok: 15, CacheReadUSDPerMTok: 0.3, CacheWriteUSDPerMTok: 3.75}

	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "priced", State: "new", Price: price, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "unpriced", State: "new", CreatedAt: now, UpdatedAt: now}))

	got, err := s.GetBlueprint(ctx, "priced")
	require.NoError(t, err)
	require.NotNil(t, got.Price)
	assert.Equal(t, *price, *got.Price)

	got, err = s.GetBlueprint(ctx, "unpriced")
	require.NoError(t, err)
	assert.Nil(t, got.Price)
}

func TestBlueprintContextWindowRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "sized", State: "new", ContextWindow: 200000, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "unsized", State: "new", CreatedAt: now, UpdatedAt: now}))

	got, err := s.GetBlueprint(ctx, "sized")
	require.NoError(t, err)
	assert.Equal(t, 200000, got.ContextWindow)

	got, err = s.GetBlueprint(ctx, "unsized")
	require.NoError(t, err)
	assert.Zero(t, got.ContextWindow)
}

func TestRunUsageRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP1", State: "new", CreatedAt: now, UpdatedAt: now}))

	id, err := s.StartRun(ctx, Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: RunRunning, Model: "m", StartedAt: now})
	require.NoError(t, err)

	usage := RunUsage{PromptTokens: 1000, CompletionTokens: 200, CacheReadTokens: 300, CacheWriteTokens: 50, CostUSD: 0.0123}
	require.NoError(t, s.EndRun(ctx, id, RunDone, "done", usage, now))

	runs, err := s.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, usage, runs[0].Usage)

	cost, err := s.BlueprintCost(ctx, "BP1")
	require.NoError(t, err)
	assert.InDelta(t, 0.0123, cost, 1e-9)
}

func TestSourcesNumberPerBlueprint(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, id := range []string{"BP1", "BP2"} {
		require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: id, State: "new", CreatedAt: now, UpdatedAt: now}))
	}

	first, err := s.AddSource(ctx, Source{BlueprintID: "BP1", Kind: "repo", Identity: "https://example.com/alpha.git", Version: "abc", FetchedAt: now, FirstUsedBy: "grounding"})
	require.NoError(t, err)
	second, err := s.AddSource(ctx, Source{BlueprintID: "BP1", Kind: "repo", Identity: "https://example.com/beta.git", Version: "def", FetchedAt: now, FirstUsedBy: "grounding"})
	require.NoError(t, err)
	other, err := s.AddSource(ctx, Source{BlueprintID: "BP2", Kind: "url", Identity: "https://example.com/doc", FetchedAt: now})
	require.NoError(t, err)

	assert.Equal(t, "S1", first.Tag())
	assert.Equal(t, "S2", second.Tag())
	assert.Equal(t, "S1", other.Tag(), "numbers restart per blueprint")

	got, err := s.ListSources(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "abc", got[0].Version)
	assert.Equal(t, now, got[0].FetchedAt)
}

func TestOpen_MigratesV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "architect.db")

	// A v1 file: the slice 0 tables without the price and token columns.
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)

	for _, stmt := range []string{
		`CREATE TABLE blueprints (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', projects TEXT NOT NULL DEFAULT '[]',
			mode TEXT NOT NULL DEFAULT '', cost_cap_usd REAL NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE stage_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, blueprint_id TEXT NOT NULL, stage TEXT NOT NULL,
			attempt INTEGER NOT NULL, round INTEGER NOT NULL, status TEXT NOT NULL, model TEXT NOT NULL DEFAULT '',
			started_at INTEGER NOT NULL, ended_at INTEGER, cost_usd REAL NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO blueprints (id, state, created_at, updated_at) VALUES ('old', 'idle', 1, 1)`,
		`PRAGMA user_version = 1`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}

	require.NoError(t, db.Close())

	s, err := Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.GetBlueprint(context.Background(), "old")
	require.NoError(t, err)
	assert.Nil(t, got.Price)
	assert.Zero(t, got.ContextWindow, "a blueprint stored before the window has none")

	var version int
	require.NoError(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	assert.Equal(t, schemaVersion, version)

	// Every ALTER landed: usage written to the migrated table reads back.
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	id, err := s.StartRun(ctx, Run{BlueprintID: "old", Stage: "intake", Attempt: 1, Round: 1, Status: RunRunning, StartedAt: now})
	require.NoError(t, err)

	usage := RunUsage{PromptTokens: 1, CompletionTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, CostUSD: 0.5}
	require.NoError(t, s.EndRun(ctx, id, RunDone, "done", usage, now))

	runs, err := s.ListRuns(ctx, "old")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, usage, runs[0].Usage)
	assert.Equal(t, RunKindStage, runs[0].Kind, "the kind column landed too")
}

func TestOpen_FailedMigrationLeavesV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "architect.db")

	// stage_runs already has one of the new columns, so a later ALTER fails
	// after the blueprints ALTER succeeded.
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)

	for _, stmt := range []string{
		`CREATE TABLE blueprints (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', projects TEXT NOT NULL DEFAULT '[]',
			mode TEXT NOT NULL DEFAULT '', cost_cap_usd REAL NOT NULL DEFAULT 0, model TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE stage_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, blueprint_id TEXT NOT NULL, stage TEXT NOT NULL,
			attempt INTEGER NOT NULL, round INTEGER NOT NULL, status TEXT NOT NULL, model TEXT NOT NULL DEFAULT '',
			started_at INTEGER NOT NULL, ended_at INTEGER, cost_usd REAL NOT NULL DEFAULT 0, reason TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0)`,
		`PRAGMA user_version = 1`,
	} {
		_, err := db.Exec(stmt)
		require.NoError(t, err)
	}

	require.NoError(t, db.Close())

	_, err = Open(context.Background(), path)
	require.Error(t, err)

	db, err = sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = db.Close() })

	var version int
	require.NoError(t, db.QueryRow("PRAGMA user_version").Scan(&version))
	assert.Equal(t, 1, version)

	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('blueprints') WHERE name = 'price'`).Scan(&n))
	assert.Zero(t, n, "the blueprints ALTER was rolled back")
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
	require.NoError(t, s.EndRun(ctx, id1, RunDone, "done", RunUsage{CostUSD: 0.25}, ended))
	require.NoError(t, s.EndRun(ctx, id2, RunFailed, "max_cost", RunUsage{CostUSD: 1.5}, ended))

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

func TestEventsRoundTrip(t *testing.T) {
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

func TestLatestEvent_IsTheNewestOfAKindForABlueprint(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "BP1", Kind: "a", Payload: json.RawMessage(`{"n":1}`)}))
	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "BP1", RunID: 3, Kind: "a", Payload: json.RawMessage(`{"n":2}`)}))
	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "BP1", Kind: "b", Payload: json.RawMessage(`{"n":3}`)}))
	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "OTHER", Kind: "a", Payload: json.RawMessage(`{"n":4}`)}))

	got, err := s.LatestEvent(ctx, "BP1", "a")
	require.NoError(t, err)
	assert.Equal(t, "a", got.Kind)
	assert.Equal(t, "BP1", got.BlueprintID)
	assert.Equal(t, int64(3), got.RunID)
	assert.JSONEq(t, `{"n":2}`, string(got.Payload))

	_, err = s.LatestEvent(ctx, "BP1", "c")
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.LatestEvent(ctx, "BP2", "a")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestTranscriptsRoundTrip(t *testing.T) {
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
	assert.Contains(t, err.Error(), path, "the error names the database")
}

func TestAppendOnlyTablesRefuseRawWrites(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	require.NoError(t, s.AppendEvent(ctx, Event{Time: now, BlueprintID: "BP1", Kind: "a", Payload: json.RawMessage(`{}`)}))
	require.NoError(t, s.PutTranscript(ctx, 1, json.RawMessage(`[]`)))
	_, err := s.PutArtifact(ctx, Artifact{BlueprintID: "BP1", Stage: "intake", Body: "b"})
	require.NoError(t, err)
	_, err = s.AddGate(ctx, Gate{BlueprintID: "BP1", Stage: "signoff", Version: 1, Decision: GateApprove, Responder: ResponderHuman, DecidedAt: now})
	require.NoError(t, err)
	require.NoError(t, s.AddEmission(ctx, Emission{BlueprintID: "BP1", Kind: EmitCard, DraftID: "D1", Project: "alpha", CMID: "ALPHA-1", CreatedAt: now}))
	require.NoError(t, s.AddQuestion(ctx, Question{BlueprintID: "BP1", RunID: 1, Stage: "intake", Number: 1, Text: "q", Answer: "a", Responder: ResponderHuman, AnsweredAt: now}))

	// Each table's one row, read back to prove a refused write changed nothing.
	rows := map[string]struct{ query, want string }{
		"events":      {`SELECT kind FROM events`, "a"},
		"transcripts": {`SELECT messages FROM transcripts`, "[]"},
		"artifacts":   {`SELECT body FROM artifacts`, "b"},
		"gates":       {`SELECT decision FROM gates`, "approve"},
		"emissions":   {`SELECT cm_id FROM emissions`, "ALPHA-1"},
		"questions":   {`SELECT answer FROM questions`, "a"},
	}

	tests := []struct {
		name  string
		table string
		stmt  string
	}{
		{"events update", "events", `UPDATE events SET kind = 'z'`},
		{"events delete", "events", `DELETE FROM events`},
		{"events replace", "events", `INSERT OR REPLACE INTO events (id, ts, blueprint_id, run_id, kind, payload)
			SELECT id, ts, blueprint_id, run_id, 'z', payload FROM events`},
		{"transcripts update", "transcripts", `UPDATE transcripts SET messages = '[]'`},
		{"transcripts delete", "transcripts", `DELETE FROM transcripts`},
		{"transcripts replace", "transcripts", `INSERT OR REPLACE INTO transcripts (run_id, messages, created_at) VALUES (1, '[{}]', 0)`},
		{"artifacts update", "artifacts", `UPDATE artifacts SET body = 'x'`},
		{"artifacts delete", "artifacts", `DELETE FROM artifacts`},
		{"artifacts replace", "artifacts", `INSERT OR REPLACE INTO artifacts (blueprint_id, stage, version, body, created_at)
			VALUES ('BP1', 'intake', 1, 'x', 0)`},
		{"gates update", "gates", `UPDATE gates SET decision = 'reject'`},
		{"gates delete", "gates", `DELETE FROM gates`},
		{"emissions update", "emissions", `UPDATE emissions SET cm_id = 'X'`},
		{"emissions delete", "emissions", `DELETE FROM emissions`},
		{"emissions replace", "emissions", `INSERT OR REPLACE INTO emissions (blueprint_id, kind, draft_id, project, cm_id, created_at)
			VALUES ('BP1', 'card', 'D1', 'alpha', 'X', 0)`},
		{"questions update", "questions", `UPDATE questions SET answer = 'z'`},
		{"questions delete", "questions", `DELETE FROM questions`},
		{"questions replace", "questions", `INSERT OR REPLACE INTO questions (blueprint_id, run_id, stage, number, text, answer, responder, answered_at)
			VALUES ('BP1', 1, 'intake', 1, 'q', 'z', 'human', 0)`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.db.ExecContext(ctx, tt.stmt)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "append-only")

			var n int
			require.NoError(t, s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tt.table).Scan(&n))
			assert.Equal(t, 1, n)

			var got string
			require.NoError(t, s.db.QueryRowContext(ctx, rows[tt.table].query).Scan(&got))
			assert.Equal(t, rows[tt.table].want, got)
		})
	}
}

func TestListArtifacts_AllStagesAllVersions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP1", State: "new", CreatedAt: now, UpdatedAt: now}))

	for _, a := range []Artifact{
		{BlueprintID: "BP1", Stage: "need", Body: "n1"},
		{BlueprintID: "BP1", Stage: "intake", Body: "i1"},
		{BlueprintID: "BP1", Stage: "intake", Body: "i2"},
	} {
		_, err := s.PutArtifact(ctx, a)
		require.NoError(t, err)
	}

	got, err := s.ListArtifacts(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "need", got[0].Stage)
	assert.Equal(t, 2, got[2].Version)
}

func TestGatesRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	_, err := s.AddGate(ctx, Gate{BlueprintID: "BP1", Stage: "signoff", Version: 2, Decision: GateReject, Feedback: "split D2", Responder: ResponderHuman, DecidedAt: now})
	require.NoError(t, err)
	_, err = s.AddGate(ctx, Gate{BlueprintID: "BP1", Stage: "signoff", Version: 3, Decision: GateApprove, Responder: ResponderAuto, DecidedAt: now})
	require.NoError(t, err)
	_, err = s.AddGate(ctx, Gate{BlueprintID: "BP2", Stage: "signoff", Version: 1, Decision: GateApprove, Responder: ResponderHuman, DecidedAt: now})
	require.NoError(t, err)

	got, err := s.ListGates(ctx, "BP1", "signoff")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, GateReject, got[0].Decision)
	assert.Equal(t, "split D2", got[0].Feedback)
	assert.Equal(t, 2, got[0].Version)
	assert.Equal(t, now, got[0].DecidedAt)
	assert.Equal(t, ResponderAuto, got[1].Responder)
}

func TestEmissionsRoundTripAndRefuseDuplicates(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.AddEmission(ctx, Emission{BlueprintID: "BP1", Kind: EmitCard, DraftID: "D1", Project: "alpha", CMID: "ALPHA-007", CreatedAt: now}))
	require.NoError(t, s.AddEmission(ctx, Emission{BlueprintID: "BP1", Kind: EmitPlaybook, CMID: "ship-the-thing", CreatedAt: now}))

	err := s.AddEmission(ctx, Emission{BlueprintID: "BP1", Kind: EmitCard, DraftID: "D1", Project: "alpha", CMID: "ALPHA-008", CreatedAt: now})
	require.ErrorIs(t, err, ErrDuplicate)

	got, err := s.ListEmissions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, []Emission{
		{BlueprintID: "BP1", Kind: EmitCard, DraftID: "D1", Project: "alpha", CMID: "ALPHA-007", CreatedAt: now},
		{BlueprintID: "BP1", Kind: EmitPlaybook, CMID: "ship-the-thing", CreatedAt: now},
	}, got)
}

func TestOpen_MigratesV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "architect.db")

	s, err := Open(context.Background(), path)
	require.NoError(t, err)

	// A v2 file: the v3 tables and their triggers do not exist yet.
	for _, stmt := range []string{
		`DROP TRIGGER gates_no_update`, `DROP TRIGGER gates_no_delete`, `DROP TABLE gates`,
		`DROP TRIGGER emissions_no_update`, `DROP TRIGGER emissions_no_delete`, `DROP TABLE emissions`,
		`DROP TRIGGER questions_no_update`, `DROP TRIGGER questions_no_delete`, `DROP TABLE questions`,
		`ALTER TABLE stage_runs DROP COLUMN kind`,
		`ALTER TABLE blueprints DROP COLUMN context_window`,
		`ALTER TABLE blueprints DROP COLUMN plain_language`,
		`ALTER TABLE blueprints DROP COLUMN unattended`,
		`PRAGMA user_version = 2`,
	} {
		_, err := s.db.Exec(stmt)
		require.NoError(t, err)
	}

	require.NoError(t, s.Close())

	s, err = Open(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var version int
	require.NoError(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	assert.Equal(t, schemaVersion, version)

	_, err = s.AddGate(context.Background(), Gate{BlueprintID: "BP1", Stage: "signoff", Version: 1, Decision: GateApprove, Responder: ResponderHuman, DecidedAt: time.Now()})
	require.NoError(t, err)

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	_, err = s.StartRun(ctx, Run{BlueprintID: "BP1", Stage: "intake", Kind: RunKindResponder, Attempt: 1, Round: 1, Status: RunRunning, StartedAt: now})
	require.NoError(t, err)

	runs, err := s.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, RunKindResponder, runs[0].Kind, "the migrated stage_runs keeps a kind")

	q := Question{BlueprintID: "BP1", RunID: runs[0].ID, Stage: "intake", Number: 1, Text: "q", Answer: "a", Responder: ResponderHuman, AnsweredAt: now}
	require.NoError(t, s.AddQuestion(ctx, q))

	got, err := s.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, []Question{q}, got)
}

func TestRunKindRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	_, err := s.StartRun(ctx, Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: RunRunning, StartedAt: now})
	require.NoError(t, err)
	_, err = s.StartRun(ctx, Run{BlueprintID: "BP1", Stage: "intake", Kind: RunKindResponder, Attempt: 1, Round: 1, Status: RunRunning, StartedAt: now})
	require.NoError(t, err)

	runs, err := s.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	assert.Equal(t, RunKindStage, runs[0].Kind, "a run without a kind is a stage run")
	assert.Equal(t, RunKindResponder, runs[1].Kind)
}

func TestQuestionsRoundTripAndNumbersAreUnique(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	q1 := Question{
		BlueprintID: "BP1", RunID: 3, Stage: "intake", Number: 1, Text: "Helm or kustomize?",
		Options: []string{"helm", "kustomize"}, Context: "how it deploys", Answer: "helm",
		Responder: ResponderHuman, MessageID: "m2", AnsweredAt: now,
	}
	q2 := Question{
		BlueprintID: "BP1", RunID: 4, Stage: "intake", Number: 2, Text: "Which cluster?", Answer: "dev",
		Responder: ResponderAuto, Assumption: true, Grounded: true, AnsweredAt: now,
	}

	require.NoError(t, s.AddQuestion(ctx, q2))
	require.NoError(t, s.AddQuestion(ctx, q1))
	require.NoError(t, s.AddQuestion(ctx, Question{BlueprintID: "BP2", RunID: 9, Stage: "intake", Number: 1, Text: "x", Answer: "y", Responder: ResponderHuman, AnsweredAt: now}))

	got, err := s.ListQuestions(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, []Question{q1, q2}, got, "in number order, this blueprint only")
	assert.Equal(t, "Q2", got[1].Tag())

	require.Error(t, s.AddQuestion(ctx, q1), "a number is answered once per blueprint")
}

func TestOpen_MigratesV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "architect.db")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	s, err := Open(ctx, path)
	require.NoError(t, err)

	_, err = s.StartRun(ctx, Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: RunDone, StartedAt: now})
	require.NoError(t, err)

	// A v3 file: no questions table, and stage_runs without a kind.
	for _, stmt := range []string{
		`DROP TRIGGER questions_no_update`, `DROP TRIGGER questions_no_delete`, `DROP TABLE questions`,
		`ALTER TABLE stage_runs DROP COLUMN kind`,
		`ALTER TABLE blueprints DROP COLUMN context_window`,
		`ALTER TABLE blueprints DROP COLUMN plain_language`,
		`ALTER TABLE blueprints DROP COLUMN unattended`,
		`PRAGMA user_version = 3`,
	} {
		_, err := s.db.Exec(stmt)
		require.NoError(t, err)
	}

	require.NoError(t, s.Close())

	s, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var version int
	require.NoError(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	assert.Equal(t, schemaVersion, version)

	runs, err := s.ListRuns(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, RunKindStage, runs[0].Kind, "every run a v3 file holds was a stage run")

	require.NoError(t, s.AddQuestion(ctx, Question{
		BlueprintID: "BP1", RunID: runs[0].ID, Stage: "intake", Number: 1, Text: "q", Answer: "a",
		Responder: ResponderHuman, AnsweredAt: now,
	}))
}

func TestListEventsOfKind(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, e := range []Event{
		{Time: now, BlueprintID: "BP1", Kind: "policy", Payload: json.RawMessage(`{"n":1}`)},
		{Time: now, BlueprintID: "BP1", Kind: "note", Payload: json.RawMessage(`{"n":2}`)},
		{Time: now, BlueprintID: "BP1", Kind: "policy", Payload: json.RawMessage(`{"n":3}`)},
		{Time: now, BlueprintID: "BP2", Kind: "policy", Payload: json.RawMessage(`{"n":4}`)},
	} {
		require.NoError(t, s.AppendEvent(ctx, e))
	}

	got, err := s.ListEventsOfKind(ctx, "BP1", "policy")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.JSONEq(t, `{"n":1}`, string(got[0].Payload))
	assert.JSONEq(t, `{"n":3}`, string(got[1].Payload))
}

func TestListRunEventsOfKind(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, e := range []Event{
		{Time: now, BlueprintID: "BP1", RunID: 1, Kind: "harness", Payload: json.RawMessage(`{"n":1}`)},
		{Time: now, BlueprintID: "BP1", RunID: 2, Kind: "harness", Payload: json.RawMessage(`{"n":2}`)},
		{Time: now, BlueprintID: "BP1", RunID: 1, Kind: "note", Payload: json.RawMessage(`{"n":3}`)},
		{Time: now, BlueprintID: "BP1", RunID: 1, Kind: "harness", Payload: json.RawMessage(`{"n":4}`)},
		{Time: now, BlueprintID: "BP2", RunID: 1, Kind: "harness", Payload: json.RawMessage(`{"n":5}`)},
	} {
		require.NoError(t, s.AppendEvent(ctx, e))
	}

	got, err := s.ListRunEventsOfKind(ctx, "BP1", 1, "harness")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.JSONEq(t, `{"n":1}`, string(got[0].Payload))
	assert.JSONEq(t, `{"n":4}`, string(got[1].Payload))
}

func TestAllGates_EveryStageOldestFirst(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, g := range []Gate{
		{BlueprintID: "BP1", Stage: "shape", Version: 1, Decision: GateReject, Feedback: "smaller", Responder: ResponderHuman, DecidedAt: now},
		{BlueprintID: "BP1", Stage: "signoff", Version: 1, Decision: GateApprove, Responder: ResponderHuman, DecidedAt: now},
		{BlueprintID: "BP2", Stage: "shape", Version: 1, Decision: GateApprove, Responder: ResponderAuto, DecidedAt: now},
	} {
		_, err := s.AddGate(ctx, g)
		require.NoError(t, err)
	}

	got, err := s.AllGates(ctx, "BP1")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "shape", got[0].Stage)
	assert.Equal(t, "signoff", got[1].Stage)
}

func TestCountEvents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, e := range []Event{
		{Time: now, BlueprintID: "BP1", Kind: "question"},
		{Time: now, BlueprintID: "BP1", Kind: "answer"},
		{Time: now, BlueprintID: "BP1", Kind: "question"},
		{Time: now, BlueprintID: "BP2", Kind: "question"},
	} {
		require.NoError(t, s.AppendEvent(ctx, e))
	}

	n, err := s.CountEvents(ctx, "BP1", "question")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestLastEventID(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	id, err := s.LastEventID(ctx, "BP1")
	require.NoError(t, err)
	assert.Zero(t, id, "a blueprint with no events")

	for _, bp := range []string{"BP1", "BP2", "BP1"} {
		require.NoError(t, s.AppendEvent(ctx, Event{Time: time.Now(), BlueprintID: bp, Kind: "note"}))
	}

	all, err := s.ListEvents(ctx, "BP1")
	require.NoError(t, err)

	id, err = s.LastEventID(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, all[len(all)-1].ID, id, "the newest of its own events, not another blueprint's")
}

func TestGetArtifact(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)

	for _, body := range []string{"v1", "v2"} {
		_, err := s.PutArtifact(ctx, Artifact{BlueprintID: "BP1", Stage: "intake", Body: body, Data: json.RawMessage(`{"n":1}`)})
		require.NoError(t, err)
	}

	a, err := s.GetArtifact(ctx, "BP1", "intake", 1)
	require.NoError(t, err)
	assert.Equal(t, "v1", a.Body)
	assert.JSONEq(t, `{"n":1}`, string(a.Data))

	_, err = s.GetArtifact(ctx, "BP1", "intake", 3)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = s.GetArtifact(ctx, "BP2", "intake", 1)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCreateBlueprint_KeepsPlainLanguage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP1", Mode: "default", PlainLanguage: true, State: "new", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP2", Mode: "default", State: "new", CreatedAt: now, UpdatedAt: now}))

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, got.PlainLanguage)

	got, err = s.GetBlueprint(ctx, "BP2")
	require.NoError(t, err)
	assert.False(t, got.PlainLanguage)
}

func TestCreateBlueprint_KeepsUnattended(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP1", Mode: "default", Unattended: true, State: "new", CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP2", Mode: "default", State: "new", CreatedAt: now, UpdatedAt: now}))

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.True(t, got.Unattended)

	got, err = s.GetBlueprint(ctx, "BP2")
	require.NoError(t, err)
	assert.False(t, got.Unattended)
}

func TestOpen_MigratesV7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "architect.db")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	s, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP1", Mode: "default", State: "new", CreatedAt: now, UpdatedAt: now}))

	// A v7 file: blueprints without unattended.
	for _, stmt := range []string{
		`ALTER TABLE blueprints DROP COLUMN unattended`,
		`PRAGMA user_version = 7`,
	} {
		_, err := s.db.Exec(stmt)
		require.NoError(t, err)
	}

	require.NoError(t, s.Close())

	s, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var version int
	require.NoError(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	assert.Equal(t, 8, version)

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.False(t, got.Unattended, "a blueprint stored before the flag is not unattended")

	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP2", Mode: "default", Unattended: true, State: "new", CreatedAt: now, UpdatedAt: now}))

	got, err = s.GetBlueprint(ctx, "BP2")
	require.NoError(t, err)
	assert.True(t, got.Unattended)
}

func TestOpen_MigratesV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "architect.db")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	s, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP1", Mode: "default", State: "new", CreatedAt: now, UpdatedAt: now}))

	// A v5 file: blueprints without plain_language.
	for _, stmt := range []string{
		`ALTER TABLE blueprints DROP COLUMN plain_language`,
		`ALTER TABLE blueprints DROP COLUMN unattended`,
		`PRAGMA user_version = 5`,
	} {
		_, err := s.db.Exec(stmt)
		require.NoError(t, err)
	}

	require.NoError(t, s.Close())

	s, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var version int
	require.NoError(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	assert.Equal(t, schemaVersion, version)

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.False(t, got.PlainLanguage, "a blueprint stored before the flag is not plain")

	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP2", Mode: "default", PlainLanguage: true, State: "new", CreatedAt: now, UpdatedAt: now}))

	got, err = s.GetBlueprint(ctx, "BP2")
	require.NoError(t, err)
	assert.True(t, got.PlainLanguage)
}

func TestOpen_MigratesV6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "architect.db")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	s, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, s.CreateBlueprint(ctx, Blueprint{ID: "BP1", Mode: "default", State: "new", CreatedAt: now, UpdatedAt: now}))

	// A v6 file: no lesson tables.
	for _, stmt := range []string{
		`DROP TABLE lessons`,
		`DROP TABLE blueprint_lessons`,
		`ALTER TABLE blueprints DROP COLUMN unattended`,
		`PRAGMA user_version = 6`,
	} {
		_, err := s.db.Exec(stmt)
		require.NoError(t, err)
	}

	require.NoError(t, s.Close())

	s, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	var version int
	require.NoError(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	assert.Equal(t, schemaVersion, version)

	got, err := s.GetBlueprint(ctx, "BP1")
	require.NoError(t, err)
	assert.Equal(t, "default", got.Mode)

	_, err = s.AddLessons(ctx, []Lesson{{BlueprintID: "BP1", Scope: ScopePack, Text: "kept"}})
	require.NoError(t, err)

	var n int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM blueprint_lessons").Scan(&n))
	assert.Equal(t, 0, n)

	set, err := s.BlueprintLessons(ctx, "BP1")
	require.NoError(t, err)
	assert.Empty(t, set)
}

func TestRunModel(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.StartRun(ctx, Run{BlueprintID: "BP1", Stage: "intake", Attempt: 1, Round: 1, Status: RunRunning, Model: "anthropic/claude-sonnet-5.5", StartedAt: time.Now().UTC()})
	require.NoError(t, err)

	got, err := s.RunModel(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude-sonnet-5.5", got)

	_, err = s.RunModel(ctx, id+1)
	assert.ErrorIs(t, err, ErrNotFound)
}

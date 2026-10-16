package store

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion stamps PRAGMA user_version. Bump it when the schema changes;
// a newer database is refused.
const schemaVersion = 2

func ensureSchema(ctx context.Context, db *sql.DB) error {
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}

	if current > schemaVersion {
		return fmt.Errorf("store: db schema v%d is newer than this binary (expects v%d)", current, schemaVersion)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS blueprints (
			id           TEXT PRIMARY KEY,
			title        TEXT NOT NULL DEFAULT '',
			projects     TEXT NOT NULL DEFAULT '[]',
			mode         TEXT NOT NULL DEFAULT '',
			cost_cap_usd REAL NOT NULL DEFAULT 0,
			model        TEXT NOT NULL DEFAULT '',
			price        TEXT,
			state        TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS stage_runs (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			blueprint_id TEXT NOT NULL,
			stage        TEXT NOT NULL,
			attempt      INTEGER NOT NULL,
			round        INTEGER NOT NULL,
			status       TEXT NOT NULL,
			model        TEXT NOT NULL DEFAULT '',
			started_at   INTEGER NOT NULL,
			ended_at     INTEGER,
			cost_usd     REAL NOT NULL DEFAULT 0,
			reason       TEXT NOT NULL DEFAULT '',
			prompt_tokens      INTEGER NOT NULL DEFAULT 0,
			completion_tokens  INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
			cache_write_tokens INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_stage_runs_blueprint ON stage_runs(blueprint_id, id)`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			blueprint_id TEXT NOT NULL,
			stage        TEXT NOT NULL,
			version      INTEGER NOT NULL,
			body         TEXT NOT NULL,
			data         TEXT,
			created_at   INTEGER NOT NULL,
			UNIQUE(blueprint_id, stage, version)
		)`,
		`CREATE TABLE IF NOT EXISTS transcripts (
			run_id     INTEGER PRIMARY KEY,
			messages   TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			ts           INTEGER NOT NULL,
			blueprint_id TEXT NOT NULL,
			run_id       INTEGER NOT NULL DEFAULT 0,
			kind         TEXT NOT NULL,
			payload      TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_blueprint ON events(blueprint_id, id)`,
		`CREATE TABLE IF NOT EXISTS sources (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			blueprint_id  TEXT NOT NULL,
			number        INTEGER NOT NULL,
			kind          TEXT NOT NULL,
			identity      TEXT NOT NULL,
			version       TEXT NOT NULL DEFAULT '',
			fetched_at    INTEGER NOT NULL,
			first_used_by TEXT NOT NULL DEFAULT '',
			UNIQUE(blueprint_id, number)
		)`,
	}

	// Append-only: the audit record is never rewritten. Triggers make that
	// true for every path, including a raw SQL console.
	for _, table := range []string{"events", "transcripts", "artifacts"} {
		stmts = append(stmts,
			fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_no_update BEFORE UPDATE ON %s
				BEGIN SELECT RAISE(ABORT, '%s is append-only'); END`, table, table, table),
			fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s_no_delete BEFORE DELETE ON %s
				BEGIN SELECT RAISE(ABORT, '%s is append-only'); END`, table, table, table),
		)
	}

	// A v1 file has the tables without these columns; a fresh file (version
	// 0) gets them from the CREATE statements above, so only v1 is altered.
	// The sources table is new and created by the statement above either way.
	if current == 1 {
		stmts = append(stmts,
			`ALTER TABLE blueprints ADD COLUMN price TEXT`,
			`ALTER TABLE stage_runs ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE stage_runs ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE stage_runs ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE stage_runs ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0`,
		)
	}

	// One transaction: a failed or interrupted migration leaves the file
	// exactly as it was, never half-altered at the old version.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin schema: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: ensure schema: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return fmt.Errorf("store: stamp schema version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit schema: %w", err)
	}

	return nil
}

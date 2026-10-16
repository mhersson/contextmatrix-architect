package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Artifact is one versioned stage output.
type Artifact struct {
	BlueprintID string
	Stage       string
	Version     int
	Body        string
	Data        json.RawMessage
	CreatedAt   time.Time
}

// PutArtifact stores the next version of a stage's artifact and returns it.
func (s *Store) PutArtifact(ctx context.Context, a Artifact) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM artifacts
		WHERE blueprint_id = ? AND stage = ?`, a.BlueprintID, a.Stage).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: next artifact version: %w", err)
	}

	var data any
	if len(a.Data) > 0 {
		data = string(a.Data)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts (blueprint_id, stage, version, body, data, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, a.BlueprintID, a.Stage, version, a.Body, data, time.Now().UTC().Unix()); err != nil {
		return 0, fmt.Errorf("store: insert artifact: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit artifact: %w", err)
	}

	return version, nil
}

// LatestArtifact returns the newest version of a stage's artifact, or ErrNotFound.
func (s *Store) LatestArtifact(ctx context.Context, blueprintID, stage string) (Artifact, error) {
	var (
		a         Artifact
		data      sql.NullString
		createdAt int64
	)

	err := s.db.QueryRowContext(ctx, `SELECT blueprint_id, stage, version, body, data, created_at FROM artifacts
		WHERE blueprint_id = ? AND stage = ? ORDER BY version DESC LIMIT 1`, blueprintID, stage).
		Scan(&a.BlueprintID, &a.Stage, &a.Version, &a.Body, &data, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, ErrNotFound
	}

	if err != nil {
		return Artifact{}, fmt.Errorf("store: latest artifact: %w", err)
	}

	if data.Valid {
		a.Data = json.RawMessage(data.String)
	}

	a.CreatedAt = time.Unix(createdAt, 0).UTC()

	return a, nil
}

// ListArtifacts returns every artifact of a blueprint, every stage and
// version, oldest first.
func (s *Store) ListArtifacts(ctx context.Context, blueprintID string) ([]Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blueprint_id, stage, version, body, data, created_at FROM artifacts
		WHERE blueprint_id = ? ORDER BY id`, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("store: list artifacts: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []Artifact

	for rows.Next() {
		var (
			a         Artifact
			data      sql.NullString
			createdAt int64
		)

		if err := rows.Scan(&a.BlueprintID, &a.Stage, &a.Version, &a.Body, &data, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan artifact: %w", err)
		}

		if data.Valid {
			a.Data = json.RawMessage(data.String)
		}

		a.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, a)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list artifacts: %w", err)
	}

	return out, nil
}

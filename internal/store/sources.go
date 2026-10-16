package store

import (
	"context"
	"fmt"
	"time"
)

// Source is something external a blueprint read: a repository at a sha, a
// url, a file. Tool results carry its tag so claims can cite it.
type Source struct {
	BlueprintID string
	Number      int
	Kind        string
	Identity    string
	Version     string
	FetchedAt   time.Time
	FirstUsedBy string
}

// Tag is the citation form, S1, S2, ... per blueprint.
func (s Source) Tag() string { return fmt.Sprintf("S%d", s.Number) }

// AddSource stores src with the next number for its blueprint and returns it.
func (s *Store) AddSource(ctx context.Context, src Source) (Source, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Source{}, fmt.Errorf("store: begin: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number), 0) + 1 FROM sources WHERE blueprint_id = ?`,
		src.BlueprintID).Scan(&src.Number); err != nil {
		return Source{}, fmt.Errorf("store: next source number: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO sources (blueprint_id, number, kind, identity, version, fetched_at, first_used_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, src.BlueprintID, src.Number, src.Kind, src.Identity, src.Version,
		src.FetchedAt.Unix(), src.FirstUsedBy); err != nil {
		return Source{}, fmt.Errorf("store: insert source: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Source{}, fmt.Errorf("store: commit source: %w", err)
	}

	return src, nil
}

// ListSources returns a blueprint's sources in number order.
func (s *Store) ListSources(ctx context.Context, blueprintID string) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blueprint_id, number, kind, identity, version, fetched_at, first_used_by
		FROM sources WHERE blueprint_id = ? ORDER BY number`, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("store: list sources: %w", err)
	}

	defer rows.Close()

	var out []Source

	for rows.Next() {
		var (
			src       Source
			fetchedAt int64
		)

		if err := rows.Scan(&src.BlueprintID, &src.Number, &src.Kind, &src.Identity, &src.Version, &fetchedAt, &src.FirstUsedBy); err != nil {
			return nil, fmt.Errorf("store: scan source: %w", err)
		}

		src.FetchedAt = time.Unix(fetchedAt, 0).UTC()
		out = append(out, src)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sources: %w", err)
	}

	return out, nil
}

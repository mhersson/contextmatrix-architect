package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Emission kinds.
const (
	EmitCard     = "card"
	EmitPlaybook = "playbook"
	EmitRun      = "run"
)

// Emission is one thing emission created in CM: a card for a draft id, the
// playbook, or the start of a run.
type Emission struct {
	BlueprintID string
	Kind        string
	DraftID     string
	Project     string
	CMID        string
	CreatedAt   time.Time
}

// ErrDuplicate is returned when an emission for the same blueprint, kind and draft id exists.
var ErrDuplicate = errors.New("store: duplicate")

// AddEmission records something emission created in CM. A second emission
// for the same blueprint, kind and draft id is ErrDuplicate, so a retried
// emission can never record two CM ids for one draft.
func (s *Store) AddEmission(ctx context.Context, e Emission) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO emissions (blueprint_id, kind, draft_id, project, cm_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, e.BlueprintID, e.Kind, e.DraftID, e.Project, e.CMID, e.CreatedAt.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return fmt.Errorf("store: emission %s %s of %s: %w", e.Kind, e.DraftID, e.BlueprintID, ErrDuplicate)
		}

		return fmt.Errorf("store: insert emission: %w", err)
	}

	return nil
}

// ListEmissions returns a blueprint's emissions, oldest first.
func (s *Store) ListEmissions(ctx context.Context, blueprintID string) ([]Emission, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blueprint_id, kind, draft_id, project, cm_id, created_at
		FROM emissions WHERE blueprint_id = ? ORDER BY id`, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("store: list emissions: %w", err)
	}

	defer rows.Close()

	var out []Emission

	for rows.Next() {
		var (
			e         Emission
			createdAt int64
		)

		if err := rows.Scan(&e.BlueprintID, &e.Kind, &e.DraftID, &e.Project, &e.CMID, &createdAt); err != nil {
			return nil, fmt.Errorf("store: scan emission: %w", err)
		}

		e.CreatedAt = time.Unix(createdAt, 0).UTC()
		out = append(out, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list emissions: %w", err)
	}

	return out, nil
}

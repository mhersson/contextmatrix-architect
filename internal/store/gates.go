package store

import (
	"context"
	"fmt"
	"time"
)

// Gate decisions and responders.
const (
	GateApprove = "approve"
	GateReject  = "reject"
	// GateFold records that a stage's artifact folded into a later gate,
	// which judges it; its feedback names that gate.
	GateFold = "fold"
	// GateSuperseded records the version a gate judged when the human
	// rejected a stage folded into it, which runs again with everything
	// after it; its feedback names that stage. It decides nothing.
	GateSuperseded = "superseded"
	ResponderHuman = "human"
	ResponderAuto  = "auto"
)

// Gate is one decision at a gate against the version of the artifact it judged.
type Gate struct {
	ID          int64
	BlueprintID string
	Stage       string
	Version     int
	Decision    string
	Feedback    string
	Responder   string
	DecidedAt   time.Time
}

// AddGate records a decision; gates are append-only.
func (s *Store) AddGate(ctx context.Context, g Gate) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO gates
		(blueprint_id, stage, version, decision, feedback, responder, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		g.BlueprintID, g.Stage, g.Version, g.Decision, g.Feedback, g.Responder, g.DecidedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: insert gate: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: gate id: %w", err)
	}

	return id, nil
}

// ListGates returns a blueprint's decisions at stage, oldest first.
func (s *Store) ListGates(ctx context.Context, blueprintID, stage string) ([]Gate, error) {
	return s.listGates(ctx, `SELECT id, blueprint_id, stage, version, decision, feedback, responder, decided_at
		FROM gates WHERE blueprint_id = ? AND stage = ? ORDER BY id`, blueprintID, stage)
}

// AllGates returns every decision of a blueprint, at every gate, oldest
// first.
func (s *Store) AllGates(ctx context.Context, blueprintID string) ([]Gate, error) {
	return s.listGates(ctx, `SELECT id, blueprint_id, stage, version, decision, feedback, responder, decided_at
		FROM gates WHERE blueprint_id = ? ORDER BY id`, blueprintID)
}

func (s *Store) listGates(ctx context.Context, query string, args ...any) ([]Gate, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list gates: %w", err)
	}

	defer rows.Close()

	var out []Gate

	for rows.Next() {
		var (
			g         Gate
			decidedAt int64
		)

		if err := rows.Scan(&g.ID, &g.BlueprintID, &g.Stage, &g.Version, &g.Decision, &g.Feedback, &g.Responder, &decidedAt); err != nil {
			return nil, fmt.Errorf("store: scan gate: %w", err)
		}

		g.DecidedAt = time.Unix(decidedAt, 0).UTC()
		out = append(out, g)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list gates: %w", err)
	}

	return out, nil
}

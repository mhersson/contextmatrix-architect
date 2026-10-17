package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Event is one row of the append-only audit log.
type Event struct {
	ID          int64
	Time        time.Time
	BlueprintID string
	RunID       int64
	Kind        string
	Payload     json.RawMessage
}

// AppendEvent writes one event.
func (s *Store) AppendEvent(ctx context.Context, e Event) error {
	var payload any
	if len(e.Payload) > 0 {
		payload = string(e.Payload)
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO events (ts, blueprint_id, run_id, kind, payload)
		VALUES (?, ?, ?, ?, ?)`, e.Time.UnixMilli(), e.BlueprintID, e.RunID, e.Kind, payload); err != nil {
		return fmt.Errorf("store: append event: %w", err)
	}

	return nil
}

// ListEvents returns a blueprint's events oldest first.
func (s *Store) ListEvents(ctx context.Context, blueprintID string) ([]Event, error) {
	return s.listEvents(ctx, `SELECT id, ts, blueprint_id, run_id, kind, payload
		FROM events WHERE blueprint_id = ? ORDER BY id`, blueprintID)
}

// ListEventsOfKind returns a blueprint's events of kind, oldest first.
func (s *Store) ListEventsOfKind(ctx context.Context, blueprintID, kind string) ([]Event, error) {
	return s.listEvents(ctx, `SELECT id, ts, blueprint_id, run_id, kind, payload
		FROM events WHERE blueprint_id = ? AND kind = ? ORDER BY id`, blueprintID, kind)
}

func (s *Store) listEvents(ctx context.Context, query string, args ...any) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}

	defer rows.Close()

	var out []Event

	for rows.Next() {
		var (
			e       Event
			ts      int64
			payload sql.NullString
		)

		if err := rows.Scan(&e.ID, &ts, &e.BlueprintID, &e.RunID, &e.Kind, &payload); err != nil {
			return nil, fmt.Errorf("store: scan event: %w", err)
		}

		e.Time = time.UnixMilli(ts).UTC()

		if payload.Valid {
			e.Payload = json.RawMessage(payload.String)
		}

		out = append(out, e)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}

	return out, nil
}

// LastEventID returns the id of a blueprint's newest event, 0 when it has
// none. Ids rise across restarts, so the view's sequence is this id.
func (s *Store) LastEventID(ctx context.Context, blueprintID string) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM events WHERE blueprint_id = ?`,
		blueprintID).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: last event id: %w", err)
	}

	return id, nil
}

// CountEvents counts a blueprint's events of kind.
func (s *Store) CountEvents(ctx context.Context, blueprintID, kind string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE blueprint_id = ? AND kind = ?`,
		blueprintID, kind).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count events: %w", err)
	}

	return n, nil
}

// LatestEvent returns a blueprint's newest event of kind, or ErrNotFound.
func (s *Store) LatestEvent(ctx context.Context, blueprintID, kind string) (Event, error) {
	var (
		e       Event
		ts      int64
		payload sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `SELECT id, ts, blueprint_id, run_id, kind, payload
		FROM events WHERE blueprint_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, blueprintID, kind).
		Scan(&e.ID, &ts, &e.BlueprintID, &e.RunID, &e.Kind, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}

	if err != nil {
		return Event{}, fmt.Errorf("store: latest event: %w", err)
	}

	e.Time = time.UnixMilli(ts).UTC()

	if payload.Valid {
		e.Payload = json.RawMessage(payload.String)
	}

	return e, nil
}

// PutTranscript stores a run's full message history once.
func (s *Store) PutTranscript(ctx context.Context, runID int64, messages json.RawMessage) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO transcripts (run_id, messages, created_at) VALUES (?, ?, ?)`,
		runID, string(messages), time.Now().UTC().Unix()); err != nil {
		return fmt.Errorf("store: put transcript %d: %w", runID, err)
	}

	return nil
}

// GetTranscript returns a run's stored messages, or ErrNotFound.
func (s *Store) GetTranscript(ctx context.Context, runID int64) (json.RawMessage, error) {
	var messages string

	err := s.db.QueryRowContext(ctx, `SELECT messages FROM transcripts WHERE run_id = ?`, runID).Scan(&messages)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("store: get transcript %d: %w", runID, err)
	}

	return json.RawMessage(messages), nil
}

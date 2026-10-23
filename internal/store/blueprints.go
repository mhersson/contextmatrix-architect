package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	protocol "github.com/mhersson/contextmatrix-protocol"
)

// Blueprint is one need being turned into cards. Its id is the CM session id.
type Blueprint struct {
	ID         string
	Title      string
	Projects   []string
	Mode       string
	CostCapUSD float64
	Model      string
	// Price is the session model's resolved per-token price as CM sent it at
	// creation; nil when CM had none.
	Price *protocol.ModelPrice
	// ContextWindow is the session model's context length as CM sent it at
	// creation; 0 when CM did not know it.
	ContextWindow int
	// PlainLanguage is set once, at creation, and never changes.
	PlainLanguage bool
	State         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// CreateBlueprint inserts a blueprint and fixes its lesson set: every active
// lesson scoped to the pack or to one of its projects. A duplicate id is an error.
func (s *Store) CreateBlueprint(ctx context.Context, b Blueprint) error {
	projects, err := json.Marshal(b.Projects)
	if err != nil {
		return fmt.Errorf("store: marshal projects: %w", err)
	}

	var price any

	if b.Price != nil {
		raw, err := json.Marshal(b.Price)
		if err != nil {
			return fmt.Errorf("store: marshal price: %w", err)
		}

		price = string(raw)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin create blueprint %s: %w", b.ID, err)
	}

	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `INSERT INTO blueprints
		(id, title, projects, mode, cost_cap_usd, model, price, context_window, plain_language, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.Title, string(projects), b.Mode, b.CostCapUSD, b.Model, price, b.ContextWindow, b.PlainLanguage, b.State,
		b.CreatedAt.Unix(), b.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("store: insert blueprint %s: %w", b.ID, err)
	}

	// The set is read and written in this transaction, so a later state change
	// of a lesson cannot fall between the two.
	cover := "scope = 'pack'"
	args := []any{b.ID}

	if len(b.Projects) > 0 {
		cover += " OR (scope = 'project' AND project IN (?" + strings.Repeat(", ?", len(b.Projects)-1) + "))"

		for _, p := range b.Projects {
			args = append(args, p)
		}
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO blueprint_lessons (blueprint_id, lesson_id)
		SELECT ?, id FROM lessons WHERE state = 'active' AND (`+cover+`) ORDER BY id`, args...)
	if err != nil {
		return fmt.Errorf("store: take lessons for blueprint %s: %w", b.ID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit create blueprint %s: %w", b.ID, err)
	}

	return nil
}

// GetBlueprint returns the blueprint with id, or ErrNotFound.
func (s *Store) GetBlueprint(ctx context.Context, id string) (Blueprint, error) {
	var (
		b                    Blueprint
		projects             string
		price                sql.NullString
		createdAt, updatedAt int64
	)

	err := s.db.QueryRowContext(ctx, `SELECT id, title, projects, mode, cost_cap_usd, model, price, context_window, plain_language, state, created_at, updated_at
		FROM blueprints WHERE id = ?`, id).
		Scan(&b.ID, &b.Title, &projects, &b.Mode, &b.CostCapUSD, &b.Model, &price, &b.ContextWindow, &b.PlainLanguage, &b.State, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Blueprint{}, ErrNotFound
	}

	if err != nil {
		return Blueprint{}, fmt.Errorf("store: get blueprint %s: %w", id, err)
	}

	if err := json.Unmarshal([]byte(projects), &b.Projects); err != nil {
		return Blueprint{}, fmt.Errorf("store: decode projects of %s: %w", id, err)
	}

	if price.Valid {
		var p protocol.ModelPrice
		if err := json.Unmarshal([]byte(price.String), &p); err != nil {
			return Blueprint{}, fmt.Errorf("store: decode price of %s: %w", id, err)
		}

		b.Price = &p
	}

	b.CreatedAt = time.Unix(createdAt, 0).UTC()
	b.UpdatedAt = time.Unix(updatedAt, 0).UTC()

	return b, nil
}

// SetBlueprintState records a state change and bumps updated_at.
func (s *Store) SetBlueprintState(ctx context.Context, id, state string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE blueprints SET state = ?, updated_at = ? WHERE id = ?`,
		state, time.Now().UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: set state of %s: %w", id, err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// SetBlueprintMode records a new mode and bumps updated_at.
func (s *Store) SetBlueprintMode(ctx context.Context, id, mode string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE blueprints SET mode = ?, updated_at = ? WHERE id = ?`,
		mode, time.Now().UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: set the mode of %s: %w", id, err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// SetBlueprintCostCap records a new cost cap, 0 for none, and bumps
// updated_at.
func (s *Store) SetBlueprintCostCap(ctx context.Context, id string, capUSD float64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE blueprints SET cost_cap_usd = ?, updated_at = ? WHERE id = ?`,
		capUSD, time.Now().UTC().Unix(), id)
	if err != nil {
		return fmt.Errorf("store: set the cost cap of %s: %w", id, err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

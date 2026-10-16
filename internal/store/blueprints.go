package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	Price     *protocol.ModelPrice
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateBlueprint inserts a blueprint; a duplicate id is an error.
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

	_, err = s.db.ExecContext(ctx, `INSERT INTO blueprints
		(id, title, projects, mode, cost_cap_usd, model, price, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.Title, string(projects), b.Mode, b.CostCapUSD, b.Model, price, b.State,
		b.CreatedAt.Unix(), b.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("store: insert blueprint %s: %w", b.ID, err)
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

	err := s.db.QueryRowContext(ctx, `SELECT id, title, projects, mode, cost_cap_usd, model, price, state, created_at, updated_at
		FROM blueprints WHERE id = ?`, id).
		Scan(&b.ID, &b.Title, &projects, &b.Mode, &b.CostCapUSD, &b.Model, &price, &b.State, &createdAt, &updatedAt)
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

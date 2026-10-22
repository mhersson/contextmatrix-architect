package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Run statuses.
const (
	RunRunning     = "running"
	RunDone        = "done"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"
	RunCanceled    = "canceled"
	// RunAsked is a stage run that ended by posting a question round, or by
	// answering an open round's chat; the stage goes on in a later run.
	RunAsked = "asked"
)

// Run kinds. A stage run produces a stage's artifact; a reviewer run judges
// one before its gate; a responder run answers a question inside the asking
// stage's run; a retro run judges a blueprint's emitted cards and places
// nothing. Only stage runs place the pipeline.
const (
	RunKindStage     = "stage"
	RunKindReviewer  = "reviewer"
	RunKindResponder = "responder"
	RunKindRetro     = "retro"
)

// Run is one harness run of one stage.
type Run struct {
	ID          int64
	BlueprintID string
	Stage       string
	Kind        string
	Attempt     int
	Round       int
	Status      string
	Model       string
	StartedAt   time.Time
	EndedAt     *time.Time
	Usage       RunUsage
	Reason      string
}

// RunUsage is what a run consumed: the four token buckets the harness
// reports and the cost the ledger recorded for them.
type RunUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
}

// StartRun inserts a run and returns its id. A run without a kind is a stage
// run.
func (s *Store) StartRun(ctx context.Context, r Run) (int64, error) {
	kind := r.Kind
	if kind == "" {
		kind = RunKindStage
	}

	res, err := s.db.ExecContext(ctx, `INSERT INTO stage_runs
		(blueprint_id, stage, kind, attempt, round, status, model, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.BlueprintID, r.Stage, kind, r.Attempt, r.Round, r.Status, r.Model, r.StartedAt.Unix())
	if err != nil {
		return 0, fmt.Errorf("store: insert run: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: run id: %w", err)
	}

	return id, nil
}

// EndRun records how a run ended and what it consumed.
func (s *Store) EndRun(ctx context.Context, id int64, status, reason string, usage RunUsage, endedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE stage_runs SET status = ?, reason = ?, cost_usd = ?, ended_at = ?,
		prompt_tokens = ?, completion_tokens = ?, cache_read_tokens = ?, cache_write_tokens = ? WHERE id = ?`,
		status, reason, usage.CostUSD, endedAt.Unix(),
		usage.PromptTokens, usage.CompletionTokens, usage.CacheReadTokens, usage.CacheWriteTokens, id)
	if err != nil {
		return fmt.Errorf("store: end run %d: %w", id, err)
	}

	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}

	return nil
}

// RunModel returns the model a run ran on.
func (s *Store) RunModel(ctx context.Context, id int64) (string, error) {
	var model string

	err := s.db.QueryRowContext(ctx, `SELECT model FROM stage_runs WHERE id = ?`, id).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}

	if err != nil {
		return "", fmt.Errorf("store: run %d model: %w", id, err)
	}

	return model, nil
}

// ListRuns returns a blueprint's runs oldest first.
func (s *Store) ListRuns(ctx context.Context, blueprintID string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, blueprint_id, stage, kind, attempt, round, status, model,
		started_at, ended_at, cost_usd, reason,
		prompt_tokens, completion_tokens, cache_read_tokens, cache_write_tokens FROM stage_runs WHERE blueprint_id = ? ORDER BY id`, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}

	defer rows.Close()

	var out []Run

	for rows.Next() {
		var (
			r         Run
			startedAt int64
			endedAt   sql.NullInt64
		)

		if err := rows.Scan(&r.ID, &r.BlueprintID, &r.Stage, &r.Kind, &r.Attempt, &r.Round, &r.Status, &r.Model,
			&startedAt, &endedAt, &r.Usage.CostUSD, &r.Reason,
			&r.Usage.PromptTokens, &r.Usage.CompletionTokens, &r.Usage.CacheReadTokens, &r.Usage.CacheWriteTokens); err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}

		r.StartedAt = time.Unix(startedAt, 0).UTC()

		if endedAt.Valid {
			t := time.Unix(endedAt.Int64, 0).UTC()
			r.EndedAt = &t
		}

		out = append(out, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}

	return out, nil
}

// BlueprintCost sums every run's cost for a blueprint.
func (s *Store) BlueprintCost(ctx context.Context, blueprintID string) (float64, error) {
	var cost float64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(cost_usd), 0) FROM stage_runs WHERE blueprint_id = ?`,
		blueprintID).Scan(&cost); err != nil {
		return 0, fmt.Errorf("store: blueprint cost: %w", err)
	}

	return cost, nil
}

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Question is one answered question: what a stage asked, who answered and
// how. It is written once, when the answer exists, and never changed: a
// later verdict on an auto answer is an artifact of the stage that judged it.
type Question struct {
	BlueprintID string
	RunID       int64
	Stage       string
	Number      int
	Text        string
	Options     []string
	Context     string
	Answer      string
	Responder   string
	// Assumption marks an answer no human gave: the auto responder's.
	// Grounded says the responder found evidence for it.
	Assumption bool
	Grounded   bool
	MessageID  string
	AnsweredAt time.Time
}

// Tag is the citation form, Q1, Q2, ... per blueprint.
func (q Question) Tag() string { return fmt.Sprintf("Q%d", q.Number) }

// AddQuestion records an answered question; a number is answered once per
// blueprint.
func (s *Store) AddQuestion(ctx context.Context, q Question) error {
	options := []byte("[]")

	if len(q.Options) > 0 {
		raw, err := json.Marshal(q.Options)
		if err != nil {
			return fmt.Errorf("store: marshal options: %w", err)
		}

		options = raw
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO questions
		(blueprint_id, run_id, stage, number, text, options, context, answer, responder, assumption, grounded, message_id, answered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		q.BlueprintID, q.RunID, q.Stage, q.Number, q.Text, string(options), q.Context, q.Answer, q.Responder,
		q.Assumption, q.Grounded, q.MessageID, q.AnsweredAt.Unix()); err != nil {
		return fmt.Errorf("store: insert question %s: %w", q.Tag(), err)
	}

	return nil
}

// ListQuestions returns a blueprint's answered questions in number order.
func (s *Store) ListQuestions(ctx context.Context, blueprintID string) ([]Question, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT blueprint_id, run_id, stage, number, text, options, context, answer,
		responder, assumption, grounded, message_id, answered_at FROM questions WHERE blueprint_id = ? ORDER BY number`, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("store: list questions: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var out []Question

	for rows.Next() {
		var (
			q                    Question
			options              string
			assumption, grounded int64
			answeredAt           int64
		)

		if err := rows.Scan(&q.BlueprintID, &q.RunID, &q.Stage, &q.Number, &q.Text, &options, &q.Context, &q.Answer,
			&q.Responder, &assumption, &grounded, &q.MessageID, &answeredAt); err != nil {
			return nil, fmt.Errorf("store: scan question: %w", err)
		}

		if err := json.Unmarshal([]byte(options), &q.Options); err != nil {
			return nil, fmt.Errorf("store: decode the options of Q%d: %w", q.Number, err)
		}

		if len(q.Options) == 0 {
			q.Options = nil
		}

		q.Assumption, q.Grounded = assumption != 0, grounded != 0
		q.AnsweredAt = time.Unix(answeredAt, 0).UTC()
		out = append(out, q)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list questions: %w", err)
	}

	return out, nil
}

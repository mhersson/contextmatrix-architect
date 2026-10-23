package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Lesson states. A lesson is added proposed and decided once.
const (
	LessonProposed = "proposed"
	LessonActive   = "active"
	LessonRejected = "rejected"
	LessonRetired  = "retired"
)

// Lesson scopes.
const (
	ScopePack    = "pack"
	ScopeProject = "project"
)

// Lesson is one proposed rule a retro pass drew from a blueprint's emitted cards.
type Lesson struct {
	ID          int64
	BlueprintID string // the blueprint whose pass proposed it
	RunID       int64  // the retro run
	State       string // LessonProposed ... LessonRetired
	Scope       string // ScopePack or ScopeProject
	Project     string // the evidence card's project; read only for ScopeProject
	Text        string
	Rationale   string
	Evidence    []string        // CM card ids
	Overlay     json.RawMessage // a contract.Overlay as JSON; the store reads no pack code
	CreatedAt   time.Time
	DecidedAt   *time.Time
	DecidedBy   string
}

// Tag is the id an operator types and reads: "L<id>".
func (l Lesson) Tag() string { return "L" + strconv.FormatInt(l.ID, 10) }

// LessonDecision is an operator's answer to a proposed lesson.
type LessonDecision struct {
	State, Scope, Text, By string
	At                     time.Time
}

// AddLessons stores ls as proposed in one transaction and returns them with
// their ids, in input order. Any State on the input is ignored.
func (s *Store) AddLessons(ctx context.Context, ls []Lesson) ([]Lesson, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("store: begin add lessons: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	out := make([]Lesson, 0, len(ls))

	for _, l := range ls {
		if l.CreatedAt.IsZero() {
			l.CreatedAt = time.Now().UTC()
		}

		evidence, err := json.Marshal(nonNilStrings(l.Evidence))
		if err != nil {
			return nil, fmt.Errorf("store: encode lesson evidence: %w", err)
		}

		overlay := l.Overlay
		if len(overlay) == 0 {
			overlay = json.RawMessage(`{}`)
		}

		res, err := tx.ExecContext(ctx, `INSERT INTO lessons
			(blueprint_id, run_id, state, scope, project, text, rationale, evidence, overlay, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			l.BlueprintID, l.RunID, LessonProposed, l.Scope, l.Project, l.Text, l.Rationale,
			string(evidence), string(overlay), l.CreatedAt.Unix())
		if err != nil {
			return nil, fmt.Errorf("store: insert lesson: %w", err)
		}

		l.ID, err = res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("store: lesson id: %w", err)
		}

		l.State = LessonProposed
		l.Evidence = nonNilStrings(l.Evidence)
		l.Overlay = overlay
		out = append(out, l)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("store: commit add lessons: %w", err)
	}

	return out, nil
}

const lessonColumns = `id, blueprint_id, run_id, state, scope, project, text, rationale, evidence, overlay,
	created_at, decided_at, decided_by`

// GetLesson returns the lesson with id, or ErrNotFound.
func (s *Store) GetLesson(ctx context.Context, id int64) (Lesson, error) {
	l, err := scanLesson(s.db.QueryRowContext(ctx, `SELECT `+lessonColumns+` FROM lessons WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Lesson{}, ErrNotFound
	}

	if err != nil {
		return Lesson{}, fmt.Errorf("store: get lesson %d: %w", id, err)
	}

	return l, nil
}

// ListLessons returns every lesson in id order.
func (s *Store) ListLessons(ctx context.Context) ([]Lesson, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+lessonColumns+` FROM lessons ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list lessons: %w", err)
	}

	defer rows.Close()

	var out []Lesson

	for rows.Next() {
		l, err := scanLesson(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan lesson: %w", err)
		}

		out = append(out, l)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list lessons: %w", err)
	}

	return out, nil
}

// BlueprintLessons returns the lessons fixed into a blueprint at its creation,
// in id order, each in the state it has now. A blueprint with none gets nil.
func (s *Store) BlueprintLessons(ctx context.Context, blueprintID string) ([]Lesson, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+lessonColumns+` FROM lessons
		WHERE id IN (SELECT lesson_id FROM blueprint_lessons WHERE blueprint_id = ?) ORDER BY id`, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("store: blueprint %s lessons: %w", blueprintID, err)
	}

	defer rows.Close()

	var out []Lesson

	for rows.Next() {
		l, err := scanLesson(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan lesson: %w", err)
		}

		out = append(out, l)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: blueprint %s lessons: %w", blueprintID, err)
	}

	return out, nil
}

// DecideLesson records d on a proposed lesson. It reports false, with no
// change, when the lesson is missing or already decided.
func (s *Store) DecideLesson(ctx context.Context, id int64, d LessonDecision) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE lessons
		SET state = ?, scope = ?, text = ?, decided_at = ?, decided_by = ?
		WHERE id = ? AND state = ?`,
		d.State, d.Scope, d.Text, d.At.Unix(), d.By, id, LessonProposed)
	if err != nil {
		return false, fmt.Errorf("store: decide lesson %d: %w", id, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: decide lesson %d: %w", id, err)
	}

	return n == 1, nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanLesson(r rowScanner) (Lesson, error) {
	var (
		l         Lesson
		evidence  string
		overlay   string
		createdAt int64
		decidedAt sql.NullInt64
	)

	if err := r.Scan(&l.ID, &l.BlueprintID, &l.RunID, &l.State, &l.Scope, &l.Project, &l.Text, &l.Rationale,
		&evidence, &overlay, &createdAt, &decidedAt, &l.DecidedBy); err != nil {
		return Lesson{}, err //nolint:wrapcheck // callers wrap with the operation
	}

	if err := json.Unmarshal([]byte(evidence), &l.Evidence); err != nil {
		return Lesson{}, fmt.Errorf("decode evidence: %w", err)
	}

	l.Overlay = json.RawMessage(overlay)
	l.CreatedAt = time.Unix(createdAt, 0).UTC()

	if decidedAt.Valid {
		t := time.Unix(decidedAt.Int64, 0).UTC()
		l.DecidedAt = &t
	}

	return l, nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}

	return s
}

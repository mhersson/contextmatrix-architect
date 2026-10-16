// Package store is the architect's record: SQLite, one file, with events,
// transcripts and artifacts append-only by trigger.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // database/sql driver
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies the schema.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?" + url.Values{
		"_pragma": {"journal_mode(WAL)", "foreign_keys(1)", "busy_timeout(5000)"},
	}.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	if err := ensureSchema(ctx, db); err != nil {
		_ = db.Close()

		return nil, err
	}

	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

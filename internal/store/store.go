// Package store is the architect's record: SQLite, one file, with events,
// transcripts and artifacts append-only by trigger.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite" // database/sql driver
)

var uriPath = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies the schema.
func Open(ctx context.Context, path string) (*Store, error) {
	// recursive_triggers makes the implicit delete of INSERT OR REPLACE fire
	// the append-only delete triggers; SQLite skips them otherwise.
	// A '%', '?' or '#' in a URI filename is decoded or cuts it, so the path is escaped.
	dsn := "file:" + uriPath.Replace(path) + "?" + url.Values{
		"_pragma": {"journal_mode(WAL)", "foreign_keys(1)", "busy_timeout(5000)", "recursive_triggers(1)"},
	}.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	if err := ensureSchema(ctx, db); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

package store

import (
	"context"
	"database/sql"
	"errors"
)

// InsertWebSession stores a browser session row as it is, for the tests
// that name their sessions.
func (s *Store) InsertWebSession(ctx context.Context, w WebSession) error {
	_, err := s.q.insertWebSession(ctx, w)
	return err
}

// SessionLive reports whether the browser session id is live at now.
func (s *Store) SessionLive(ctx context.Context, id string, now int64) (bool, error) {
	_, err := s.q.liveWebSession(ctx, id, now)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ExecForTest runs a statement without arguments on the database, for the
// tests that store rows the queries never write (rows from before authlume).
func (s *Store) ExecForTest(ctx context.Context, query string) error {
	_, err := s.db.ExecContext(ctx, query)
	return err
}

// CountForTest returns the result of a SELECT COUNT(*) statement.
func (s *Store) CountForTest(ctx context.Context, query string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, query).Scan(&n)
	return n, err
}

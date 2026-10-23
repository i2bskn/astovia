package store

import (
	"context"
	"database/sql"
	"github.com/i2bskn/astovia/internal/store/postgresdb"
	"github.com/i2bskn/astovia/internal/store/sqlitedb"
)

type dbtx interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

// bind returns the driver adapter for conn, which is either the pool or a
// transaction started from it.
func (s *Store) bind(conn dbtx) queries {
	if s.driver == "postgres" {
		q := postgresdb.New(s.db)
		if tx, ok := conn.(*sql.Tx); ok {
			q = q.WithTx(tx)
		}
		return postgresQueries{q}
	}
	q := sqlitedb.New(s.db)
	if tx, ok := conn.(*sql.Tx); ok {
		q = q.WithTx(tx)
	}
	return sqliteQueries{q}
}

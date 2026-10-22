// Package testutil creates isolated disposable databases for contract tests.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/i2bskn/astovia/db"
	"github.com/i2bskn/astovia/internal/store"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func Database(t *testing.T, driver string) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	var dsn string
	if driver == "sqlite" {
		dsn = "file:" + filepath.Join(t.TempDir(), "test.db")
		raw, e := sql.Open("sqlite", dsn)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = raw.ExecContext(ctx, db.SQLiteSQL); e != nil {
			t.Fatal(e)
		}
		if e = raw.Close(); e != nil {
			t.Fatal(e)
		}
	} else {
		base := os.Getenv("ASTOVIA_TEST_POSTGRES")
		if base == "" {
			t.Skip("ASTOVIA_TEST_POSTGRES is not set")
		}
		u, e := url.Parse(base)
		if e != nil {
			t.Fatal(e)
		}
		raw, e := sql.Open("pgx", base)
		if e != nil {
			t.Fatal(e)
		}
		name := fmt.Sprintf("test_%d", time.Now().UnixNano())
		if _, e = raw.ExecContext(ctx, `CREATE SCHEMA "`+name+`"`); e != nil {
			_ = raw.Close()
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if _, e := raw.ExecContext(context.Background(), `DROP SCHEMA "`+name+`" CASCADE`); e != nil {
				t.Error(e)
			}
			if e := raw.Close(); e != nil {
				t.Error(e)
			}
		})
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		dsn = u.String()
		isolated, e := sql.Open("pgx", dsn)
		if e != nil {
			t.Fatal(e)
		}
		// Generated DDL is unqualified; create it only in the explicit test schema.
		if _, e = isolated.ExecContext(ctx, db.PostgresSQL); e != nil {
			_ = isolated.Close()
			t.Fatal(e)
		}
		if e = isolated.Close(); e != nil {
			t.Fatal(e)
		}

	}
	s, e := store.Open(ctx, driver, dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	if e = s.CheckSchema(ctx); e != nil {
		t.Fatal(e)
	}
	return s, dsn
}

package store_test

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/i2bskn/astovia/internal/testutil"
)

// schemaShape is the part of a schema that must match between SQLite and
// PostgreSQL. Column types are not compared: integer and bigint differ on
// purpose.
type schemaShape struct {
	Tables map[string]tableShape
}

type tableShape struct {
	// Columns maps a column name to whether it is NOT NULL.
	Columns     map[string]bool
	PrimaryKey  []string
	Indexes     map[string]indexShape
	ForeignKeys []foreignKeyShape
}

type indexShape struct {
	Unique  bool
	Columns []string
}

type foreignKeyShape struct {
	Columns    []string
	RefTable   string
	RefColumns []string
	OnDelete   string
	OnUpdate   string
}

func (f foreignKeyShape) String() string {
	return fmt.Sprintf("%v->%s%v/%s/%s", f.Columns, f.RefTable, f.RefColumns, f.OnDelete, f.OnUpdate)
}

func sortForeignKeys(fks []foreignKeyShape) {
	sort.Slice(fks, func(i, j int) bool { return fks[i].String() < fks[j].String() })
}

func queryStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sqliteShape(t *testing.T, dsn string) schemaShape {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	shape := schemaShape{Tables: map[string]tableShape{}}
	for _, table := range queryStrings(t, db, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'") {
		ts := tableShape{Columns: map[string]bool{}, Indexes: map[string]indexShape{}}
		rows, err := db.QueryContext(ctx, "SELECT name, \"notnull\", pk FROM pragma_table_info(?)", table)
		if err != nil {
			t.Fatal(err)
		}
		pk := map[int]string{}
		for rows.Next() {
			var name string
			var notNull, position int
			if err = rows.Scan(&name, &notNull, &position); err != nil {
				t.Fatal(err)
			}
			ts.Columns[name] = notNull == 1
			if position > 0 {
				pk[position] = name
			}
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		for i := 1; i <= len(pk); i++ {
			ts.PrimaryKey = append(ts.PrimaryKey, pk[i])
		}

		rows, err = db.QueryContext(ctx, "SELECT name, \"unique\" FROM pragma_index_list(?) WHERE name NOT LIKE 'sqlite_autoindex_%'", table)
		if err != nil {
			t.Fatal(err)
		}
		unique := map[string]bool{}
		for rows.Next() {
			var name string
			var u int
			if err = rows.Scan(&name, &u); err != nil {
				t.Fatal(err)
			}
			unique[name] = u == 1
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		for name, u := range unique {
			cols := queryStrings(t, db, "SELECT name FROM pragma_index_info(?) ORDER BY seqno", name)
			ts.Indexes[name] = indexShape{Unique: u, Columns: cols}
		}

		rows, err = db.QueryContext(ctx, "SELECT id, \"table\", \"from\", \"to\", on_update, on_delete FROM pragma_foreign_key_list(?) ORDER BY id, seq", table)
		if err != nil {
			t.Fatal(err)
		}
		byID := map[int]*foreignKeyShape{}
		var ids []int
		for rows.Next() {
			var id int
			var ref, from, to, onUpdate, onDelete string
			if err = rows.Scan(&id, &ref, &from, &to, &onUpdate, &onDelete); err != nil {
				t.Fatal(err)
			}
			fk, ok := byID[id]
			if !ok {
				fk = &foreignKeyShape{RefTable: ref, OnDelete: onDelete, OnUpdate: onUpdate}
				byID[id] = fk
				ids = append(ids, id)
			}
			fk.Columns = append(fk.Columns, from)
			fk.RefColumns = append(fk.RefColumns, to)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		for _, id := range ids {
			ts.ForeignKeys = append(ts.ForeignKeys, *byID[id])
		}
		sortForeignKeys(ts.ForeignKeys)
		shape.Tables[table] = ts
	}
	return shape
}

// postgresShape reads only the isolated schema that testutil created; the DSN
// search_path points at it.
func postgresShape(t *testing.T, dsn string) schemaShape {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var schema string
	if err = db.QueryRowContext(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(schema, "test_") {
		t.Fatalf("not an isolated test schema: %q", schema)
	}
	shape := schemaShape{Tables: map[string]tableShape{}}
	for _, table := range queryStrings(t, db, "SELECT table_name FROM information_schema.tables WHERE table_schema = $1 AND table_type = 'BASE TABLE'", schema) {
		ts := tableShape{Columns: map[string]bool{}, Indexes: map[string]indexShape{}}
		rows, err := db.QueryContext(ctx, "SELECT column_name, is_nullable FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2", schema, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name, nullable string
			if err = rows.Scan(&name, &nullable); err != nil {
				t.Fatal(err)
			}
			ts.Columns[name] = nullable == "NO"
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()

		ts.PrimaryKey = queryStrings(t, db, `SELECT k.column_name FROM information_schema.table_constraints c
JOIN information_schema.key_column_usage k ON k.constraint_schema = c.constraint_schema AND k.constraint_name = c.constraint_name AND k.table_name = c.table_name
WHERE c.table_schema = $1 AND c.table_name = $2 AND c.constraint_type = 'PRIMARY KEY' ORDER BY k.ordinal_position`, schema, table)

		rows, err = db.QueryContext(ctx, `SELECT i.relname, x.indisunique, a.attname FROM pg_index x
JOIN pg_class t ON t.oid = x.indrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_class i ON i.oid = x.indexrelid
CROSS JOIN LATERAL unnest(x.indkey) WITH ORDINALITY AS k(attnum, ord)
JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
WHERE n.nspname = $1 AND t.relname = $2 AND NOT x.indisprimary
ORDER BY i.relname, k.ord`, schema, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var name, column string
			var unique bool
			if err = rows.Scan(&name, &unique, &column); err != nil {
				t.Fatal(err)
			}
			idx := ts.Indexes[name]
			idx.Unique = unique
			idx.Columns = append(idx.Columns, column)
			ts.Indexes[name] = idx
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()

		rows, err = db.QueryContext(ctx, `SELECT c.conname, rt.relname, a.attname, ra.attname, c.confdeltype, c.confupdtype FROM pg_constraint c
JOIN pg_class t ON t.oid = c.conrelid
JOIN pg_namespace n ON n.oid = t.relnamespace
JOIN pg_class rt ON rt.oid = c.confrelid
CROSS JOIN LATERAL unnest(c.conkey, c.confkey) WITH ORDINALITY AS k(attnum, refnum, ord)
JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
JOIN pg_attribute ra ON ra.attrelid = c.confrelid AND ra.attnum = k.refnum
WHERE c.contype = 'f' AND n.nspname = $1 AND t.relname = $2
ORDER BY c.conname, k.ord`, schema, table)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]*foreignKeyShape{}
		var names []string
		for rows.Next() {
			var name, ref, column, refColumn, onDelete, onUpdate string
			if err = rows.Scan(&name, &ref, &column, &refColumn, &onDelete, &onUpdate); err != nil {
				t.Fatal(err)
			}
			fk, ok := byName[name]
			if !ok {
				fk = &foreignKeyShape{RefTable: ref, OnDelete: postgresAction(t, onDelete), OnUpdate: postgresAction(t, onUpdate)}
				byName[name] = fk
				names = append(names, name)
			}
			fk.Columns = append(fk.Columns, column)
			fk.RefColumns = append(fk.RefColumns, refColumn)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		for _, name := range names {
			ts.ForeignKeys = append(ts.ForeignKeys, *byName[name])
		}
		sortForeignKeys(ts.ForeignKeys)
		shape.Tables[table] = ts
	}
	return shape
}

// postgresAction converts pg_constraint action codes to the names SQLite's
// pragma_foreign_key_list reports.
func postgresAction(t *testing.T, code string) string {
	t.Helper()
	switch code {
	case "a":
		return "NO ACTION"
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	}
	t.Fatalf("unknown foreign key action %q", code)
	return ""
}

func TestSchemaParity(t *testing.T) {
	shapes := map[string]schemaShape{}
	for _, driver := range []string{"sqlite", "postgres"} {
		// testutil skips the test when PostgreSQL is not configured.
		_, dsn := testutil.Database(t, driver)
		if driver == "sqlite" {
			shapes[driver] = sqliteShape(t, dsn)
		} else {
			shapes[driver] = postgresShape(t, dsn)
		}
	}
	if len(shapes["sqlite"].Tables) == 0 {
		t.Fatal("no tables found in SQLite")
	}
	if !reflect.DeepEqual(shapes["sqlite"], shapes["postgres"]) {
		t.Fatalf("schemas differ:\nsqlite:   %+v\npostgres: %+v", shapes["sqlite"], shapes["postgres"])
	}
}

var queryName = regexp.MustCompile(`^--\s*name:\s*(\S+)\s+(:\S+)\s*$`)

func queryNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var names []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "-- name:") {
			continue
		}
		m := queryName.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%s: malformed name line %q", path, line)
		}
		names = append(names, m[1]+" "+m[2])
	}
	if err = sc.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

func TestQueryParity(t *testing.T) {
	sqlite := queryNames(t, "../../db/sqlite/queries.sql")
	postgres := queryNames(t, "../../db/postgres/queries.sql")
	if len(sqlite) == 0 {
		t.Fatal("no queries found")
	}
	if !reflect.DeepEqual(sqlite, postgres) {
		t.Fatalf("queries differ:\nsqlite:   %v\npostgres: %v", sqlite, postgres)
	}
}

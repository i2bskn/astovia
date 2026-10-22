package db

import _ "embed"

// SQLiteSQL is generated from sqlite/schema.hcl by Atlas.
//
//go:embed sqlite/schema.sql
var SQLiteSQL string

// PostgresSQL is generated from postgres/schema.hcl by Atlas.
//
//go:embed postgres/schema.sql
var PostgresSQL string

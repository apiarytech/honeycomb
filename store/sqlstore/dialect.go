/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v2.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package sqlstore

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"sync"
)

// Dialect captures everything that differs between SQL databases. Adding a new
// database means implementing Dialect, shipping its migration scripts, and
// calling Register.
type Dialect interface {
	// Name is the dialect's canonical name, e.g. "postgres".
	Name() string
	// Placeholder returns the n-th (1-based) bind parameter, e.g. "?", "$1" or "@p1".
	Placeholder(n int) string
	// Upsert returns an insert-or-update statement for table. Bind parameters
	// follow the order of columns; keys are the primary-key columns.
	Upsert(table string, columns, keys []string) string
	// CreateMigrationsTable returns idempotent DDL for the schema-version table.
	CreateMigrationsTable(table string) string
	// Migrations holds NNNN_name.up.sql / NNNN_name.down.sql scripts at its root.
	Migrations() fs.FS
}

//go:embed migrations
var migrationsFS embed.FS

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Dialect)
)

func init() {
	Register(sqliteDialect{}, "sqlite3")
	Register(postgresDialect{}, "pgx", "postgresql")
	Register(mysqlDialect{}, "mariadb")
	Register(sqlserverDialect{}, "mssql")
}

// Register makes a dialect available to Lookup under its Name and any aliases,
// such as the database/sql driver names that speak it.
func Register(d Dialect, aliases ...string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	for _, name := range append([]string{d.Name()}, aliases...) {
		registry[strings.ToLower(name)] = d
	}
}

// Lookup returns the dialect registered under name (case-insensitive).
func Lookup(name string) (Dialect, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if d, ok := registry[strings.ToLower(name)]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("sqlstore: no dialect registered for %q", name)
}

// embeddedMigrations returns the scripts in migrations/<dir>.
func embeddedMigrations(dir string) fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations/"+dir)
	if err != nil {
		panic(err) // The directory is embedded at build time; this cannot fail.
	}
	return sub
}

// placeholders returns n bind parameters starting at 1, joined by ", ".
func placeholders(d Dialect, n int) string {
	params := make([]string, n)
	for i := range params {
		params[i] = d.Placeholder(i + 1)
	}
	return strings.Join(params, ", ")
}

// nonKeys returns the columns that are not part of the primary key.
func nonKeys(columns, keys []string) []string {
	var out []string
	for _, c := range columns {
		isKey := false
		for _, k := range keys {
			isKey = isKey || c == k
		}
		if !isKey {
			out = append(out, c)
		}
	}
	return out
}

// onConflictUpsert builds the INSERT ... ON CONFLICT form shared by SQLite and PostgreSQL.
func onConflictUpsert(d Dialect, table string, columns, keys []string) string {
	sets := make([]string, 0, len(columns))
	for _, c := range nonKeys(columns, keys) {
		sets = append(sets, fmt.Sprintf("%s = excluded.%s", c, c))
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
		table, strings.Join(columns, ", "), placeholders(d, len(columns)),
		strings.Join(keys, ", "), strings.Join(sets, ", "))
}

// sqliteDialect targets SQLite 3.24+ (modernc.org/sqlite or mattn/go-sqlite3).
type sqliteDialect struct{}

func (sqliteDialect) Name() string           { return "sqlite" }
func (sqliteDialect) Placeholder(int) string { return "?" }
func (sqliteDialect) Migrations() fs.FS      { return embeddedMigrations("sqlite") }
func (d sqliteDialect) Upsert(table string, columns, keys []string) string {
	return onConflictUpsert(d, table, columns, keys)
}
func (sqliteDialect) CreateMigrationsTable(table string) string {
	return "CREATE TABLE IF NOT EXISTS " + table +
		" (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TIMESTAMP NOT NULL)"
}

// postgresDialect targets PostgreSQL 9.5+ (pgx or lib/pq).
type postgresDialect struct{}

func (postgresDialect) Name() string             { return "postgres" }
func (postgresDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (postgresDialect) Migrations() fs.FS        { return embeddedMigrations("postgres") }
func (d postgresDialect) Upsert(table string, columns, keys []string) string {
	return onConflictUpsert(d, table, columns, keys)
}
func (postgresDialect) CreateMigrationsTable(table string) string {
	return "CREATE TABLE IF NOT EXISTS " + table +
		" (version BIGINT PRIMARY KEY, name VARCHAR(255) NOT NULL, applied_at TIMESTAMPTZ NOT NULL)"
}

// mysqlDialect targets MySQL 5.7+ and MariaDB 10.2+ (go-sql-driver/mysql).
// The DSN must include parseTime=true.
type mysqlDialect struct{}

func (mysqlDialect) Name() string           { return "mysql" }
func (mysqlDialect) Placeholder(int) string { return "?" }
func (mysqlDialect) Migrations() fs.FS      { return embeddedMigrations("mysql") }
func (d mysqlDialect) Upsert(table string, columns, keys []string) string {
	sets := make([]string, 0, len(columns))
	for _, c := range nonKeys(columns, keys) {
		sets = append(sets, fmt.Sprintf("%s = VALUES(%s)", c, c))
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
		table, strings.Join(columns, ", "), placeholders(d, len(columns)), strings.Join(sets, ", "))
}
func (mysqlDialect) CreateMigrationsTable(table string) string {
	return "CREATE TABLE IF NOT EXISTS " + table +
		" (version BIGINT PRIMARY KEY, name VARCHAR(255) NOT NULL, applied_at DATETIME(6) NOT NULL) ENGINE=InnoDB"
}

// sqlserverDialect targets SQL Server 2016+ (microsoft/go-mssqldb).
type sqlserverDialect struct{}

func (sqlserverDialect) Name() string             { return "sqlserver" }
func (sqlserverDialect) Placeholder(n int) string { return fmt.Sprintf("@p%d", n) }
func (sqlserverDialect) Migrations() fs.FS        { return embeddedMigrations("sqlserver") }
func (d sqlserverDialect) Upsert(table string, columns, keys []string) string {
	on := make([]string, len(keys))
	for i, k := range keys {
		on[i] = fmt.Sprintf("dst.%s = src.%s", k, k)
	}
	var sets []string
	for _, c := range nonKeys(columns, keys) {
		sets = append(sets, fmt.Sprintf("%s = src.%s", c, c))
	}
	srcCols := make([]string, len(columns))
	for i, c := range columns {
		srcCols[i] = "src." + c
	}
	cols := strings.Join(columns, ", ")
	return fmt.Sprintf("MERGE INTO %s WITH (HOLDLOCK) AS dst USING (VALUES (%s)) AS src (%s) ON %s "+
		"WHEN MATCHED THEN UPDATE SET %s WHEN NOT MATCHED THEN INSERT (%s) VALUES (%s);",
		table, placeholders(d, len(columns)), cols, strings.Join(on, " AND "),
		strings.Join(sets, ", "), cols, strings.Join(srcCols, ", "))
}
func (sqlserverDialect) CreateMigrationsTable(table string) string {
	return "IF OBJECT_ID(N'" + table + "', N'U') IS NULL CREATE TABLE " + table +
		" (version BIGINT PRIMARY KEY, name NVARCHAR(255) NOT NULL, applied_at DATETIME2 NOT NULL)"
}

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
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MigrationsTable records which schema versions have been applied.
const MigrationsTable = "honeycomb_schema_migrations"

// Migration is one versioned schema change with its rollback.
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

var migrationFile = regexp.MustCompile(`^(\d+)_([A-Za-z0-9_]+)\.(up|down)\.sql$`)

// LoadMigrations reads NNNN_name.up.sql / NNNN_name.down.sql pairs from fsys,
// sorted by version. Every version must have both scripts.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("sqlstore: read migrations: %w", err)
	}

	byVersion := make(map[int]*Migration)
	for _, entry := range entries {
		m := migrationFile.FindStringSubmatch(entry.Name())
		if entry.IsDir() || m == nil {
			continue
		}
		version, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("sqlstore: read %s: %w", entry.Name(), err)
		}
		mig, ok := byVersion[version]
		if !ok {
			mig = &Migration{Version: version, Name: m[2]}
			byVersion[version] = mig
		} else if mig.Name != m[2] {
			return nil, fmt.Errorf("sqlstore: migration %d has conflicting names %q and %q", version, mig.Name, m[2])
		}
		if m[3] == "up" {
			mig.Up = string(body)
		} else {
			mig.Down = string(body)
		}
	}

	migrations := make([]Migration, 0, len(byVersion))
	for _, mig := range byVersion {
		if mig.Up == "" || mig.Down == "" {
			return nil, fmt.Errorf("sqlstore: migration %d_%s needs both .up.sql and .down.sql", mig.Version, mig.Name)
		}
		migrations = append(migrations, *mig)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

// Setup creates or upgrades the schema by applying every pending migration,
// each in its own transaction. It is idempotent: run it at every power-up.
func Setup(ctx context.Context, db *sql.DB, d Dialect) error {
	migrations, err := LoadMigrations(d.Migrations())
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, d.CreateMigrationsTable(MigrationsTable)); err != nil {
		return fmt.Errorf("sqlstore: create %s: %w", MigrationsTable, err)
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	record := fmt.Sprintf("INSERT INTO %s (version, name, applied_at) VALUES (%s)", MigrationsTable, placeholders(d, 3))
	for _, mig := range migrations {
		if applied[mig.Version] {
			continue
		}
		err := inTx(ctx, db, func(tx *sql.Tx) error {
			if err := execScript(ctx, tx, mig.Up); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, record, mig.Version, mig.Name, time.Now().UTC())
			return err
		})
		if err != nil {
			return fmt.Errorf("sqlstore: apply migration %d_%s: %w", mig.Version, mig.Name, err)
		}
	}
	return nil
}

// Teardown rolls back every applied migration in reverse order and drops the
// migrations table, removing all honeycomb tables and data. It is meant for
// decommissioning and tests; it is not part of a normal shutdown.
func Teardown(ctx context.Context, db *sql.DB, d Dialect) error {
	migrations, err := LoadMigrations(d.Migrations())
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, d.CreateMigrationsTable(MigrationsTable)); err != nil {
		return fmt.Errorf("sqlstore: create %s: %w", MigrationsTable, err)
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	forget := fmt.Sprintf("DELETE FROM %s WHERE version = %s", MigrationsTable, d.Placeholder(1))
	for i := len(migrations) - 1; i >= 0; i-- {
		mig := migrations[i]
		if !applied[mig.Version] {
			continue
		}
		err := inTx(ctx, db, func(tx *sql.Tx) error {
			if err := execScript(ctx, tx, mig.Down); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, forget, mig.Version)
			return err
		})
		if err != nil {
			return fmt.Errorf("sqlstore: roll back migration %d_%s: %w", mig.Version, mig.Name, err)
		}
	}

	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+MigrationsTable); err != nil {
		return fmt.Errorf("sqlstore: drop %s: %w", MigrationsTable, err)
	}
	return nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT version FROM "+MigrationsTable)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: read %s: %w", MigrationsTable, err)
	}
	defer rows.Close()

	applied := make(map[int]bool)
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// execScript runs each statement of a migration script. Not every driver
// accepts several statements per Exec, so scripts are split on lines ending in
// ";". Full-line "--" comments are dropped. Statements must therefore not
// contain a line-ending ";" internally (no procedure bodies).
func execScript(ctx context.Context, tx *sql.Tx, script string) error {
	for _, stmt := range splitStatements(script) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w\nstatement: %s", err, stmt)
		}
	}
	return nil
}

func splitStatements(script string) []string {
	var stmts []string
	var current strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(script, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			stmts = append(stmts, strings.TrimSpace(current.String()))
			current.Reset()
		}
	}
	if rest := strings.TrimSpace(current.String()); rest != "" {
		stmts = append(stmts, rest)
	}
	return stmts
}

func inTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

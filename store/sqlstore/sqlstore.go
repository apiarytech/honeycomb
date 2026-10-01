/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v2.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// Package sqlstore implements honeycomb.TagStore on top of database/sql.
// It does not import any database driver: the application imports the driver
// it needs (for example _ "modernc.org/sqlite") and sqlstore selects the
// matching Dialect. SQLite, PostgreSQL, MySQL/MariaDB and SQL Server are
// built in; other databases can be added with Register.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/apiarytech/honeycomb"
)

// TagsTable holds one row per persisted tag.
const TagsTable = "honeycomb_tags"

// DefaultInstanceID is used when Options.InstanceID is empty.
const DefaultInstanceID = "default"

// columns lists TagsTable's columns in bind-parameter order. The first two form the primary key.
var columns = []string{
	"instance_id", "tag_name", "data_type", "type_info", "tag_value", "alias",
	"description", "direct_address", "is_retain", "is_constant", "is_forced",
	"force_value", "remote_db_id", "remote_tag_name", "updated_at",
}

// Options configures a Store.
type Options struct {
	// Dialect overrides the dialect inferred from the driver name.
	Dialect Dialect
	// InstanceID separates TagDatabase instances (one per PLC/runtime) that
	// share a database. Defaults to DefaultInstanceID.
	InstanceID string
	// AutoSetup runs Setup when the store is opened.
	AutoSetup bool
}

// Store is a honeycomb.TagStore backed by a SQL database.
type Store struct {
	db       *sql.DB
	dialect  Dialect
	instance string
	ownsDB   bool

	upsertSQL string
	selectSQL string
	deleteSQL string
	purgeSQL  string
}

var _ honeycomb.TagStore = (*Store)(nil)

// Open opens a database with the given database/sql driver and DSN and wraps it
// in a Store. The Store owns the connection and closes it on Close.
//
//	store, err := sqlstore.Open(ctx, "sqlite", "file:plc.db", sqlstore.Options{AutoSetup: true})
func Open(ctx context.Context, driverName, dsn string, opts Options) (*Store, error) {
	if opts.Dialect == nil {
		d, err := Lookup(driverName)
		if err != nil {
			return nil, err
		}
		opts.Dialect = d
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: open: %w", err)
	}
	if opts.Dialect.Name() == "sqlite" {
		// SQLite allows one writer at a time; a single connection avoids
		// SQLITE_BUSY errors and keeps ":memory:" databases on one connection.
		db.SetMaxOpenConns(1)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlstore: connect: %w", err)
	}

	store, err := New(ctx, db, opts)
	if err != nil {
		db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// New wraps an existing connection pool. The caller keeps ownership of db.
// opts.Dialect is required.
func New(ctx context.Context, db *sql.DB, opts Options) (*Store, error) {
	if opts.Dialect == nil {
		return nil, errors.New("sqlstore: Options.Dialect is required")
	}
	if opts.InstanceID == "" {
		opts.InstanceID = DefaultInstanceID
	}
	d := opts.Dialect
	s := &Store{
		db:        db,
		dialect:   d,
		instance:  opts.InstanceID,
		upsertSQL: d.Upsert(TagsTable, columns, columns[:2]),
		selectSQL: fmt.Sprintf("SELECT %s FROM %s WHERE instance_id = %s ORDER BY tag_name",
			strings.Join(columns[1:], ", "), TagsTable, d.Placeholder(1)),
		deleteSQL: fmt.Sprintf("DELETE FROM %s WHERE instance_id = %s AND tag_name = %s",
			TagsTable, d.Placeholder(1), d.Placeholder(2)),
		purgeSQL: fmt.Sprintf("DELETE FROM %s WHERE instance_id = %s", TagsTable, d.Placeholder(1)),
	}
	if opts.AutoSetup {
		if err := s.Setup(ctx); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// DB returns the underlying connection pool.
func (s *Store) DB() *sql.DB { return s.db }

// Setup creates or upgrades the schema. See the package-level Setup.
func (s *Store) Setup(ctx context.Context) error { return Setup(ctx, s.db, s.dialect) }

// Teardown drops every honeycomb table, for all instances. See the package-level Teardown.
func (s *Store) Teardown(ctx context.Context) error { return Teardown(ctx, s.db, s.dialect) }

// Purge deletes every tag of this store's instance but keeps the schema.
func (s *Store) Purge(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, s.purgeSQL, s.instance); err != nil {
		return fmt.Errorf("sqlstore: purge: %w", err)
	}
	return nil
}

// LoadTags implements honeycomb.TagStore.
func (s *Store) LoadTags(ctx context.Context) ([]honeycomb.StoredTag, error) {
	rows, err := s.db.QueryContext(ctx, s.selectSQL, s.instance)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: load tags: %w", err)
	}
	defer rows.Close()

	var tags []honeycomb.StoredTag
	for rows.Next() {
		var (
			t                           honeycomb.StoredTag
			dataType                    string
			typeInfo, value, forceValue sql.NullString
			updatedAt                   any
		)
		err := rows.Scan(&t.Name, &dataType, &typeInfo, &value, &t.Alias, &t.Description,
			&t.DirectAddress, &t.Retain, &t.Constant, &t.Forced, &forceValue,
			&t.RemoteDBID, &t.RemoteTagName, &updatedAt)
		if err != nil {
			return nil, fmt.Errorf("sqlstore: load tags: %w", err)
		}
		t.DataType = honeycomb.DataType(dataType)
		t.TypeInfo = rawJSON(typeInfo)
		t.Value = rawJSON(value)
		t.ForceValue = rawJSON(forceValue)
		if ts, ok := updatedAt.(time.Time); ok {
			t.UpdatedAt = ts
		}
		tags = append(tags, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlstore: load tags: %w", err)
	}
	return tags, nil
}

// SaveTags implements honeycomb.TagStore. All tags are written in one transaction.
func (s *Store) SaveTags(ctx context.Context, tags []honeycomb.StoredTag) error {
	if len(tags) == 0 {
		return nil
	}
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, s.upsertSQL)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, t := range tags {
			updatedAt := t.UpdatedAt
			if updatedAt.IsZero() {
				updatedAt = time.Now().UTC()
			}
			_, err := stmt.ExecContext(ctx, s.instance, t.Name, string(t.DataType),
				nullJSON(t.TypeInfo), nullJSON(t.Value), t.Alias, t.Description,
				t.DirectAddress, t.Retain, t.Constant, t.Forced, nullJSON(t.ForceValue),
				t.RemoteDBID, t.RemoteTagName, updatedAt)
			if err != nil {
				return fmt.Errorf("tag '%s': %w", t.Name, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlstore: save tags: %w", err)
	}
	return nil
}

// DeleteTags implements honeycomb.TagStore. All tags are deleted in one transaction.
func (s *Store) DeleteTags(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, s.deleteSQL)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, name := range names {
			if _, err := stmt.ExecContext(ctx, s.instance, name); err != nil {
				return fmt.Errorf("tag '%s': %w", name, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("sqlstore: delete tags: %w", err)
	}
	return nil
}

// Close implements honeycomb.TagStore. It closes the connection only if Open created it.
func (s *Store) Close() error {
	if s.ownsDB {
		return s.db.Close()
	}
	return nil
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return string(b)
}

func rawJSON(s sql.NullString) json.RawMessage {
	if !s.Valid {
		return nil
	}
	return json.RawMessage(s.String)
}

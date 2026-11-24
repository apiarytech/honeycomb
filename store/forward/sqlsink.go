/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package forward

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"sync"
	"time"

	"github.com/apiarytech/honeycomb/store/sqlstore"
)

// HistoryTable holds the changes an SQLSink delivers.
const HistoryTable = "honeycomb_history"

// HistoryMigrationsTable records the history schema's versions, apart from
// sqlstore's, so the two schemas never collide.
const HistoryMigrationsTable = "honeycomb_history_migrations"

// KindLost marks records a full spool dropped before they were sent.
const KindLost = "lost"

//go:embed migrations
var migrationsFS embed.FS

// historyDialect is sqlstore's dialect with the history scripts.
type historyDialect struct {
	sqlstore.Dialect
	migrations fs.FS
}

func (d historyDialect) Migrations() fs.FS { return d.migrations }

var historyColumns = []string{
	"source_id", "epoch", "seq", "kind", "tag_name", "tag_value", "quality", "value_time", "reason", "recorded_at",
}

// SQLSinkOptions configures an SQLSink.
type SQLSinkOptions struct {
	// Name identifies the sink in the spool; default "sql".
	Name string
	// SourceID identifies what is forwarded: one per TagDatabase and node,
	// such as "unit-CM5004000". Both nodes of a redundant pair forward under
	// their own SourceID. Required, at most 128 characters.
	SourceID string
	// Dialect is the database's dialect, e.g. from sqlstore.Lookup("postgres").
	Dialect sqlstore.Dialect
}

// SQLSink stores a collector's records in HistoryTable of a database
// sqlstore supports: SQLite, PostgreSQL, CockroachDB, MySQL/MariaDB or SQL
// Server. It creates the table on its first Send, so the database may be
// unreachable when the program starts. Rows are written with the dialect's
// upsert on (source, epoch, seq, kind, tag), so a batch sent twice is
// stored once.
type SQLSink struct {
	db   *sql.DB
	opts SQLSinkOptions
	d    historyDialect

	mu     sync.Mutex
	ready  bool
	upsert string
}

// NewSQLSink returns a sink writing into db. It does not touch db.
func NewSQLSink(db *sql.DB, opts SQLSinkOptions) (*SQLSink, error) {
	if db == nil || opts.Dialect == nil {
		return nil, errors.New("forward: an SQL sink needs a database and its dialect")
	}
	if opts.SourceID == "" || len(opts.SourceID) > 128 {
		return nil, errors.New("forward: an SQL sink needs a SourceID of at most 128 characters")
	}
	if opts.Name == "" {
		opts.Name = "sql"
	}
	sub, err := fs.Sub(migrationsFS, "migrations/"+opts.Dialect.Name())
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "."); err != nil {
		return nil, fmt.Errorf("forward: no history schema for %s", opts.Dialect.Name())
	}
	d := historyDialect{Dialect: opts.Dialect, migrations: sub}
	return &SQLSink{db: db, opts: opts, d: d,
		upsert: d.Upsert(HistoryTable, historyColumns, historyColumns[:5])}, nil
}

// Name implements Sink.
func (s *SQLSink) Name() string { return s.opts.Name }

// Setup creates or upgrades the history table. Send calls it once.
func (s *SQLSink) Setup(ctx context.Context) error {
	return sqlstore.SetupTable(ctx, s.db, s.d, HistoryMigrationsTable)
}

// Teardown drops the history table and its version table.
func (s *SQLSink) Teardown(ctx context.Context) error {
	return sqlstore.TeardownTable(ctx, s.db, s.d, HistoryMigrationsTable)
}

// Send implements Sink: the batch in one transaction.
func (s *SQLSink) Send(ctx context.Context, b Batch) error {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		if err := s.Setup(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		s.ready = true
		s.mu.Unlock()
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("forward: %s: begin: %w", s.opts.Name, err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, s.upsert)
	if err != nil {
		return fmt.Errorf("forward: %s: %w", s.opts.Name, err)
	}
	defer stmt.Close()
	if b.Lost > 0 {
		reason := fmt.Sprintf("%d records dropped from the spool before they were sent (from %s to %s)",
			b.Lost, b.LostFrom.Format(time.RFC3339Nano), b.LostTo.Format(time.RFC3339Nano))
		// Keyed by the drop's end, which is unique for the life of a spool.
		if _, err := stmt.ExecContext(ctx, s.opts.SourceID, "", b.LostTo.UnixNano(), KindLost, "",
			nil, 0, b.LostFrom.UTC(), reason, now); err != nil {
			return fmt.Errorf("forward: %s: lost records: %w", s.opts.Name, err)
		}
	}
	for _, r := range b.Records {
		e, err := DecodeEvent(r)
		if err != nil {
			continue // not a collector's record: nothing to store
		}
		var value any
		if len(e.Value) > 0 && string(e.Value) != "null" {
			value = string(e.Value)
		}
		at := e.Time
		if at.IsZero() {
			at = r.Time
		}
		// #nosec G115 -- a feed sequence stays far below 2^63; the column is BIGINT.
		if _, err := stmt.ExecContext(ctx, s.opts.SourceID, e.Epoch, int64(e.Seq), e.Kind, e.Name,
			value, int64(e.Quality), at.UTC(), e.Reason, now); err != nil {
			return fmt.Errorf("forward: %s: record %d: %w", s.opts.Name, r.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("forward: %s: commit: %w", s.opts.Name, err)
	}
	return nil
}

// HistoryRow is one row of HistoryTable.
type HistoryRow struct {
	SourceID, Epoch string
	Seq             uint64
	Kind, Name      string
	Value           json.RawMessage // nil for a gap, a lost row or a Bad value without one
	Quality         int64
	Time            time.Time
	Reason          string
}

// History returns tag's rows of this sink's source in [from, to], oldest
// first, at most limit (0: all), with the gaps and drops of the period
// (rows without a tag).
func (s *SQLSink) History(ctx context.Context, tag string, from, to time.Time, limit int) ([]HistoryRow, error) {
	p := s.d.Placeholder
	// #nosec G201 -- the table is a constant and the placeholders the dialect's; values go as arguments.
	q := fmt.Sprintf("SELECT source_id, epoch, seq, kind, tag_name, tag_value, quality, value_time, reason FROM %s "+
		"WHERE source_id = %s AND (tag_name = %s OR tag_name = '')", HistoryTable, p(1), p(2))
	args := []any{s.opts.SourceID, tag}
	// SQLite keeps times as text whose fractions vary in length, so they do
	// not compare in SQL: it filters and sorts here.
	inSQL := s.d.Name() != "sqlite"
	if inSQL {
		q += fmt.Sprintf(" AND value_time >= %s AND value_time <= %s ORDER BY value_time, seq", p(3), p(4))
		args = append(args, from.UTC(), to.UTC())
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("forward: history: %w", err)
	}
	defer rows.Close()
	var out []HistoryRow
	for rows.Next() {
		var h HistoryRow
		var seq int64
		var value sql.NullString
		var at any
		if err := rows.Scan(&h.SourceID, &h.Epoch, &seq, &h.Kind, &h.Name, &value, &h.Quality, &at, &h.Reason); err != nil {
			return nil, fmt.Errorf("forward: history: %w", err)
		}
		h.Seq, h.Time = uint64(seq), asTime(at) // #nosec G115 -- stored from a uint64 by Send
		if value.Valid {
			h.Value = json.RawMessage(value.String)
		}
		if !inSQL && (h.Time.Before(from) || h.Time.After(to)) {
			continue
		}
		out = append(out, h)
		if inSQL && limit > 0 && len(out) == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !inSQL {
		sort.SliceStable(out, func(i, j int) bool {
			if !out[i].Time.Equal(out[j].Time) {
				return out[i].Time.Before(out[j].Time)
			}
			return out[i].Seq < out[j].Seq
		})
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
	}
	return out, nil
}

// timeLayouts are text forms a driver may return for a timestamp column.
var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999",
}

func asTime(v any) time.Time {
	var text string
	switch v := v.(type) {
	case time.Time:
		return v.UTC()
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return time.Time{}
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, text); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

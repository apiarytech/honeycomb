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
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Spool defaults.
const (
	// DefaultMaxBytes caps a spool's data when SpoolOptions.MaxBytes is 0.
	DefaultMaxBytes = 1 << 30
	// lowMark: a spool over a limit drops its oldest records until it is
	// under this share of the limit, so it does not trim on every append.
	lowMark = 0.9
	// dropChunk is the most records dropped in one statement.
	dropChunk = 5000
)

// Record is one entry of a spool. Kind and Payload belong to the program
// that appends it; the spool only keeps them in order.
type Record struct {
	ID      int64 // set by the spool: rises by one per record
	Kind    string
	Time    time.Time // when it happened; used for MaxAge and reports
	Payload []byte
}

// Batch is what a sink receives: the next records it has not confirmed,
// oldest first, and how many records before them were dropped unsent
// because the spool was full or too old.
type Batch struct {
	Records []Record
	// Lost records were dropped before this sink received them; LostFrom
	// and LostTo are the times of the first and last of them.
	Lost             int64
	LostFrom, LostTo time.Time
	// Through is the ID to confirm once the batch is stored.
	Through int64
}

// Empty reports whether there is nothing to send.
func (b Batch) Empty() bool { return len(b.Records) == 0 && b.Lost == 0 }

// SpoolOptions configures a Spool.
type SpoolOptions struct {
	// Sinks are the names of the destinations; a record is deleted once
	// every one of them has confirmed it. A destination no longer listed is
	// forgotten when the spool opens, and a new one receives every record
	// still in the spool. At least one is required.
	Sinks []string
	// MaxBytes caps the spool's data (DefaultMaxBytes when 0). Over it, the
	// oldest records are dropped, sent or not, and the drop is reported to
	// each sink that had not received them.
	MaxBytes int64
	// MaxRows caps the number of records; 0 is no limit.
	MaxRows int64
	// MaxAge drops records older than this; 0 keeps them until sent.
	MaxAge time.Duration
	// OnDrop is told of each drop: how many records, and their times.
	OnDrop func(n int64, from, to time.Time)
}

// Spool is a durable first-in, first-out queue in a local SQLite database.
// Appends are committed with synchronous=FULL, so an appended record
// survives a power cut. It is safe for concurrent use.
type Spool struct {
	db   *sql.DB
	opts SpoolOptions
	own  bool

	mu      sync.Mutex
	wake    chan struct{} // closed and replaced by each append
	rows    int64         // records in the spool
	dropped int64         // records dropped since the spool was made
}

// OpenSpool opens (or creates) the spool at path with the SQLite driver the
// program links: "sqlite" (modernc.org/sqlite) or "sqlite3"
// (mattn/go-sqlite3). The Spool closes the database on Close.
func OpenSpool(ctx context.Context, path string, opts SpoolOptions) (*Spool, error) {
	driver := ""
	for _, d := range []string{"sqlite", "sqlite3"} {
		if slices.Contains(sql.Drivers(), d) {
			driver = d
			break
		}
	}
	if driver == "" {
		return nil, errors.New("forward: no SQLite driver: import modernc.org/sqlite or github.com/mattn/go-sqlite3")
	}
	db, err := sql.Open(driver, path)
	if err != nil {
		return nil, fmt.Errorf("forward: open spool: %w", err)
	}
	s, err := NewSpool(ctx, db, opts)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.own = true
	return s, nil
}

// NewSpool makes a spool in db, which must be a SQLite database. The caller
// keeps ownership of db.
func NewSpool(ctx context.Context, db *sql.DB, opts SpoolOptions) (*Spool, error) {
	if len(opts.Sinks) == 0 {
		return nil, errors.New("forward: a spool needs at least one sink")
	}
	for i, name := range opts.Sinks {
		if name == "" || slices.Contains(opts.Sinks[:i], name) {
			return nil, fmt.Errorf("forward: sink names must be unique and not empty: %q", opts.Sinks)
		}
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxRows < 0 || opts.MaxAge < 0 {
		return nil, errors.New("forward: negative spool limit")
	}
	if opts.OnDrop == nil {
		opts.OnDrop = func(int64, time.Time, time.Time) {}
	}
	db.SetMaxOpenConns(1) // one writer; also keeps the pragmas on the one connection
	for _, stmt := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		`CREATE TABLE IF NOT EXISTS spool_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			at INTEGER NOT NULL,
			payload BLOB NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS spool_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS spool_sinks (sink TEXT PRIMARY KEY, acked INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS spool_drops (
			through INTEGER PRIMARY KEY,
			first_id INTEGER NOT NULL,
			first_at INTEGER NOT NULL,
			last_at INTEGER NOT NULL)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("forward: prepare spool: %w", err)
		}
	}
	s := &Spool{db: db, opts: opts, wake: make(chan struct{})}
	if err := s.registerSinks(ctx); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM spool_records").Scan(&s.rows); err != nil {
		return nil, fmt.Errorf("forward: count spool: %w", err)
	}
	if v, ok, err := s.State(ctx, stateDropped); err != nil {
		return nil, err
	} else if ok {
		s.dropped, _ = strconv.ParseInt(v, 10, 64)
	}
	if err := s.prune(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// stateDropped keeps the count of dropped records across restarts.
const stateDropped = "spool.dropped"

// registerSinks adds new sinks and forgets those no longer configured, so a
// removed sink does not keep records for ever.
func (s *Spool) registerSinks(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT sink FROM spool_sinks")
		if err != nil {
			return err
		}
		var have []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return err
			}
			have = append(have, name)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, name := range have {
			if !slices.Contains(s.opts.Sinks, name) {
				if _, err := tx.ExecContext(ctx, "DELETE FROM spool_sinks WHERE sink = ?", name); err != nil {
					return err
				}
			}
		}
		for _, name := range s.opts.Sinks {
			if !slices.Contains(have, name) {
				if _, err := tx.ExecContext(ctx, "INSERT INTO spool_sinks (sink, acked) VALUES (?, 0)", name); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Close closes the database if OpenSpool opened it.
func (s *Spool) Close() error {
	if s.own {
		return s.db.Close()
	}
	return nil
}

// Wake returns a channel that is closed by the next append.
func (s *Spool) Wake() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wake
}

// Append adds records and sets state keys in one transaction: a reader that
// keeps its position in the state never records a change twice or loses
// one, whenever the power goes. Then it enforces the spool's limits.
func (s *Spool) Append(ctx context.Context, recs []Record, state map[string]string) error {
	if len(recs) == 0 && len(state) == 0 {
		return nil
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if len(recs) > 0 {
			stmt, err := tx.PrepareContext(ctx, "INSERT INTO spool_records (kind, at, payload) VALUES (?, ?, ?)")
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, r := range recs {
				payload := r.Payload
				if payload == nil {
					payload = []byte{}
				}
				if _, err := stmt.ExecContext(ctx, r.Kind, r.Time.UnixNano(), payload); err != nil {
					return err
				}
			}
		}
		return setState(ctx, tx, state)
	})
	if err != nil {
		return fmt.Errorf("forward: append to spool: %w", err)
	}
	if len(recs) == 0 {
		return nil
	}
	s.mu.Lock()
	s.rows += int64(len(recs))
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
	return s.enforce(ctx)
}

func setState(ctx context.Context, tx *sql.Tx, state map[string]string) error {
	for k, v := range state {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO spool_state (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", k, v); err != nil {
			return err
		}
	}
	return nil
}

// State returns a value set by Append or SetState.
func (s *Spool) State(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM spool_state WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("forward: spool state %s: %w", key, err)
	}
	return v, true, nil
}

// SetState sets state keys without appending records.
func (s *Spool) SetState(ctx context.Context, state map[string]string) error {
	return s.Append(ctx, nil, state)
}

// Read returns up to max records sink has not confirmed, oldest first,
// and the records dropped before it received them.
func (s *Spool) Read(ctx context.Context, sink string, max int) (Batch, error) {
	if max <= 0 {
		max = 500
	}
	var b Batch
	acked, err := s.acked(ctx, sink)
	if err != nil {
		return b, err
	}
	b.Through = acked

	drops, err := s.db.QueryContext(ctx, "SELECT through, first_id, first_at, last_at FROM spool_drops WHERE through > ? ORDER BY through", acked)
	if err != nil {
		return b, fmt.Errorf("forward: read spool: %w", err)
	}
	for drops.Next() {
		var through, first, fromNs, toNs int64
		if err := drops.Scan(&through, &first, &fromNs, &toNs); err != nil {
			_ = drops.Close()
			return b, fmt.Errorf("forward: read spool: %w", err)
		}
		b.Lost += through - max64(first-1, acked)
		if from := time.Unix(0, fromNs).UTC(); b.LostFrom.IsZero() || from.Before(b.LostFrom) {
			b.LostFrom = from
		}
		if to := time.Unix(0, toNs).UTC(); to.After(b.LostTo) {
			b.LostTo = to
		}
		b.Through = max64(b.Through, through)
	}
	_ = drops.Close()
	if err := drops.Err(); err != nil {
		return b, fmt.Errorf("forward: read spool: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, "SELECT id, kind, at, payload FROM spool_records WHERE id > ? ORDER BY id LIMIT ?", acked, max)
	if err != nil {
		return b, fmt.Errorf("forward: read spool: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Record
		var at int64
		if err := rows.Scan(&r.ID, &r.Kind, &at, &r.Payload); err != nil {
			return b, fmt.Errorf("forward: read spool: %w", err)
		}
		r.Time = time.Unix(0, at).UTC()
		b.Records = append(b.Records, r)
		b.Through = max64(b.Through, r.ID)
	}
	return b, rows.Err()
}

func (s *Spool) acked(ctx context.Context, sink string) (int64, error) {
	var acked int64
	err := s.db.QueryRowContext(ctx, "SELECT acked FROM spool_sinks WHERE sink = ?", sink).Scan(&acked)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("forward: spool has no sink %q", sink)
	}
	if err != nil {
		return 0, fmt.Errorf("forward: spool sink %s: %w", sink, err)
	}
	return acked, nil
}

// Ack confirms that sink stored every record up to through, and deletes
// the records every sink has confirmed.
func (s *Spool) Ack(ctx context.Context, sink string, through int64) error {
	// One transaction, so one sync to disk, for the position and the prune.
	var deleted int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE spool_sinks SET acked = MAX(acked, ?) WHERE sink = ?", through, sink)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("spool has no sink %q", sink)
		}
		deleted, err = pruneTx(ctx, tx)
		return err
	})
	if err != nil {
		return fmt.Errorf("forward: confirm %s: %w", sink, err)
	}
	s.mu.Lock()
	s.rows -= deleted
	s.mu.Unlock()
	return nil
}

// prune deletes what every sink has confirmed.
func (s *Spool) prune(ctx context.Context) error {
	var deleted int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		deleted, err = pruneTx(ctx, tx)
		return err
	})
	if err != nil {
		return fmt.Errorf("forward: prune spool: %w", err)
	}
	s.mu.Lock()
	s.rows -= deleted
	s.mu.Unlock()
	return nil
}

// pruneTx deletes, in tx, what every sink has confirmed, and returns how
// many records it deleted.
func pruneTx(ctx context.Context, tx *sql.Tx) (int64, error) {
	var deleted int64
	err := func() error {
		var low int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MIN(acked), 0) FROM spool_sinks").Scan(&low); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM spool_records WHERE id <= ?", low)
		if err != nil {
			return err
		}
		deleted, _ = res.RowsAffected()
		_, err = tx.ExecContext(ctx, "DELETE FROM spool_drops WHERE through <= ?", low)
		return err
	}()
	return deleted, err
}

// enforce drops the oldest records while the spool is over a limit.
func (s *Spool) enforce(ctx context.Context) error {
	if s.opts.MaxAge > 0 {
		cutoff := time.Now().Add(-s.opts.MaxAge).UnixNano()
		for {
			n, err := s.dropOlder(ctx, cutoff, dropChunk)
			if err != nil {
				return err
			}
			if n < dropChunk {
				break
			}
		}
	}
	if s.opts.MaxRows > 0 {
		s.mu.Lock()
		over := s.rows - s.opts.MaxRows
		target := s.rows - int64(float64(s.opts.MaxRows)*lowMark)
		s.mu.Unlock()
		if over > 0 {
			for target > 0 {
				n, err := s.dropOldest(ctx, min(target, dropChunk))
				if err != nil {
					return err
				}
				if n == 0 {
					break
				}
				target -= n
			}
		}
	}
	size, err := s.Bytes(ctx)
	if err != nil {
		return err
	}
	if size <= s.opts.MaxBytes {
		return nil
	}
	low := int64(float64(s.opts.MaxBytes) * lowMark)
	for size > low {
		s.mu.Lock()
		chunk := min(max(s.rows/100, 100), dropChunk)
		s.mu.Unlock()
		n, err := s.dropOldest(ctx, chunk)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
		if size, err = s.Bytes(ctx); err != nil {
			return err
		}
	}
	return nil
}

// dropOldest drops up to n of the oldest records and returns how many.
func (s *Spool) dropOldest(ctx context.Context, n int64) (int64, error) {
	var through sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		"SELECT MAX(id) FROM (SELECT id FROM spool_records ORDER BY id LIMIT ?)", n).Scan(&through)
	if err != nil {
		return 0, fmt.Errorf("forward: drop from spool: %w", err)
	}
	if !through.Valid {
		return 0, nil
	}
	return s.dropThrough(ctx, through.Int64)
}

// dropOlder drops up to n of the oldest records, up to the first one at or
// after before, and returns how many.
func (s *Spool) dropOlder(ctx context.Context, before int64, n int64) (int64, error) {
	var lo, young sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(id) FROM spool_records").Scan(&lo); err != nil {
		return 0, fmt.Errorf("forward: drop from spool: %w", err)
	}
	if !lo.Valid {
		return 0, nil
	}
	// Records are appended in time order, so the old ones are a run from
	// the oldest; the scan stops at the first young one.
	err := s.db.QueryRowContext(ctx, "SELECT id FROM spool_records WHERE at >= ? ORDER BY id LIMIT 1", before).Scan(&young)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("forward: drop from spool: %w", err)
	}
	through := lo.Int64 + n - 1
	if young.Valid {
		through = min(through, young.Int64-1)
	}
	if through < lo.Int64 {
		return 0, nil
	}
	return s.dropThrough(ctx, through)
}

// dropThrough deletes every record up to through, sent or not, records the
// drop for the sinks that had not received them all, and returns how many
// it deleted.
func (s *Spool) dropThrough(ctx context.Context, through int64) (int64, error) {
	var count, first, firstAt, lastAt int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*), COALESCE(MIN(id), 0), COALESCE(MIN(at), 0), COALESCE(MAX(at), 0) FROM spool_records WHERE id <= ?",
			through).Scan(&count, &first, &firstAt, &lastAt); err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM spool_records WHERE id <= ?", through); err != nil {
			return err
		}
		// A sink that had every one of them loses nothing; for the others,
		// Read counts the records between their position and through.
		var behind int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM spool_sinks WHERE acked < ?", through).Scan(&behind); err != nil {
			return err
		}
		if behind > 0 {
			if _, err := tx.ExecContext(ctx, "INSERT INTO spool_drops (through, first_id, first_at, last_at) VALUES (?, ?, ?, ?)",
				through, first, firstAt, lastAt); err != nil {
				return err
			}
		}
		s.mu.Lock()
		total := s.dropped + count
		s.mu.Unlock()
		return setState(ctx, tx, map[string]string{stateDropped: strconv.FormatInt(total, 10)})
	})
	if err != nil {
		return 0, fmt.Errorf("forward: drop from spool: %w", err)
	}
	if count > 0 {
		s.mu.Lock()
		s.rows -= count
		s.dropped += count
		s.mu.Unlock()
		s.opts.OnDrop(count, time.Unix(0, firstAt).UTC(), time.Unix(0, lastAt).UTC())
	}
	return count, nil
}

// Bytes is the size of the spool's data: its pages in use.
func (s *Spool) Bytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	for _, p := range []struct {
		q string
		v *int64
	}{{"PRAGMA page_count", &pages}, {"PRAGMA freelist_count", &free}, {"PRAGMA page_size", &size}} {
		if err := s.db.QueryRowContext(ctx, p.q).Scan(p.v); err != nil {
			return 0, fmt.Errorf("forward: spool size: %w", err)
		}
	}
	return (pages - free) * size, nil
}

// SpoolStats describes a spool.
type SpoolStats struct {
	Rows    int64     // records in the spool
	Bytes   int64     // size of its data
	Oldest  time.Time // time of the oldest record; zero when empty
	Dropped int64     // records dropped since the spool was made
	// Pending is, per sink, the records it has not confirmed yet.
	Pending map[string]int64
	// OldestPending is, per sink, the time of the oldest record it has not
	// confirmed; zero when it has them all.
	OldestPending map[string]time.Time
}

// Stats reports the spool's size and each sink's backlog.
func (s *Spool) Stats(ctx context.Context) (SpoolStats, error) {
	st := SpoolStats{Pending: map[string]int64{}, OldestPending: map[string]time.Time{}}
	var err error
	if st.Bytes, err = s.Bytes(ctx); err != nil {
		return st, err
	}
	var lo, hi, at sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT MIN(id), MAX(id) FROM spool_records").Scan(&lo, &hi); err != nil {
		return st, fmt.Errorf("forward: spool stats: %w", err)
	}
	s.mu.Lock()
	st.Rows, st.Dropped = s.rows, s.dropped
	s.mu.Unlock()
	if lo.Valid {
		if err := s.db.QueryRowContext(ctx, "SELECT at FROM spool_records WHERE id = ?", lo.Int64).Scan(&at); err != nil {
			return st, fmt.Errorf("forward: spool stats: %w", err)
		}
		st.Oldest = time.Unix(0, at.Int64).UTC()
	}
	for _, sink := range s.opts.Sinks {
		acked, err := s.acked(ctx, sink)
		if err != nil {
			return st, err
		}
		if !hi.Valid || acked >= hi.Int64 {
			st.Pending[sink] = 0
			continue
		}
		next := max64(acked, lo.Int64-1) // ids are consecutive: AUTOINCREMENT, one writer
		st.Pending[sink] = hi.Int64 - next
		var t int64
		if err := s.db.QueryRowContext(ctx, "SELECT at FROM spool_records WHERE id > ? ORDER BY id LIMIT 1", acked).Scan(&t); err != nil {
			return st, fmt.Errorf("forward: spool stats: %w", err)
		}
		st.OldestPending[sink] = time.Unix(0, t).UTC()
	}
	return st, nil
}

func (s *Spool) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

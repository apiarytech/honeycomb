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
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/apiarytech/honeycomb"
	"github.com/apiarytech/honeycomb/store/sqlstore"
	plc "github.com/apiarytech/royaljelly/iec"
)

func openTestSpool(t *testing.T, path string, opts SpoolOptions) *Spool {
	t.Helper()
	s, err := OpenSpool(context.Background(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func recs(kind string, n int, start time.Time) []Record {
	out := make([]Record, n)
	for i := range out {
		out[i] = Record{Kind: kind, Time: start.Add(time.Duration(i) * time.Second), Payload: fmt.Appendf(nil, "%d", i)}
	}
	return out
}

func TestSpoolAppendReadAckAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spool.db")
	s := openTestSpool(t, path, SpoolOptions{Sinks: []string{"a", "b"}})
	if err := s.Append(ctx, recs("x", 10, time.Now()), map[string]string{"pos": "10"}); err != nil {
		t.Fatal(err)
	}
	b, err := s.Read(ctx, "a", 4)
	if err != nil || len(b.Records) != 4 || b.Records[0].ID != 1 || b.Through != 4 {
		t.Fatalf("read a: %+v %v", b, err)
	}
	if err := s.Ack(ctx, "a", b.Through); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats(ctx)
	if st.Rows != 10 || st.Pending["a"] != 6 || st.Pending["b"] != 10 {
		t.Fatalf("b has not confirmed: nothing deleted: %+v", st)
	}
	if err := s.Ack(ctx, "b", 6); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.Stats(ctx); st.Rows != 6 {
		t.Fatalf("records both confirmed are deleted: %+v", st)
	}
	s.Close()

	// Reopened: the records, positions and state survive; a sink no longer
	// listed is forgotten and stops holding records.
	s = openTestSpool(t, path, SpoolOptions{Sinks: []string{"a"}})
	defer s.Close()
	if v, ok, _ := s.State(ctx, "pos"); !ok || v != "10" {
		t.Fatalf("state after restart: %q %v", v, ok)
	}
	b, _ = s.Read(ctx, "a", 100)
	if len(b.Records) != 6 || b.Records[0].ID != 5 {
		t.Fatalf("a resumes after its position: %+v", b)
	}
	if string(b.Records[0].Payload) != "4" || b.Records[0].Kind != "x" {
		t.Fatalf("payload: %+v", b.Records[0])
	}
	if err := s.Ack(ctx, "a", b.Through); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.Stats(ctx); st.Rows != 0 {
		t.Fatalf("b removed: everything a confirmed is deleted: %+v", st)
	}
}

func TestSpoolDropsOldestAndReportsLoss(t *testing.T) {
	ctx := context.Background()
	var dropped atomic.Int64
	s := openTestSpool(t, filepath.Join(t.TempDir(), "spool.db"), SpoolOptions{
		Sinks: []string{"fast", "slow"}, MaxRows: 100,
		OnDrop: func(n int64, _, _ time.Time) { dropped.Add(n) },
	})
	defer s.Close()
	t0 := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if err := s.Append(ctx, recs("x", 50, t0), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, "fast", 10); err != nil { // fast has 1..10
		t.Fatal(err)
	}
	if err := s.Append(ctx, recs("x", 60, t0.Add(time.Hour)), nil); err != nil { // 110 > 100
		t.Fatal(err)
	}
	st, _ := s.Stats(ctx)
	if st.Rows > 100 || st.Rows < 85 || dropped.Load() != 110-st.Rows || st.Dropped != dropped.Load() {
		t.Fatalf("over MaxRows the oldest go, down to 90%%: rows %d dropped %d stats %+v", st.Rows, dropped.Load(), st)
	}
	n := dropped.Load() // records 1..n are gone
	slow, _ := s.Read(ctx, "slow", 1000)
	if slow.Lost != n || slow.LostFrom != t0 || slow.Records[0].ID != n+1 {
		t.Fatalf("slow lost all %d: %+v", n, slow)
	}
	fast, _ := s.Read(ctx, "fast", 1000)
	if fast.Lost != n-10 || fast.Records[0].ID != n+1 {
		t.Fatalf("fast had 1..10, lost the rest of the %d: lost %d", n, fast.Lost)
	}
	if err := s.Ack(ctx, "slow", slow.Through); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Read(ctx, "slow", 1000); !again.Empty() {
		t.Fatalf("a confirmed loss is not reported again: %+v", again)
	}
	if fast, _ = s.Read(ctx, "fast", 1000); fast.Lost != n-10 {
		t.Fatalf("fast still has its loss to report: %+v", fast.Lost)
	}
}

func TestSpoolMaxAgeAndMaxBytes(t *testing.T) {
	ctx := context.Background()
	s := openTestSpool(t, filepath.Join(t.TempDir(), "spool.db"), SpoolOptions{Sinks: []string{"a"}, MaxAge: time.Hour})
	now := time.Now()
	if err := s.Append(ctx, append(recs("old", 5, now.Add(-3*time.Hour)), recs("new", 5, now)...), nil); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Read(ctx, "a", 100)
	if b.Lost != 5 || len(b.Records) != 5 || b.Records[0].Kind != "new" {
		t.Fatalf("records older than MaxAge go: %+v", b)
	}
	s.Close()

	big := make([]byte, 4096)
	s = openTestSpool(t, filepath.Join(t.TempDir(), "spool.db"), SpoolOptions{Sinks: []string{"a"}, MaxBytes: 256 << 10})
	defer s.Close()
	for i := range 40 {
		r := make([]Record, 10)
		for j := range r {
			r[j] = Record{Kind: "big", Time: now, Payload: big}
		}
		if err := s.Append(ctx, r, nil); err != nil {
			t.Fatal(i, err)
		}
	}
	size, _ := s.Bytes(ctx)
	st, _ := s.Stats(ctx)
	if size > 256<<10 || st.Dropped == 0 {
		t.Fatalf("spool kept under MaxBytes by dropping: %d bytes, %+v", size, st)
	}
}

// flakySink records what it stores, and fails while down.
type flakySink struct {
	name  string
	down  atomic.Bool
	fails atomic.Int64
	mu    sync.Mutex
	got   []Record
	lost  int64
}

func (f *flakySink) Name() string { return f.name }
func (f *flakySink) Send(_ context.Context, b Batch) error {
	if f.down.Load() {
		f.fails.Add(1)
		return errors.New("connection refused")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, b.Records...)
	f.lost += b.Lost
	return nil
}
func (f *flakySink) records() []Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.got...)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestForwarderRetriesThenSendsBacklogInOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openTestSpool(t, filepath.Join(t.TempDir(), "spool.db"), SpoolOptions{Sinks: []string{"plant"}})
	defer s.Close()
	sink := &flakySink{name: "plant"}
	sink.down.Store(true)
	f := NewForwarder(s, sink, ForwarderOptions{BatchSize: 7, MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond})
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()

	for i := range 10 {
		if err := s.Append(ctx, recs("x", 5, time.Now()), nil); err != nil {
			t.Fatal(i, err)
		}
	}
	waitFor(t, "failures", func() bool { return sink.fails.Load() >= 3 })
	if st := f.Status(); st.Online || st.LastError == "" || st.Failures < 3 {
		t.Fatalf("down: %+v", st)
	}
	sink.down.Store(false)
	waitFor(t, "backlog", func() bool { return len(sink.records()) == 50 })
	for i, r := range sink.records() {
		if r.ID != int64(i+1) {
			t.Fatalf("record %d has ID %d: out of order", i, r.ID)
		}
	}
	waitFor(t, "status back", func() bool { st := f.Status(); return st.Online && st.Sent == 50 && st.Failures == 0 })
	waitFor(t, "spool emptied", func() bool { st, _ := s.Stats(ctx); return st.Rows == 0 })
	cancel()
	<-done
}

func newLevelDB(t *testing.T) *honeycomb.TagDatabase {
	t.Helper()
	db := honeycomb.NewTagDatabase()
	for _, tag := range []*honeycomb.Tag{
		{Name: "Level", Value: plc.LREAL(0)},
		{Name: "Pump", Value: plc.BOOL(false)},
		{Name: "Mode", Value: plc.STRING("auto")},
	} {
		if err := db.AddTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func events(t *testing.T, rs []Record) []Event {
	t.Helper()
	var out []Event
	for _, r := range rs {
		e, err := DecodeEvent(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// collect runs a collector until it has appended want records.
func collect(t *testing.T, s *Spool, opts CollectorOptions, want int, write func()) []Event {
	t.Helper()
	opts.Wait, opts.CommitEvery = 50*time.Millisecond, time.Millisecond
	c, err := NewCollector(s, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	waitFor(t, "start", func() bool { _, ok, _ := s.State(context.Background(), "collector.cursor"); return ok })
	if write != nil {
		write()
	}
	waitFor(t, fmt.Sprintf("%d records", want), func() bool { st, _ := s.Stats(context.Background()); return st.Rows >= int64(want) })
	cancel()
	<-done
	b, err := s.Read(context.Background(), "a", 10000)
	if err != nil {
		t.Fatal(err)
	}
	return events(t, b.Records)
}

func TestCollectorEveryChangeAndDeadband(t *testing.T) {
	db := newLevelDB(t)
	s := openTestSpool(t, filepath.Join(t.TempDir(), "spool.db"), SpoolOptions{Sinks: []string{"a"}})
	defer s.Close()
	evs := collect(t, s, CollectorOptions{Source: db, Reader: db,
		Deadbands: []Deadband{{Tags: "Pump"}, {Tags: "*", Absolute: 1}}}, 3+6, func() {
		// 0.5 is kept: Level was never written, so Unknown to Good is a
		// quality change. Then 2.5 and 10 move by more than 1.
		for _, v := range []float64{0.5, 0.9, 2.5, 2.6, 2.4, 10} {
			_ = db.SetTagValue("Level", plc.LREAL(v))
		}
		_ = db.SetTagQuality("Level", honeycomb.QualityUncertain) // a quality change is always kept
		_ = db.SetTagValue("Pump", plc.BOOL(true))                // exempt: every change
		_ = db.SetTagValue("Mode", plc.STRING("hand"))            // not a number: every change
	})
	if len(evs) != 9 {
		t.Fatalf("want 3 snapshots + 6 changes, got %d: %+v", len(evs), evs)
	}
	for _, e := range evs[:3] {
		if e.Kind != KindSnapshot {
			t.Fatalf("the first start records current values: %+v", e)
		}
	}
	var got []string
	for _, e := range evs[3:] {
		if e.Kind != KindChange || e.Epoch == "" || e.Seq == 0 || e.Time.IsZero() {
			t.Fatalf("change: %+v", e)
		}
		got = append(got, fmt.Sprintf("%s=%s/%d", e.Name, e.Value, e.Quality))
	}
	want := []string{"Level=0.5/1", "Level=2.5/1", "Level=10/1", "Level=10/2", "Pump=true/1", `Mode="hand"/1`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("deadband:\n got %v\nwant %v", got, want)
	}
}

func TestCollectorResumesAndRecordsGapOnRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spool.db")
	db := newLevelDB(t)
	s := openTestSpool(t, path, SpoolOptions{Sinks: []string{"a"}})
	collect(t, s, CollectorOptions{Source: db, Reader: db, Names: []string{"Level"}}, 3, func() {
		_ = db.SetTagValue("Level", plc.LREAL(1))
		_ = db.SetTagValue("Level", plc.LREAL(2))
	})
	s.Close()

	// Changes while the collector is stopped are picked up from the feed.
	_ = db.SetTagValue("Level", plc.LREAL(3))
	s = openTestSpool(t, path, SpoolOptions{Sinks: []string{"a"}})
	evs := collect(t, s, CollectorOptions{Source: db, Reader: db, Names: []string{"Level"}}, 4, nil)
	if e := evs[3]; e.Kind != KindChange || string(e.Value) != "3" {
		t.Fatalf("resumed: no change twice, none missing: %+v", evs)
	}
	s.Close()

	// The database restarted (a new epoch): a gap, then current values.
	db2 := newLevelDB(t)
	_ = db2.SetTagValue("Level", plc.LREAL(7))
	s = openTestSpool(t, path, SpoolOptions{Sinks: []string{"a"}})
	defer s.Close()
	evs = collect(t, s, CollectorOptions{Source: db2, Reader: db2, Names: []string{"Level"}}, 6, nil)
	if evs[4].Kind != KindGap || evs[4].Reason == "" || evs[5].Kind != KindSnapshot || string(evs[5].Value) != "7" {
		t.Fatalf("restart: %+v", evs[4:])
	}
	if _, err := s.Read(ctx, "a", 1); err != nil {
		t.Fatal(err)
	}
}

func openSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "plant.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSQLSinkIdempotentWithGapsAndLoss(t *testing.T) {
	ctx := context.Background()
	d, _ := sqlstore.Lookup("sqlite")
	sink, err := NewSQLSink(openSQLite(t), SQLSinkOptions{SourceID: "unit-1", Dialect: d})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	ev := func(e Event) Record {
		p, _ := json.Marshal(e)
		return Record{Kind: e.Kind, Time: e.Time, Payload: p}
	}
	b := Batch{
		Lost: 12, LostFrom: t0.Add(-time.Hour), LostTo: t0.Add(-time.Minute),
		Records: []Record{
			ev(Event{Kind: KindSnapshot, Epoch: "e1", Seq: 4, Name: "Level", Value: json.RawMessage("1.5"), Quality: 1, Time: t0}),
			ev(Event{Kind: KindChange, Epoch: "e1", Seq: 5, Name: "Level", Value: json.RawMessage("2.5"), Quality: 1, Time: t0.Add(time.Second)}),
			ev(Event{Kind: KindGap, Epoch: "e2", Seq: 0, Time: t0.Add(2 * time.Second), Reason: "the source restarted"}),
			ev(Event{Kind: KindChange, Epoch: "e2", Seq: 1, Name: "Level", Value: json.RawMessage("null"), Quality: 3, Time: t0.Add(3 * time.Second)}),
		},
	}
	for range 2 { // sent twice: stored once
		if err := sink.Send(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := sink.History(ctx, "Level", t0.Add(-2*time.Hour), t0.Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s:%s:%s", r.Kind, r.Name, string(r.Value)))
	}
	want := []string{"lost::", "snapshot:Level:1.5", "change:Level:2.5", "gap::", "change:Level:"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("history:\n got %v\nwant %v", got, want)
	}
	if rows[0].Reason == "" || !rows[1].Time.Equal(t0) {
		t.Fatalf("rows: %+v", rows[:2])
	}
	if err := sink.Teardown(ctx); err != nil {
		t.Fatal(err)
	}
}

// switchable wraps a sink that can be cut off, as a network outage would.
type switchable struct {
	Sink
	down atomic.Bool
}

func (s *switchable) Send(ctx context.Context, b Batch) error {
	if s.down.Load() {
		return errors.New("dial tcp: connection refused")
	}
	return s.Sink.Send(ctx, b)
}

func TestOpenOutageLosesNothing(t *testing.T) {
	ctx := context.Background()
	db := newLevelDB(t)
	d, _ := sqlstore.Lookup("sqlite")
	sql1, err := NewSQLSink(openSQLite(t), SQLSinkOptions{Name: "plant", SourceID: "unit-1", Dialect: d})
	if err != nil {
		t.Fatal(err)
	}
	sink := &switchable{Sink: sql1}
	sink.down.Store(true) // the database is down from the start
	path := filepath.Join(t.TempDir(), "spool.db")
	opts := Options{Source: db, Reader: db, SpoolPath: path, Sinks: []Sink{sink}, Status: db,
		Forwarder: ForwarderOptions{MinBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond}}
	svc, err := Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "collector started", func() bool { _, ok, _ := svc.Spool().State(ctx, "collector.cursor"); return ok })
	for i := 1; i <= 200; i++ {
		_ = db.SetTagValue("Level", plc.LREAL(float64(i)))
	}
	waitFor(t, "status offline", func() bool {
		r, _ := db.ReadTag("SF_Online")
		b, _ := db.ReadTag("SF_Backlog")
		return r.Value == plc.BOOL(false) && b.Value.(plc.LINT) >= 200
	})
	// Closing with the sink down keeps the backlog for the next start.
	if err := svc.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 201; i <= 250; i++ {
		_ = db.SetTagValue("Level", plc.LREAL(float64(i)))
	}
	sink.down.Store(false)
	if svc, err = Open(ctx, opts); err != nil {
		t.Fatal(err)
	}
	defer svc.Close(ctx)
	waitFor(t, "backlog sent", func() bool {
		st, _ := svc.Status(ctx)
		return st.Backlog == 0 && st.Spool.Rows == 0 && st.Online
	})
	rows, err := sql1.History(ctx, "Level", time.Time{}, time.Now().Add(time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, r := range rows {
		if r.Kind == KindChange {
			changes = append(changes, string(r.Value))
		}
		if r.Name != "Level" {
			t.Fatalf("only Level was asked for: %+v", r)
		}
	}
	if len(changes) != 250 || changes[0] != "1" || changes[249] != "250" {
		t.Fatalf("every change once, in order: %d, %v ... %v", len(changes), changes[:3], changes[len(changes)-3:])
	}
	for _, r := range rows {
		if len(r.Name) > 3 && r.Name[:3] == "SF_" {
			t.Fatalf("status tags are never collected: %+v", r)
		}
	}
	waitFor(t, "status online", func() bool { r, _ := db.ReadTag("SF_Online"); return r.Value == plc.BOOL(true) })
}

// hangingSink blocks until its context ends, as a send over a pulled cable.
type hangingSink struct{ calls atomic.Int64 }

func (h *hangingSink) Name() string { return "hang" }
func (h *hangingSink) Send(ctx context.Context, _ Batch) error {
	h.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

func TestForwarderSendTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := openTestSpool(t, filepath.Join(t.TempDir(), "spool.db"), SpoolOptions{Sinks: []string{"hang"}})
	defer s.Close()
	sink := &hangingSink{}
	f := NewForwarder(s, sink, ForwarderOptions{SendTimeout: 20 * time.Millisecond, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	go f.Run(ctx)
	if err := s.Append(ctx, recs("x", 1, time.Now()), nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "retries after timeouts", func() bool { return sink.calls.Load() >= 3 })
	if st := f.Status(); st.Online || st.LastError == "" {
		t.Fatalf("a hung send is a failure: %+v", st)
	}
}

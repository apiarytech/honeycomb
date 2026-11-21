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
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"time"

	"github.com/apiarytech/honeycomb"
)

// Kinds of Event.
const (
	// KindChange is a tag's change from the change feed.
	KindChange = "change"
	// KindSnapshot is a tag's value read after a gap (or at the first
	// start), not a change: history before it may be missing.
	KindSnapshot = "snapshot"
	// KindGap marks where changes may be missing: the source restarted, or
	// the collector fell behind its change feed.
	KindGap = "gap"
)

// Event is the payload of a collector's records, in JSON.
type Event struct {
	Kind  string `json:"kind"`
	Epoch string `json:"epoch"`
	// Seq is the change's sequence in the feed; for a snapshot or a gap,
	// the feed's position when it was taken.
	Seq     uint64            `json:"seq"`
	Name    string            `json:"name,omitempty"`
	Value   json.RawMessage   `json:"value,omitempty"`
	Quality honeycomb.Quality `json:"quality"`
	Time    time.Time         `json:"time"` // the device's time, else the time of the write
	Reason  string            `json:"reason,omitempty"`
}

// DecodeEvent reads a collector's record.
func DecodeEvent(r Record) (Event, error) {
	var e Event
	if err := json.Unmarshal(r.Payload, &e); err != nil {
		return e, fmt.Errorf("forward: record %d: %w", r.ID, err)
	}
	return e, nil
}

// Collector defaults.
const (
	DefaultWait        = 30 * time.Second
	DefaultCommitEvery = 250 * time.Millisecond
	// cursorEvery: a position with no record to keep (every change inside
	// a deadband) is saved at most this often, to spare the flash.
	cursorEvery     = 10 * time.Second
	collectRetryMin = time.Second
	collectRetryMax = 30 * time.Second
)

// CollectorOptions configures a Collector.
type CollectorOptions struct {
	// Source is the change feed: a TagDatabase, or a NetworkDatabaseClient
	// for a database in another process.
	Source honeycomb.ChangeSource
	// Reader reads current values after a gap; nil records the gap only.
	Reader honeycomb.TagReader
	// List returns every tag's name, for the values read after a gap when
	// Names is empty. Nil uses Reader's tags when Reader is a TagDatabase.
	List func(ctx context.Context) ([]string, error)
	// Names limits the collection to these tags; empty collects every tag.
	Names []string
	// Exclude drops tags matching these patterns (path.Match), e.g. the
	// forwarder's own status tags.
	Exclude []string
	// Deadbands thin numeric changes; empty keeps every change.
	Deadbands []Deadband
	// Wait is how long one poll of the feed waits for a change
	// (DefaultWait when 0). Max is the most changes per poll (1000 when 0).
	Wait time.Duration
	Max  int
	// CommitEvery: after a poll that did not fill a batch, the collector
	// waits this long, so changes are committed in groups and the flash is
	// written less often (DefaultCommitEvery when 0; negative: no wait).
	CommitEvery time.Duration
	// Key names the collector's position in the spool, so several
	// collectors can share one; default "collector".
	Key     string
	OnError func(error)
}

// Collector copies a change feed into a spool. It saves its position in
// the same transaction as the records, so after a crash it carries on
// where the spool ends: no change twice, none missing. Where the feed
// cannot give the missing changes (the source restarted, or the collector
// fell more than the feed's buffer behind), it records a gap and the tags'
// current values.
type Collector struct {
	spool *Spool
	opts  CollectorOptions
	last  map[string]kept // the last value kept per tag, for deadbands
}

// cursor is a collector's saved position.
type cursor struct {
	Epoch string `json:"epoch"`
	Next  uint64 `json:"next"`
}

// NewCollector returns a collector from opts.Source into spool.
func NewCollector(spool *Spool, opts CollectorOptions) (*Collector, error) {
	if opts.Source == nil {
		return nil, errors.New("forward: a collector needs a source")
	}
	if err := checkDeadbands(opts.Deadbands); err != nil {
		return nil, err
	}
	for _, p := range opts.Exclude {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			return nil, fmt.Errorf("forward: exclude pattern %q is invalid", p)
		}
	}
	if opts.Wait <= 0 {
		opts.Wait = DefaultWait
	}
	if opts.Max <= 0 {
		opts.Max = 1000
	}
	if opts.CommitEvery == 0 {
		opts.CommitEvery = DefaultCommitEvery
	}
	if opts.Key == "" {
		opts.Key = "collector"
	}
	if opts.OnError == nil {
		opts.OnError = func(error) {}
	}
	if opts.List == nil {
		if db, ok := opts.Reader.(*honeycomb.TagDatabase); ok {
			opts.List = func(context.Context) ([]string, error) {
				var names []string
				for _, t := range db.GetAllTags() {
					names = append(names, t.Name)
				}
				return names, nil
			}
		}
	}
	return &Collector{spool: spool, opts: opts, last: map[string]kept{}}, nil
}

// Run collects until ctx ends.
func (c *Collector) Run(ctx context.Context) {
	retry := collectRetryMin
	fail := func(err error) {
		c.opts.OnError(err)
		select {
		case <-ctx.Done():
		case <-time.After(retry):
		}
		retry = min(retry*2, collectRetryMax)
	}

	var cur cursor
	for ctx.Err() == nil {
		v, ok, err := c.spool.State(ctx, c.stateKey())
		if err == nil && ok {
			err = json.Unmarshal([]byte(v), &cur)
		}
		if err == nil {
			break
		}
		fail(err)
	}

	lastSaved, saved := time.Now(), cur
	defer func() {
		// Keep the position reached, so a restart does not read again what
		// the deadbands already thinned.
		if cur != saved {
			state, _ := json.Marshal(cur)
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := c.spool.SetState(sctx, map[string]string{c.stateKey(): string(state)}); err != nil {
				c.opts.OnError(err)
			}
		}
	}()
	for ctx.Err() == nil {
		b, err := c.opts.Source.Changes(ctx, honeycomb.ChangesRequest{
			Since: cur.Next, Epoch: cur.Epoch, Names: c.opts.Names, Wait: c.opts.Wait, Max: c.opts.Max,
		})
		if err != nil {
			if ctx.Err() == nil {
				fail(fmt.Errorf("forward: change feed: %w", err))
			}
			continue
		}
		recs, last, err := c.records(ctx, cur, b)
		if err != nil {
			if ctx.Err() == nil {
				fail(err)
			}
			continue
		}
		next := cursor{Epoch: b.Epoch, Next: b.Next}
		// A position with nothing to keep is saved now and then only: after
		// a crash the changes since are read again and thinned again.
		if len(recs) > 0 || next.Epoch != cur.Epoch || (next != cur && time.Since(lastSaved) >= cursorEvery) {
			state, _ := json.Marshal(next)
			if err := c.spool.Append(ctx, recs, map[string]string{c.stateKey(): string(state)}); err != nil {
				if ctx.Err() == nil {
					fail(err)
				}
				continue // the same batch again: the position did not move
			}
			lastSaved, saved = time.Now(), next
		}
		c.last = last
		cur = next
		retry = collectRetryMin
		if c.opts.CommitEvery > 0 && len(b.Changes) < c.opts.Max {
			select {
			case <-ctx.Done():
			case <-time.After(c.opts.CommitEvery):
			}
		}
	}
}

func (c *Collector) stateKey() string { return c.opts.Key + ".cursor" }

// records turns a batch into records, and returns the deadbands' memory as
// it will be once they are committed.
func (c *Collector) records(ctx context.Context, cur cursor, b honeycomb.ChangeBatch) ([]Record, map[string]kept, error) {
	// last is copied before its first change, so a batch that fails to
	// commit leaves c.last as it was.
	last, copied := c.last, false
	track := len(c.opts.Deadbands) > 0
	var recs []Record
	now := time.Now().UTC()
	if cur.Epoch == "" || b.Gap {
		// The first start, or changes are missing: the current values.
		last, copied = map[string]kept{}, true
		if cur.Epoch != "" {
			reason := "the source restarted"
			if b.Epoch == cur.Epoch {
				reason = fmt.Sprintf("changes after %d lost: the collector fell behind the change feed", cur.Next)
			}
			rec, err := c.record(Event{Kind: KindGap, Epoch: b.Epoch, Seq: b.Next, Time: now, Reason: reason})
			if err != nil {
				return nil, nil, err
			}
			recs = append(recs, rec)
		}
		snap, err := c.snapshot(ctx, b, last, now)
		if err != nil {
			return nil, nil, err
		}
		recs = append(recs, snap...)
	}
	for _, ch := range b.Changes {
		if c.excluded(ch.Name) {
			continue
		}
		if !c.keep(last, ch.Name, ch.Value, ch.Quality) {
			continue
		}
		if track {
			if !copied {
				last, copied = clone(last), true
			}
			last[ch.Name] = keptOf(ch.Value, ch.Quality)
		}
		at := ch.Timestamp
		if at.IsZero() {
			at = now
		}
		rec, err := c.event(KindChange, b.Epoch, ch.Seq, ch.Name, ch.Value, ch.Quality, at)
		if err != nil {
			return nil, nil, err
		}
		recs = append(recs, rec)
	}
	return recs, last, nil
}

// snapshot reads the current values of the collected tags.
func (c *Collector) snapshot(ctx context.Context, b honeycomb.ChangeBatch, last map[string]kept, now time.Time) ([]Record, error) {
	if c.opts.Reader == nil {
		return nil, nil
	}
	names := c.opts.Names
	if len(names) == 0 {
		if c.opts.List == nil {
			return nil, nil
		}
		var err error
		if names, err = c.opts.List(ctx); err != nil {
			return nil, fmt.Errorf("forward: list tags after a gap: %w", err)
		}
	}
	names = slices.DeleteFunc(slices.Clone(names), c.excluded)
	sort.Strings(names)
	readings, err := c.opts.Reader.ReadTags(ctx, names)
	if err != nil && len(readings) == 0 {
		return nil, fmt.Errorf("forward: read tags after a gap: %w", err)
	}
	if err != nil {
		c.opts.OnError(fmt.Errorf("forward: read tags after a gap: %w", err)) // the others are recorded
	}
	var recs []Record
	for _, name := range names {
		rd, ok := readings[name]
		if !ok || rd.Quality == honeycomb.QualityBad && rd.Value == nil {
			continue
		}
		at := rd.Timestamp
		if at.IsZero() {
			at = now
		}
		rec, err := c.event(KindSnapshot, b.Epoch, b.Next, name, rd.Value, rd.Quality, at)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
		last[name] = keptOf(rd.Value, rd.Quality)
	}
	return recs, nil
}

// keep reports whether a change passes its deadband.
func (c *Collector) keep(last map[string]kept, name string, v any, q honeycomb.Quality) bool {
	d, ok := deadbandFor(c.opts.Deadbands, name)
	if !ok {
		return true
	}
	prev, seen := last[name]
	return !seen || d.beyond(prev, v, q)
}

func (c *Collector) excluded(name string) bool {
	for _, p := range c.opts.Exclude {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

func (c *Collector) event(kind, epoch string, seq uint64, name string, v any, q honeycomb.Quality, at time.Time) (Record, error) {
	value, err := honeycomb.MarshalJSONSafe(v)
	if err != nil {
		return Record{}, fmt.Errorf("forward: tag %s: %w", name, err)
	}
	return c.record(Event{Kind: kind, Epoch: epoch, Seq: seq, Name: name, Value: value, Quality: q, Time: at.UTC()})
}

func (c *Collector) record(e Event) (Record, error) {
	payload, err := json.Marshal(e)
	if err != nil {
		return Record{}, fmt.Errorf("forward: encode %s: %w", e.Kind, err)
	}
	return Record{Kind: e.Kind, Time: e.Time, Payload: payload}, nil
}

func keptOf(v any, q honeycomb.Quality) kept {
	num, ok := number(v)
	return kept{num: num, numeric: ok, quality: q}
}

func clone(m map[string]kept) map[string]kept {
	out := make(map[string]kept, len(m)+8)
	for k, v := range m {
		out[k] = v
	}
	return out
}

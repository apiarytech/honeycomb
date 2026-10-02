/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// This file, changefeed.go, keeps a bounded, in-memory history of tag changes
// so a client can receive every change, in order, instead of sampling values.
// Each change has a sequence number; a client asks for the changes after the
// last sequence it saw and may wait (long-poll) until there are some. Unlike
// subscriptions, which keep only the latest update, the feed keeps every
// change until the buffer wraps.
package honeycomb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"reflect"
	"sync"
	"time"
)

// DefaultChangeFeedCapacity is the number of changes a TagDatabase keeps.
const DefaultChangeFeedCapacity = 10000

// MaxChangesWait caps how long a Changes call waits for new changes.
const MaxChangesWait = time.Minute

// ErrChangesUnsupported is returned by a NetworkDatabaseClient whose server has
// no change feed. Callers fall back to reading tags with ReadTag.
var ErrChangesUnsupported = errors.New("honeycomb: the server has no change feed")

// Change is one recorded change of a top-level tag's value or quality.
type Change struct {
	Seq       uint64    `json:"seq"`
	Name      string    `json:"name"`
	Value     any       `json:"value"`
	Quality   Quality   `json:"quality"`
	Timestamp time.Time `json:"timestamp"`
}

// ChangesRequest asks for the changes after Since.
type ChangesRequest struct {
	// Since is the sequence of the last change already seen: the Next of the
	// previous batch.
	Since uint64 `json:"since"`
	// Epoch is the Epoch of the previous batch. Empty starts a new reader: the
	// call returns at once with the current position in Next and no changes,
	// and the reader then reads current values to start from. An epoch that
	// differs from the database's (the database restarted) returns the current
	// position with Gap set.
	Epoch string `json:"epoch,omitempty"`
	// Names limits the changes to these tags; empty means every tag.
	Names []string `json:"names,omitempty"`
	// Wait is how long to wait for a change when there is none yet, capped at
	// MaxChangesWait. Zero returns at once.
	Wait time.Duration `json:"wait,omitempty"`
	// Max limits the number of changes returned; 0 means 1000.
	Max int `json:"max,omitempty"`
}

// ChangeBatch is the answer to a ChangesRequest.
type ChangeBatch struct {
	Epoch   string   `json:"epoch"`
	Next    uint64   `json:"next"` // pass as Since in the next request
	Gap     bool     `json:"gap"`  // changes were lost: read current values before applying Changes
	Changes []Change `json:"changes"`
}

// ChangeSource is implemented by TagDatabase and NetworkDatabaseClient.
type ChangeSource interface {
	Changes(ctx context.Context, req ChangesRequest) (ChangeBatch, error)
}

// changeFeed is a ring buffer of changes.
type changeFeed struct {
	mu      sync.Mutex
	epoch   string
	buf     []Change
	start   int    // index of the oldest change
	count   int    // number of changes held
	nextSeq uint64 // sequence of the next change; the first change is 1
	wake    chan struct{}
	last    map[string]Change // each tag's last recorded change, to skip unchanged rewrites
}

func newChangeFeed(capacity int) *changeFeed {
	var id [8]byte
	_, _ = rand.Read(id[:])
	return &changeFeed{
		epoch:   hex.EncodeToString(id[:]),
		buf:     make([]Change, capacity),
		nextSeq: 1,
		wake:    make(chan struct{}),
		last:    make(map[string]Change),
	}
}

// record appends c unless it repeats the tag's last recorded value and quality,
// e.g. a poller rewriting an unchanged value every cycle, which would otherwise
// push real changes out of the buffer.
func (f *changeFeed) record(c Change) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if last, ok := f.last[c.Name]; ok && last.Quality == c.Quality && reflect.DeepEqual(last.Value, c.Value) {
		return
	}
	f.last[c.Name] = c
	f.appendLocked(c)
}

// forget drops a removed tag's last recorded change, so a tag added later under
// the same name records its first write.
func (f *changeFeed) forget(name string) {
	f.mu.Lock()
	delete(f.last, name)
	f.mu.Unlock()
}

// appendLocked adds c to the buffer. It must be called with f.mu held.
func (f *changeFeed) appendLocked(c Change) {
	c.Seq = f.nextSeq
	f.nextSeq++
	end := (f.start + f.count) % len(f.buf)
	f.buf[end] = c
	if f.count < len(f.buf) {
		f.count++
	} else {
		f.start = (f.start + 1) % len(f.buf)
	}
	close(f.wake) // wake every waiting reader
	f.wake = make(chan struct{})
}

// collect returns the matching changes after since, and the channel that is
// closed by the next append. It must be called with f.mu held.
func (f *changeFeed) collect(since uint64, names map[string]bool, limit int) (ChangeBatch, chan struct{}) {
	head := f.nextSeq - 1
	oldest := f.nextSeq - uint64(f.count) // sequence of the oldest change held
	batch := ChangeBatch{Epoch: f.epoch, Next: since}
	if since > head {
		return ChangeBatch{Epoch: f.epoch, Next: head, Gap: true}, f.wake
	}
	if since+1 < oldest {
		batch.Gap = true
		batch.Next = oldest - 1
	}
	for seq := batch.Next + 1; seq <= head; seq++ {
		c := f.buf[(f.start+int(seq-oldest))%len(f.buf)]
		batch.Next = seq
		if len(names) == 0 || names[c.Name] {
			batch.Changes = append(batch.Changes, c)
			if len(batch.Changes) == limit {
				break
			}
		}
	}
	return batch, f.wake
}

// SetChangeFeedCapacity sets how many changes the database keeps, discarding
// those it holds. Call it before readers start. The default is
// DefaultChangeFeedCapacity.
func (db *TagDatabase) SetChangeFeedCapacity(n int) {
	if n < 1 {
		n = 1
	}
	db.feedOnce.Do(func() {})
	db.feed.Store(newChangeFeed(n))
}

func (db *TagDatabase) changeFeed() *changeFeed {
	db.feedOnce.Do(func() {
		if db.feed.Load() == nil {
			db.feed.Store(newChangeFeed(DefaultChangeFeedCapacity))
		}
	})
	return db.feed.Load()
}

// recordChange adds a tag's current state to the change feed. The caller must
// hold t.valMu, so the value is copied consistently.
func (db *TagDatabase) recordChange(t *Tag) {
	db.changeFeed().record(Change{
		Name:      t.Name,
		Value:     cloneValue(t.presentedValue()),
		Quality:   t.Quality,
		Timestamp: t.Timestamp,
	})
}

// cloneValue returns a deep copy of an array or UDT value. Array element and
// UDT field writes modify the stored value in place, so a recorded change must
// not share it, or its history would change after the fact. Other values are
// immutable and returned as is.
func cloneValue(v any) any {
	if v == nil {
		return nil
	}
	return cloneReflect(reflect.ValueOf(v)).Interface()
}

func cloneReflect(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneReflect(v.Index(i)))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneReflect(v.Index(i)))
		}
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(cloneReflect(v.Elem()))
		return out
	case reflect.Struct:
		// Copy the whole struct (including unexported fields, e.g. inside a
		// time.Time), then deep-copy the exported fields that can hold references.
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if f := out.Field(i); f.CanSet() {
				f.Set(cloneReflect(v.Field(i)))
			}
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			out.SetMapIndex(iter.Key(), cloneReflect(iter.Value()))
		}
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(cloneReflect(v.Elem()))
		return out
	}
	return v
}

// Changes returns the changes after req.Since, waiting up to req.Wait for at
// least one. Changes are those of top-level tags: a write to a UDT field or an
// array element records the whole tag. Each change holds its own copy of the
// value. A write that leaves both value and quality unchanged is not a change
// and is not recorded.
func (db *TagDatabase) Changes(ctx context.Context, req ChangesRequest) (ChangeBatch, error) {
	feed := db.changeFeed()
	limit := req.Max
	if limit <= 0 {
		limit = 1000
	}
	wait := min(req.Wait, MaxChangesWait)
	var names map[string]bool
	if len(req.Names) > 0 {
		names = make(map[string]bool, len(req.Names))
		for _, n := range req.Names {
			names[n] = true
		}
	}

	feed.mu.Lock()
	if req.Epoch != feed.epoch {
		// A new reader, or one whose database restarted: start from now.
		batch := ChangeBatch{Epoch: feed.epoch, Next: feed.nextSeq - 1, Gap: req.Epoch != ""}
		feed.mu.Unlock()
		return batch, nil
	}
	batch, wake := feed.collect(req.Since, names, limit)
	feed.mu.Unlock()
	if len(batch.Changes) > 0 || batch.Gap || wait <= 0 {
		return batch, nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return batch, ctx.Err()
		case <-timer.C:
			return batch, nil
		case <-wake:
		}
		feed.mu.Lock()
		batch, wake = feed.collect(batch.Next, names, limit)
		feed.mu.Unlock()
		if len(batch.Changes) > 0 || batch.Gap {
			return batch, nil
		}
	}
}

// RegisteredDatabase returns a database registered with RegisterDatabase, e.g.
// to read its change feed: test it for ChangeSource.
func (db *TagDatabase) RegisteredDatabase(id string) (DatabaseAccessor, bool) {
	return db.getDatabase(id)
}

/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// This file, persistence.go, connects a TagDatabase to a durable TagStore
// (SQLite, PostgreSQL, CockroachDB, MySQL, SQL Server, ...). The in-memory TagDatabase
// stays the system of record while the PLC runs; the Persister restores it
// at power-up, writes changed tags behind the scan cycle at runtime, and
// writes a final snapshot at shutdown.
package honeycomb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"
)

// StoredTag is the storage-neutral representation of a tag. TagStore
// implementations persist it as-is; values are JSON so that every backend
// can store primitives, arrays and UDTs in a single column.
type StoredTag struct {
	Name          string
	DataType      DataType
	TypeInfo      json.RawMessage // JSON of *TypeInfo; nil for remote aliases.
	Value         json.RawMessage // JSON of the tag's actual (not forced) value.
	Quality       Quality         // Quality of Value when it was saved.
	Timestamp     time.Time       // When Value or Quality last changed (the device time if a driver supplied it); zero if unknown.
	Alias         string
	Description   string
	DirectAddress string
	Retain        bool
	Constant      bool
	Forced        bool
	ForceValue    json.RawMessage // JSON of the force value; nil if not forced.
	RemoteDBID    string
	RemoteTagName string
	UpdatedAt     time.Time // When the record was saved.
}

// TagStore is the contract a durable backend must satisfy. Implementations
// must be safe for concurrent use. See the store/sqlstore package for a
// database/sql implementation covering SQLite, PostgreSQL, CockroachDB, MySQL and SQL Server.
type TagStore interface {
	// LoadTags returns every tag held by the store.
	LoadTags(ctx context.Context) ([]StoredTag, error)
	// SaveTags inserts or updates the given tags atomically.
	SaveTags(ctx context.Context, tags []StoredTag) error
	// DeleteTags removes the named tags. Unknown names are ignored.
	DeleteTags(ctx context.Context, names []string) error
	// Close releases the store's resources.
	Close() error
}

// PersistScope selects which tags a Persister writes to its TagStore.
type PersistScope int

const (
	// PersistRetainOnly persists only tags marked Retain (IEC 61131-3 RETAIN semantics).
	PersistRetainOnly PersistScope = iota
	// PersistAll persists every tag in the database.
	PersistAll
)

// PersistOptions configures a Persister.
type PersistOptions struct {
	// Scope selects which tags are persisted. Defaults to PersistRetainOnly.
	Scope PersistScope
	// FlushInterval is how often changed tags are written at runtime. Writes are
	// coalesced, so a tag written every scan is stored at most once per interval.
	// Defaults to one second.
	FlushInterval time.Duration
	// BatchSize is the maximum number of tags written per SaveTags call. Defaults to 500.
	BatchSize int
	// RestoreDefinitions makes Restore create tags that exist in the store but not
	// in the database. When false, only values of already-configured tags are restored.
	RestoreDefinitions bool
	// RestoreForces makes Restore re-apply force state saved at shutdown.
	RestoreForces bool
	// OnError receives errors from background flushes. Defaults to discarding them.
	OnError func(error)
}

// Persister synchronizes a TagDatabase with a TagStore. Create one with
// TagDatabase.AttachStore. The typical PLC lifecycle is:
//
//	p, _ := db.AttachStore(store, opts) // after configuring tags
//	p.Restore(ctx)                      // power-up: load persisted values
//	p.Start()                           // runtime: write-behind of changed tags
//	...
//	p.Close(ctx)                        // shutdown: final snapshot, close store
type Persister struct {
	db    *TagDatabase
	store TagStore
	opts  PersistOptions

	mu      sync.Mutex          // guards the fields below
	dirty   map[string]struct{} // tags changed since the last flush
	removed map[string]struct{} // tags removed since the last flush
	stop    context.CancelFunc
	done    chan struct{}
	closed  bool

	flushMu sync.Mutex // serializes flushes so batches are written in order
}

// AttachStore connects a TagStore to the database and returns the Persister that
// manages it. Only one store may be attached at a time; Close detaches it.
func (db *TagDatabase) AttachStore(store TagStore, opts PersistOptions) (*Persister, error) {
	if store == nil {
		return nil, errors.New("AttachStore: store is nil")
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	if opts.OnError == nil {
		opts.OnError = func(error) {}
	}

	p := &Persister{
		db:      db,
		store:   store,
		opts:    opts,
		dirty:   make(map[string]struct{}),
		removed: make(map[string]struct{}),
	}
	if !db.persister.CompareAndSwap(nil, p) {
		return nil, errors.New("AttachStore: a TagStore is already attached to this database")
	}
	return p, nil
}

// markChanged records that a tag must be written on the next flush.
// It is called from the database's write paths and is a no-op without a store.
func (db *TagDatabase) markChanged(name string) {
	if p := db.persister.Load(); p != nil {
		p.mu.Lock()
		delete(p.removed, name)
		p.dirty[name] = struct{}{}
		p.mu.Unlock()
	}
}

// markRemoved records that a tag must be deleted from the store on the next flush.
func (db *TagDatabase) markRemoved(name string) {
	if p := db.persister.Load(); p != nil {
		p.mu.Lock()
		delete(p.dirty, name)
		p.removed[name] = struct{}{}
		p.mu.Unlock()
	}
}

// Restore loads persisted tags from the store into the database. Call it once at
// power-up, after tags are configured and before the scan cycle starts.
// Constant tags and remote aliases keep their configured values.
// A value saved as Good is restored as Uncertain, since the process may have
// changed while the runtime was down; other qualities are restored unchanged.
func (p *Persister) Restore(ctx context.Context) error {
	records, err := p.store.LoadTags(ctx)
	if err != nil {
		return fmt.Errorf("Restore: %w", err)
	}

	var errs []error
	for _, rec := range records {
		if err := p.restoreTag(rec); err != nil {
			errs = append(errs, fmt.Errorf("Restore: tag '%s': %w", rec.Name, err))
		}
	}

	// Restoring goes through the normal write paths, which mark tags dirty. The
	// store already holds these values, so there is nothing to write back.
	p.mu.Lock()
	clear(p.dirty)
	p.mu.Unlock()

	return errors.Join(errs...)
}

func (p *Persister) restoreTag(rec StoredTag) error {
	val, found := p.db.tags.Load(rec.Name)
	if !found {
		if !p.opts.RestoreDefinitions {
			return nil
		}
		tag, err := p.tagFromStored(rec)
		if err != nil {
			return err
		}
		return p.db.AddTag(tag)
	}

	tag := val.(*Tag)
	tag.valMu.RLock()
	typeInfo, current := tag.TypeInfo, tag.Value
	skip := tag.RemoteAlias != nil || tag.Constant
	tag.valMu.RUnlock()
	if skip {
		return nil
	}

	value, err := decodeStoredValue(typeInfo, current, rec.Value)
	if err != nil {
		return err
	}
	if value != nil {
		if err := p.db.setSimpleTagValue(rec.Name, value, rec.Quality.restored(), rec.Timestamp); err != nil {
			return err
		}
	}

	if p.opts.RestoreForces && rec.Forced {
		force, err := decodeStoredValue(typeInfo, current, rec.ForceValue)
		if err != nil {
			return fmt.Errorf("force value: %w", err)
		}
		if force == nil {
			_, err = p.db.SetTagForced(rec.Name, true)
		} else {
			_, err = p.db.SetTagForceValue(rec.Name, force)
		}
		return err
	}
	return nil
}

// tagFromStored rebuilds a full tag definition from a stored record.
func (p *Persister) tagFromStored(rec StoredTag) (*Tag, error) {
	tag := &Tag{
		Name:          rec.Name,
		Alias:         rec.Alias,
		Description:   rec.Description,
		DirectAddress: rec.DirectAddress,
		Retain:        rec.Retain,
		Constant:      rec.Constant,
	}
	if rec.RemoteDBID != "" {
		tag.RemoteAlias = &RemoteAliasInfo{DBID: rec.RemoteDBID, TagName: rec.RemoteTagName}
		return tag, nil
	}

	typeInfo := &TypeInfo{DataType: rec.DataType}
	if len(rec.TypeInfo) > 0 {
		if err := json.Unmarshal(rec.TypeInfo, typeInfo); err != nil {
			return nil, fmt.Errorf("decode type info: %w", err)
		}
	}
	// JSON decodes Min/Max as float64; convert them back to the tag's Go type
	// so that subrange checks keep working for integer tags.
	if goType, ok := getGoType(typeInfo.DataType); ok {
		for _, bound := range []*interface{}{&typeInfo.Min, &typeInfo.Max} {
			if *bound != nil {
				converted, err := convertTo(*bound, goType)
				if err != nil {
					return nil, fmt.Errorf("decode subrange: %w", err)
				}
				*bound = converted
			}
		}
	}
	tag.TypeInfo = typeInfo

	value, err := decodeStoredValue(typeInfo, nil, rec.Value)
	if err != nil {
		return nil, err
	}
	tag.Value = value
	tag.Quality = rec.Quality.restored()
	tag.Timestamp = rec.Timestamp

	if p.opts.RestoreForces && rec.Forced {
		force, err := decodeStoredValue(typeInfo, nil, rec.ForceValue)
		if err != nil {
			return nil, fmt.Errorf("force value: %w", err)
		}
		tag.Force = &ForceInfo{Value: force}
	}
	return tag, nil
}

// Start begins writing changed tags to the store every FlushInterval.
// Calling Start more than once, or after Close, has no effect.
func (p *Persister) Start() {
	p.mu.Lock()
	if p.done != nil || p.closed {
		p.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.stop = cancel
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(p.opts.FlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// An error caused by Close cancelling ctx is not reported: the
				// tags stay queued and Close's final snapshot writes them.
				if err := p.Flush(ctx); err != nil && ctx.Err() == nil {
					p.opts.OnError(err)
				}
			}
		}
	}()
}

// Flush writes every tag changed since the last flush, and deletes removed tags.
// Tags whose write fails stay queued and are retried on the next flush.
func (p *Persister) Flush(ctx context.Context) error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	p.mu.Lock()
	dirty, removed := p.dirty, p.removed
	p.dirty, p.removed = make(map[string]struct{}), make(map[string]struct{})
	p.mu.Unlock()

	if len(dirty) == 0 && len(removed) == 0 {
		return nil
	}

	var errs []error
	records := make([]StoredTag, 0, len(dirty))
	for name := range dirty {
		val, found := p.db.tags.Load(name)
		if !found {
			removed[name] = struct{}{}
			continue
		}
		rec, inScope, err := p.snapshot(val.(*Tag))
		if err != nil {
			// Not requeued: a value that cannot be encoded will not succeed on retry.
			errs = append(errs, fmt.Errorf("Flush: tag '%s': %w", name, err))
			continue
		}
		if inScope {
			records = append(records, rec)
		}
	}

	for start := 0; start < len(records); start += p.opts.BatchSize {
		end := min(start+p.opts.BatchSize, len(records))
		if err := p.store.SaveTags(ctx, records[start:end]); err != nil {
			p.requeue(records[start:], removed)
			return errors.Join(append(errs, fmt.Errorf("Flush: save: %w", err))...)
		}
	}

	if len(removed) > 0 {
		names := make([]string, 0, len(removed))
		for name := range removed {
			names = append(names, name)
		}
		if err := p.store.DeleteTags(ctx, names); err != nil {
			p.requeue(nil, removed)
			errs = append(errs, fmt.Errorf("Flush: delete: %w", err))
		}
	}
	return errors.Join(errs...)
}

// requeue puts unwritten work back, unless a newer change superseded it.
func (p *Persister) requeue(records []StoredTag, removed map[string]struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, rec := range records {
		if _, gone := p.removed[rec.Name]; !gone {
			p.dirty[rec.Name] = struct{}{}
		}
	}
	for name := range removed {
		if _, back := p.dirty[name]; !back {
			p.removed[name] = struct{}{}
		}
	}
}

// SaveAll writes every in-scope tag to the store, regardless of whether it changed.
func (p *Persister) SaveAll(ctx context.Context) error {
	p.mu.Lock()
	p.db.tags.Range(func(key, _ interface{}) bool {
		p.dirty[key.(string)] = struct{}{}
		return true
	})
	p.mu.Unlock()
	return p.Flush(ctx)
}

// Close stops background writes, saves a final snapshot of all in-scope tags,
// detaches the store from the database and closes it. Call it at shutdown.
func (p *Persister) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	stop, done := p.stop, p.done
	p.mu.Unlock()

	if stop != nil {
		stop()
		<-done
	}

	err := p.SaveAll(ctx)
	p.db.persister.CompareAndSwap(p, nil)
	return errors.Join(err, p.store.Close())
}

// snapshot captures a tag's state. The bool result reports whether the tag is in scope.
func (p *Persister) snapshot(tag *Tag) (StoredTag, bool, error) {
	tag.valMu.RLock()
	defer tag.valMu.RUnlock()

	if p.opts.Scope == PersistRetainOnly && !tag.Retain {
		return StoredTag{}, false, nil
	}

	rec := StoredTag{
		Name:          tag.Name,
		Alias:         tag.Alias,
		Description:   tag.Description,
		DirectAddress: tag.DirectAddress,
		Retain:        tag.Retain,
		Constant:      tag.Constant,
		Forced:        tag.Force != nil,
		UpdatedAt:     time.Now().UTC(),
	}

	var err error
	if tag.RemoteAlias != nil {
		// The value lives in the remote database; only the definition is stored.
		rec.RemoteDBID = tag.RemoteAlias.DBID
		rec.RemoteTagName = tag.RemoteAlias.TagName
		return rec, true, nil
	}
	if tag.TypeInfo != nil {
		rec.DataType = tag.TypeInfo.DataType
		if rec.TypeInfo, err = json.Marshal(tag.TypeInfo); err != nil {
			return StoredTag{}, false, fmt.Errorf("encode type info: %w", err)
		}
	}
	// Store the actual value, never the force value, so a restart does not turn
	// a temporary force into the tag's real value.
	if rec.Value, err = json.Marshal(tag.Value); err != nil {
		return StoredTag{}, false, fmt.Errorf("encode value: %w", err)
	}
	rec.Quality = tag.Quality
	rec.Timestamp = tag.Timestamp
	if tag.Force != nil && tag.Force.Value != nil {
		if rec.ForceValue, err = json.Marshal(tag.Force.Value); err != nil {
			return StoredTag{}, false, fmt.Errorf("encode force value: %w", err)
		}
	}
	return rec, true, nil
}

// decodeStoredValue decodes a JSON value into the Go type of the tag. When the
// tag already holds a value its exact type is reused, which round-trips arrays
// and UDTs; otherwise the type is derived from typeInfo.
func decodeStoredValue(typeInfo *TypeInfo, current interface{}, raw json.RawMessage) (interface{}, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var goType reflect.Type
	if current != nil {
		goType = reflect.TypeOf(current)
	} else {
		if typeInfo == nil {
			return nil, errors.New("decode value: missing type information")
		}
		var err error
		if goType, err = goTypeForDataType(typeInfo.DataType, typeInfo.ElementType); err != nil {
			return nil, err
		}
	}

	ptr := reflect.New(goType)
	if err := json.Unmarshal(raw, ptr.Interface()); err != nil {
		return nil, fmt.Errorf("decode value as %s: %w", goType, err)
	}
	return ptr.Elem().Interface(), nil
}

// goTypeForDataType maps a DataType (and array element type) to the Go type used for its values.
func goTypeForDataType(dataType, elementType DataType) (reflect.Type, error) {
	if dataType == TypeARRAY {
		elemType, err := goTypeForDataType(elementType, "")
		if err != nil {
			return nil, err
		}
		return reflect.SliceOf(elemType), nil
	}
	if goType, ok := getGoType(dataType); ok {
		return goType, nil
	}
	if _, isEnum := getEnumValues(dataType); isEnum {
		return reflect.TypeOf(""), nil
	}
	udtMu.RLock()
	udtType, isUDT := udtRegistry[dataType]
	udtMu.RUnlock()
	if isUDT {
		return reflect.PointerTo(udtType), nil
	}
	return nil, fmt.Errorf("unknown data type '%s' (is the UDT or ENUM registered?)", dataType)
}

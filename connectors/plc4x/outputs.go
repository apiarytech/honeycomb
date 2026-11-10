/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

package plc4x

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/apiarytech/honeycomb"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
)

// link tracks a connection's Output and InOut tags: which changed since they
// were last written, and what the device is known to hold.
type link struct {
	wake        chan struct{} // signalled when a tag joins pending
	unsubscribe []func()

	mu      sync.Mutex
	outputs []Binding
	pending map[string]bool   // tags changed since the last flush
	device  map[string]string // snapshot of the value the device holds, per tag
}

// watchOutputs subscribes to the connection's Output and InOut tags.
func (c *Connector) watchOutputs(cs *connState) (*link, error) {
	l := &link{
		wake:    make(chan struct{}, 1),
		pending: make(map[string]bool),
		device:  make(map[string]string),
	}
	for _, b := range cs.Bindings {
		if !b.Direction.writes() {
			continue
		}
		ch, id, err := c.db.SubscribeToTag(b.Tag)
		if err != nil {
			l.stop()
			return nil, fmt.Errorf("plc4x: connection %q: watch output: %w", cs.Name, err)
		}
		l.outputs = append(l.outputs, b)
		l.unsubscribe = append(l.unsubscribe, func() { _ = c.db.UnsubscribeFromTag(b.Tag, id) })
		go func() {
			// Notifications are only a trigger: the flush reads the tag's current
			// value, so a notification dropped by a full channel loses nothing.
			for range ch {
				l.markPending(b.Tag)
			}
		}()
	}
	return l, nil
}

func (c *Connector) stopWatching() {
	for _, cs := range c.connections {
		if cs.link != nil {
			cs.link.stop()
			cs.link = nil
		}
		cs.running.Store(false)
	}
}

func (l *link) stop() {
	for _, unsubscribe := range l.unsubscribe {
		unsubscribe()
	}
}

func (l *link) markPending(tag string) {
	l.mu.Lock()
	l.pending[tag] = true
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// startSession queues every Output for writing, so a reconnected device receives
// the commanded values. InOut tags are left to the first read instead.
func (l *link) startSession() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range l.outputs {
		if b.Direction == Output {
			delete(l.device, b.Tag)
			l.pending[b.Tag] = true
		}
	}
}

func (l *link) takePending() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	names := make([]string, 0, len(l.pending))
	for name := range l.pending {
		names = append(names, name)
	}
	clear(l.pending)
	sort.Strings(names)
	return names
}

// accept reports whether a value read from the device for an InOut tag should be
// stored, and if so records that the device holds it, so storing it does not
// trigger a write back. A tag change still waiting to be written wins over the read.
func (l *link) accept(tag string, value any) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending[tag] {
		return false
	}
	l.device[tag] = snapshot(value)
	return true
}

// changed reports whether value differs from what the device is known to hold.
func (l *link) changed(tag, snap string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	held, known := l.device[tag]
	return !known || held != snap
}

func (l *link) written(tag, snap string) {
	l.mu.Lock()
	l.device[tag] = snap
	l.mu.Unlock()
}

// snapshot captures a value for change detection. Element and field writes
// modify arrays and UDTs in place, so the value itself cannot be kept.
func snapshot(value any) string {
	if b, err := json.Marshal(value); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%#v", value)
}

// flushWrites writes every pending output whose value differs from what the
// device holds. Each tag is written in its own request, since some drivers
// (Modbus) accept only one tag per write. A forced tag writes its force value.
func (c *Connector) flushWrites(ctx context.Context, plcConn plc4go.PlcConnection, cs *connState) error {
	var errs []error
	for _, name := range cs.link.takePending() {
		value, err := c.db.GetTagValue(name)
		if err != nil {
			errs = append(errs, fmt.Errorf("output '%s': %w", name, err))
			continue
		}
		if value == nil {
			continue
		}
		snap := snapshot(value)
		if !cs.link.changed(name, snap) {
			continue // e.g. the change came from the device (echo) or only the quality changed
		}
		if err := c.write(ctx, plcConn, cs, name, cs.byTag[name].Address, value); err != nil {
			errs = append(errs, fmt.Errorf("output '%s': %w", name, err))
			continue
		}
		cs.link.written(name, snap)
		c.record(cs.Name, func(s *Status) {
			s.LastWrite = time.Now()
			s.ErrorSince = time.Time{}
			s.Writes++
		}, nil)
	}
	return errors.Join(errs...)
}

func (c *Connector) write(ctx context.Context, plcConn plc4go.PlcConnection, cs *connState, name, address string, value any) error {
	var deviceValue any
	if sc := cs.byTag[name].Scaling; sc.Kind != NoScaling {
		v, err := engineeringNumber(value)
		if err != nil {
			return err
		}
		deviceValue = rawForDevice(plcConn, name, address, sc.Unscale(v))
	} else {
		dv, err := toDevice(value)
		if err != nil {
			return err
		}
		deviceValue = dv
	}
	request, err := plcConn.WriteRequestBuilder().AddTagAddress(name, address, deviceValue).Build()
	if err != nil {
		return fmt.Errorf("build write request: %w", err)
	}
	var result apiModel.PlcWriteRequestResult
	for attempt := 1; ; attempt++ {
		r, err := execute(ctx, cs, request.Execute)
		if err != nil {
			return err
		}
		result = r
		if result.GetErr() == nil || attempt >= cs.tries() {
			break
		}
	}
	if err := result.GetErr(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if code := result.GetResponse().GetResponseCode(name); code != apiModel.PlcResponseCode_OK {
		return fmt.Errorf("device returned %s", code)
	}
	return nil
}

// safeWrite is one output's safe value (R-25), in the tag's own type.
type safeWrite struct {
	tag, address string
	value        any
}

// safeTimeout bounds writing every safe value of one connection when the
// connector stops: a device that does not answer does not hold up the stop.
var safeTimeout = 3 * time.Second

// resolveSafe reads the connection's safe values into each tag's own type:
// a value of that type is taken as it is, JSON is decoded into it.
func (c *Connector) resolveSafe(cs *connState) ([]safeWrite, error) {
	var out []safeWrite
	for _, b := range cs.Bindings {
		if b.SafeValue == nil {
			continue
		}
		current, err := c.db.GetTagValue(b.Tag)
		if err != nil {
			return nil, fmt.Errorf("plc4x: connection %q: safe value of %q: %w", cs.Name, b.Tag, err)
		}
		v, err := safeValueAs(reflect.TypeOf(honeycomb.Dereference(current)), b.SafeValue)
		if err != nil {
			return nil, fmt.Errorf("plc4x: connection %q: safe value of %q: %w", cs.Name, b.Tag, err)
		}
		out = append(out, safeWrite{tag: b.Tag, address: b.Address, value: v})
	}
	return out, nil
}

// safeValueAs is v as a value of type t.
func safeValueAs(t reflect.Type, v any) (any, error) {
	if t == nil {
		return nil, errors.New("the tag has no value to take its type from")
	}
	var raw []byte
	switch x := v.(type) {
	case json.RawMessage:
		raw = x
	case []byte:
		raw = x
	default:
		rv := reflect.ValueOf(v)
		if rv.Type() == t {
			return v, nil
		}
		if rv.Type().ConvertibleTo(t) && rv.Kind() != reflect.String && t.Kind() != reflect.String {
			return rv.Convert(t).Interface(), nil
		}
		return nil, fmt.Errorf("%v is a %s, the tag a %s", v, rv.Type(), t)
	}
	if t != timeType && t.Kind() == reflect.Struct && t.ConvertibleTo(timeType) { // DT, DATE, TOD: an RFC 3339 time
		var tm time.Time
		if err := json.Unmarshal(raw, &tm); err != nil {
			return nil, fmt.Errorf("%s is not a %s: %w", raw, t, err)
		}
		return reflect.ValueOf(tm).Convert(t).Interface(), nil
	}
	p := reflect.New(t)
	if err := json.Unmarshal(raw, p.Interface()); err != nil {
		return nil, fmt.Errorf("%s is not a %s: %w", raw, t, err)
	}
	return p.Elem().Interface(), nil
}

// writeSafe writes the connection's safe values, each once, within
// safeTimeout. A failure is recorded and the others are still written.
func (c *Connector) writeSafe(plcConn plc4go.PlcConnection, cs *connState) {
	if !plcConn.IsConnected() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), safeTimeout)
	defer cancel()
	for _, w := range cs.safe {
		if err := c.write(ctx, plcConn, cs, w.tag, w.address, w.value); err != nil {
			c.record(cs.Name, nil, fmt.Errorf("safe value of '%s': %w", w.tag, err))
			continue
		}
		c.record(cs.Name, func(s *Status) { s.LastWrite = time.Now(); s.Writes++ }, nil)
	}
}

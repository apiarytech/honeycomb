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
	"sort"
	"sync"
	"time"

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
			s.Writes++
		}, nil)
	}
	return errors.Join(errs...)
}

func (c *Connector) write(ctx context.Context, plcConn plc4go.PlcConnection, cs *connState, name, address string, value any) error {
	deviceValue, err := toDevice(value)
	if err != nil {
		return err
	}
	request, err := plcConn.WriteRequestBuilder().AddTagAddress(name, address, deviceValue).Build()
	if err != nil {
		return fmt.Errorf("build write request: %w", err)
	}
	result, err := execute(ctx, cs, request.Execute)
	if err != nil {
		return err
	}
	if err := result.GetErr(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if code := result.GetResponse().GetResponseCode(name); code != apiModel.PlcResponseCode_OK {
		return fmt.Errorf("device returned %s", code)
	}
	return nil
}

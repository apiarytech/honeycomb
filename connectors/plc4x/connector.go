/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// Package plc4x connects field devices to a honeycomb TagDatabase using
// Apache PLC4X (https://plc4x.apache.org), which provides drivers for S7,
// Modbus, EtherNet/IP, Logix, ADS, OPC UA, BACnet, KNX and more. Inputs are
// polled or subscribed into tags; outputs are written to the device when their
// tags change.
//
// It is a separate Go module so that applications which do not talk to field
// devices do not pull in PLC4X and its dependencies.
package plc4x

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	"github.com/apiarytech/honeycomb"
)

// Direction says which way a binding moves data.
type Direction uint8

const (
	// Input copies the device value into the tag. It is the default.
	Input Direction = iota
	// Output writes the tag to the device whenever the tag changes, and once
	// after every (re)connect so the device always holds the commanded value.
	Output
	// InOut does both: device changes are read into the tag and tag changes are
	// written to the device. Values read from the device are never written back
	// (echo suppression), and a tag change waiting to be written is not
	// overwritten by a read. The device's value is read first after a reconnect.
	InOut
)

func (d Direction) reads() bool  { return d != Output }
func (d Direction) writes() bool { return d != Input }

// Mode selects how a connection acquires its inputs.
type Mode uint8

const (
	// Poll reads every input each Interval. It is the default.
	Poll Mode = iota
	// ChangeOfState subscribes to the inputs; the device reports each change.
	ChangeOfState
	// Cyclic subscribes to the inputs; the device reports them every Interval.
	Cyclic
)

// Binding maps a honeycomb tag to an address on a device.
type Binding struct {
	// Tag is the honeycomb tag. It must exist and hold a value, whose Go type
	// selects the conversion. Output and InOut bindings must name a top-level
	// tag, since changes are detected by subscribing to it.
	Tag string
	// Address is the PLC4X tag address, e.g. "%DB1.DBD0:REAL" (S7) or "holding-register:1:DINT" (Modbus).
	Address string
	// Direction defaults to Input.
	Direction Direction
}

// Connection describes one field device and the tags exchanged with it.
type Connection struct {
	// Name identifies the connection in status, errors and diagnostic tags.
	Name string
	// URL is the PLC4X connection string, e.g. "s7://192.168.0.10" or "modbus-tcp://10.0.0.5:502".
	URL string
	// Interval is the polling period, the Cyclic subscription period, and how
	// often a subscribed or output-only connection is checked. Defaults to one second.
	Interval time.Duration
	// Mode selects polling or subscriptions for the inputs. If the driver cannot
	// subscribe, the connection polls instead and reports that once per connect.
	Mode Mode
	// Bindings lists the tags exchanged with the device.
	Bindings []Binding
}

// Status reports the health of a connection.
type Status struct {
	Connected  bool
	Subscribed bool      // inputs arrive by subscription rather than polling
	LastRead   time.Time // time of the last successful poll or subscription event
	LastWrite  time.Time // time of the last successful output write
	LastError  error     // most recent error, nil after a fully successful poll or event
	Reads      uint64    // successful polls and subscription events
	Writes     uint64    // successful output writes
	Errors     uint64    // errors of any kind
}

// Connector exchanges values between field devices and a TagDatabase.
type Connector struct {
	db          *honeycomb.TagDatabase
	manager     plc4go.PlcDriverManager
	connections []*connState
	onError     func(connection string, err error)
	diagPrefix  string

	mu     sync.RWMutex
	status map[string]Status

	diagMu    sync.Mutex
	published map[string]Status // last Status written to the diagnostic tags
}

// connState is a Connection with its bindings indexed.
type connState struct {
	Connection
	byTag     map[string]Binding
	hasInputs bool
	link      *link // output tracking; set by Run
	// inflight counts requests a session stopped waiting for; the session
	// closes its connection only once they are done (see execute).
	inflight sync.WaitGroup
}

// requestTimeout bounds one request to a device.
const requestTimeout = 10 * time.Second

// execute runs a request and waits for its result or for ctx to end.
//
// The request runs on a context that the end of a session does not cancel.
// PLC4X hands back the result of a cancelled request at once, even while its
// worker is still writing the request, and its transports race when a
// connection is closed during a write (the serial transport's Close and Write
// share the port without a lock). So a session that ends stops waiting at
// once, but the result is still collected in the background, and the session
// closes its connection only after that (closeAfterRequests).
func execute[T any](ctx context.Context, cs *connState, run func(context.Context) <-chan T) (T, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), requestTimeout)
	results := run(rctx)
	select {
	case r := <-results:
		cancel()
		return r, nil
	case <-ctx.Done():
		cs.inflight.Add(1)
		go func() {
			defer cs.inflight.Done()
			defer cancel()
			<-results
		}()
		var zero T
		return zero, ctx.Err()
	}
}

// closeAfterRequests closes a session's connection once the requests it
// stopped waiting for are done, or after requestTimeout and a margin if PLC4X
// never answers one.
func closeAfterRequests(cs *connState, plcConn plc4go.PlcConnection) {
	done := make(chan struct{})
	go func() { cs.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(requestTimeout + time.Second):
	}
	plcConn.Close()
}

// Option configures a Connector.
type Option func(*Connector)

// WithDriverManager uses a caller-configured PLC4X driver manager instead of
// one with every driver registered.
func WithDriverManager(m plc4go.PlcDriverManager) Option {
	return func(c *Connector) { c.manager = m }
}

// WithErrorHandler receives connection, read and write errors. By default they
// are only kept in Status.
func WithErrorHandler(fn func(connection string, err error)) Option {
	return func(c *Connector) { c.onError = fn }
}

// WithDiagnostics publishes each connection's Status as tags named
// prefix + connection name + "." + field, e.g. "PLC4X.press1.Connected" for the
// prefix "PLC4X.". The fields are Connected and Subscribed (BOOL), Reads, Writes
// and Errors (ULINT), LastError (STRING) and LastRead (DT). Missing tags are created.
func WithDiagnostics(prefix string) Option {
	return func(c *Connector) { c.diagPrefix = prefix }
}

// New creates a Connector for the given devices.
func New(db *honeycomb.TagDatabase, connections []Connection, opts ...Option) (*Connector, error) {
	c := &Connector{
		db:        db,
		onError:   func(string, error) {},
		status:    make(map[string]Status),
		published: make(map[string]Status),
	}
	for _, opt := range opts {
		opt(c)
	}
	seen := make(map[string]bool)
	for i, conn := range connections {
		if conn.Name == "" || conn.URL == "" {
			return nil, fmt.Errorf("plc4x: connection %d needs a Name and a URL", i)
		}
		if seen[conn.Name] {
			return nil, fmt.Errorf("plc4x: duplicate connection name %q", conn.Name)
		}
		seen[conn.Name] = true
		if conn.Interval <= 0 {
			conn.Interval = time.Second
		}
		if conn.Mode > Cyclic {
			return nil, fmt.Errorf("plc4x: connection %q: invalid mode %d", conn.Name, conn.Mode)
		}
		cs := &connState{Connection: conn, byTag: make(map[string]Binding)}
		for _, b := range conn.Bindings {
			if b.Tag == "" || b.Address == "" {
				return nil, fmt.Errorf("plc4x: connection %q: every binding needs a Tag and an Address", conn.Name)
			}
			if b.Direction > InOut {
				return nil, fmt.Errorf("plc4x: connection %q: tag %q: invalid direction %d", conn.Name, b.Tag, b.Direction)
			}
			// PLC4X keys requests and responses by tag name.
			if _, dup := cs.byTag[b.Tag]; dup {
				return nil, fmt.Errorf("plc4x: connection %q: tag %q is bound twice", conn.Name, b.Tag)
			}
			cs.byTag[b.Tag] = b
			cs.hasInputs = cs.hasInputs || b.Direction.reads()
		}
		c.connections = append(c.connections, cs)
	}
	if c.manager == nil {
		// Only the drivers the connections use: a device reaches no other
		// protocol's code.
		m, err := NewDriverManagerFor(Protocols(connections)...)
		if err != nil {
			return nil, err
		}
		c.manager = m
	}
	if err := c.createDiagnostics(); err != nil {
		return nil, err
	}
	return c, nil
}

// Status returns the current status of the named connection.
func (c *Connector) Status(name string) (Status, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.status[name]
	return s, ok
}

// Run exchanges values with every connection until ctx is cancelled. Device
// errors never stop Run: the failing connection reconnects with backoff while
// the others continue. Run returns early only if an output tag cannot be watched.
func (c *Connector) Run(ctx context.Context) error {
	for _, cs := range c.connections {
		l, err := c.watchOutputs(cs)
		if err != nil {
			c.stopWatching()
			return err
		}
		cs.link = l
	}
	defer c.stopWatching()

	var wg sync.WaitGroup
	for _, cs := range c.connections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runConnection(ctx, cs)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

const (
	minBackoff = 500 * time.Millisecond
	maxBackoff = 30 * time.Second
)

func (c *Connector) runConnection(ctx context.Context, cs *connState) {
	backoff := minBackoff
	for ctx.Err() == nil {
		err := c.session(ctx, cs)
		if ctx.Err() != nil {
			return
		}
		c.setInputQuality(cs, honeycomb.QualityBad)
		c.record(cs.Name, func(s *Status) { s.Connected, s.Subscribed = false, false }, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session connects once and exchanges values until the connection fails or ctx ends.
func (c *Connector) session(ctx context.Context, cs *connState) error {
	plcConn, err := c.manager.GetConnection(ctx, cs.URL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer closeAfterRequests(cs, plcConn)

	var read apiModel.PlcReadRequest
	subscribed := false
	if cs.hasInputs {
		// Asking a driver that cannot subscribe for a subscription builder panics.
		if cs.Mode != Poll && plcConn.GetMetadata().CanSubscribe() {
			active := new(atomic.Bool)
			active.Store(true)
			defer active.Store(false) // ignore events that arrive after the session
			if err := c.subscribe(ctx, plcConn, cs, active); err != nil {
				return fmt.Errorf("subscribe: %w", err)
			}
			subscribed = true
		} else {
			if cs.Mode != Poll {
				c.record(cs.Name, nil, fmt.Errorf("driver cannot subscribe; polling every %s instead", cs.Interval))
			}
			builder := plcConn.ReadRequestBuilder()
			for _, b := range cs.Bindings {
				if b.Direction.reads() {
					builder.AddTagAddress(b.Tag, b.Address)
				}
			}
			if read, err = builder.Build(); err != nil {
				return fmt.Errorf("build read request: %w", err)
			}
		}
	}
	c.record(cs.Name, func(s *Status) { s.Connected, s.Subscribed = true, subscribed }, nil)
	cs.link.startSession()

	// cycle writes changed outputs and, when due, reads the inputs. It returns an
	// error only when the connection is lost; other errors are recorded.
	cycle := func(due bool) error {
		if err := c.flushWrites(ctx, plcConn, cs); err != nil {
			if !plcConn.IsConnected() {
				return err
			}
			c.record(cs.Name, nil, err)
		}
		switch {
		case !due:
		case read != nil:
			if err := c.poll(ctx, cs, read); err != nil {
				if !plcConn.IsConnected() {
					return err
				}
				c.record(cs.Name, nil, err) // The device did not answer; keep the session.
			}
		case !plcConn.IsConnected():
			return errors.New("connection lost")
		}
		return nil
	}

	ticker := time.NewTicker(cs.Interval)
	defer ticker.Stop()
	if err := cycle(true); err != nil {
		return err
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cs.link.wake:
			err = cycle(false)
		case <-ticker.C:
			err = cycle(true)
		}
		if err != nil {
			return err
		}
	}
}

// poll executes one read and writes every successfully read value to its tag.
// A response counts as a read even if some tags failed; those failures are
// recorded with it. The returned error means the read itself failed.
func (c *Connector) poll(ctx context.Context, cs *connState, request apiModel.PlcReadRequest) error {
	result, err := execute(ctx, cs, request.Execute)
	if err != nil {
		return err
	}
	if err := result.GetErr(); err != nil {
		// The device did not answer; the values held may be stale.
		c.setInputQuality(cs, honeycomb.QualityUncertain)
		return fmt.Errorf("read: %w", err)
	}

	response := result.GetResponse()
	var errs []error
	for _, name := range response.GetTagNames() {
		if err := c.receive(cs, name, response.GetResponseCode(name), response.GetValue(name)); err != nil {
			errs = append(errs, err)
		}
	}
	c.recordRead(cs.Name, errors.Join(errs...))
	return nil
}

// receive stores one value read from the device, or records why there is none.
func (c *Connector) receive(cs *connState, name string, code apiModel.PlcResponseCode, value apiValues.PlcValue) error {
	if code != apiModel.PlcResponseCode_OK {
		_ = c.db.SetTagQuality(name, honeycomb.QualityFromPLC4X(code.GetName()))
		return fmt.Errorf("tag '%s': device returned %s", name, code)
	}
	if err := c.store(cs, name, value); err != nil {
		_ = c.db.SetTagQuality(name, honeycomb.QualityBad)
		return fmt.Errorf("tag '%s': %w", name, err)
	}
	return nil
}

// setInputQuality sets the quality of every tag the connection reads.
func (c *Connector) setInputQuality(cs *connState, quality honeycomb.Quality) {
	for _, b := range cs.Bindings {
		if b.Direction.reads() {
			_ = c.db.SetTagQuality(b.Tag, quality)
		}
	}
}

// record updates a connection's status. A non-nil err becomes LastError,
// counts as an error and is passed to the error handler.
func (c *Connector) record(name string, update func(*Status), err error) {
	c.mu.Lock()
	s := c.status[name]
	if update != nil {
		update(&s)
	}
	if err != nil {
		s.LastError = err
		s.Errors++
	}
	c.status[name] = s
	c.mu.Unlock()
	c.publishDiagnostics(name)
	if err != nil {
		c.onError(name, err)
	}
}

// recordRead records a successful poll or subscription event.
func (c *Connector) recordRead(name string, err error) {
	c.record(name, func(s *Status) {
		s.LastError = nil
		s.LastRead = time.Now()
		s.Reads++
	}, err)
}

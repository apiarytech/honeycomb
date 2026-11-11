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
	"strings"
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
	// Server answers Modbus masters, on a serial port or over TCP, serving
	// the bound tags (server.go, drone D-66).
	Server
	// Event subscribes to the inputs as events: the device sends them
	// unsolicited, when it has something to report. A driver that cannot
	// subscribe is polled instead, as in the other subscribing modes.
	Event
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
	// Scaling converts between the raw device value and the tag's
	// engineering value; the zero Scaling stores the raw value.
	Scaling Scaling
	// PollGroup names one of the connection's PollGroups; empty reads the
	// binding every Interval.
	PollGroup string
	// SafeValue, on an Output or InOut binding, is written to the device
	// when the connector stops (comm going offline or shutting down, R-25),
	// so the device is left in a known state. It is the tag's own Go type,
	// or JSON (json.RawMessage) decoded into that type when Run starts. The
	// tag itself keeps its value: outputs are written again on return (D-3).
	// nil writes nothing.
	SafeValue any
	// StaleAfter, on an Input or InOut binding, marks frozen data (beehive
	// R-4): a read that brings the same value as before, unchanged for at
	// least this long, is stored with Uncertain quality instead of Good, so
	// a stuck sensor or a device that repeats its last value shows. The
	// next changed value is Good again. Zero: never.
	StaleAfter time.Duration
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

	// Description is for people.
	Description string
	// Disabled keeps the connection configured but stops the exchange:
	// no connection is made, its inputs are Bad, its outputs not written.
	Disabled bool
	// Channel names the shared transport this device is on (WithChannels);
	// connections on one channel take turns. Empty: a transport of its own.
	Channel string
	// ConnectTimeout bounds a connection attempt; 0 waits for the driver.
	ConnectTimeout time.Duration
	// RequestTimeout bounds one request; 0 is ten seconds.
	RequestTimeout time.Duration
	// Attempts is how often a request the device does not answer is sent
	// before it counts as failed; 0 or 1 sends it once.
	Attempts int
	// InterRequestDelay is the least time between two requests to the device.
	InterRequestDelay time.Duration
	// Demotion takes a failing device out of the cycle for a while.
	Demotion Demotion
	// PollGroups are rates of their own for some inputs.
	PollGroups []PollGroup
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

	Disabled     bool      // the connection is configured but disabled, or disabled while running
	Override     bool      // enabled or disabled while running, against its configuration (SetEnabled)
	Demoted      bool      // out of the cycle until DemotedUntil (Demotion)
	DemotedUntil time.Time // zero unless demoted
	Demotions    uint64    // times demoted
	ErrorSince   time.Time // start of the current run of errors; zero after a success
	Failures     uint32    // successive failed exchanges now, counted toward Demotion.After; 0 after a success
}

// Connector exchanges values between field devices and a TagDatabase.
type Connector struct {
	db          *honeycomb.TagDatabase
	manager     plc4go.PlcDriverManager
	connections []*connState
	onError     func(connection string, err error)
	// onServerWrite reports each tag a Modbus master wrote to a server
	// connection (D-66): the connection, the tag, its new value, from where.
	onServerWrite func(connection, tag string, value any, from string)
	diagPrefix    string
	channels      []Channel

	runMu   sync.Mutex // Run's start and end, against SetEnabled preparing a connection
	started bool

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
	link      *link   // output tracking; set by Run
	server    *server // a Server connection's port and register map
	// inflight counts requests a session stopped waiting for; the session
	// closes its connection only once they are done (see execute).
	inflight sync.WaitGroup

	safe []safeWrite // resolved SafeValues, in binding order; set by Run

	// enabled is whether the connection runs now: !Disabled until SetEnabled.
	enabled  atomic.Bool
	ctl      chan struct{} // wakes the supervisor when enabled changes
	pollNow  chan struct{} // asks the session to read every input now (PollNow)
	running  atomic.Bool   // Run prepared it: its link and safe values are set
	turn     *turn         // the channel's, if the connection is on one
	failures int           // successive failed exchanges, for Demotion (runConnection's goroutine)

	lastMu  sync.Mutex
	lastEnd time.Time // when the last request ended, for InterRequestDelay

	staleMu sync.Mutex
	changed map[string]time.Time // when a StaleAfter binding's value last changed

	// dialURL is the URL PLC4X dials when it is not URL: an IEC
	// 60870-5-104 client over TLS dials its tunnel (iec62351.go).
	dialURL atomic.Pointer[string]
}

// plc4xURL is the URL PLC4X connects to.
func (cs *connState) plc4xURL() string {
	if u := cs.dialURL.Load(); u != nil {
		return *u
	}
	return cs.URL
}

// now is the connector's clock; tests replace it.
var now = time.Now

// requestTimeout bounds one request to a device.
var requestTimeout = 10 * time.Second

// execute runs a request and waits for its result or for ctx to end.
//
// The request runs on a context that the end of a session does not cancel.
// PLC4X hands back the result of a cancelled request at once, even while its
// worker is still writing the request, and its transports race when a
// connection is closed during a write (the serial transport's Close and Write
// share the port without a lock). So a session that ends stops waiting at
// once, but the result is still collected in the background, and the session
// closes its connection only after that (closeAfterRequests).
//
// A connection on a channel waits for its turn first, and holds it until
// the result is in (or the request timeout and a margin have passed).
func execute[T any](ctx context.Context, cs *connState, run func(context.Context) <-chan T) (T, error) {
	var zero T
	release, err := cs.acquire(ctx)
	if err != nil {
		return zero, err
	}
	timeout := cs.timeout()
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	results := run(rctx)
	select {
	case r := <-results:
		cancel()
		release()
		return r, nil
	case <-ctx.Done():
		cs.inflight.Add(1)
		go func() {
			defer cs.inflight.Done()
			defer cancel()
			defer release()
			select {
			case <-results:
			case <-time.After(timeout + time.Second):
				// PLC4X never answered: give the channel back; the
				// result is still collected for the connection's close.
				release()
				<-results
			}
		}()
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
	case <-time.After(cs.timeout() + time.Second):
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

// WithServerWrites reports each tag a Modbus master writes through a
// server connection, for the record: who changed what.
func WithServerWrites(fn func(connection, tag string, value any, from string)) Option {
	return func(c *Connector) { c.onServerWrite = fn }
}

// WithDiagnostics publishes each connection's Status as tags named
// prefix + connection name + "." + field, e.g. "PLC4X.press1.Connected" for the
// prefix "PLC4X.". The fields are Connected and Subscribed (BOOL), Reads, Writes
// and Errors (ULINT), LastError (STRING) and LastRead (DT). Missing tags are created.
func WithDiagnostics(prefix string) Option {
	return func(c *Connector) { c.diagPrefix = prefix }
}

// WithChannels describes the shared transports connections name in
// Connection.Channel. A channel no option describes has no delay between
// devices.
func WithChannels(channels ...Channel) Option {
	return func(c *Connector) { c.channels = append(c.channels, channels...) }
}

// New creates a Connector for the given devices.
func New(db *honeycomb.TagDatabase, connections []Connection, opts ...Option) (*Connector, error) {
	c := &Connector{
		db:            db,
		onError:       func(string, error) {},
		onServerWrite: func(string, string, any, string) {},
		status:        make(map[string]Status),
		published:     make(map[string]Status),
	}
	for _, opt := range opts {
		opt(c)
	}
	turns := make(map[string]*turn)
	for _, ch := range c.channels {
		if ch.Name == "" {
			return nil, errors.New("plc4x: a channel needs a Name")
		}
		if _, dup := turns[ch.Name]; dup {
			return nil, fmt.Errorf("plc4x: duplicate channel name %q", ch.Name)
		}
		turns[ch.Name] = newTurn(max(ch.InterDeviceDelay, 0))
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
		if conn.Mode > Event {
			return nil, fmt.Errorf("plc4x: connection %q: invalid mode %d", conn.Name, conn.Mode)
		}
		if conn.ConnectTimeout < 0 || conn.RequestTimeout < 0 || conn.InterRequestDelay < 0 || conn.Attempts < 0 ||
			conn.Demotion.After < 0 || conn.Demotion.Period < 0 {
			return nil, fmt.Errorf("plc4x: connection %q: timeouts, delays, attempts and demotion cannot be negative", conn.Name)
		}
		cs := &connState{Connection: conn, byTag: make(map[string]Binding), ctl: make(chan struct{}, 1), pollNow: make(chan struct{}, 1)}
		cs.enabled.Store(!conn.Disabled)
		if conn.Channel != "" {
			if conn.Mode == Server {
				return nil, fmt.Errorf("plc4x: connection %q: a server owns its transport and shares no channel", conn.Name)
			}
			t, ok := turns[conn.Channel]
			if !ok {
				t = newTurn(0)
				turns[conn.Channel] = t
			}
			cs.turn = t
		}
		groups := make(map[string]bool)
		for _, g := range conn.PollGroups {
			if g.Name == "" || groups[g.Name] {
				return nil, fmt.Errorf("plc4x: connection %q: poll group %q: empty or repeated name", conn.Name, g.Name)
			}
			if g.Interval <= 0 {
				return nil, fmt.Errorf("plc4x: connection %q: poll group %q needs an interval", conn.Name, g.Name)
			}
			groups[g.Name] = true
		}
		if conn.Mode == Server {
			s, err := newServer(conn)
			if err != nil {
				return nil, err
			}
			cs.server = s
		}
		for _, b := range conn.Bindings {
			if b.Tag == "" || b.Address == "" {
				return nil, fmt.Errorf("plc4x: connection %q: every binding needs a Tag and an Address", conn.Name)
			}
			if b.Direction > InOut {
				return nil, fmt.Errorf("plc4x: connection %q: tag %q: invalid direction %d", conn.Name, b.Tag, b.Direction)
			}
			if err := b.Scaling.check(); err != nil {
				return nil, fmt.Errorf("plc4x: connection %q: tag %q: %w", conn.Name, b.Tag, err)
			}
			if conn.Mode == Server && (b.Scaling.Kind != NoScaling || b.PollGroup != "") {
				return nil, fmt.Errorf("plc4x: connection %q: tag %q: a server's points have no scaling and no poll group", conn.Name, b.Tag)
			}
			if b.SafeValue != nil && (!b.Direction.writes() || conn.Mode == Server) {
				return nil, fmt.Errorf("plc4x: connection %q: tag %q: only a client's Output and InOut points have a safe value", conn.Name, b.Tag)
			}
			if b.PollGroup != "" && !groups[b.PollGroup] {
				return nil, fmt.Errorf("plc4x: connection %q: tag %q: no poll group %q", conn.Name, b.Tag, b.PollGroup)
			}
			// PLC4X keys requests and responses by tag name. A server may
			// serve one tag at two addresses: it makes no requests.
			if _, dup := cs.byTag[b.Tag]; dup && conn.Mode != Server {
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
		// A disabled connection's driver too, so it can be enabled while
		// running (SetEnabled), when PLC4X knows its protocol.
		var clients []Connection // a server needs no driver: it answers, it does not connect
		for _, conn := range connections {
			if conn.Mode == Server {
				continue
			}
			if conn.Disabled {
				if _, known := protocols()[strings.ToLower(ProtocolOf(conn.URL))]; !known {
					continue
				}
			}
			clients = append(clients, conn)
		}
		m, err := NewDriverManagerFor(Protocols(clients)...)
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
		if !cs.enabled.Load() {
			continue // prepared when it is enabled (SetEnabled)
		}
		if err := c.prepare(cs); err != nil {
			c.stopWatching()
			return err
		}
	}
	c.runMu.Lock()
	c.started = true
	c.runMu.Unlock()
	defer func() {
		c.runMu.Lock()
		c.started = false
		c.stopWatching()
		c.runMu.Unlock()
	}()

	var wg sync.WaitGroup
	for _, cs := range c.connections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.supervise(ctx, cs)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// prepare watches a client connection's outputs and reads its safe values.
// A server reads its tags at each request.
func (c *Connector) prepare(cs *connState) error {
	if cs.server == nil && cs.link == nil {
		safe, err := c.resolveSafe(cs)
		if err != nil {
			return err
		}
		l, err := c.watchOutputs(cs)
		if err != nil {
			return err
		}
		cs.safe, cs.link = safe, l
	}
	cs.running.Store(true)
	return nil
}

// supervise runs a connection while it is enabled and holds it while it is
// disabled, until ctx ends. Disabling ends its session as stopping does: its
// safe values are written (R-25).
func (c *Connector) supervise(ctx context.Context, cs *connState) {
	if cs.server == nil {
		stop, err := startTunnel(cs)
		if err != nil {
			c.onError(cs.Name, err)
			c.record(cs.Name, func(s *Status) { s.Connected = false }, err)
		}
		if stop != nil {
			defer stop()
		}
	}
	for ctx.Err() == nil {
		if !cs.enabled.Load() {
			c.setInputQuality(cs, honeycomb.QualityBad)
			c.record(cs.Name, func(s *Status) {
				s.Disabled, s.Connected, s.Subscribed, s.Demoted, s.DemotedUntil = true, false, false, false, time.Time{}
			}, nil)
			select {
			case <-ctx.Done():
				return
			case <-cs.ctl:
				continue
			}
		}
		c.record(cs.Name, func(s *Status) { s.Disabled = false }, nil)
		run, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if cs.server != nil {
				c.runServer(run, cs)
			} else {
				c.runConnection(run, cs)
			}
		}()
	held:
		for {
			select {
			case <-ctx.Done():
				cancel()
				<-done
				return
			case <-done:
				cancel()
				break held // runConnection returns after a demotion's period too: it runs again
			case <-cs.ctl:
				if !cs.enabled.Load() {
					cancel()
					<-done
					break held
				}
			}
		}
	}
}

// connection is the named connection's state.
func (c *Connector) connection(name string) (*connState, error) {
	for _, cs := range c.connections {
		if cs.Name == name {
			return cs, nil
		}
	}
	return nil, fmt.Errorf("plc4x: no connection %q", name)
}

// SetEnabled enables or disables a connection while the connector runs
// (beehive R-135), without changing its configuration: a new Connector
// starts from Connection.Disabled again. Disabling ends its session, writing
// its safe values, and makes its inputs Bad; enabling connects it, and its
// outputs are written as after every reconnect.
func (c *Connector) SetEnabled(name string, enabled bool) error {
	cs, err := c.connection(name)
	if err != nil {
		return err
	}
	if enabled && cs.server == nil {
		if _, known := protocols()[strings.ToLower(ProtocolOf(cs.URL))]; !known {
			return fmt.Errorf("plc4x: connection %q: unknown protocol %q", name, ProtocolOf(cs.URL))
		}
	}
	c.runMu.Lock()
	if enabled && c.started && !cs.running.Load() {
		if err := c.prepare(cs); err != nil {
			c.runMu.Unlock()
			return fmt.Errorf("plc4x: connection %q: %w", name, err)
		}
	}
	cs.enabled.Store(enabled)
	c.runMu.Unlock()
	c.record(name, func(s *Status) { s.Override, s.Disabled = enabled == cs.Disabled, !enabled }, nil) // writes are refused from now
	select {
	case cs.ctl <- struct{}{}:
	default:
	}
	return nil
}

// PollNow reads every input of a polled connection now, besides its
// intervals. It returns at once; the values arrive as with any poll.
func (c *Connector) PollNow(name string) error {
	cs, err := c.connection(name)
	if err != nil {
		return err
	}
	switch {
	case cs.server != nil:
		return fmt.Errorf("plc4x: connection %q is a server: it answers, it does not poll", name)
	case !cs.enabled.Load():
		return fmt.Errorf("plc4x: connection %q is disabled", name)
	}
	select {
	case cs.pollNow <- struct{}{}:
	default: // already asked
	}
	return nil
}

// ResetCounters sets a connection's reads, writes, errors and demotions
// back to 0.
func (c *Connector) ResetCounters(name string) error {
	if _, err := c.connection(name); err != nil {
		return err
	}
	c.record(name, func(s *Status) { s.Reads, s.Writes, s.Errors, s.Demotions = 0, 0, 0, 0 }, nil)
	return nil
}

const (
	minBackoff = 500 * time.Millisecond
	maxBackoff = 30 * time.Second
)

// lostAfter ends a session whose reads or writes have failed without a
// break for this long, even while the driver still says it is connected:
// PLC4X's Modbus TCP connection keeps saying so after the device closed the
// socket (a restarted drone, 2026-11-04: "broken pipe" ten times a second
// for hours), so only a new connection gets the device back.
var lostAfter = 5 * time.Second

func (c *Connector) runConnection(ctx context.Context, cs *connState) {
	backoff := minBackoff
	for ctx.Err() == nil {
		err := c.session(ctx, cs)
		if ctx.Err() != nil {
			return
		}
		c.setInputQuality(cs, honeycomb.QualityBad)
		c.record(cs.Name, func(s *Status) { s.Connected, s.Subscribed = false, false }, err)
		if cs.demoteDue() {
			c.demote(ctx, cs)
			backoff = minBackoff
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// demote keeps a failing connection out of the cycle for its demotion
// period. With DiscardWrites, in-out changes made meanwhile are dropped.
func (c *Connector) demote(ctx context.Context, cs *connState) {
	until := time.Now().Add(cs.demotionPeriod())
	c.record(cs.Name, func(s *Status) {
		s.Demoted, s.DemotedUntil = true, until
		s.Demotions++
	}, nil)
	select {
	case <-ctx.Done():
	case <-time.After(time.Until(until)):
	}
	if cs.Demotion.DiscardWrites && cs.link != nil {
		cs.link.takePending()
	}
	cs.failures = 0
	c.record(cs.Name, func(s *Status) { s.Demoted, s.DemotedUntil, s.Failures = false, time.Time{}, 0 }, nil)
}

// readGroup is the inputs read at one rate: the connection's interval, or a
// poll group's.
type readGroup struct {
	name     string // "" for the connection's interval
	interval time.Duration
	request  apiModel.PlcReadRequest
}

// session connects once and exchanges values until the connection fails or ctx ends.
func (c *Connector) session(ctx context.Context, cs *connState) error {
	plcConn, err := c.connect(ctx, cs)
	if err != nil {
		c.countFailure(cs)
		return fmt.Errorf("connect: %w", err)
	}
	defer closeAfterRequests(cs, plcConn)
	// Stopping leaves the device its safe values (R-25), before the
	// connection closes (defers run last in, first out).
	defer func() {
		if ctx.Err() != nil && len(cs.safe) > 0 {
			c.writeSafe(plcConn, cs)
		}
	}()

	var reads []*readGroup
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
			if reads, err = buildReads(plcConn, cs); err != nil {
				return err
			}
		}
	}
	c.record(cs.Name, func(s *Status) { s.Connected, s.Subscribed = true, subscribed }, nil)
	cs.link.startSession()

	// failing is when reads or writes started failing without a break; zero
	// while they succeed.
	var failing time.Time
	succeeded := func() {
		failing = time.Time{}
		c.clearFailures(cs)
	}
	// failed records err and keeps the session, unless the connection is
	// lost, nothing has succeeded for lostAfter, or the connection is to be
	// demoted: then the session ends and runConnection connects again.
	failed := func(err error) error {
		c.countFailure(cs)
		if !plcConn.IsConnected() {
			return err
		}
		if cs.demoteDue() {
			return fmt.Errorf("%w: %w", errDemoted, err)
		}
		c.record(cs.Name, nil, err) // The device did not answer; keep the session for now.
		if failing.IsZero() {
			failing = time.Now()
		} else if time.Since(failing) >= lostAfter {
			return fmt.Errorf("nothing answered for %s, connecting again: %w", lostAfter, err)
		}
		return nil
	}

	// cycle writes changed outputs and reads the inputs that are due. It
	// returns an error only when the session is to end; other errors are
	// recorded.
	cycle := func(due []*readGroup) error {
		if err := c.flushWrites(ctx, plcConn, cs); err != nil {
			if err := failed(err); err != nil {
				return err
			}
		} else if len(reads) == 0 {
			succeeded()
		}
		for _, g := range due {
			if err := c.poll(ctx, cs, g.request); err != nil {
				if err := failed(err); err != nil {
					return err
				}
			} else {
				succeeded()
			}
		}
		if len(reads) == 0 && due != nil && !plcConn.IsConnected() {
			return errors.New("connection lost")
		}
		return nil
	}

	// One ticker per rate; the connection's own also checks a subscribed or
	// output-only connection.
	main := &readGroup{interval: cs.Interval}
	tickers := []*readGroup{main}
	for _, g := range reads {
		if g.name == "" {
			main.request = g.request
		} else {
			tickers = append(tickers, g)
		}
	}
	dueCh := make(chan *readGroup, len(tickers))
	for _, g := range tickers {
		t := time.NewTicker(g.interval)
		defer t.Stop()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					select {
					case dueCh <- g:
					default: // still due from the last tick
					}
				}
			}
		}()
	}
	all := reads
	if all == nil {
		all = []*readGroup{}
	}
	if err := cycle(all); err != nil {
		return err
	}
	for {
		var err error
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cs.link.wake:
			err = cycle(nil)
		case <-cs.pollNow:
			err = cycle(all)
		case g := <-dueCh:
			if g.request != nil {
				err = cycle([]*readGroup{g})
			} else {
				err = cycle([]*readGroup{})
			}
		}
		if err != nil {
			return err
		}
	}
}

// buildReads builds one read request per rate: the inputs without a poll
// group, and each poll group's.
func buildReads(plcConn plc4go.PlcConnection, cs *connState) ([]*readGroup, error) {
	builders := map[string]apiModel.PlcReadRequestBuilder{}
	order := []string{}
	for _, b := range cs.Bindings {
		if !b.Direction.reads() {
			continue
		}
		bld, ok := builders[b.PollGroup]
		if !ok {
			bld = plcConn.ReadRequestBuilder()
			builders[b.PollGroup] = bld
			order = append(order, b.PollGroup)
		}
		bld.AddTagAddress(b.Tag, b.Address)
	}
	interval := map[string]time.Duration{"": cs.Interval}
	for _, g := range cs.PollGroups {
		interval[g.Name] = g.Interval
	}
	var out []*readGroup
	for _, name := range order {
		req, err := builders[name].Build()
		if err != nil {
			return nil, fmt.Errorf("build read request: %w", err)
		}
		out = append(out, &readGroup{name: name, interval: interval[name], request: req})
	}
	return out, nil
}

// poll executes one read and writes every successfully read value to its tag.
// A response counts as a read even if some tags failed; those failures are
// recorded with it. The returned error means the read itself failed.
func (c *Connector) poll(ctx context.Context, cs *connState, request apiModel.PlcReadRequest) error {
	var result apiModel.PlcReadRequestResult
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
		// The device did not answer; the values held may be stale.
		for _, name := range request.GetTagNames() {
			_ = c.db.SetTagQuality(name, honeycomb.QualityUncertain)
		}
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
		if s.ErrorSince.IsZero() {
			s.ErrorSince = time.Now()
		}
	}
	c.status[name] = s
	c.mu.Unlock()
	c.publishDiagnostics(name)
	if err != nil {
		c.onError(name, err)
	}
}

// countFailure counts one more failed exchange of cs toward its demotion,
// in cs and in its status. The diagnostic tags show it with the error that
// follows.
func (c *Connector) countFailure(cs *connState) {
	cs.failures++
	c.mu.Lock()
	s := c.status[cs.Name]
	s.Failures = uint32(cs.failures)
	c.status[cs.Name] = s
	c.mu.Unlock()
}

// clearFailures restarts cs's count of failed exchanges after a success.
func (c *Connector) clearFailures(cs *connState) {
	if cs.failures == 0 {
		return
	}
	cs.failures = 0
	c.mu.Lock()
	s := c.status[cs.Name]
	s.Failures = 0
	c.status[cs.Name] = s
	c.mu.Unlock()
}

// recordRead records a successful poll or subscription event.
func (c *Connector) recordRead(name string, err error) {
	c.record(name, func(s *Status) {
		s.LastError = nil
		s.ErrorSince = time.Time{}
		s.LastRead = time.Now()
		s.Reads++
	}, err)
}

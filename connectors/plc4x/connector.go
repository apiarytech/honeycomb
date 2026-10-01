/*
 * Copyright (C) 2026 Franklin D. Amador
 *
 * This software is dual-licensed under the terms of the GPL v3.0 and
 * a commercial license. You may choose to use this software under either
 * license.
 *
 * See the LICENSE files in the project root for full license text.
 */

// Package plc4x reads field-device data into a honeycomb TagDatabase using
// Apache PLC4X (https://plc4x.apache.org), which provides drivers for S7,
// Modbus, EtherNet/IP, Logix, ADS, OPC UA, BACnet, KNX and more.
//
// It is a separate Go module so that applications which do not talk to field
// devices do not pull in PLC4X and its dependencies.
package plc4x

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	"github.com/apache/plc4x/plc4go/pkg/api/config"
	"github.com/apache/plc4x/plc4go/pkg/api/drivers"
	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	"github.com/apiarytech/honeycomb"
)

// Binding maps a honeycomb tag to an address on a device.
type Binding struct {
	// Tag is the honeycomb tag that receives the value. It must exist and hold
	// a value, whose Go type selects the conversion.
	Tag string
	// Address is the PLC4X tag address, e.g. "%DB1.DBD0:REAL" (S7) or "holding-register:1:DINT" (Modbus).
	Address string
}

// Connection describes one field device and the tags read from it.
type Connection struct {
	// Name identifies the connection in status and errors.
	Name string
	// URL is the PLC4X connection string, e.g. "s7://192.168.0.10" or "modbus-tcp://10.0.0.5:502".
	URL string
	// Interval is the polling period. Defaults to one second.
	Interval time.Duration
	// Bindings lists the tags read on every poll.
	Bindings []Binding
}

// Status reports the health of a connection.
type Status struct {
	Connected bool
	LastRead  time.Time // time of the last successful poll
	LastError error     // most recent error, nil after a fully successful poll
}

// Connector polls field devices and writes their values into a TagDatabase.
type Connector struct {
	db          *honeycomb.TagDatabase
	manager     plc4go.PlcDriverManager
	connections []Connection
	onError     func(connection string, err error)

	mu     sync.RWMutex
	status map[string]Status
}

// Option configures a Connector.
type Option func(*Connector)

// WithDriverManager uses a caller-configured PLC4X driver manager instead of
// one with every driver registered.
func WithDriverManager(m plc4go.PlcDriverManager) Option {
	return func(c *Connector) { c.manager = m }
}

// WithErrorHandler receives connection and read errors. By default they are only kept in Status.
func WithErrorHandler(fn func(connection string, err error)) Option {
	return func(c *Connector) { c.onError = fn }
}

// New creates a Connector for the given devices.
func New(db *honeycomb.TagDatabase, connections []Connection, opts ...Option) (*Connector, error) {
	c := &Connector{
		db:          db,
		connections: connections,
		onError:     func(string, error) {},
		status:      make(map[string]Status),
	}
	for _, opt := range opts {
		opt(c)
	}
	seen := make(map[string]bool)
	for i, conn := range c.connections {
		if conn.Name == "" || conn.URL == "" {
			return nil, fmt.Errorf("plc4x: connection %d needs a Name and a URL", i)
		}
		if seen[conn.Name] {
			return nil, fmt.Errorf("plc4x: duplicate connection name %q", conn.Name)
		}
		seen[conn.Name] = true
		if conn.Interval <= 0 {
			c.connections[i].Interval = time.Second
		}
	}
	if c.manager == nil {
		c.manager = NewDriverManager()
	}
	return c, nil
}

// NewDriverManager returns a PLC4X driver manager with every PLC4X driver registered.
func NewDriverManager() plc4go.PlcDriverManager {
	m := plc4go.NewPlcDriverManager()
	for _, register := range []func(plc4go.PlcDriverManager, ...config.WithOption) plc4go.PlcDriver{
		drivers.RegisterAbEthDriver, drivers.RegisterAdsDriver, drivers.RegisterBacnetDriver,
		drivers.RegisterCBusDriver, drivers.RegisterEipDriver, drivers.RegisterLogixDriver,
		drivers.RegisterFirmataDriver, drivers.RegisterIec608705104Driver, drivers.RegisterKnxDriver,
		drivers.RegisterModbusTcpDriver, drivers.RegisterModbusRtuDriver, drivers.RegisterModbusAsciiDriver,
		drivers.RegisterOpcuaDriver, drivers.RegisterS7Driver, drivers.RegisterSlmpDriver, drivers.RegisterUmasDriver,
	} {
		register(m)
	}
	return m
}

// Status returns the current status of the named connection.
func (c *Connector) Status(name string) (Status, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.status[name]
	return s, ok
}

// Run polls every connection until ctx is cancelled. Device errors never stop
// Run: the failing connection reconnects with backoff while the others continue.
func (c *Connector) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, conn := range c.connections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runConnection(ctx, conn)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

const (
	minBackoff = 500 * time.Millisecond
	maxBackoff = 30 * time.Second
)

func (c *Connector) runConnection(ctx context.Context, conn Connection) {
	backoff := minBackoff
	for ctx.Err() == nil {
		err := c.session(ctx, conn)
		if ctx.Err() != nil {
			return
		}
		c.report(conn.Name, false, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session connects once and polls until the connection fails or ctx ends.
func (c *Connector) session(ctx context.Context, conn Connection) error {
	plcConn, err := c.manager.GetConnection(ctx, conn.URL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer plcConn.Close()

	builder := plcConn.ReadRequestBuilder()
	for _, b := range conn.Bindings {
		builder.AddTagAddress(b.Tag, b.Address)
	}
	request, err := builder.Build()
	if err != nil {
		return fmt.Errorf("build read request: %w", err)
	}

	ticker := time.NewTicker(conn.Interval)
	defer ticker.Stop()
	for {
		if err := c.poll(ctx, request); err != nil {
			if !plcConn.IsConnected() {
				return err
			}
			c.report(conn.Name, true, err) // A tag-level error; keep the session.
		} else {
			c.report(conn.Name, true, nil)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// poll executes one read and writes every successfully read value to its tag.
func (c *Connector) poll(ctx context.Context, request apiModel.PlcReadRequest) error {
	var result apiModel.PlcReadRequestResult
	select {
	case result = <-request.Execute(ctx):
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := result.GetErr(); err != nil {
		return fmt.Errorf("read: %w", err)
	}

	response := result.GetResponse()
	var errs []error
	for _, name := range response.GetTagNames() {
		if code := response.GetResponseCode(name); code != apiModel.PlcResponseCode_OK {
			errs = append(errs, fmt.Errorf("tag '%s': device returned %s", name, code))
			continue
		}
		if err := c.store(name, response.GetValue(name)); err != nil {
			errs = append(errs, fmt.Errorf("tag '%s': %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Connector) report(name string, connected bool, err error) {
	c.mu.Lock()
	s := c.status[name]
	s.Connected, s.LastError = connected, err
	if connected && err == nil {
		s.LastRead = time.Now()
	}
	c.status[name] = s
	c.mu.Unlock()
	if err != nil {
		c.onError(name, err)
	}
}

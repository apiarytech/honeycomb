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
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	apiModel "github.com/apache/plc4x/plc4go/pkg/api/model"
	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
)

// Channel is a transport shared by several connections, such as an RS-485
// port with several Modbus units on it. Connections naming the same Channel
// take turns: one request at a time on the channel, and InterDeviceDelay
// before a request to another device than the one asked last.
type Channel struct {
	Name             string
	Description      string
	InterDeviceDelay time.Duration
}

// PollGroup is a rate of its own for some of a connection's inputs: a
// binding naming the group is read every Interval, the others every
// Connection.Interval.
type PollGroup struct {
	Name     string
	Interval time.Duration
}

// Demotion takes a connection that keeps failing out of the cycle for a
// while, so that it does not hold up the other devices on its channel.
type Demotion struct {
	// After this many successive failed exchanges the connection is
	// demoted; 0 never demotes.
	After int
	// Period it stays demoted; 0 is 10 seconds.
	Period time.Duration
	// DiscardWrites drops in-out changes made while demoted (Writable
	// callers refuse writes then too). Outputs are written again when the
	// connection comes back, as after every reconnect.
	DiscardWrites bool
}

const defaultDemotion = 10 * time.Second

// ScalingKind selects a Scaling's curve.
type ScalingKind uint8

const (
	// NoScaling stores the device's value as it is.
	NoScaling ScalingKind = iota
	// Linear maps the raw range onto the scaled range on a straight line.
	Linear
	// SquareRoot maps the square root of the raw fraction: flow from a
	// differential pressure.
	SquareRoot
)

// Scaling converts between a device's raw value and the engineering value
// a tag holds: reads are scaled, writes unscaled. The tag's type is the
// engineering type; the address gives the raw type. Numeric scalars only.
type Scaling struct {
	Kind                  ScalingKind
	RawLow, RawHigh       float64
	ScaledLow, ScaledHigh float64
	// ClampLow and ClampHigh hold the engineering value inside its range.
	ClampLow, ClampHigh bool
	// Negate changes the engineering value's sign, after clamping.
	Negate bool
	// Units are the engineering units, for people.
	Units string
}

func (s Scaling) check() error {
	switch {
	case s.Kind == NoScaling:
		return nil
	case s.Kind > SquareRoot:
		return fmt.Errorf("invalid scaling kind %d", s.Kind)
	case s.RawHigh == s.RawLow || math.IsNaN(s.RawHigh-s.RawLow):
		return errors.New("scaling: raw high and raw low are the same")
	case s.ScaledHigh == s.ScaledLow || math.IsNaN(s.ScaledHigh-s.ScaledLow):
		return errors.New("scaling: scaled high and scaled low are the same")
	}
	return nil
}

func (s Scaling) clamp(v float64) float64 {
	lo, hi := min(s.ScaledLow, s.ScaledHigh), max(s.ScaledLow, s.ScaledHigh)
	if s.ClampLow && v < lo {
		v = lo
	}
	if s.ClampHigh && v > hi {
		v = hi
	}
	return v
}

// Scale converts a raw value into the engineering value.
func (s Scaling) Scale(raw float64) float64 {
	if s.Kind == NoScaling {
		return raw
	}
	f := (raw - s.RawLow) / (s.RawHigh - s.RawLow)
	if s.Kind == SquareRoot {
		f = math.Sqrt(max(f, 0))
	}
	v := s.clamp(f*(s.ScaledHigh-s.ScaledLow) + s.ScaledLow)
	if s.Negate {
		v = -v
	}
	return v
}

// Unscale converts an engineering value into the raw value: Scale's inverse.
func (s Scaling) Unscale(v float64) float64 {
	if s.Kind == NoScaling {
		return v
	}
	if s.Negate {
		v = -v
	}
	f := (s.clamp(v) - s.ScaledLow) / (s.ScaledHigh - s.ScaledLow)
	if s.Kind == SquareRoot {
		f = max(f, 0)
		f *= f
	}
	return f*(s.RawHigh-s.RawLow) + s.RawLow
}

// rawNumber reads a numeric PLC4X value as a float64.
func rawNumber(v apiValues.PlcValue) (float64, error) {
	if v == nil || v.IsNull() {
		return 0, errors.New("device returned no value")
	}
	switch v.GetPlcValueType() {
	case apiValues.BYTE, apiValues.WORD, apiValues.DWORD, apiValues.LWORD,
		apiValues.USINT, apiValues.UINT, apiValues.UDINT, apiValues.ULINT,
		apiValues.SINT, apiValues.INT, apiValues.DINT, apiValues.LINT,
		apiValues.REAL, apiValues.LREAL:
		return v.GetFloat64(), nil
	}
	return 0, fmt.Errorf("scaling needs a number, the device returned %s", v.GetPlcValueType())
}

// engineering converts a scaled value into the tag's numeric type; whole
// types round half to even and refuse values out of their range.
func engineering(target reflect.Type, v float64) (any, error) {
	out := reflect.New(target).Elem()
	switch target.Kind() {
	case reflect.Float32, reflect.Float64:
		out.SetFloat(v)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		r := math.RoundToEven(v)
		if out.OverflowInt(int64(r)) || r < math.MinInt64 || r > math.MaxInt64 {
			return nil, fmt.Errorf("scaled value %g is out of %s's range", v, target)
		}
		out.SetInt(int64(r))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		r := math.RoundToEven(v)
		if r < 0 || r > math.MaxUint64 || out.OverflowUint(uint64(r)) {
			return nil, fmt.Errorf("scaled value %g is out of %s's range", v, target)
		}
		out.SetUint(uint64(r))
	default:
		return nil, fmt.Errorf("scaling needs a numeric tag, not %s", target)
	}
	return out.Interface(), nil
}

// engineeringNumber reads a numeric tag value as a float64.
func engineeringNumber(value any) (float64, error) {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Float32, reflect.Float64:
		return v.Float(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(v.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(v.Uint()), nil
	}
	return 0, fmt.Errorf("scaling needs a numeric tag, not %T", value)
}

// wholeTypes are the PLC4X value types that take whole numbers.
var wholeTypes = map[apiValues.PlcValueType]bool{
	apiValues.BYTE: true, apiValues.WORD: true, apiValues.DWORD: true, apiValues.LWORD: true,
	apiValues.USINT: true, apiValues.UINT: true, apiValues.UDINT: true, apiValues.ULINT: true,
	apiValues.SINT: true, apiValues.INT: true, apiValues.DINT: true, apiValues.LINT: true,
}

// rawForDevice is the value written for an unscaled raw number: a whole
// number (rounded half to even) for an address of a whole type, else the
// number as it is. It is given as text, which the driver's value handler
// reads as the address's type, and refuses when out of its range.
func rawForDevice(plcConn plc4go.PlcConnection, name, address string, raw float64) any {
	if req, err := plcConn.ReadRequestBuilder().AddTagAddress(name, address).Build(); err == nil {
		if tag := req.GetTag(name); tag != nil && wholeTypes[tag.GetValueType()] {
			return strconv.FormatFloat(math.RoundToEven(raw), 'f', 0, 64)
		}
	}
	return strconv.FormatFloat(raw, 'g', -1, 64)
}

// turn is a channel's right to send: one request at a time.
type turn struct {
	sem   chan struct{}
	delay time.Duration

	mu      sync.Mutex
	last    string    // the connection that asked last
	lastEnd time.Time // when its request ended
}

func newTurn(delay time.Duration) *turn {
	return &turn{sem: make(chan struct{}, 1), delay: delay}
}

// sleepCtx waits d, or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// acquire waits for the connection's turn on its channel and for its
// pauses (the channel's delay between devices, the connection's delay
// between requests). release ends the turn; it may be called more than
// once.
func (cs *connState) acquire(ctx context.Context) (release func(), err error) {
	t := cs.turn
	if t != nil {
		select {
		case t.sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		t.mu.Lock()
		var wait time.Duration
		if t.delay > 0 && t.last != "" && t.last != cs.Name {
			wait = t.delay - time.Since(t.lastEnd)
		}
		t.mu.Unlock()
		if err := sleepCtx(ctx, wait); err != nil {
			<-t.sem
			return nil, err
		}
	}
	if d := cs.InterRequestDelay; d > 0 {
		cs.lastMu.Lock()
		last := cs.lastEnd
		cs.lastMu.Unlock()
		if !last.IsZero() {
			if err := sleepCtx(ctx, d-time.Since(last)); err != nil {
				if t != nil {
					<-t.sem
				}
				return nil, err
			}
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			now := time.Now()
			cs.lastMu.Lock()
			cs.lastEnd = now
			cs.lastMu.Unlock()
			if t != nil {
				t.mu.Lock()
				t.last, t.lastEnd = cs.Name, now
				t.mu.Unlock()
				<-t.sem
			}
		})
	}, nil
}

// timeout is the connection's request timeout.
func (cs *connState) timeout() time.Duration {
	if cs.RequestTimeout > 0 {
		return cs.RequestTimeout
	}
	return requestTimeout
}

// tries is how often a request the device does not answer is sent.
func (cs *connState) tries() int { return max(cs.Attempts, 1) }

// demoteDue reports whether the connection has failed often enough to be
// demoted.
func (cs *connState) demoteDue() bool {
	return cs.Demotion.After > 0 && cs.failures >= cs.Demotion.After
}

func (cs *connState) demotionPeriod() time.Duration {
	if cs.Demotion.Period > 0 {
		return cs.Demotion.Period
	}
	return defaultDemotion
}

// errDemoted ends a session whose device is to be demoted.
var errDemoted = errors.New("demoted after successive failures")

// connect gets a PLC4X connection, within ConnectTimeout when one is set.
// A connection that arrives after the timeout is closed.
func (c *Connector) connect(ctx context.Context, cs *connState) (plc4go.PlcConnection, error) {
	if cs.ConnectTimeout <= 0 {
		return c.manager.GetConnection(ctx, cs.plc4xURL())
	}
	type result struct {
		conn plc4go.PlcConnection
		err  error
	}
	results := make(chan result, 1)
	go func() {
		conn, err := c.manager.GetConnection(ctx, cs.plc4xURL())
		results <- result{conn, err}
	}()
	late := func() {
		go func() {
			if r := <-results; r.err == nil && r.conn != nil {
				r.conn.Close()
			}
		}()
	}
	timer := time.NewTimer(cs.ConnectTimeout)
	defer timer.Stop()
	select {
	case r := <-results:
		return r.conn, r.err
	case <-timer.C:
		late()
		return nil, fmt.Errorf("no connection within %s", cs.ConnectTimeout)
	case <-ctx.Done():
		late()
		return nil, ctx.Err()
	}
}

// Capabilities says what a PLC4X driver can do.
type Capabilities struct {
	Protocol string
	// Subscribe: change-of-state and cyclic subscriptions.
	Subscribe bool
	// Events: unsolicited messages from the device (Event mode).
	Events bool
	// Browse: the device lists its addresses (Browse).
	Browse bool
	// Server: this connector can be the protocol's server (Server mode).
	Server bool
	// Discover: the driver finds devices on a network (Discover).
	Discover bool
	Note     string
}

// capabilities are the PLC4X drivers' abilities, as their connection
// metadata reports them (plc4go internal/<driver>/Connection.go), and the
// server side this package has.
var capabilities = map[string]Capabilities{
	"ab-eth":          {Subscribe: true, Events: true},
	"ads":             {Subscribe: true, Events: true, Browse: true},
	"bacnet-ip":       {Subscribe: true, Events: true},
	"c-bus":           {Subscribe: true, Events: true, Browse: true},
	"eip":             {},
	"firmata":         {Subscribe: true, Events: true},
	"iec-60870-5-104": {Subscribe: true, Events: true, Server: true, Note: "server: an IEC 60870-5-104 outstation over TCP (general interrogation, spontaneous transmission, commands)"},
	"knxnet-ip":       {Subscribe: true, Events: true, Browse: true},
	"logix":           {},
	"modbus-ascii":    {Server: true, Note: "server: a Modbus ASCII slave on a serial port"},
	"modbus-rtu":      {Server: true, Note: "server: a Modbus RTU slave on a serial port"},
	"modbus-tcp":      {Server: true, Note: "server: a Modbus TCP server"},
	"opcua":           {Subscribe: true, Events: true},
	"s7":              {Subscribe: true, Events: true, Browse: true, Server: true, Note: "server: an S7 server over ISO-on-TCP (read/write var on DB, M, I, Q); as a client, subscriptions and browsing only on PLCs with user-data services"},
	"slmp":            {Subscribe: true, Events: true},
	"umas":            {Subscribe: true, Events: true, Browse: true},
}

// CapabilitiesOf returns what the driver of a protocol can do; an unknown
// protocol can do nothing.
func CapabilitiesOf(protocol string) Capabilities {
	c := capabilities[protocol]
	c.Protocol = protocol
	c.Discover = discovers(protocol)
	return c
}

// BrowseItem is one address a device reported to Browse.
type BrowseItem struct {
	// Name is the item's name; children's are joined to their parent's
	// with dots.
	Name                             string
	Address                          string
	DataType                         string
	Readable, Writable, Subscribable bool
}

// Browse asks the device at url which addresses it has, with a driver
// query (empty asks for everything). It opens a connection of its own and
// closes it. A driver that cannot browse returns an error saying so.
func Browse(ctx context.Context, url, query string) ([]BrowseItem, error) {
	code := ProtocolOf(url)
	if !CapabilitiesOf(code).Browse {
		return nil, fmt.Errorf("plc4x: %s devices cannot list their addresses", code)
	}
	m, err := NewDriverManagerFor(code)
	if err != nil {
		return nil, err
	}
	conn, err := m.GetConnection(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("plc4x: browse: connect: %w", err)
	}
	defer conn.Close()
	if !conn.GetMetadata().CanBrowse() {
		return nil, fmt.Errorf("plc4x: this %s device cannot list its addresses", code)
	}
	if query == "" {
		query = "*"
	}
	req, err := conn.BrowseRequestBuilder().AddQuery("q", query).Build()
	if err != nil {
		return nil, fmt.Errorf("plc4x: browse: %w", err)
	}
	select {
	case r := <-req.Execute(ctx):
		if err := r.GetErr(); err != nil {
			return nil, fmt.Errorf("plc4x: browse: %w", err)
		}
		var out []BrowseItem
		for _, item := range r.GetResponse().GetQueryResults("q") {
			out = appendBrowse(out, "", item)
		}
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// appendBrowse appends an item and its children, depth first.
func appendBrowse(out []BrowseItem, parent string, item apiModel.PlcBrowseItem) []BrowseItem {
	name := item.GetName()
	if parent != "" {
		name = parent + "." + name
	}
	bi := BrowseItem{Name: name, Readable: item.IsReadable(), Writable: item.IsWritable(), Subscribable: item.IsSubscribable()}
	if tag := item.GetTag(); tag != nil {
		bi.Address = tag.GetAddressString()
		if vt := tag.GetValueType(); vt != apiValues.NULL {
			bi.DataType = vt.String()
		}
	}
	out = append(out, bi)
	children := item.GetChildren()
	names := make([]string, 0, len(children))
	for k := range children {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		out = appendBrowse(out, name, children[k])
	}
	return out
}

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
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	plc4go "github.com/apache/plc4x/plc4go/pkg/api"
	"github.com/apache/plc4x/plc4go/spi"
	"github.com/apache/plc4x/plc4go/spi/options"
	"github.com/apache/plc4x/plc4go/spi/transports"
	"github.com/apache/plc4x/plc4go/spi/transports/serial"
	"github.com/apache/plc4x/plc4go/spi/transports/tcp"
)

// The traffic view of a device (R-137): the bytes sent to it and received
// from it, with their times, for commissioning. The connector records
// them only when built WithTraffic, and only for a connection whose
// traffic is switched on (SetTraffic), into a ring of its own; nothing of
// it goes to a log or the journal.
//
// It works under PLC4X: the TCP and serial transports are wrapped before
// the drivers register theirs, so every byte a driver writes or reads on
// them passes through. Bytes read close together are one frame.

// Frame is one exchange's bytes in one direction.
type Frame struct {
	At   time.Time
	Sent bool // to the device; false: from it
	Data []byte
}

// maxTrafficFrames is how many frames a connection's ring keeps.
const maxTrafficFrames = 2000

// frameJoin: bytes read within this of the last are the same frame.
const frameJoin = 5 * time.Millisecond

// WithTraffic lets connections' traffic be recorded (SetTraffic). Without
// it the transports are PLC4X's own, untouched.
func WithTraffic() Option {
	return func(c *Connector) { c.traffic = &trafficHub{rings: map[string]*trafficRing{}} }
}

// ErrNoTraffic: the connector was not built WithTraffic.
var ErrNoTraffic = errors.New("plc4x: this connector records no traffic (WithTraffic)")

type trafficHub struct {
	mu    sync.Mutex
	rings map[string]*trafficRing // by transport address, while switched on
}

type trafficRing struct {
	frames []Frame
	first  int // the sequence of frames[0]
}

func (h *trafficHub) record(addr string, sent bool, data []byte) {
	if len(data) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rings[addr]
	if r == nil {
		return // not switched on
	}
	now := time.Now()
	if n := len(r.frames); !sent && n > 0 && !r.frames[n-1].Sent && now.Sub(r.frames[n-1].At) < frameJoin {
		r.frames[n-1].Data = append(r.frames[n-1].Data, data...)
		return
	}
	r.frames = append(r.frames, Frame{At: now, Sent: sent, Data: append([]byte(nil), data...)})
	if over := len(r.frames) - maxTrafficFrames; over > 0 {
		r.frames = r.frames[over:]
		r.first += over
	}
}

// trafficAddress is where a connection string's transport goes: host and
// port for TCP, the port's path for serial. PLC4X's transport URLs give
// the same.
func trafficAddress(raw string) string {
	rest := raw
	if p, r, ok := strings.Cut(raw, ":"); ok && p != "" {
		rest = r
	}
	rest = strings.TrimPrefix(rest, "//")
	if !strings.Contains(rest, "://") {
		rest = "x://" + rest
	}
	u, err := url.Parse(rest)
	if err != nil {
		return ""
	}
	return u.Host + u.Path
}

// SetTraffic starts or stops recording a connection's traffic. Starting
// empties its ring.
func (c *Connector) SetTraffic(name string, on bool) error {
	if c.traffic == nil {
		return ErrNoTraffic
	}
	cs, err := c.connection(name)
	if err != nil {
		return err
	}
	addr := trafficAddress(cs.plc4xURL())
	if addr == "" {
		return errors.New("plc4x: no transport address in the connection string")
	}
	c.traffic.mu.Lock()
	defer c.traffic.mu.Unlock()
	if on {
		c.traffic.rings[addr] = &trafficRing{}
	} else {
		delete(c.traffic.rings, addr)
	}
	return nil
}

// Traffic returns a connection's recorded frames from sequence since on,
// the next sequence to ask for, and whether it is being recorded.
func (c *Connector) Traffic(name string, since int) (frames []Frame, next int, on bool, err error) {
	if c.traffic == nil {
		return nil, 0, false, ErrNoTraffic
	}
	cs, err := c.connection(name)
	if err != nil {
		return nil, 0, false, err
	}
	c.traffic.mu.Lock()
	defer c.traffic.mu.Unlock()
	r := c.traffic.rings[trafficAddress(cs.plc4xURL())]
	if r == nil {
		return nil, since, false, nil
	}
	from := max(since-r.first, 0)
	for _, f := range r.frames[min(from, len(r.frames)):] {
		frames = append(frames, Frame{At: f.At, Sent: f.Sent, Data: append([]byte(nil), f.Data...)})
	}
	return frames, r.first + len(r.frames), true, nil
}

// registerTracing puts the recording transports in m before any driver
// registers its own (PLC4X keeps the first of a code).
func (h *trafficHub) registerTracing(m plc4go.PlcDriverManager) {
	ta, ok := m.(spi.TransportAware)
	if !ok {
		return // a manager that takes no transports: nothing is recorded
	}
	ta.RegisterTransport(&tracingTransport{Transport: tcp.NewTransport(), hub: h})
	ta.RegisterTransport(&tracingSerial{tracingTransport{Transport: serial.NewTransport(), hub: h}})
}

// tracingTransport wraps a transport's instances.
type tracingTransport struct {
	transports.Transport
	hub *trafficHub
}

func (t *tracingTransport) CreateTransportInstance(u url.URL, o map[string][]string, opts ...options.WithOption) (transports.TransportInstance, error) {
	in, err := t.Transport.CreateTransportInstance(u, o, opts...)
	if err != nil {
		return nil, err
	}
	return &tracingInstance{TransportInstance: in, hub: t.hub, addr: u.Host + u.Path}, nil
}

// tracingSerial keeps the serial transport's local-address form.
type tracingSerial struct{ tracingTransport }

func (t *tracingSerial) CreateTransportInstanceForLocalAddress(u url.URL, o map[string][]string, local *net.UDPAddr, opts ...options.WithOption) (transports.TransportInstance, error) {
	la, ok := t.Transport.(interface {
		CreateTransportInstanceForLocalAddress(url.URL, map[string][]string, *net.UDPAddr, ...options.WithOption) (transports.TransportInstance, error)
	})
	if !ok {
		return t.CreateTransportInstance(u, o, opts...)
	}
	in, err := la.CreateTransportInstanceForLocalAddress(u, o, local, opts...)
	if err != nil {
		return nil, err
	}
	return &tracingInstance{TransportInstance: in, hub: t.hub, addr: u.Host + u.Path}, nil
}

// tracingInstance records what is written and read.
type tracingInstance struct {
	transports.TransportInstance
	hub  *trafficHub
	addr string
}

func (i *tracingInstance) Write(ctx context.Context, data []byte) error {
	err := i.TransportInstance.Write(ctx, data)
	if err == nil {
		i.hub.record(i.addr, true, data)
	}
	return err
}

func (i *tracingInstance) Read(ctx context.Context, n uint32) ([]byte, error) {
	data, err := i.TransportInstance.Read(ctx, n)
	i.hub.record(i.addr, false, data)
	return data, err
}

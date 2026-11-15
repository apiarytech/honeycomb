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

// DNP3 outstation (beehive R-143), the connector's own code (package dnp3):
// PLC4X has no DNP3 driver. A server connection
//
//	dnp3://0.0.0.0:20000?address=10&master=1&events=1000
//
// answers DNP3 masters over TCP (port 20000 by default) as outstation
// address, from master only when master is set. Its bindings are points,
// "<kind>:<index>[:<class>]":
//
//   - BI:0 a binary input (g1v2, events g2v2 with time);
//   - AI:3 an analog input in single precision (g30v5, events g32v7);
//   - AI32:4 a 32-bit analog input (g30v1, events g32v3);
//   - BO:0 a binary output (status g10v2; commanded with a CROB, g12v1);
//   - AO:1 an analog output (status g40v3; commanded with g41v1 or g41v3).
//
// An input's class is 1 by default (1 to 3; 0 sends no events); its events
// carry the tag's own time and wait in a buffer of events until a master
// confirms them, so a master that comes back after an outage gets them.
// Outputs are Output bindings to report and InOut ones a master may
// command; select-before-operate is honoured. The TLS options of IEC
// 60870-5-104 apply (tls=true, tls-cert, tls-key, tls-ca, tls-roles): with
// roles on, only a master whose certificate carries the IEC 62351-8 role
// OPERATOR commands. A read-only interface reads only. Secure
// Authentication (IEC 62351-5) is not built.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	apiValues "github.com/apache/plc4x/plc4go/pkg/api/values"
	spiValues "github.com/apache/plc4x/plc4go/spi/values"
	"github.com/apiarytech/honeycomb"

	"github.com/apiarytech/honeycomb/connectors/plc4x/dnp3"
)

// ownProtocols are served by the connector's own code, not a PLC4X driver.
var ownProtocols = map[string]bool{"dnp3": true}

// dnp3Server is a DNP3 outstation connection.
type dnp3Server struct {
	listen   string
	readOnly []string
	tls      *tls.Config
	roles    bool
	out      *dnp3.Outstation
	src      *dnp3Source
}

// dnp3Source reads and commands the bindings' tags.
type dnp3Source struct {
	c    *Connector
	name string
	tags map[[2]int]string // kind, index -> tag
	mu   sync.Mutex
}

var dnp3Address = regexp.MustCompile(`^(BI|AI|AI32|BO|AO):(\d{1,5})(?::([0-3]))?$`)

var dnp3Kinds = map[string]dnp3.Kind{"BI": dnp3.BinaryInput, "AI": dnp3.AnalogInput, "AI32": dnp3.AnalogInt, "BO": dnp3.BinaryOutput, "AO": dnp3.AnalogOutput}

func newDNP3Server(c *Connector, conn Connection) (*dnp3Server, error) {
	_, host, port, q, err := parseServerTCPURL(conn.URL, "20000")
	if err != nil {
		return nil, fmt.Errorf("plc4x: server %q: %w", conn.Name, err)
	}
	s := &dnp3Server{listen: net.JoinHostPort(host, port), readOnly: readOnlyInterfaces(q)}
	num := func(key string, def, lo, hi int) (int, error) {
		v := q.Get(key)
		if v == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return 0, fmt.Errorf("plc4x: server %q: %s %q: %d to %d", conn.Name, key, v, lo, hi)
		}
		return n, nil
	}
	addr, err := num("address", -1, 0, 65519)
	if err != nil {
		return nil, err
	}
	if addr < 0 {
		return nil, fmt.Errorf("plc4x: server %q: no address: the outstation's link address, 0 to 65519", conn.Name)
	}
	master, err := num("master", 0, 0, 65519)
	if err != nil {
		return nil, err
	}
	events, err := num("events", 1000, 1, 100000)
	if err != nil {
		return nil, err
	}
	topts, err := parseTLS(q, host)
	if err != nil {
		return nil, fmt.Errorf("plc4x: server %q: %w", conn.Name, err)
	}
	if topts.on {
		if s.tls, err = topts.config(true); err != nil {
			return nil, fmt.Errorf("plc4x: server %q: %w", conn.Name, err)
		}
		s.roles = topts.roles
	}
	s.src = &dnp3Source{c: c, name: conn.Name, tags: map[[2]int]string{}}
	var points []dnp3.Point
	for _, b := range conn.Bindings {
		m := dnp3Address.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(b.Address)))
		if m == nil {
			return nil, fmt.Errorf("plc4x: server %q: tag %q: address %q: <kind>:<index>[:<class>], kinds BI, AI, AI32, BO, AO, e.g. BI:0 or AI:3:2", conn.Name, b.Tag, b.Address)
		}
		idx, _ := strconv.Atoi(m[2])
		if idx > 65535 {
			return nil, fmt.Errorf("plc4x: server %q: tag %q: index %d: 0 to 65535", conn.Name, b.Tag, idx)
		}
		p := dnp3.Point{Kind: dnp3Kinds[m[1]], Index: uint16(idx)}
		output := p.Kind == dnp3.BinaryOutput || p.Kind == dnp3.AnalogOutput
		switch {
		case output:
			if m[3] != "" {
				return nil, fmt.Errorf("plc4x: server %q: tag %q: an output has no event class", conn.Name, b.Tag)
			}
			p.Writable = b.Direction == InOut
		case b.Direction == InOut:
			return nil, fmt.Errorf("plc4x: server %q: tag %q: an input is read only: bind it as an output", conn.Name, b.Tag)
		default:
			p.Class = 1
			if m[3] != "" {
				c, _ := strconv.Atoi(m[3])
				p.Class = uint8(c)
			}
		}
		key := [2]int{int(p.Kind), idx}
		if _, dup := s.src.tags[key]; dup {
			return nil, fmt.Errorf("plc4x: server %q: two points at %s:%d", conn.Name, m[1], idx)
		}
		s.src.tags[key] = b.Tag
		points = append(points, p)
	}
	s.out, err = dnp3.New(dnp3.Config{Address: uint16(addr), Master: uint16(master), Points: points, Source: s.src, Events: events,
		OnError: func(err error) { c.onError(conn.Name, err) }})
	if err != nil {
		return nil, fmt.Errorf("plc4x: server %q: %w", conn.Name, err)
	}
	return s, nil
}

// Read is a point's tag: its value, whether it is good, its time.
func (s *dnp3Source) Read(p dnp3.Point) (dnp3.Value, bool) {
	tag, ok := s.tags[[2]int{int(p.Kind), int(p.Index)}]
	if !ok {
		return dnp3.Value{}, false
	}
	t, ok := s.c.db.GetTag(tag)
	if !ok {
		return dnp3.Value{}, false
	}
	raw, err := s.c.db.GetTagValue(tag)
	if err != nil || raw == nil {
		return dnp3.Value{}, false
	}
	return dnp3.Value{Bool: asBool(raw), Number: asFloat(raw), Online: t.Quality != honeycomb.QualityBad, Time: t.Timestamp}, true
}

// Operate writes a commanded output to its tag.
func (s *dnp3Source) Operate(p dnp3.Point, v dnp3.Value, by string) uint8 {
	tag := s.tags[[2]int{int(p.Kind), int(p.Index)}]
	var pv apiValues.PlcValue = spiValues.NewPlcLREAL(v.Number)
	if p.Kind == dnp3.BinaryOutput {
		pv = spiValues.NewPlcBOOL(v.Bool)
	}
	typed, ex := s.c.serverValue(serverPoint{tag: tag}, pv)
	if ex != 0 {
		return dnp3.StatusNotSupported
	}
	if s.c.serverSet(s.name, by, []pending{{tag: tag, value: typed}}) != 0 {
		return dnp3.StatusNotSupported
	}
	return dnp3.StatusSuccess
}

// serveDNP3 answers masters until ctx ends or the listener fails; the
// outstation records its events meanwhile.
func (c *Connector) serveDNP3(ctx context.Context, cs *connState) error {
	s := cs.server.dnp3
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.listen, err)
	}
	if s.tls != nil {
		ln = tls.NewListener(ln, s.tls)
	}
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer func() {
		if stop() {
			ln.Close()
		}
	}()
	c.record(cs.Name, func(st *Status) { st.Connected = true }, nil)
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(iecScanInterval)
		defer t.Stop()
		for {
			s.out.Scan()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	slots := make(chan struct{}, maxTCPClients)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept on %s: %w", s.listen, err)
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			commands := writesFromReadOnly(s.readOnly, conn.LocalAddr())
			who := "dnp3 master " + conn.RemoteAddr().String()
			if s.tls != nil {
				ok, name, err := stationRights(conn, s.roles)
				if err != nil {
					c.onError(cs.Name, fmt.Errorf("dnp3 master %s refused: %w", conn.RemoteAddr(), err))
					conn.Close()
					return
				}
				commands = commands && ok
				if name != "" {
					who = name + " at " + conn.RemoteAddr().String()
				}
			}
			c.record(cs.Name, func(st *Status) { st.Reads++; st.LastRead = time.Now() }, nil)
			s.out.Serve(ctx, conn, commands, who)
		}()
	}
}
